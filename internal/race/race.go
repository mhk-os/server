package race

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
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
	maxStoredReports  = 100 // per reporter and race: bounds what a flooding client writes to cheat_reports
	hitGrace          = 300 * time.Millisecond
	finishTolerance   = time.Second
	zeroHealthLimit   = 2 * time.Second
	unknownWindow     = 2 * time.Second
	projectileEvery   = 500 * time.Millisecond
	projectileSilence = 10 * time.Second // no report for this long ends tracking
	projectileSpread  = 2.0              // m: correct when a report is farther from the median
	outlierMin        = 4.0              // m
	bridgeWindow      = 150 * time.Millisecond
	pickupSlack       = 100 * time.Millisecond
	pickupMinRadius   = 8.0 // m
	pickupExtraRadius = 4.0 // m
	rouletteSlack     = 100 * time.Millisecond
	ticksPerSecond    = 60
	specialBox        = "@special"
	gamePack          = "game" // the built-in pack: content of every lobby
)

// env is how a race acts on the world; the Engine implements it, unit tests record it.
type env interface {
	push(accountID uint64, typ proto.Type, m proto.Message)
	sendUDP(addr netip.AddrPort, b []byte)
	dropSeat(s *seat) // stop serving the seat's UDP token
	going(raceID uint64)
	finished(raceID uint64, placements []Placement)
	cheatKick(k store.CheatKick)
	cheatReport(r store.CheatReport)
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
	live        map[uint32]*use // uses whose projectile is still tracked (judged every tick)
	hitsTaken   map[hitKey]bool
	claims      map[hitKey]*hitClaim
	reports     map[uint8]map[uint8]cheatVote // suspect → reporter → latest report
	unknown     map[uint8]*unknownClaim       // racer → claims about their content
	bridges     map[uint16]*bridge
	firstFinish time.Time
	elimCount   int

	hasAI    bool  // some seat is an AI kart
	aiHost   *seat // the human reporting for the AI karts, nil = none (no AI or no human left)
	progress proto.UDPAiProgressPayload

	worldBuf []byte
	log      *slog.Logger
}

func newRaceState(id uint64, spec Spec, seats []*seat, e env, cat Catalog, log *slog.Logger, seed [32]byte,
	now time.Time) *raceState {
	rs := &raceState{
		id:        id,
		spec:      spec,
		env:       e,
		cat:       cat,
		rng:       rand.New(rand.NewChaCha8(seed)),
		seats:     seats,
		started:   now,
		uses:      map[uint32]*use{},
		live:      map[uint32]*use{},
		hitsTaken: map[hitKey]bool{},
		claims:    map[hitKey]*hitClaim{},
		reports:   map[uint8]map[uint8]cheatVote{},
		unknown:   map[uint8]*unknownClaim{},
		bridges:   map[uint16]*bridge{},
		worldBuf:  make([]byte, 0, proto.UDPMaxDatagram),
		log:       log.With("race", id),
	}
	rs.hasAI = slices.ContainsFunc(seats, func(s *seat) bool { return s.ai })
	rs.electHost()
	return rs
}

// electHost makes the human with the lowest slot still in the race the AI host (nil without AI or humans).
func (rs *raceState) electHost() {
	rs.aiHost = nil
	if !rs.hasAI {
		return
	}
	for _, s := range rs.seats {
		if !s.ai && !s.out {
			rs.aiHost = s
			return
		}
	}
}

// aiHostSlot is RaceSetup.aiHost: the host's slot, NoSlot without one.
func (rs *raceState) aiHostSlot() uint8 {
	if rs.aiHost == nil {
		return proto.NoSlot
	}
	return rs.aiHost.slot
}

// kart resolves the slot field of a kart notify from s: OwnSlot is s's own kart, an AI slot counts only from the AI
// host. Anything else (another human's slot, an AI slot from a non-host) is ignored: nil.
func (rs *raceState) kart(s *seat, slot uint8) *seat {
	if slot == proto.OwnSlot {
		return s
	}
	if k := rs.seatAt(slot); k != nil && k.ai && s == rs.aiHost {
		return k
	}
	rs.debug("notify about another kart ignored (not the AI host, or not an AI slot)", s, "about", slot,
		"aiHost", rs.aiHostSlot())
	return nil
}

