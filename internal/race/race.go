package race

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"maps"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

// Timing rules (docs/protocol.md, "Race rules (server)" and "Eligibility and thresholds").
const (
	loadTimeout       = 45 * time.Second
	goDelay           = 3500 * time.Millisecond
	afterFirstFinish  = 30 * time.Second
	raceTimeLimit     = 20 * time.Minute
	battleTimeLimit   = 4 * time.Minute
	maxEligibleRTT    = 200 * time.Millisecond
	reportWindow      = 15 * time.Second
	strikeWindow      = 30 * time.Second
	strikesToKick     = 5
	hitGrace          = 300 * time.Millisecond
	finishTolerance   = time.Second
	zeroHealthLimit   = 2 * time.Second
	unknownWindow     = 2 * time.Second
	projectileEvery   = 500 * time.Millisecond
	projectileSpread  = 2.0 // m: correct when a report is farther from the median
	outlierMin        = 4.0 // m
	bridgeWindow      = 150 * time.Millisecond
	pickupSlack       = 100 * time.Millisecond
	pickupMinRadius   = 8.0 // m
	pickupExtraRadius = 4.0 // m
	rouletteSlack     = 100 * time.Millisecond
	ticksPerSecond    = 60
	specialBox        = "@special"
)

// env is how a race acts on the world; the Engine implements it, unit tests record it.
type env interface {
	push(accountID uint64, typ proto.Type, m proto.Message)
	sendUDP(addr netip.AddrPort, b []byte)
	dropSeat(s *seat) // stop serving the seat's UDP token
	going(raceID uint64)
	finished(raceID uint64, placements []Placement)
	cheatKick(k store.CheatKick)
}

type phase uint8

const (
	phaseLoading phase = iota
	phaseRacing        // GO scheduled (countdown included)
	phaseEnded
)

type use struct {
	owner     uint8
	powerupID string
	proj      *projectile // nil once tracking ended
}

type hitKey struct {
	victim uint8
	useID  uint32
}

type hitClaim struct {
	attacker  uint8
	effectID  string
	decideAt  time.Time
	claimers  map[uint8]bool
	forced    bool
	confirmBy time.Time
}

type cheatVote struct {
	at     time.Time
	reason proto.FlagReason
}

type unknownClaim struct {
	first     time.Time
	reporters map[uint8]bool
	firstBy   uint8
}

type bridge struct {
	tick     uint32
	decideAt time.Time
	done     bool
}

// raceState is one race's rules engine. It is driven by one goroutine (or a test) and never blocks: every input
// carries the time it happened, outputs go through env.
type raceState struct {
	id    uint64
	spec  Spec
	env   env
	cat   Catalog
	rng   *rand.Rand
	seats []*seat

	phase   phase
	started time.Time
	goAt    time.Time

	agreed      *proto.RaceLoaded
	isEffect    map[string]bool
	boxFreeAt   []time.Time
	uses        map[uint32]*use
	hitsTaken   map[hitKey]bool
	claims      map[hitKey]*hitClaim
	reports     map[uint8]map[uint8]cheatVote // suspect → reporter → latest report
	unknown     map[uint8]*unknownClaim       // racer → claims about their content
	bridges     map[uint16]*bridge
	firstFinish time.Time
	elimCount   int

	worldBuf []byte
}

func newRaceState(id uint64, spec Spec, seats []*seat, e env, cat Catalog, seed [32]byte, now time.Time) *raceState {
	return &raceState{
		id:        id,
		spec:      spec,
		env:       e,
		cat:       cat,
		rng:       rand.New(rand.NewChaCha8(seed)),
		seats:     seats,
		started:   now,
		uses:      map[uint32]*use{},
		hitsTaken: map[hitKey]bool{},
		claims:    map[hitKey]*hitClaim{},
		reports:   map[uint8]map[uint8]cheatVote{},
		unknown:   map[uint8]*unknownClaim{},
		bridges:   map[uint16]*bridge{},
		worldBuf:  make([]byte, 0, proto.UDPMaxDatagram),
	}
}

func (rs *raceState) seatOf(accountID uint64) *seat {
	for _, s := range rs.seats {
		if s.racer.AccountID == accountID {
			return s
		}
	}
	return nil
}

func (rs *raceState) seatAt(slot uint8) *seat {
	if int(slot) < len(rs.seats) {
		return rs.seats[slot]
	}
	return nil
}

// arbitrated reports whether items, hits and projectiles are judged: never in solo races or before agreement.
func (rs *raceState) arbitrated() bool { return !rs.spec.Solo && rs.phase == phaseRacing }

func (rs *raceState) raceTick(now time.Time) uint32 {
	if now.Before(rs.goAt) {
		return 0
	}
	return uint32(now.Sub(rs.goAt) * ticksPerSecond / time.Second)
}

// pushAll sends m to every racer still in the race, except skip (nil = nobody).
func (rs *raceState) pushAll(typ proto.Type, m proto.Message, skip *seat) {
	for _, s := range rs.seats {
		if !s.out && s != skip {
			rs.env.push(s.racer.AccountID, typ, m)
		}
	}
}

func (rs *raceState) rttOK(s *seat) bool {
	d, ok := s.rtt()
	return ok && d <= maxEligibleRTT
}

// eligible: a racer whose checks, reports and claims count.
func (rs *raceState) eligible(s *seat) bool { return !s.out && !s.everKicked && rs.rttOK(s) }

// observerThreshold is max(2, ⌈eligible observers of x / 2⌉).
func (rs *raceState) observerThreshold(x *seat) int {
	n := 0
	for _, s := range rs.seats {
		if s != x && rs.eligible(s) {
			n++
		}
	}
	return max(2, (n+1)/2)
}

// --- loading ---

// loadHash is the canonical hash of what racers must agree on. Pool and specials are compared as sets (sorted);
// boxes and effects are index tables, so their order matters.
func loadHash(m *proto.RaceLoaded) [32]byte {
	c := *m
	c.Pool = slices.Clone(m.Pool)
	slices.SortStableFunc(c.Pool, func(a, b proto.PoolEntry) int { return strings.Compare(a.PowerupID, b.PowerupID) })
	c.Specials = slices.Clone(m.Specials)
	slices.SortStableFunc(c.Specials, func(a, b proto.Special) int { return cmp.Compare(a.Slot, b.Slot) })
	w := wire.NewWriter(1024)
	c.EncodeContent(w)
	return sha256.Sum256(w.Bytes())
}

func (rs *raceState) onLoaded(s *seat, m *proto.RaceLoaded, now time.Time) {
	if rs.phase != phaseLoading || s.loaded != nil {
		return
	}
	s.loaded, s.loadedHash = m, loadHash(m)
	rs.maybeDecideLoad(now)
}

func (rs *raceState) maybeDecideLoad(now time.Time) {
	if rs.phase != phaseLoading {
		return
	}
	for _, s := range rs.seats {
		if !s.out && s.loaded == nil {
			return
		}
	}
	rs.decideLoad(now)
}

// majority returns the hash with strictly the most racers; on a tie for the top count it returns ok false.
func majority(hashes [][32]byte) (best [32]byte, ok bool) {
	counts := map[[32]byte]int{}
	for _, h := range hashes {
		counts[h]++
	}
	top, tied := 0, false
	for h, n := range counts {
		switch {
		case n > top:
			best, top, tied = h, n, false
		case n == top:
			tied = true
		}
	}
	return best, top > 0 && !tied
}

// decideLoad runs once every racer loaded or the load timeout passed: the majority content wins, dissenters leave,
// GO is scheduled.
func (rs *raceState) decideLoad(now time.Time) {
	var loaded []*seat
	var hashes [][32]byte
	for _, s := range rs.seats {
		if !s.out && s.loaded != nil {
			loaded = append(loaded, s)
			hashes = append(hashes, s.loadedHash)
		}
	}
	if len(loaded) == 0 {
		rs.end(now)
		return
	}
	best, ok := majority(hashes)
	if !ok {
		// Nobody can be shown to be the odd one out: everybody races with the lowest slot's content.
		best = loaded[0].loadedHash
	}
	var agreed *seat
	for _, s := range loaded {
		if s.loadedHash == best && agreed == nil {
			agreed = s
		}
	}
	rs.agreed = agreed.loaded
	rs.isEffect = make(map[string]bool, len(rs.agreed.Effects))
	for _, e := range rs.agreed.Effects {
		rs.isEffect[e] = true
	}
	rs.boxFreeAt = make([]time.Time, len(rs.agreed.Boxes))
	// Racing before dissenters leave, so their leaving does not decide the load again.
	rs.phase = phaseRacing
	rs.goAt = now.Add(goDelay)
	if ok {
		rs.dropDissenters(loaded, best, now)
	}
	if rs.phase == phaseEnded {
		return
	}
	rs.pushAll(proto.PushRaceGo, &proto.RaceGo{RaceID: rs.id, Go: rs.goAt}, nil)
	rs.env.going(rs.id)
}