// debug logs a race event at Debug level with the racer's slot and account (s may be nil). Client developers
// compare their side against these lines.
func (rs *raceState) debug(msg string, s *seat, args ...any) {
	if !rs.debugging() {
		return
	}
	if s != nil {
		args = append(args, "slot", s.slot, "account", s.racer.AccountID)
		if s.ai {
			args = append(args, "ai", true)
		}
	}
	rs.log.Debug(msg, args...)
}

// debugging gates debug arguments that cost work to compute.
func (rs *raceState) debugging() bool { return rs.log.Enabled(context.Background(), slog.LevelDebug) }

func shortHash(h [32]byte) string { return hex.EncodeToString(h[:6]) }

// seatOf returns the human seat of an account.
func (rs *raceState) seatOf(accountID uint64) *seat {
	for _, s := range rs.seats {
		if !s.ai && s.racer.AccountID == accountID {
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

// arbitrated reports whether items, hits and projectiles are judged: after the load agreement until the end.
func (rs *raceState) arbitrated() bool { return rs.phase == phaseRacing }

func (rs *raceState) raceTick(now time.Time) uint32 {
	if now.Before(rs.goAt) {
		return 0
	}
	return uint32(now.Sub(rs.goAt) * ticksPerSecond / time.Second)
}

// pushAll sends m to every human still in the race, except skip (nil = nobody). AI karts have no connection.
func (rs *raceState) pushAll(typ proto.Type, m proto.Message, skip *seat) {
	for _, s := range rs.seats {
		if !s.ai && !s.out && s != skip {
			rs.env.push(s.racer.AccountID, typ, m)
		}
	}
}

func (rs *raceState) rttOK(s *seat) bool {
	d, ok := s.rtt()
	return ok && d <= maxEligibleRTT
}

// eligible: a human racer whose checks, reports and claims count.
func (rs *raceState) eligible(s *seat) bool { return !s.ai && !s.out && !s.everKicked && rs.rttOK(s) }

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

// lobbySpecials drops the specials whose powerup is not content of the lobby: a client with more packs installed
// resolves cross-pack specials that the others lack, and nobody may hold a powerup outside the lobby's packs.
func (rs *raceState) lobbySpecials(m *proto.RaceLoaded) {
	m.Specials = slices.DeleteFunc(m.Specials, func(sp proto.Special) bool {
		pack := packOf(sp.PowerupID)
		return !strings.EqualFold(pack, gamePack) &&
			!slices.ContainsFunc(rs.spec.Packs, func(p string) bool { return strings.EqualFold(p, pack) })
	})
}

func (rs *raceState) onLoaded(s *seat, m *proto.RaceLoaded, now time.Time) {
	if rs.phase != phaseLoading || s.loaded != nil {
		return
	}
	rs.lobbySpecials(m)
	s.loaded, s.loadedHash = m, loadHash(m)
	rs.debug("race loaded", s, "hash", shortHash(s.loadedHash), "trackHash", m.TrackHash, "laps", m.Laps,
		"boxes", len(m.Boxes), "pool", len(m.Pool), "effects", len(m.Effects))
	rs.maybeDecideLoad(now)
}

func (rs *raceState) maybeDecideLoad(now time.Time) {
	if rs.phase != phaseLoading {
		return
	}
	for _, s := range rs.seats {
		if !s.ai && !s.out && s.loaded == nil {
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
	rs.debug("load agreement", agreed, "majority", ok, "loaded", len(loaded), "agreedHash", shortHash(best),
		"trackHash", agreed.loaded.TrackHash)
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
	rs.debug("GO scheduled", nil, "go", rs.goAt.UnixMilli(), "laps", rs.agreed.Laps, "boxes", len(rs.agreed.Boxes),
		"pool", len(rs.agreed.Pool))
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
			rs.debug("content strike", s, "pack", pack, "commit", majorityCommit)
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

// leave takes a human out of the race; a leaving AI host hands the AI karts to the next human. Idempotent; AI
// karts never leave.
func (rs *raceState) leave(s *seat, outcome Outcome, message string, now time.Time) {
	if s.ai || s.out || rs.phase == phaseEnded {
		return
	}
	rs.debug("racer left", s, "outcome", outcome, "message", message)
	msg := &proto.PlayerLeftRace{RaceID: rs.id, Slot: s.slot, Outcome: uint8(outcome), Message: message}
	rs.pushAll(proto.PushPlayerLeftRace, msg, nil)
	s.out, s.outcome, s.outAt = true, outcome, now
	s.held, s.charges = "", 0
	rs.env.dropSeat(s)
	if s == rs.aiHost {
		rs.electHost()
		if rs.aiHost != nil {
			rs.log.Info("new AI host", "slot", rs.aiHost.slot, "account", rs.aiHost.racer.AccountID, "previous",
				s.slot)
			rs.pushAll(proto.PushAiHost, &proto.AiHost{RaceID: rs.id, Slot: rs.aiHost.slot}, nil)
		}
	}
	if rs.phase == phaseLoading {
		rs.maybeDecideLoad(now)
	}
	rs.checkEnd(now)
}

// strike adds a server-side anticheat strike; 5 within 30 s kick. Anticheat-off races never strike.
func (rs *raceState) strike(s *seat, why string, now time.Time) {
	if !rs.spec.Anticheat || s.ai || s.out {
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
	rs.debug("strike", s, "why", why, "within30s", len(s.strikes))
	if len(s.strikes) >= strikesToKick {
		rs.kickCheater(s, "server strikes: "+strings.Join(s.strikeWhy, ", "), "server", now)
	}
}

// kickCheater records the kick and removes the racer (outcome kicked: cheating).
func (rs *raceState) kickCheater(s *seat, reason, reporters string, now time.Time) {
	if s.ai || s.out {
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
	rs.debug("cheat kick", s, "reason", reason, "reporters", reporters)
	s.everKicked = true
	rs.leave(s, OutcomeCheatKick, "Removed from the race: cheating detected.", now)
}

// --- items ---

// deny tells the sender s that kart k (s itself, or an AI kart s hosts) does not get the box.
func (rs *raceState) deny(s, k *seat, box uint16, reason uint8, why string, args ...any) {
	rs.debug("item denied", k, append(args, "box", box, "reason", reason, "why", why)...)
	rs.env.push(s.racer.AccountID, proto.PushItemDenied, &proto.ItemDenied{RaceID: rs.id, BoxID: box, Reason: reason,
		Slot: k.slot})
}

// onPickup arbitrates a box pickup of the sender's kart or, from the AI host, of an AI kart. AI pickups have no
// distance check (the server has no AI positions) and never strike; their grant goes to every racer.
func (rs *raceState) onPickup(s *seat, m *proto.ItemPickup, now time.Time) {
	k := rs.kart(s, m.Slot)
	if k == nil {
		return
	}
	if !rs.arbitrated() || !rs.spec.Items {
		rs.debug("item pickup ignored (items off or not racing)", k, "box", m.BoxID)
		return
	}
	if int(m.BoxID) >= len(rs.agreed.Boxes) || now.Before(rs.goAt) || k.eliminated {
		rs.deny(s, k, m.BoxID, proto.DenyNotAvailable, "unknown box, before GO or eliminated", "boxes",
			len(rs.agreed.Boxes))
		return
	}
	box := &rs.agreed.Boxes[m.BoxID]
	if rs.isEffect[box.Fixed] || now.Before(rs.boxFreeAt[m.BoxID]) {
		// Effect boxes are applied by each client and never arbitrated.
		rs.deny(s, k, m.BoxID, proto.DenyNotAvailable, "effect box or respawning", "fixed", box.Fixed)
		return
	}
	if !k.ai && !rs.nearBox(k, box.Pos) {
		rs.deny(s, k, m.BoxID, proto.DenyTooFar, "too far", "hasState", k.hasState, "distance",
			dist(k.state.Pos, box.Pos), "speed", k.state.Speed(), "rttMs", k.rttMs())
		rs.strike(k, "item pickup too far from the box", now)
		return
	}
	if k.charges > 0 && !rs.agreed.Rules.ReplacesHeld {
		rs.deny(s, k, m.BoxID, proto.DenyCannotReceive, "holds "+k.held)
		return
	}
	id, charges := rs.roll(k, box.Fixed)
	if id == "" {
		rs.deny(s, k, m.BoxID, proto.DenyNotAvailable, "nothing to roll (empty pool or odds)")
		return
	}
	rs.boxFreeAt[m.BoxID] = now.Add(time.Duration(box.RespawnTenths) * 100 * time.Millisecond)
	k.held, k.charges = id, charges
	k.usableAt = now.Add(time.Duration(rs.agreed.Rules.RouletteMs)*time.Millisecond - rouletteSlack)
	grant := &proto.ItemGranted{RaceID: rs.id, BoxID: m.BoxID, PowerupID: id, Charges: charges, Tick: m.Tick,
		Slot: k.slot}
	if k.ai {
		// Info, not Debug: live tests watch AI items without the per-datagram noise of debug logging.
		rs.log.Info("AI item granted", "slot", k.slot, "box", m.BoxID, "powerup", id, "charges", charges,
			"tick", m.Tick, "fixed", box.Fixed, "fraction", rs.positionFraction(k), "host", s.slot)
		rs.pushAll(proto.PushItemGranted, grant, nil)
		return
	}
	if rs.debugging() {
		rs.debug("item granted", k, "box", m.BoxID, "powerup", id, "charges", charges, "tick", m.Tick,
			"fixed", box.Fixed, "fraction", rs.positionFraction(k))
	}
	rs.env.push(k.racer.AccountID, proto.PushItemGranted, grant)
	rs.pushAll(proto.PushItemBoxTaken, &proto.ItemBoxTaken{RaceID: rs.id, Slot: k.slot, BoxID: m.BoxID,
		Tick: m.Tick}, k)
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

// onUse arbitrates an item use of the sender's kart or, from the AI host, of an AI kart (use id
// (aiSlot + 1) << 24 | n). It is relayed to everyone but the sender.
func (rs *raceState) onUse(s *seat, m *proto.ItemUse, now time.Time) {
	k := rs.kart(s, m.Slot)
	if k == nil {
		return
	}
	if !rs.arbitrated() || k.eliminated {
		rs.debug("item use ignored (not racing or eliminated)", k, "use", m.UseID)
		return
	}
	reject := func(reason uint8, why string) {
		rs.debug("item use rejected", k, "use", m.UseID, "reason", reason, "why", why, "held", k.held,
			"charges", k.charges)
		rs.env.push(s.racer.AccountID, proto.PushItemUseRejected,
			&proto.ItemUseRejected{RaceID: rs.id, UseID: m.UseID, Reason: reason})
	}
	_, reused := rs.uses[m.UseID]
	if m.UseID>>24 != uint32(k.slot)+1 || m.UseID&0xFFFFFF == 0 || reused || k.charges == 0 {
		why := "nothing held"
		switch {
		case m.UseID>>24 != uint32(k.slot)+1 || m.UseID&0xFFFFFF == 0:
			why = "use id not (slot+1)<<24|n"
		case reused:
			why = "use id reused"
		}
		reject(proto.RejectNotHeld, why)
		rs.strike(k, "item use without holding it", now)
		return
	}
	if now.Before(k.usableAt) {
		reject(proto.RejectCooldown, "before roulette end")
		return
	}
	u := &use{owner: k.slot, powerupID: k.held}
	u.proj = newProjectile(now)
	rs.uses[m.UseID] = u
	rs.live[m.UseID] = u
	k.charges--
	if k.charges == 0 {
		k.held = ""
	}
	rs.debug("item used", k, "use", m.UseID, "powerup", u.powerupID, "chargesLeft", k.charges, "tick", m.Tick,
		"target", m.TargetSlot)
	rs.pushAll(proto.PushItemUsed, &proto.ItemUsed{RaceID: rs.id, Slot: k.slot, UseID: m.UseID, PowerupID: u.powerupID,
		Tick: m.Tick, Pos: m.Pos, Rot: m.Rot, Backwards: m.Backwards, TargetSlot: m.TargetSlot}, s)
}

// --- hits ---

// onHitTaken relays a hit the victim (the sender's kart, or an AI kart from the AI host) applied to itself.
func (rs *raceState) onHitTaken(s *seat, m *proto.HitTaken, now time.Time) {
	v := rs.kart(s, m.Slot)
	if v == nil || !rs.arbitrated() {
		return
	}
	if m.AttackerSlot != proto.NoSlot && rs.seatAt(m.AttackerSlot) == nil {
		return
	}
	if m.UseID != 0 {
		k := hitKey{v.slot, m.UseID}
		if rs.hitsTaken[k] {
			return
		}
		rs.hitsTaken[k] = true
		delete(rs.claims, k)
	}
	rs.debug("hit taken", v, "use", m.UseID, "attacker", m.AttackerSlot, "effect", m.EffectID, "tick", m.Tick)
	rs.pushAll(proto.PushHitApplied, &proto.HitApplied{RaceID: rs.id, VictimSlot: v.slot, AttackerSlot: m.AttackerSlot,
		UseID: m.UseID, EffectID: m.EffectID, Tick: m.Tick}, s)
}

// onHitSeen collects observers' claims that a human was hit; claims about AI karts are ignored.
func (rs *raceState) onHitSeen(obs *seat, m *proto.HitSeen, now time.Time) {
	if !rs.arbitrated() || m.UseID == 0 {
		return
	}
	victim := rs.seatAt(m.VictimSlot)
	if victim == nil || victim.ai || victim == obs || victim.out || rs.uses[m.UseID] == nil {
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
	counted := !c.forced && !now.After(c.decideAt) && rs.eligible(obs)
	if counted {
		c.claimers[obs.slot] = true
	}
	rs.debug("hit seen", obs, "victim", victim.slot, "use", m.UseID, "attacker", m.AttackerSlot, "effect", m.EffectID,
		"counted", counted, "eligible", rs.eligible(obs), "claimers", len(c.claimers))
}

// tickHits forces hits enough eligible observers saw and strikes victims that ignore the force.
func (rs *raceState) tickHits(now time.Time) {
	for k, c := range rs.claims {
		victim := rs.seats[k.victim]
		switch {
		case victim.out:
			delete(rs.claims, k)
		case !c.forced && !now.Before(c.decideAt):
			if need := rs.observerThreshold(victim); len(c.claimers) < need {
				rs.debug("hit claims below threshold", victim, "use", k.useID, "claimers", len(c.claimers),
					"need", need)
				delete(rs.claims, k)
				continue
			}
			c.forced = true
			c.confirmBy = now.Add(victim.rttOr(maxEligibleRTT) + hitGrace)
			rs.debug("hit forced", victim, "use", k.useID, "attacker", c.attacker, "effect", c.effectID,
				"claimers", len(c.claimers))
			rs.env.push(victim.racer.AccountID, proto.PushHitForce, &proto.HitForce{RaceID: rs.id,
				AttackerSlot: c.attacker, UseID: k.useID, EffectID: c.effectID})
		case c.forced && !now.Before(c.confirmBy):
			delete(rs.claims, k)
			rs.debug("forced hit not confirmed", victim, "use", k.useID)
			rs.strike(victim, "ignored a forced hit", now)
		}
	}
}

// --- finish, battles, bridges ---

// onFinished takes the finish of the sender's kart or, from the AI host, of an AI kart. A time the server cannot
// believe is replaced by its clock (a strike for humans).
func (rs *raceState) onFinished(s *seat, m *proto.Finished, now time.Time) {
	k := rs.kart(s, m.Slot)
	if k == nil {
		return
	}
	if rs.phase != phaseRacing || k.finished || now.Before(rs.goAt) {
		rs.debug("finish ignored (not racing, before GO or already finished)", k, "finishMs", m.FinishMs)
		return
	}
	if rs.spec.Mode == ModeBattle || k.eliminated {
		return
	}
	elapsed := now.Sub(rs.goAt)
	k.finishMs = m.FinishMs
	plausible := finishPlausible(m, elapsed, s.rttOr(0), rs.agreed.Laps)
	if !plausible {
		rs.strike(k, "finish time or laps disagree with the server", now)
		k.finishMs = int32(elapsed.Milliseconds())
	}
	if k.ai {
		rs.log.Info("AI finished", "slot", k.slot, "claimedMs", m.FinishMs, "takenMs", k.finishMs,
			"plausible", plausible, "host", s.slot)
	} else {
		rs.debug("finished", k, "claimedMs", m.FinishMs, "serverMs", elapsed.Milliseconds(), "takenMs", k.finishMs,
			"laps", len(m.LapMs), "raceLaps", rs.agreed.Laps, "plausible", plausible, "tick", m.Tick)
	}
	k.finished, k.finishAt, k.lapMs = true, now, m.LapMs
	if rs.firstFinish.IsZero() {
		rs.firstFinish = now
	}
	rs.pushAll(proto.PushPlayerFinished, &proto.PlayerFinished{RaceID: rs.id, Slot: k.slot, FinishMs: k.finishMs,
		LapMs: k.lapMs}, nil)
	rs.checkEnd(now)
}

// finishPlausible: the claimed time is within RTT + 1 s of the server's clock since GO and the lap count matches.
func finishPlausible(m *proto.Finished, elapsed, rtt time.Duration, laps uint8) bool {
	diff := time.Duration(m.FinishMs)*time.Millisecond - elapsed
	return diff.Abs() <= rtt+finishTolerance && len(m.LapMs) == int(laps)
}

// onEliminatedSelf takes a battle elimination of the sender's kart or, from the AI host, of an AI kart.
func (rs *raceState) onEliminatedSelf(s *seat, m *proto.EliminatedSelf, now time.Time) {
	k := rs.kart(s, m.Slot)
	if k == nil || !rs.arbitrated() || rs.spec.Mode != ModeBattle || now.Before(rs.goAt) || k.eliminated {
		return
	}
	rs.eliminate(k, m.AttackerSlot, m.Tick, now)
}

func (rs *raceState) eliminate(s *seat, attacker uint8, tick uint32, now time.Time) {
	rs.elimCount++
	s.eliminated, s.elimOrder, s.elimAt = true, rs.elimCount, now
	s.held, s.charges = "", 0
	rs.debug("eliminated", s, "attacker", attacker, "tick", tick, "order", s.elimOrder)
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
		if s.ai || s.out || s.eliminated || !s.hasState || s.state.Health > 0 {
			s.zeroHealthFrom = time.Time{}
			continue
		}
		if s.zeroHealthFrom.IsZero() {
			s.zeroHealthFrom = now
		} else if now.Sub(s.zeroHealthFrom) >= zeroHealthLimit {
			rs.debug("zero health without EliminatedSelf", s, "since", s.zeroHealthFrom.UnixMilli())
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
		rs.debug("bridge trigger", s, "bridge", m.BridgeID, "tick", m.Tick)
		rs.bridges[m.BridgeID] = &bridge{tick: m.Tick, decideAt: now.Add(bridgeWindow)}
	case !b.done && m.Tick < b.tick:
		b.tick = m.Tick
	}
}

func (rs *raceState) tickBridges(now time.Time) {
	for id, b := range rs.bridges {
		if !b.done && !now.Before(b.decideAt) {
			b.done = true
			rs.debug("bridge triggered", nil, "bridge", id, "tick", b.tick)
			rs.pushAll(proto.PushBridgeTriggered, &proto.BridgeTriggered{RaceID: rs.id, BridgeID: id, Tick: b.tick}, nil)
		}
	}
}

// --- anticheat reports, unknown content ---

// onCheatReport stores every report about another human racer (moderators review races where nobody could be
// kicked) and counts it towards a kick when reportIgnored allows. Reports about AI karts are dropped.
func (rs *raceState) onCheatReport(rep *seat, m *proto.CheatReport, now time.Time) {
	suspect := rs.seatAt(m.SuspectSlot)
	if suspect == nil || suspect.ai || suspect == rep {
		rs.debug("cheat report about nobody or an AI kart", rep, "suspect", m.SuspectSlot)
		return
	}
	ignored := rs.reportIgnored(rep, suspect, m.Reason)
	if rep.storedReports < maxStoredReports {
		rep.storedReports++
		rs.env.cheatReport(store.CheatReport{LobbyRef: formatID(rs.spec.LobbyID), RaceRef: formatID(rs.id),
			ReporterID: rep.racer.AccountID, SuspectID: suspect.racer.AccountID, Reason: m.Reason.String(),
			Detail: strings.ToValidUTF8(m.Detail, "?"), Counted: ignored == "", Note: ignored, CreatedAt: now})
	}
	if ignored != "" {
		rs.debug("cheat report not counted", rep, "suspect", suspect.slot, "reason", m.Reason.String(), "why", ignored,
			"reporterRttMs", rep.rttMs(), "suspectRttMs", suspect.rttMs())
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
	need := rs.observerThreshold(suspect)
	rs.debug("cheat report", rep, "suspect", suspect.slot, "reason", m.Reason.String(), "detail", m.Detail,
		"reporters", len(reporters), "need", need)
	if len(reporters) < need {
		return
	}
	slices.Sort(reporters)
	why := slices.Sorted(maps.Keys(reasons))
	delete(rs.reports, suspect.slot)
	rs.kickCheater(suspect, "cheat reports: "+strings.Join(why, ", "), strings.Join(reporters, ","), now)
}

// reportIgnored returns why a cheat report does not count, "" when it does. Speed and acceleration reports count
// whatever the suspect's ping: latency does not change those magnitudes, and a faked ping must not shield them.
func (rs *raceState) reportIgnored(rep, suspect *seat, reason proto.FlagReason) string {
	switch {
	case !rs.spec.Anticheat:
		return "anticheat off"
	case !rs.arbitrated():
		return "race not running"
	case !reason.Valid():
		return "unknown reason"
	case suspect.out:
		return "suspect out of the race"
	case rep.out:
		return "reporter out of the race"
	case rep.everKicked:
		return "reporter was kicked for cheating"
	case !rs.rttOK(rep):
		return "reporter ping unknown or over 200 ms"
	case !rs.rttOK(suspect) && reason != proto.FlagSpeed && reason != proto.FlagAcceleration:
		return "suspect ping unknown or over 200 ms"
	}
	return ""
}

func (rs *raceState) onUnknownContent(rep *seat, m *proto.UnknownContent, now time.Time) {
	if rs.phase == phaseEnded {
		return
	}
	x := rs.seatAt(m.Slot)
	if x == nil || x.ai || x == rep || x.out {
		rs.debug("unknown content ignored", rep, "about", m.Slot, "id", m.ID)
		return
	}
	c := rs.unknown[x.slot]
	if c == nil {
		c = &unknownClaim{first: now, reporters: map[uint8]bool{}, firstBy: rep.slot}
		rs.unknown[x.slot] = c
	}
	c.reporters[rep.slot] = true
	rs.debug("unknown content", rep, "about", x.slot, "kind", m.Kind, "id", m.ID, "reporters", len(c.reporters))
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
		rs.debug("unknown content window closed", rs.seats[slot], "reporters", len(c.reporters), "firstBy", c.firstBy)
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
	if err := s.state.Decode(r); err != nil {
		if !s.stateBad { // logged on transitions only: states arrive at 30 Hz
			rs.debug("malformed kart state", s, "err", err, "len", len(payload))
		}
		s.stateBad, s.hasState = true, false
		return
	}
	if !s.hasState {
		rs.debug("kart states arriving", s, "tick", s.state.Tick, "pos", s.state.Pos, "lap", s.state.Lap)
	}
	s.stateBad, s.hasState = false, true
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

// onAiProgress stores the AI host's report of its AI karts (UDP AiProgress): lap, raceDistance and health feed
// item rolls, DNF ranking and battle survivor ranking. Datagrams from anybody else are ignored.
func (rs *raceState) onAiProgress(s *seat, payload []byte, now time.Time) {
	if s != rs.aiHost || s.out || rs.phase == phaseEnded {
		return
	}
	if err := rs.progress.Decode(wire.NewReader(payload)); err != nil {
		rs.debug("malformed AI progress", s, "err", err, "len", len(payload))
		return
	}
	for _, e := range rs.progress.Entries {
		if k := rs.seatAt(e.Slot); k != nil && k.ai {
			k.state.Lap, k.state.RaceDistance, k.state.Health = e.Lap, e.RaceDistance, e.Health
		}
	}
}

// world sends every human with a known address the other humans' latest states, splitting at the datagram limit.
// AI karts have no address and no state: every client simulates them itself.
func (rs *raceState) world(now time.Time) {
	for _, to := range rs.seats {
		if to.ai || to.out {
			continue
		}
		addr := to.addr.Load()
		if addr == nil {
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
				if !s.ai && !s.out && s.loaded == nil {
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

// checkEnd ends the race when no human is left; races when every kart finished, 30 s after the first finisher
// (human or AI) or after 20 min; battles when at most one kart is alive (of a battle that started with two or
// more), when no human is alive, or after 4 min.
func (rs *raceState) checkEnd(now time.Time) {
	if rs.phase == phaseEnded {
		return
	}
	humans, alive, humansAlive, unfinished := 0, 0, 0, 0
	for _, s := range rs.seats {
		if s.out {
			continue
		}
		if !s.ai {
			humans++
		}
		if !s.eliminated {
			alive++
			if !s.ai {
				humansAlive++
			}
		}
		if !s.finished {
			unfinished++
		}
	}
	if humans == 0 {
		rs.end(now)
		return
	}
	if rs.phase != phaseRacing {
		return
	}
	since := now.Sub(rs.goAt)
	switch {
	case rs.spec.Mode == ModeBattle:
		if (alive <= 1 && len(rs.seats) > 1) || humansAlive == 0 || since >= battleTimeLimit {
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
		if !s.ai && !s.out {
			rs.env.dropSeat(s)
		}
	}
	placements := rs.placements(now)
	if rs.debugging() {
		rows := make([]string, len(placements))
		for i, p := range placements {
			rows[i] = fmt.Sprintf("%d:slot%d:acc%d:ai%v:%dms:outcome%d", p.Position, p.Slot, p.AccountID, p.AI,
				p.FinishMs, p.Outcome)
		}
		rs.debug("race ended", nil, "placements", strings.Join(rows, " "))
	}
	rs.env.finished(rs.id, placements)
}

// placements orders every kart, AI included: race mode finishers by time, then DNF by distance (AI: from
// AiProgress); battles survivors by health, then eliminated karts by reverse elimination order (both count as
// finished, FinishMs = time survived); racers that left last, latest leaver first.
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
	battle := rs.spec.Mode == ModeBattle
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
		out = append(out, Placement{Slot: s.slot, AccountID: s.racer.AccountID, Username: s.racer.Username,
			Position: uint8(len(out) + 1), FinishMs: finishMs, Outcome: outcome, AI: s.ai,
			CharacterID: s.racer.CharacterID})
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