func (rs *raceState) dropDissenters(loaded []*seat, best [32]byte, now time.Time) {
	pack := packOf(rs.spec.TrackID)
	majorityCommit := ""
	if rs.cat != nil {
		commits := map[string]int{}
		for _, s := range loaded {
			if s.loadedHash == best {
				commits[rs.cat.PackCommit(s.racer.AccountID, pack)]++
			}
		}
		top := 0
		for c, n := range commits {
			if n > top || (n == top && c < majorityCommit) {
				majorityCommit, top = c, n
			}
		}
	}
	for _, s := range loaded {
		if s.loadedHash == best {
			continue
		}
		// Same pack version but different content: the copy was modified (or broken), which moderators see.
		if rs.cat != nil && majorityCommit != "" && rs.cat.PackCommit(s.racer.AccountID, pack) == majorityCommit {
			rs.cat.Strike(s.racer.AccountID, pack, "race content of "+rs.spec.TrackID+
				" differs from other players with the same pack version")
		}
		rs.leave(s, OutcomeDesync, "Your game content differs from the other players' (track, items or rules).", now)
	}
}

// packOf is the namespace of a content id ("mhk2.egypt" → "mhk2"): ids are namespaced by the pack that first
// defines them.
func packOf(id string) string {
	if i := strings.IndexByte(id, '.'); i > 0 {
		return id[:i]
	}
	return id
}

// --- leaving, strikes, kicks ---

// leave takes s out of the race. Idempotent.
func (rs *raceState) leave(s *seat, outcome Outcome, message string, now time.Time) {
	if s.out || rs.phase == phaseEnded {
		return
	}
	msg := &proto.PlayerLeftRace{RaceID: rs.id, Slot: s.slot, Outcome: uint8(outcome), Message: message}
	rs.pushAll(proto.PushPlayerLeftRace, msg, nil)
	s.out, s.outcome, s.outAt = true, outcome, now
	s.held, s.charges = "", 0
	rs.env.dropSeat(s)
	if rs.phase == phaseLoading {
		rs.maybeDecideLoad(now)
	}
	rs.checkEnd(now)
}

// strike adds a server-side anticheat strike; 5 within 30 s kick. Anticheat-off races never strike.
func (rs *raceState) strike(s *seat, why string, now time.Time) {
	if !rs.spec.Anticheat || rs.spec.Solo || s.out {
		return
	}
	keep := 0
	for i, t := range s.strikes {
		if now.Sub(t) < strikeWindow {
			s.strikes[keep], s.strikeWhy[keep] = t, s.strikeWhy[i]
			keep++
		}
	}
	s.strikes = append(s.strikes[:keep], now)
	s.strikeWhy = append(s.strikeWhy[:keep], why)
	if len(s.strikes) >= strikesToKick {
		rs.kickCheater(s, "server strikes: "+strings.Join(s.strikeWhy, ", "), "server", now)
	}
}

// kickCheater records the kick and removes the racer (outcome kicked: cheating).
func (rs *raceState) kickCheater(s *seat, reason, reporters string, now time.Time) {
	if s.out {
		return
	}
	rs.env.cheatKick(store.CheatKick{
		AccountID: s.racer.AccountID,
		LobbyRef:  formatID(rs.spec.LobbyID),
		RaceRef:   formatID(rs.id),
		Reason:    truncate(reason, 255),
		Reporters: truncate(reporters, 1024),
		CreatedAt: now,
	})
	s.everKicked = true
	rs.leave(s, OutcomeCheatKick, "Removed from the race: cheating detected.", now)
}

// --- items ---

func (rs *raceState) deny(s *seat, box uint16, reason uint8) {
	rs.env.push(s.racer.AccountID, proto.PushItemDenied, &proto.ItemDenied{RaceID: rs.id, BoxID: box, Reason: reason})
}

func (rs *raceState) onPickup(s *seat, m *proto.ItemPickup, now time.Time) {
	if !rs.arbitrated() || !rs.spec.Items {
		return
	}
	if int(m.BoxID) >= len(rs.agreed.Boxes) || now.Before(rs.goAt) || s.eliminated {
		rs.deny(s, m.BoxID, proto.DenyNotAvailable)
		return
	}
	box := &rs.agreed.Boxes[m.BoxID]
	if rs.isEffect[box.Fixed] || now.Before(rs.boxFreeAt[m.BoxID]) {
		// Effect boxes are applied by each client and never arbitrated.
		rs.deny(s, m.BoxID, proto.DenyNotAvailable)
		return
	}
	if !rs.nearBox(s, box.Pos) {
		rs.deny(s, m.BoxID, proto.DenyTooFar)
		rs.strike(s, "item pickup too far from the box", now)
		return
	}
	if s.charges > 0 && !rs.agreed.Rules.ReplacesHeld {
		rs.deny(s, m.BoxID, proto.DenyCannotReceive)
		return
	}
	id, charges := rs.roll(s, box.Fixed)
	if id == "" {
		rs.deny(s, m.BoxID, proto.DenyNotAvailable)
		return
	}
	rs.boxFreeAt[m.BoxID] = now.Add(time.Duration(box.RespawnTenths) * 100 * time.Millisecond)
	s.held, s.charges = id, charges
	s.usableAt = now.Add(time.Duration(rs.agreed.Rules.RouletteMs)*time.Millisecond - rouletteSlack)
	rs.env.push(s.racer.AccountID, proto.PushItemGranted, &proto.ItemGranted{RaceID: rs.id, BoxID: m.BoxID,
		PowerupID: id, Charges: charges, Tick: m.Tick})
	rs.pushAll(proto.PushItemBoxTaken, &proto.ItemBoxTaken{RaceID: rs.id, Slot: s.slot, BoxID: m.BoxID,
		Tick: m.Tick}, s)
}

// nearBox checks the racer's latest UDP position against the box: max(8 m, speed × (RTT + 100 ms) + 4 m).
func (rs *raceState) nearBox(s *seat, box proto.Vec3) bool {
	if !s.hasState {
		return false
	}
	radius := max(pickupMinRadius, s.state.Speed()*(s.rttOr(0)+pickupSlack).Seconds()+pickupExtraRadius)
	return dist(s.state.Pos, box) <= radius
}

// roll picks the powerup for a box: fixed id, the slot's special, or a weighted draw at the racer's position.
func (rs *raceState) roll(s *seat, fixed string) (string, uint8) {
	switch fixed {
	case "":
	case specialBox:
		for _, sp := range rs.agreed.Specials {
			if sp.Slot == s.slot && sp.PowerupID != "" {
				return sp.PowerupID, rs.chargesOf(sp.PowerupID)
			}
		}
	default:
		return fixed, rs.chargesOf(fixed)
	}
	i := drawPool(rs.rng, rs.agreed.Pool, rs.spec.ItemOdds, rs.positionFraction(s))
	if i < 0 {
		return "", 0
	}
	return rs.agreed.Pool[i].PowerupID, rs.chargesOf(rs.agreed.Pool[i].PowerupID)
}

func (rs *raceState) chargesOf(id string) uint8 {
	for i := range rs.agreed.Pool {
		if p := &rs.agreed.Pool[i]; p.PowerupID == id {
			c := p.Charges
			if rs.spec.Mode == ModeBattle {
				c = p.BattleCharges
			}
			return max(c, 1)
		}
	}
	return 1
}

// ranking returns the racers still in the race, leader first: finishers in finish order, then by raceDistance.
func (rs *raceState) ranking() []*seat {
	var in []*seat
	for _, s := range rs.seats {
		if !s.out {
			in = append(in, s)
		}
	}
	slices.SortStableFunc(in, func(a, b *seat) int {
		switch {
		case a.finished && b.finished:
			return cmp.Or(cmp.Compare(a.finishMs, b.finishMs), a.finishAt.Compare(b.finishAt))
		case a.finished:
			return -1
		case b.finished:
			return 1
		}
		return cmp.Compare(b.state.RaceDistance, a.state.RaceDistance)
	})
	return in
}

// positionFraction is (position − 1) / (racers − 1); battles and single racers use 0.5.
func (rs *raceState) positionFraction(s *seat) float64 {
	if rs.spec.Mode == ModeBattle {
		return 0.5
	}
	ranked := rs.ranking()
	if len(ranked) < 2 {
		return 0.5
	}
	pos := slices.Index(ranked, s)
	return float64(pos) / float64(len(ranked)-1)
}

// poolWeight interpolates the 11 weights linearly at position fraction f.
func poolWeight(w *[proto.PoolWeights]float32, f float64) float64 {
	x := min(max(f, 0), 1) * float64(proto.PoolWeights-1)
	lo := int(x)
	if lo >= proto.PoolWeights-1 {
		return float64(w[proto.PoolWeights-1])
	}
	t := x - float64(lo)
	return float64(w[lo]) + (float64(w[lo+1])-float64(w[lo]))*t
}

// drawPool draws a pool index by weight (−1 when nothing has weight). Custom odds replace the position weights:
// listed powerups weigh their percent everywhere, unlisted ones are off.
func drawPool(rng *rand.Rand, pool []proto.PoolEntry, odds []proto.ItemOdds, f float64) int {
	weights := make([]float64, len(pool))
	total := 0.0
	for i := range pool {
		w := poolWeight(&pool[i].Weights, f)
		if len(odds) > 0 {
			w = 0
			for _, o := range odds {
				if o.PowerupID == pool[i].PowerupID {
					w = float64(o.Percent)
				}
			}
		}
		if w > 0 {
			weights[i] = w
			total += w
		}
	}
	if total <= 0 {
		return -1
	}
	x := rng.Float64() * total
	last := -1
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		last = i
		if x < w {
			return i
		}
		x -= w
	}
	return last
}

func (rs *raceState) onUse(s *seat, m *proto.ItemUse, now time.Time) {
	if !rs.arbitrated() || s.eliminated {
		return
	}
	reject := func(reason uint8) {
		rs.env.push(s.racer.AccountID, proto.PushItemUseRejected,
			&proto.ItemUseRejected{RaceID: rs.id, UseID: m.UseID, Reason: reason})
	}
	_, reused := rs.uses[m.UseID]
	if m.UseID>>24 != uint32(s.slot)+1 || m.UseID&0xFFFFFF == 0 || reused || s.charges == 0 {
		reject(proto.RejectNotHeld)
		rs.strike(s, "item use without holding it", now)
		return
	}
	if now.Before(s.usableAt) {
		reject(proto.RejectCooldown)
		return
	}
	u := &use{owner: s.slot, powerupID: s.held}
	u.proj = &projectile{pending: map[uint8]pendingReport{}, reporters: map[uint8]bool{}, gone: map[uint8]bool{},
		struck: map[uint8]bool{}}
	rs.uses[m.UseID] = u
	s.charges--
	if s.charges == 0 {
		s.held = ""
	}
	rs.pushAll(proto.PushItemUsed, &proto.ItemUsed{RaceID: rs.id, Slot: s.slot, UseID: m.UseID, PowerupID: u.powerupID,
		Tick: m.Tick, Pos: m.Pos, Rot: m.Rot, Backwards: m.Backwards, TargetSlot: m.TargetSlot}, s)
}

// --- hits ---

func (rs *raceState) onHitTaken(s *seat, m *proto.HitTaken, now time.Time) {
	if !rs.arbitrated() {
		return
	}
	if m.AttackerSlot != proto.NoSlot && rs.seatAt(m.AttackerSlot) == nil {
		return
	}
	if m.UseID != 0 {
		k := hitKey{s.slot, m.UseID}
		if rs.hitsTaken[k] {
			return
		}
		rs.hitsTaken[k] = true
		delete(rs.claims, k)
	}
	rs.pushAll(proto.PushHitApplied, &proto.HitApplied{RaceID: rs.id, VictimSlot: s.slot, AttackerSlot: m.AttackerSlot,
		UseID: m.UseID, EffectID: m.EffectID, Tick: m.Tick}, s)
}

func (rs *raceState) onHitSeen(obs *seat, m *proto.HitSeen, now time.Time) {
	if !rs.arbitrated() || m.UseID == 0 {
		return
	}
	victim := rs.seatAt(m.VictimSlot)
	if victim == nil || victim == obs || victim.out || rs.uses[m.UseID] == nil {
		return
	}
	k := hitKey{victim.slot, m.UseID}
	if rs.hitsTaken[k] {
		return
	}
	c := rs.claims[k]
	if c == nil {
		c = &hitClaim{attacker: m.AttackerSlot, effectID: m.EffectID, claimers: map[uint8]bool{},
			decideAt: now.Add(victim.rttOr(maxEligibleRTT) + hitGrace)}
		rs.claims[k] = c
	}
	if !c.forced && !now.After(c.decideAt) && rs.eligible(obs) {
		c.claimers[obs.slot] = true
	}
}

// tickHits forces hits enough eligible observers saw and strikes victims that ignore the force.
func (rs *raceState) tickHits(now time.Time) {
	for k, c := range rs.claims {
		victim := rs.seats[k.victim]
		switch {
		case victim.out:
			delete(rs.claims, k)
		case !c.forced && !now.Before(c.decideAt):
			if len(c.claimers) < rs.observerThreshold(victim) {
				delete(rs.claims, k)
				continue
			}
			c.forced = true
			c.confirmBy = now.Add(victim.rttOr(maxEligibleRTT) + hitGrace)
			rs.env.push(victim.racer.AccountID, proto.PushHitForce, &proto.HitForce{RaceID: rs.id,
				AttackerSlot: c.attacker, UseID: k.useID, EffectID: c.effectID})
		case c.forced && !now.Before(c.confirmBy):
			delete(rs.claims, k)
			rs.strike(victim, "ignored a forced hit", now)
		}
	}
}

// --- finish, battles, bridges ---

func (rs *raceState) onFinished(s *seat, m *proto.Finished, now time.Time) {
	if rs.phase != phaseRacing || s.finished || now.Before(rs.goAt) {
		return
	}
	if rs.spec.Solo {
		s.finished, s.finishAt, s.lapMs = true, now, m.LapMs
		s.finishMs = m.FinishMs
		if s.finishMs <= 0 {
			s.finishMs = int32(now.Sub(rs.goAt).Milliseconds())
		}
		s.soloPosition = clampPosition(m.Position, rs.spec.AICount)
		rs.end(now)
		return
	}
	if rs.spec.Mode == ModeBattle || s.eliminated {
		return
	}
	elapsed := now.Sub(rs.goAt)
	s.finishMs = m.FinishMs
	if !finishPlausible(m, elapsed, s.rttOr(0), rs.agreed.Laps) {
		rs.strike(s, "finish time or laps disagree with the server", now)
		s.finishMs = int32(elapsed.Milliseconds())
	}
	s.finished, s.finishAt, s.lapMs = true, now, m.LapMs
	if rs.firstFinish.IsZero() {
		rs.firstFinish = now
	}
	rs.pushAll(proto.PushPlayerFinished, &proto.PlayerFinished{RaceID: rs.id, Slot: s.slot, FinishMs: s.finishMs,
		LapMs: s.lapMs}, nil)
	rs.checkEnd(now)
}

// finishPlausible: the claimed time is within RTT + 1 s of the server's clock since GO and the lap count matches.
func finishPlausible(m *proto.Finished, elapsed, rtt time.Duration, laps uint8) bool {
	diff := time.Duration(m.FinishMs)*time.Millisecond - elapsed
	return diff.Abs() <= rtt+finishTolerance && len(m.LapMs) == int(laps)
}

// clampPosition maps a solo racer's reported place into 1..AI+1 (0 or out of range: last).
func clampPosition(p, ai uint8) uint8 {
	if p == 0 || int(p) > int(ai)+1 {
		return ai + 1
	}
	return p
}

func (rs *raceState) onEliminatedSelf(s *seat, m *proto.EliminatedSelf, now time.Time) {
	if !rs.arbitrated() || rs.spec.Mode != ModeBattle || now.Before(rs.goAt) || s.eliminated {
		return
	}
	rs.eliminate(s, m.AttackerSlot, m.Tick, now)
}

func (rs *raceState) eliminate(s *seat, attacker uint8, tick uint32, now time.Time) {
	rs.elimCount++
	s.eliminated, s.elimOrder, s.elimAt = true, rs.elimCount, now
	s.held, s.charges = "", 0
	rs.pushAll(proto.PushEliminated, &proto.Eliminated{RaceID: rs.id, Slot: s.slot, AttackerSlot: attacker,
		Tick: tick}, nil)
	rs.checkEnd(now)
}

// tickHealth eliminates battle racers whose own states have shown health 0 for 2 s without EliminatedSelf: what
// every observer renders is the victim's own state, so its health is the observers' view.
func (rs *raceState) tickHealth(now time.Time) {
	if rs.spec.Mode != ModeBattle || !rs.arbitrated() || now.Before(rs.goAt) {
		return
	}
	for _, s := range rs.seats {
		if s.out || s.eliminated || !s.hasState || s.state.Health > 0 {
			s.zeroHealthFrom = time.Time{}
			continue
		}
		if s.zeroHealthFrom.IsZero() {
			s.zeroHealthFrom = now
		} else if now.Sub(s.zeroHealthFrom) >= zeroHealthLimit {
			rs.strike(s, "did not report its elimination", now)
			if !s.out {
				rs.eliminate(s, proto.NoSlot, rs.raceTick(now), now)
			}
		}
	}
}

func (rs *raceState) onBridgeTrigger(s *seat, m *proto.BridgeTrigger, now time.Time) {
	if rs.phase != phaseRacing || now.Before(rs.goAt) {
		return
	}
	b := rs.bridges[m.BridgeID]
	switch {
	case b == nil:
		// Collect briefly so the lowest tick wins, not the closest client.
		rs.bridges[m.BridgeID] = &bridge{tick: m.Tick, decideAt: now.Add(bridgeWindow)}
	case !b.done && m.Tick < b.tick:
		b.tick = m.Tick
	}
}

func (rs *raceState) tickBridges(now time.Time) {
	for id, b := range rs.bridges {
		if !b.done && !now.Before(b.decideAt) {
			b.done = true
			rs.pushAll(proto.PushBridgeTriggered, &proto.BridgeTriggered{RaceID: rs.id, BridgeID: id, Tick: b.tick}, nil)
		}
	}
}

// --- anticheat reports, unknown content ---

func (rs *raceState) onCheatReport(rep *seat, m *proto.CheatReport, now time.Time) {
	if !rs.spec.Anticheat || !rs.arbitrated() || !m.Reason.Valid() {
		return
	}
	suspect := rs.seatAt(m.SuspectSlot)
	if suspect == nil || suspect == rep || suspect.out || !rs.eligible(rep) || !rs.rttOK(suspect) {
		return
	}
	votes := rs.reports[suspect.slot]
	if votes == nil {
		votes = map[uint8]cheatVote{}
		rs.reports[suspect.slot] = votes
	}
	votes[rep.slot] = cheatVote{at: now, reason: m.Reason}
	var reporters []string
	reasons := map[string]bool{}
	for slot, v := range votes {
		r := rs.seats[slot]
		if now.Sub(v.at) > reportWindow || !rs.eligible(r) {
			delete(votes, slot)
			continue
		}
		reporters = append(reporters, formatID(r.racer.AccountID))
		reasons[v.reason.String()] = true
	}
	if len(reporters) < rs.observerThreshold(suspect) {
		return
	}
	slices.Sort(reporters)
	why := slices.Sorted(maps.Keys(reasons))
	delete(rs.reports, suspect.slot)
	rs.kickCheater(suspect, "cheat reports: "+strings.Join(why, ", "), strings.Join(reporters, ","), now)
}

func (rs *raceState) onUnknownContent(rep *seat, m *proto.UnknownContent, now time.Time) {
	if rs.spec.Solo || rs.phase == phaseEnded {
		return
	}
	x := rs.seatAt(m.Slot)
	if x == nil || x == rep || x.out {
		return
	}
	c := rs.unknown[x.slot]
	if c == nil {
		c = &unknownClaim{first: now, reporters: map[uint8]bool{}, firstBy: rep.slot}
		rs.unknown[x.slot] = c
	}
	c.reporters[rep.slot] = true
	if len(c.reporters) < 2 {
		return
	}
	delete(rs.unknown, x.slot)
	// Content absent from the catalog majority of x's own pack version cannot come from an honest copy.
	if rs.spec.Anticheat && rs.cat != nil && !rs.cat.InMajority(x.racer.AccountID, packOf(m.ID), m.Kind, m.ID) {
		var reporters []string
		for slot := range c.reporters {
			reporters = append(reporters, formatID(rs.seats[slot].racer.AccountID))
		}
		slices.Sort(reporters)
		rs.kickCheater(x, "fabricated content "+m.ID, strings.Join(reporters, ","), now)
		return
	}
	rs.leave(x, OutcomeDesync, "Other players do not have some of your game content.", now)
}

func (rs *raceState) tickUnknown(now time.Time) {
	for slot, c := range rs.unknown {
		if now.Sub(c.first) < unknownWindow {
			continue
		}
		delete(rs.unknown, slot)
		// Only one racer misses x's content: the reporter's copy is the odd one.
		if rep := rs.seats[c.firstBy]; len(c.reporters) == 1 && !rep.out {
			rs.leave(rep, OutcomeDesync, "Your game is missing content the other players have.", now)
		}
	}
}

// --- UDP state, World ---

// onState stores a racer's latest kart state and its projectile reports.
func (rs *raceState) onState(s *seat, payload []byte, now time.Time) {
	if s.out || rs.phase == phaseEnded {
		return
	}
	r := wire.NewReader(payload)
	if s.state.Decode(r) != nil {
		s.hasState = false
		return
	}
	s.hasState = true
	s.stateRaw = append(s.stateRaw[:0], payload[:len(payload)-r.Remaining()]...)
	if !rs.arbitrated() || r.Remaining() == 0 {
		return
	}
	n := int(r.U8())
	for range n {
		var p proto.ProjectileReport
		if p.Decode(r) != nil {
			return
		}
		if u := rs.uses[p.UseID]; u != nil && u.proj != nil {
			u.proj.addReport(s.slot, pendingReport{ProjectileReport: p, eligible: rs.eligible(s)}, now)
		}
	}
}

// world sends every racer with a known address the other racers' latest states, splitting at the datagram limit.
func (rs *raceState) world(now time.Time) {
	for _, to := range rs.seats {
		addr := to.addr.Load()
		if to.out || addr == nil {
			continue
		}
		countAt := -1 // offset of the entry count of the datagram being built, -1 = none
		for _, o := range rs.seats {
			if o == to || o.out || !o.hasState {
				continue
			}
			entry := 1 + 2 + len(o.stateRaw)
			if countAt >= 0 && len(rs.worldBuf)+entry+proto.UDPMacSize > proto.UDPMaxDatagram {
				rs.sendWorld(to, *addr)
				countAt = -1
			}
			if countAt < 0 {
				rs.worldBuf = proto.AppendUDPHeader(rs.worldBuf[:0], proto.UDPHeader{Type: proto.UDPWorld,
					Token: to.token, Seq: to.txSeq.Add(1)})
				rs.worldBuf = binary.LittleEndian.AppendUint64(rs.worldBuf, uint64(now.UnixMilli()))
				countAt = len(rs.worldBuf)
				rs.worldBuf = append(rs.worldBuf, 0)
			}
			rs.worldBuf[countAt]++
			rs.worldBuf = append(rs.worldBuf, o.slot)
			rs.worldBuf = binary.LittleEndian.AppendUint16(rs.worldBuf, o.rttMs())
			rs.worldBuf = append(rs.worldBuf, o.stateRaw...)
		}
		if countAt >= 0 {
			rs.sendWorld(to, *addr)
		}
	}
}

func (rs *raceState) sendWorld(to *seat, addr netip.AddrPort) {
	rs.worldBuf = to.worldMac.Seal(rs.worldBuf)
	rs.env.sendUDP(addr, rs.worldBuf)
}

// --- ending ---

// tick runs the timers; the engine calls it at 30 Hz.
func (rs *raceState) tick(now time.Time) {
	switch rs.phase {
	case phaseLoading:
		if now.Sub(rs.started) >= loadTimeout {
			for _, s := range rs.seats {
				if !s.out && s.loaded == nil {
					rs.leave(s, OutcomeLeft, "Did not finish loading in time.", now)
				}
			}
			rs.maybeDecideLoad(now)
		}
	case phaseRacing:
		rs.world(now)
		rs.tickHits(now)
		rs.tickProjectiles(now)
		rs.tickHealth(now)
		rs.tickBridges(now)
		rs.tickUnknown(now)
		rs.checkEnd(now)
	}
}

func (rs *raceState) checkEnd(now time.Time) {
	if rs.phase == phaseEnded {
		return
	}
	in, alive, unfinished := 0, 0, 0
	for _, s := range rs.seats {
		if s.out {
			continue
		}
		in++
		if !s.eliminated {
			alive++
		}
		if !s.finished {
			unfinished++
		}
	}
	if in == 0 {
		rs.end(now)
		return
	}
	if rs.phase != phaseRacing {
		return
	}
	since := now.Sub(rs.goAt)
	switch {
	case rs.spec.Solo:
		if since >= raceTimeLimit {
			rs.end(now)
		}
	case rs.spec.Mode == ModeBattle:
		if alive <= 1 || since >= battleTimeLimit {
			rs.end(now)
		}
	default:
		if unfinished == 0 || since >= raceTimeLimit ||
			(!rs.firstFinish.IsZero() && now.Sub(rs.firstFinish) >= afterFirstFinish) {
			rs.end(now)
		}
	}
}

// end finishes the race (also EndEarly): unfinished racers are DNF.
func (rs *raceState) end(now time.Time) {
	if rs.phase == phaseEnded {
		return
	}
	rs.phase = phaseEnded
	for _, s := range rs.seats {
		if !s.out {
			rs.env.dropSeat(s)
		}
	}
	rs.env.finished(rs.id, rs.placements(now))
}

// placements orders every racer: race mode finishers by time, then DNF by distance; battles survivors by health,
// then eliminated racers by reverse elimination order (both count as finished, FinishMs = time survived); racers
// that left last, latest leaver first.
func (rs *raceState) placements(now time.Time) []Placement {
	var stayed, gone []*seat
	for _, s := range rs.seats {
		if s.out {
			gone = append(gone, s)
		} else {
			stayed = append(stayed, s)
		}
	}
	slices.SortStableFunc(gone, func(a, b *seat) int { return b.outAt.Compare(a.outAt) })
	battle := rs.spec.Mode == ModeBattle && !rs.spec.Solo
	if battle {
		slices.SortStableFunc(stayed, func(a, b *seat) int {
			switch {
			case a.eliminated && b.eliminated:
				return cmp.Compare(b.elimOrder, a.elimOrder)
			case a.eliminated:
				return 1
			case b.eliminated:
				return -1
			}
			return cmp.Compare(b.state.Health, a.state.Health)
		})
	} else {
		stayed = rs.ranking()
	}
	out := make([]Placement, 0, len(rs.seats))
	add := func(s *seat, outcome Outcome, finishMs int32) {
		p := Placement{Slot: s.slot, AccountID: s.racer.AccountID, Username: s.racer.Username,
			Position: uint8(len(out) + 1), FinishMs: finishMs, Outcome: outcome}
		if rs.spec.Solo {
			p.Position = rs.spec.AICount + 1
			if outcome == OutcomeFinished {
				p.Position = s.soloPosition
			}
		}
		out = append(out, p)
	}
	for _, s := range stayed {
		switch {
		case battle && rs.phase == phaseEnded && !rs.goAt.IsZero() && !now.Before(rs.goAt):
			survived := now
			if s.eliminated {
				survived = s.elimAt
			}
			add(s, OutcomeFinished, int32(max(survived.Sub(rs.goAt), 0).Milliseconds()))
		case s.finished:
			add(s, OutcomeFinished, s.finishMs)
		default:
			add(s, OutcomeDNF, 0)
		}
	}
	for _, s := range gone {
		add(s, s.outcome, 0)
	}
	return out
}
