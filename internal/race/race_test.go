package race

import (
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/wire"
)

var t0 = time.Unix(1_800_000_000, 0)

type sent struct {
	acc uint64
	typ proto.Type
	m   proto.Message
}

type fakeEnv struct {
	pushes     []sent
	udp        int
	dropped    []uint8
	goingN     int
	finishedN  int
	placements []Placement
	kicks      []store.CheatKick
}

func (f *fakeEnv) push(acc uint64, typ proto.Type, m proto.Message) {
	f.pushes = append(f.pushes, sent{acc, typ, m})
}
func (f *fakeEnv) sendUDP(netip.AddrPort, []byte) { f.udp++ }
func (f *fakeEnv) dropSeat(s *seat)               { f.dropped = append(f.dropped, s.slot) }
func (f *fakeEnv) going(uint64)                   { f.goingN++ }
func (f *fakeEnv) finished(_ uint64, p []Placement) {
	f.finishedN++
	f.placements = p
}
func (f *fakeEnv) cheatKick(k store.CheatKick) { f.kicks = append(f.kicks, k) }

// of returns the pushes of typ to acc (0 = any account) and forgets every push.
func (f *fakeEnv) of(acc uint64, typ proto.Type) []proto.Message {
	var out []proto.Message
	for _, p := range f.pushes {
		if p.typ == typ && (acc == 0 || p.acc == acc) {
			out = append(out, p.m)
		}
	}
	return out
}

func (f *fakeEnv) reset() { f.pushes = nil }

type fakeCatalog struct {
	commits     map[uint64]string
	notInMajor  map[string]bool
	strikes     []uint64
	strikePacks []string
}

func (c *fakeCatalog) PackCommit(acc uint64, _ string) string { return c.commits[acc] }
func (c *fakeCatalog) InMajority(_ uint64, _ string, _ uint8, id string) bool {
	return !c.notInMajor[id]
}
func (c *fakeCatalog) Strike(acc uint64, pack, _ string) {
	c.strikes = append(c.strikes, acc)
	c.strikePacks = append(c.strikePacks, pack)
}

func acc(slot int) uint64 { return uint64(100 + slot) }

func newTestRace(n int, spec Spec) (*raceState, *fakeEnv, *fakeCatalog) {
	env := &fakeEnv{}
	cat := &fakeCatalog{commits: map[uint64]string{}, notInMajor: map[string]bool{}}
	seats := make([]*seat, n)
	for i := range seats {
		key := make([]byte, proto.UDPKeySize)
		seats[i] = &seat{slot: uint8(i), racer: Racer{AccountID: acc(i), Username: "r"}, token: uint64(i + 1),
			key: key, worldMac: proto.NewUDPMac(key)}
		seats[i].rttUs.Store(50_000)
	}
	if spec.TrackID == "" {
		spec.TrackID = "mhk2.egypt"
	}
	// A discarding debug logger still evaluates every debug line, so the tests cover them.
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return newRaceState(7, spec, seats, env, cat, log, [32]byte{1}, t0), env, cat
}

func testLoaded() *proto.RaceLoaded {
	w := func(xs ...float32) (a [proto.PoolWeights]float32) {
		for i := range a {
			a[i] = xs[0]
			if len(xs) > 1 {
				a[i] = xs[i]
			}
		}
		return a
	}
	return &proto.RaceLoaded{
		RaceID: 7, TrackHash: "0123456789abcdef0123456789abcdef", Laps: 3,
		Boxes: []proto.ItemBoxInfo{
			{Pos: proto.Vec3{0, 0, 0}, RespawnTenths: 30},
			{Pos: proto.Vec3{100, 0, 0}, RespawnTenths: 30, Fixed: specialBox},
			{Pos: proto.Vec3{200, 0, 0}, RespawnTenths: 30, Fixed: "mhk2.boost"},
			{Pos: proto.Vec3{300, 0, 0}, RespawnTenths: 30, Fixed: "mhk2.hit.slimed"},
		},
		Pool: []proto.PoolEntry{
			{PowerupID: "mhk2.rocket", Charges: 1, BattleCharges: 2, Weights: w(1)},
			{PowerupID: "mhk2.egg", Charges: 3, BattleCharges: 5, Weights: w(0)},
		},
		Specials: []proto.Special{{Slot: 0, PowerupID: "mhk2.super"}},
		Rules:    proto.ItemRules{ReplacesHeld: false, RouletteMs: 1000},
		Effects:  []string{"mhk2.hit.slimed"},
	}
}

// startRace loads every racer with the same content and returns the GO instant.
func startRace(t *testing.T, rs *raceState) time.Time {
	t.Helper()
	for _, s := range rs.seats {
		rs.onLoaded(s, testLoaded(), t0)
	}
	if rs.phase != phaseRacing {
		t.Fatalf("phase %d after loading", rs.phase)
	}
	return rs.goAt
}

func setState(rs *raceState, s *seat, st proto.KartState, reports ...proto.ProjectileReport) {
	w := wire.NewWriter(64)
	(&proto.UDPStatePayload{State: st, Reports: reports}).Encode(w)
	rs.onState(s, w.Bytes(), t0)
}

func TestReplayWindow(t *testing.T) {
	var w replayWindow
	steps := []struct {
		seq    uint32
		strict bool
		want   bool
	}{
		{5, true, true}, {5, true, false}, {4, true, false}, {4, false, true}, {4, false, false},
		{6, false, true}, {3, false, true}, {100, true, true}, {36, false, false}, {37, false, true}, {99, true, false},
	}
	for i, s := range steps {
		if got := w.accept(s.seq, s.strict); got != s.want {
			t.Fatalf("step %d seq %d strict %v: %v, want %v", i, s.seq, s.strict, got, s.want)
		}
	}
}

func TestMajority(t *testing.T) {
	a, b, c := [32]byte{1}, [32]byte{2}, [32]byte{3}
	cases := []struct {
		in   [][32]byte
		want [32]byte
		ok   bool
	}{
		{[][32]byte{a, a, b}, a, true},
		{[][32]byte{a, b}, [32]byte{}, false},
		{[][32]byte{a, a, b, b}, [32]byte{}, false},
		{[][32]byte{b, a, c, a}, a, true},
		{[][32]byte{c}, c, true},
		{nil, [32]byte{}, false},
	}
	for i, tc := range cases {
		got, ok := majority(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("case %d: %v %v", i, got[0], ok)
		}
	}
}

func TestLoadHashIgnoresPoolOrder(t *testing.T) {
	a, b := testLoaded(), testLoaded()
	slices.Reverse(b.Pool)
	b.RaceID = 99
	if loadHash(a) != loadHash(b) {
		t.Fatal("pool order or race id changed the hash")
	}
	b.Boxes[0].RespawnTenths++
	if loadHash(a) == loadHash(b) {
		t.Fatal("box change not detected")
	}
}

func TestLoadAgreementDropsDissenter(t *testing.T) {
	rs, env, cat := newTestRace(4, Spec{Items: true})
	for i := range 4 {
		cat.commits[acc(i)] = "c1"
	}
	cat.commits[acc(2)] = "c0" // an older pack version: desync, no strike
	for i, s := range rs.seats {
		m := testLoaded()
		if i >= 2 {
			m.Laps = uint8(i + 2) // two different minorities
		}
		rs.onLoaded(s, m, t0.Add(time.Second))
	}
	if rs.phase != phaseRacing || env.goingN != 1 {
		t.Fatalf("phase %d going %d", rs.phase, env.goingN)
	}
	if !rs.seats[2].out || !rs.seats[3].out || rs.seats[2].outcome != OutcomeDesync {
		t.Fatal("dissenters still racing")
	}
	if !slices.Equal(cat.strikes, []uint64{acc(3)}) || cat.strikePacks[0] != "mhk2" {
		t.Fatalf("content strikes %v %v", cat.strikes, cat.strikePacks)
	}
	if got := env.of(acc(3), proto.PushPlayerLeftRace); len(got) != 2 {
		t.Fatalf("dissenter saw %d PlayerLeftRace", len(got))
	}
	gos := env.of(0, proto.PushRaceGo)
	if len(gos) != 2 || !gos[0].(*proto.RaceGo).Go.Equal(t0.Add(time.Second+goDelay)) {
		t.Fatalf("RaceGo %v", gos)
	}
}

func TestLoadAgreementTieKeepsEveryone(t *testing.T) {
	rs, _, _ := newTestRace(2, Spec{})
	other := testLoaded()
	other.TrackHash = "ffffffffffffffffffffffffffffffff"
	rs.onLoaded(rs.seats[1], other, t0)
	rs.onLoaded(rs.seats[0], testLoaded(), t0)
	if rs.phase != phaseRacing || rs.seats[0].out || rs.seats[1].out {
		t.Fatal("tie dropped a racer")
	}
	if rs.agreed.TrackHash != testLoaded().TrackHash {
		t.Fatal("tie did not use the lowest slot's content")
	}
}

func TestLoadTimeout(t *testing.T) {
	rs, env, _ := newTestRace(3, Spec{})
	rs.onLoaded(rs.seats[0], testLoaded(), t0)
	rs.onLoaded(rs.seats[1], testLoaded(), t0)
	rs.tick(t0.Add(loadTimeout - time.Millisecond))
	if rs.phase != phaseLoading {
		t.Fatal("went before the timeout")
	}
	rs.tick(t0.Add(loadTimeout))
	if rs.phase != phaseRacing || !rs.seats[2].out || rs.seats[2].outcome != OutcomeLeft || env.goingN != 1 {
		t.Fatalf("phase %d out %v", rs.phase, rs.seats[2].out)
	}
}

func TestPoolWeightInterpolation(t *testing.T) {
	var w [proto.PoolWeights]float32
	for i := range w {
		w[i] = float32(10 * i) // 0 at the lead, 100 last
	}
	for _, c := range []struct{ f, want float64 }{{0, 0}, {0.25, 25}, {0.5, 50}, {0.97, 97}, {1, 100}, {-1, 0}, {2, 100}} {
		if got := poolWeight(&w, c.f); math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("f %v: %v, want %v", c.f, got, c.want)
		}
	}
}

func TestDrawPoolDistribution(t *testing.T) {
	var lead, last [proto.PoolWeights]float32
	for i := range lead {
		lead[i] = float32(10 - i) // good at the front
		last[i] = float32(i)      // good at the back
	}
	pool := []proto.PoolEntry{{PowerupID: "a", Weights: lead}, {PowerupID: "b", Weights: last},
		{PowerupID: "never"}}
	share := func(seed uint64, f float64, odds []proto.ItemOdds) (float64, []int) {
		rng := rand.New(rand.NewChaCha8([32]byte{byte(seed)}))
		var seq []int
		counts := make([]int, len(pool))
		for range 20000 {
			i := drawPool(rng, pool, odds, f)
			counts[i]++
			if len(seq) < 50 {
				seq = append(seq, i)
			}
		}
		if counts[2] != 0 {
			t.Fatalf("zero-weight entry drawn %d times", counts[2])
		}
		return float64(counts[0]) / 20000, seq
	}
	if s, _ := share(1, 0, nil); s != 1 {
		t.Fatalf("leader drew b: share a %v", s)
	}
	if s, _ := share(1, 0.3, nil); math.Abs(s-0.7) > 0.02 { // weights 7 : 3
		t.Fatalf("f 0.3: share a %v", s)
	}
	if s, _ := share(1, 1, nil); s != 0 {
		t.Fatalf("last drew a: share a %v", s)
	}
	if s, _ := share(1, 0, []proto.ItemOdds{{PowerupID: "b", Percent: 80}, {PowerupID: "a", Percent: 20}}); math.Abs(s-0.2) > 0.02 {
		t.Fatalf("custom odds: share a %v", s)
	}
	_, s1 := share(9, 0.5, nil)
	_, s2 := share(9, 0.5, nil)
	if !slices.Equal(s1, s2) {
		t.Fatal("same seed, different draws")
	}
	if drawPool(rand.New(rand.NewPCG(1, 1)), pool[2:], nil, 0.5) != -1 {
		t.Fatal("empty pool drew something")
	}
}

func TestPositionFraction(t *testing.T) {
	rs, _, _ := newTestRace(4, Spec{})
	startRace(t, rs)
	for i, d := range []float32{100, 300, 200, 50} {
		setState(rs, rs.seats[i], proto.KartState{RaceDistance: d})
	}
	want := []float64{2.0 / 3, 0, 1.0 / 3, 1}
	for i, s := range rs.seats {
		if got := rs.positionFraction(s); math.Abs(got-want[i]) > 1e-9 {
			t.Fatalf("slot %d: %v, want %v", i, got, want[i])
		}
	}
	rs.seats[3].finished, rs.seats[3].finishMs = true, 60000 // a finisher leads whatever its distance
	if got := rs.positionFraction(rs.seats[3]); got != 0 {
		t.Fatalf("finisher fraction %v", got)
	}
	battle, _, _ := newTestRace(4, Spec{Mode: ModeBattle})
	if battle.positionFraction(battle.seats[0]) != 0.5 {
		t.Fatal("battle fraction")
	}
}

func TestItemPickup(t *testing.T) {
	rs, env, _ := newTestRace(3, Spec{Items: true, Anticheat: true})
	goAt := startRace(t, rs)
	s0, s1 := rs.seats[0], rs.seats[1]
	now := goAt.Add(time.Second)
	setState(rs, s0, proto.KartState{Pos: proto.Vec3{3, 0, 4}}) // 5 m from box 0
	setState(rs, s1, proto.KartState{Pos: proto.Vec3{60, 0, 0}, Vel: [3]int16{3000, 0, 0}})

	denied := func(a uint64) uint8 {
		t.Helper()
		d := env.of(a, proto.PushItemDenied)
		if len(d) != 1 {
			t.Fatalf("%d denials", len(d))
		}
		env.reset()
		return d[0].(*proto.ItemDenied).Reason
	}
	rs.onPickup(s0, &proto.ItemPickup{BoxID: 0}, goAt.Add(-time.Millisecond))
	if r := denied(acc(0)); r != proto.DenyNotAvailable {
		t.Fatalf("before GO: reason %d", r)
	}
	rs.onPickup(s0, &proto.ItemPickup{BoxID: 0, Tick: 60}, now)
	g := env.of(acc(0), proto.PushItemGranted)
	if len(g) != 1 || g[0].(*proto.ItemGranted).PowerupID != "mhk2.rocket" || g[0].(*proto.ItemGranted).Tick != 60 {
		t.Fatalf("grant %v", g)
	}
	if len(env.of(acc(1), proto.PushItemBoxTaken)) != 1 || len(env.of(acc(0), proto.PushItemBoxTaken)) != 0 {
		t.Fatal("ItemBoxTaken not sent to the others only")
	}
	env.reset()
	rs.onPickup(s1, &proto.ItemPickup{BoxID: 0}, now) // taken
	if r := denied(acc(1)); r != proto.DenyNotAvailable {
		t.Fatalf("taken box: reason %d", r)
	}
	// s1 is 60 m away at 30 m/s with 50 ms RTT: radius 30 × 0.15 + 4 = 8.5 m.
	rs.onPickup(s1, &proto.ItemPickup{BoxID: 0}, now.Add(3*time.Second))
	if r := denied(acc(1)); r != proto.DenyTooFar || len(s1.strikes) != 1 {
		t.Fatalf("far: reason %d strikes %d", r, len(s1.strikes))
	}
	setState(rs, s1, proto.KartState{Pos: proto.Vec3{8, 0, 0}, Vel: [3]int16{3000, 0, 0}})
	rs.onPickup(s1, &proto.ItemPickup{BoxID: 0}, now.Add(3*time.Second))
	if len(env.of(acc(1), proto.PushItemGranted)) != 1 {
		t.Fatal("respawned box within the speed radius not granted")
	}
	env.reset()
	setState(rs, s0, proto.KartState{Pos: proto.Vec3{100, 0, 0}})
	rs.onPickup(s0, &proto.ItemPickup{BoxID: 1}, now)
	if r := denied(acc(0)); r != proto.DenyCannotReceive {
		t.Fatalf("holding: reason %d", r)
	}
	s0.held, s0.charges = "", 0
	rs.onPickup(s0, &proto.ItemPickup{BoxID: 1}, now)
	if g := env.of(acc(0), proto.PushItemGranted); len(g) != 1 || g[0].(*proto.ItemGranted).PowerupID != "mhk2.super" {
		t.Fatalf("special grant %v", g)
	}
	env.reset()
	s0.held, s0.charges = "", 0
	setState(rs, s0, proto.KartState{Pos: proto.Vec3{200, 0, 0}})
	rs.onPickup(s0, &proto.ItemPickup{BoxID: 2}, now)
	if g := env.of(acc(0), proto.PushItemGranted); len(g) != 1 || g[0].(*proto.ItemGranted).PowerupID != "mhk2.boost" ||
		g[0].(*proto.ItemGranted).Charges != 1 {
		t.Fatalf("fixed grant %v", g)
	}
	env.reset()
	s0.held, s0.charges = "", 0
	setState(rs, s0, proto.KartState{Pos: proto.Vec3{300, 0, 0}})
	rs.onPickup(s0, &proto.ItemPickup{BoxID: 3}, now)
	if r := denied(acc(0)); r != proto.DenyNotAvailable {
		t.Fatalf("effect box: reason %d", r)
	}
}

func TestItemUse(t *testing.T) {
	rs, env, _ := newTestRace(3, Spec{Items: true, Anticheat: true})
	goAt := startRace(t, rs)
	s := rs.seats[1]
	now := goAt.Add(5 * time.Second)
	use := func(id uint32, at time.Time) uint8 {
		env.reset()
		rs.onUse(s, &proto.ItemUse{UseID: id, Tick: 9, TargetSlot: proto.NoSlot}, at)
		if r := env.of(acc(1), proto.PushItemUseRejected); len(r) == 1 {
			return r[0].(*proto.ItemUseRejected).Reason
		}
		return 0
	}
	if r := use(2<<24|1, now); r != proto.RejectNotHeld || len(s.strikes) != 1 {
		t.Fatalf("not held: %d strikes %d", r, len(s.strikes))
	}
	s.held, s.charges, s.usableAt = "mhk2.egg", 2, now.Add(900*time.Millisecond)
	if r := use(2<<24|1, now); r != proto.RejectCooldown || len(s.strikes) != 1 {
		t.Fatalf("roulette: %d strikes %d", r, len(s.strikes))
	}
	now = now.Add(time.Second)
	if r := use(1<<24|1, now); r != proto.RejectNotHeld {
		t.Fatalf("someone else's use id: %d", r)
	}
	if r := use(2<<24|1, now); r != 0 {
		t.Fatalf("valid use rejected: %d", r)
	}
	u := env.of(0, proto.PushItemUsed)
	if len(u) != 2 || u[0].(*proto.ItemUsed).PowerupID != "mhk2.egg" || u[0].(*proto.ItemUsed).Slot != 1 ||
		len(env.of(acc(1), proto.PushItemUsed)) != 0 {
		t.Fatalf("ItemUsed %v", u)
	}
	if r := use(2<<24|1, now); r != proto.RejectNotHeld {
		t.Fatalf("reused id: %d", r)
	}
	if r := use(2<<24|2, now); r != 0 || s.held != "" || s.charges != 0 {
		t.Fatalf("second charge: %d %q %d", r, s.held, s.charges)
	}
}

func TestJudgeProjectile(t *testing.T) {
	rep := func(tick uint32, x float32, eligible bool) pendingReport {
		return pendingReport{ProjectileReport: proto.ProjectileReport{Tick: tick, Pos: proto.Vec3{x, 0, 0},
			Vel: proto.Vec3{60, 0, 0}}, eligible: eligible}
	}
	// Reports at different ticks agree after extrapolation (60 m/s, 60 ticks/s: 1 m per tick).
	j := judgeProjectile([]pendingReport{rep(100, 10, true), rep(98, 8.2, true), rep(99, 9.1, true)})
	if j.tick != 100 || math.Abs(float64(j.pos[0])-10.1) > 1e-4 || j.correct || len(j.outliers) != 0 {
		t.Fatalf("%+v", j)
	}
	// One liar among four eligible reporters.
	j = judgeProjectile([]pendingReport{rep(100, 10, true), rep(100, 10.5, true), rep(100, 9.6, true),
		rep(100, 40, true)})
	if !j.correct || !slices.Equal(j.outliers, []int{3}) || math.Abs(float64(j.pos[0])-10.25) > 1e-4 {
		t.Fatalf("liar: %+v", j)
	}
	// Spread over 2 m but within max(4 m, 3 × MAD): corrected, nobody struck.
	j = judgeProjectile([]pendingReport{rep(100, 10, true), rep(100, 13.5, true), rep(100, 11, true)})
	if !j.correct || len(j.outliers) != 0 {
		t.Fatalf("spread: %+v", j)
	}
	// Only two eligible reporters: corrections still happen, outliers are not judged.
	j = judgeProjectile([]pendingReport{rep(100, 10, true), rep(100, 40, true), rep(100, 10, false)})
	if !j.correct || len(j.outliers) != 0 {
		t.Fatalf("two eligible: %+v", j)
	}
}

func TestProjectileStrikesLiarOnce(t *testing.T) {
	rs, env, _ := newTestRace(4, Spec{Items: true, Anticheat: true})
	goAt := startRace(t, rs)
	owner := rs.seats[0]
	owner.held, owner.charges = "mhk2.rocket", 1
	id := uint32(1<<24 | 1)
	rs.onUse(owner, &proto.ItemUse{UseID: id, TargetSlot: proto.NoSlot}, goAt.Add(2*time.Second))
	for round := range 2 {
		for i, x := range []float32{10, 10.4, 9.8, 50} {
			setState(rs, rs.seats[i], proto.KartState{}, proto.ProjectileReport{UseID: id, Tick: 120,
				Pos: proto.Vec3{x, 0, 0}})
		}
		env.reset()
		rs.tickProjectiles(goAt.Add(time.Duration(3+round) * time.Second))
		if c := env.of(0, proto.PushProjectileCorrect); len(c) != 4 {
			t.Fatalf("round %d: %d corrections", round, len(c))
		}
	}
	if len(rs.seats[3].strikes) != 1 || len(rs.seats[1].strikes) != 0 {
		t.Fatalf("strikes liar %d honest %d", len(rs.seats[3].strikes), len(rs.seats[1].strikes))
	}
	rs.onProjectileGone(rs.seats[1], &proto.ProjectileGone{UseID: id}, goAt.Add(5*time.Second))
	if rs.uses[id].proj == nil {
		t.Fatal("one of four ended tracking")
	}
	rs.onProjectileGone(owner, &proto.ProjectileGone{UseID: id}, goAt.Add(5*time.Second))
	if rs.uses[id].proj != nil {
		t.Fatal("owner's ProjectileGone ignored")
	}
}

func TestHitArbitration(t *testing.T) {
	setup := func(anticheat bool) (*raceState, *fakeEnv, time.Time, uint32) {
		rs, env, _ := newTestRace(4, Spec{Items: true, Anticheat: anticheat})
		goAt := startRace(t, rs)
		rs.seats[0].held, rs.seats[0].charges = "mhk2.rocket", 1
		rs.onUse(rs.seats[0], &proto.ItemUse{UseID: 1<<24 | 1, TargetSlot: 2}, goAt.Add(time.Second))
		env.reset()
		return rs, env, goAt.Add(2 * time.Second), 1<<24 | 1
	}
	seen := func(rs *raceState, obs int, id uint32, at time.Time) {
		rs.onHitSeen(rs.seats[obs], &proto.HitSeen{VictimSlot: 2, AttackerSlot: 0, UseID: id, EffectID: "hit"}, at)
	}

	// Two eligible observers (threshold max(2, ⌈3/2⌉) = 2), no HitTaken: forced after RTT + 300 ms, then a strike.
	rs, env, now, id := setup(true)
	seen(rs, 0, id, now)
	seen(rs, 1, id, now.Add(100*time.Millisecond))
	rs.tickHits(now.Add(349 * time.Millisecond))
	if len(env.of(0, proto.PushHitForce)) != 0 {
		t.Fatal("forced before victim RTT + 300 ms")
	}
	rs.tickHits(now.Add(350 * time.Millisecond))
	f := env.of(acc(2), proto.PushHitForce)
	if len(f) != 1 || f[0].(*proto.HitForce).UseID != id || len(env.of(0, proto.PushHitForce)) != 1 {
		t.Fatalf("HitForce %v", f)
	}
	rs.tickHits(now.Add(699 * time.Millisecond))
	if len(rs.seats[2].strikes) != 0 {
		t.Fatal("struck before the confirmation window ended")
	}
	rs.tickHits(now.Add(700 * time.Millisecond))
	if len(rs.seats[2].strikes) != 1 {
		t.Fatal("ignored forced hit not struck")
	}

	// The victim confirms after the force: HitApplied once, no strike.
	rs, env, now, id = setup(true)
	seen(rs, 0, id, now)
	seen(rs, 3, id, now)
	rs.tickHits(now.Add(400 * time.Millisecond))
	rs.onHitTaken(rs.seats[2], &proto.HitTaken{AttackerSlot: 0, UseID: id, EffectID: "hit"}, now.Add(500*time.Millisecond))
	rs.onHitTaken(rs.seats[2], &proto.HitTaken{AttackerSlot: 0, UseID: id, EffectID: "hit"}, now.Add(510*time.Millisecond))
	rs.tickHits(now.Add(2 * time.Second))
	if len(rs.seats[2].strikes) != 0 || len(env.of(0, proto.PushHitApplied)) != 3 ||
		len(env.of(acc(2), proto.PushHitApplied)) != 0 {
		t.Fatalf("confirmed: strikes %d applied %d", len(rs.seats[2].strikes), len(env.of(0, proto.PushHitApplied)))
	}

	// One claimer, or an ineligible second one, is not enough.
	rs, env, now, id = setup(true)
	rs.seats[3].rttUs.Store(250_000)
	seen(rs, 0, id, now)
	seen(rs, 3, id, now)
	seen(rs, 0, id, now) // duplicates count once
	rs.tickHits(now.Add(time.Second))
	if len(env.of(0, proto.PushHitForce)) != 0 {
		t.Fatal("forced with one eligible claimer")
	}

	// Anticheat off: forced, never struck.
	rs, env, now, id = setup(false)
	seen(rs, 0, id, now)
	seen(rs, 1, id, now)
	rs.tickHits(now.Add(400 * time.Millisecond))
	rs.tickHits(now.Add(2 * time.Second))
	if len(env.of(0, proto.PushHitForce)) != 1 || len(rs.seats[2].strikes) != 0 {
		t.Fatal("anticheat-off hit arbitration")
	}
}

func TestCheatReports(t *testing.T) {
	report := func(rs *raceState, from int, at time.Time) {
		rs.onCheatReport(rs.seats[from], &proto.CheatReport{SuspectSlot: 4, Reason: proto.FlagReason(1)}, at)
	}
	rs, env, _ := newTestRace(5, Spec{Anticheat: true})
	goAt := startRace(t, rs)
	now := goAt.Add(10 * time.Second)
	rs.seats[1].rttUs.Store(201_000) // slow reporter: ignored
	rs.seats[2].everKicked = true    // former cheater: ignored
	// Eligible observers of 4: slots 0 and 3 → threshold max(2, 1) = 2.
	report(rs, 1, now)
	report(rs, 2, now)
	report(rs, 0, now)
	if rs.seats[4].out {
		t.Fatal("kicked on one eligible report")
	}
	report(rs, 3, now.Add(reportWindow+time.Millisecond)) // slot 0's report expired
	if rs.seats[4].out {
		t.Fatal("expired report counted")
	}
	report(rs, 0, now.Add(reportWindow+time.Second))
	if !rs.seats[4].out || rs.seats[4].outcome != OutcomeCheatKick {
		t.Fatal("not kicked by two eligible reporters")
	}
	if len(env.kicks) != 1 || env.kicks[0].AccountID != acc(4) || env.kicks[0].Reporters != "100,103" ||
		env.kicks[0].RaceRef != "7" {
		t.Fatalf("kick %+v", env.kicks)
	}
	left := env.of(0, proto.PushPlayerLeftRace)
	if len(left) != 5 || left[0].(*proto.PlayerLeftRace).Outcome != uint8(OutcomeCheatKick) {
		t.Fatalf("PlayerLeftRace %v", left)
	}
	if !slices.Contains(env.dropped, 4) {
		t.Fatal("UDP seat not dropped")
	}

	// A suspect with a bad link cannot be judged.
	rs, _, _ = newTestRace(3, Spec{Anticheat: true})
	goAt = startRace(t, rs)
	rs.seats[2].rttUs.Store(300_000)
	rs.onCheatReport(rs.seats[0], &proto.CheatReport{SuspectSlot: 2, Reason: 1}, goAt)
	rs.onCheatReport(rs.seats[1], &proto.CheatReport{SuspectSlot: 2, Reason: 1}, goAt)
	if rs.seats[2].out {
		t.Fatal("kicked a suspect whose RTT is over 200 ms")
	}

	// Larger races need half of the eligible observers.
	rs, _, _ = newTestRace(8, Spec{Anticheat: true})
	goAt = startRace(t, rs)
	for i := range 3 {
		rs.onCheatReport(rs.seats[i], &proto.CheatReport{SuspectSlot: 7, Reason: 1}, goAt)
	}
	if rs.seats[7].out {
		t.Fatal("3 of 7 observers kicked")
	}
	rs.onCheatReport(rs.seats[3], &proto.CheatReport{SuspectSlot: 7, Reason: 1}, goAt)
	if !rs.seats[7].out {
		t.Fatal("4 of 7 observers did not kick")
	}

	// Anticheat off: reports are ignored.
	rs, env, _ = newTestRace(3, Spec{})
	goAt = startRace(t, rs)
	rs.onCheatReport(rs.seats[0], &proto.CheatReport{SuspectSlot: 2, Reason: 1}, goAt)
	rs.onCheatReport(rs.seats[1], &proto.CheatReport{SuspectSlot: 2, Reason: 1}, goAt)
	if rs.seats[2].out || len(env.kicks) != 0 {
		t.Fatal("anticheat-off race kicked")
	}
}

func TestStrikesKick(t *testing.T) {
	rs, env, _ := newTestRace(3, Spec{Anticheat: true})
	goAt := startRace(t, rs)
	s := rs.seats[1]
	for i := range 4 {
		rs.strike(s, "x", goAt.Add(time.Duration(i)*10*time.Second))
	}
	rs.strike(s, "x", goAt.Add(41*time.Second)) // the first two expired
	if s.out {
		t.Fatal("kicked with strikes spread over more than 30 s")
	}
	rs.strike(s, "x", goAt.Add(42*time.Second))
	rs.strike(s, "y", goAt.Add(43*time.Second))
	if !s.out || s.outcome != OutcomeCheatKick || len(env.kicks) != 1 || env.kicks[0].Reporters != "server" {
		t.Fatalf("5 strikes in 30 s: out %v kicks %+v", s.out, env.kicks)
	}

	off, env2, _ := newTestRace(3, Spec{})
	goAt = startRace(t, off)
	for range 10 {
		off.strike(off.seats[1], "x", goAt)
	}
	if off.seats[1].out || len(env2.kicks) != 0 || len(off.seats[1].strikes) != 0 {
		t.Fatal("anticheat-off race struck")
	}
}

func TestFinishValidation(t *testing.T) {
	laps := []int32{20000, 20000, 20000}
	cases := []struct {
		ms      int32
		laps    []int32
		elapsed time.Duration
		rtt     time.Duration
		ok      bool
	}{
		{60000, laps, 60 * time.Second, 50 * time.Millisecond, true},
		{60000, laps, 61050 * time.Millisecond, 50 * time.Millisecond, true},
		{60000, laps, 61051 * time.Millisecond, 50 * time.Millisecond, false},
		{62000, laps, 60 * time.Second, 50 * time.Millisecond, false},
		{60000, laps[:2], 60 * time.Second, 50 * time.Millisecond, false},
	}
	for i, c := range cases {
		if got := finishPlausible(&proto.Finished{FinishMs: c.ms, LapMs: c.laps}, c.elapsed, c.rtt, 3); got != c.ok {
			t.Fatalf("case %d: %v", i, got)
		}
	}

	rs, env, _ := newTestRace(3, Spec{Anticheat: true})
	goAt := startRace(t, rs)
	rs.onFinished(rs.seats[1], &proto.Finished{FinishMs: 30000, LapMs: laps}, goAt.Add(60*time.Second))
	if !rs.seats[1].finished || rs.seats[1].finishMs != 60000 || len(rs.seats[1].strikes) != 1 {
		t.Fatalf("implausible finish: %d strikes %d", rs.seats[1].finishMs, len(rs.seats[1].strikes))
	}
	rs.onFinished(rs.seats[0], &proto.Finished{FinishMs: 61200, LapMs: laps}, goAt.Add(61500*time.Millisecond))
	if rs.seats[0].finishMs != 61200 || len(rs.seats[0].strikes) != 0 {
		t.Fatal("plausible finish changed")
	}
	pf := env.of(0, proto.PushPlayerFinished)
	if len(pf) != 6 || pf[0].(*proto.PlayerFinished).FinishMs != 60000 {
		t.Fatalf("PlayerFinished %v", pf)
	}
}

func TestRaceEnd(t *testing.T) {
	laps := []int32{1, 1, 1}
	// First finisher + 30 s: the rest are DNF by distance, leavers last (latest first).
	rs, env, _ := newTestRace(5, Spec{})
	goAt := startRace(t, rs)
	setState(rs, rs.seats[0], proto.KartState{RaceDistance: 100})
	setState(rs, rs.seats[1], proto.KartState{RaceDistance: 900})
	rs.leave(rs.seats[3], OutcomeLeft, "", goAt.Add(10*time.Second))
	rs.leave(rs.seats[4], OutcomeModKick, "", goAt.Add(20*time.Second))
	rs.onFinished(rs.seats[2], &proto.Finished{FinishMs: 60000, LapMs: laps}, goAt.Add(60*time.Second))
	rs.tick(goAt.Add(89 * time.Second))
	if env.finishedN != 0 {
		t.Fatal("ended before first finisher + 30 s")
	}
	rs.tick(goAt.Add(90 * time.Second))
	if env.finishedN != 1 {
		t.Fatal("not ended 30 s after the first finisher")
	}
	want := []Placement{
		{Slot: 2, AccountID: acc(2), Username: "r", Position: 1, FinishMs: 60000, Outcome: OutcomeFinished},
		{Slot: 1, AccountID: acc(1), Username: "r", Position: 2, Outcome: OutcomeDNF},
		{Slot: 0, AccountID: acc(0), Username: "r", Position: 3, Outcome: OutcomeDNF},
		{Slot: 4, AccountID: acc(4), Username: "r", Position: 4, Outcome: OutcomeModKick},
		{Slot: 3, AccountID: acc(3), Username: "r", Position: 5, Outcome: OutcomeLeft},
	}
	if !slices.Equal(env.placements, want) {
		t.Fatalf("placements\n%+v\nwant\n%+v", env.placements, want)
	}
	rs.tick(goAt.Add(91 * time.Second))
	rs.end(goAt.Add(92 * time.Second))
	if env.finishedN != 1 {
		t.Fatal("Finished called twice")
	}

	// Everybody finished: ends at once, ordered by finish time.
	rs, env, _ = newTestRace(2, Spec{})
	goAt = startRace(t, rs)
	rs.onFinished(rs.seats[0], &proto.Finished{FinishMs: 61000, LapMs: laps}, goAt.Add(61*time.Second))
	rs.onFinished(rs.seats[1], &proto.Finished{FinishMs: 60500, LapMs: laps}, goAt.Add(61200*time.Millisecond))
	if env.finishedN != 1 || env.placements[0].Slot != 1 || env.placements[1].Slot != 0 {
		t.Fatalf("all finished: %+v", env.placements)
	}

	// 20 minutes.
	rs, env, _ = newTestRace(2, Spec{})
	goAt = startRace(t, rs)
	rs.tick(goAt.Add(raceTimeLimit - time.Millisecond))
	rs.tick(goAt.Add(raceTimeLimit))
	if env.finishedN != 1 || env.placements[0].Outcome != OutcomeDNF {
		t.Fatal("20 min limit")
	}

	// EndEarly.
	rs, env, _ = newTestRace(2, Spec{})
	startRace(t, rs)
	rs.end(t0.Add(time.Second))
	if env.finishedN != 1 || len(env.placements) != 2 {
		t.Fatal("end early")
	}
}

func TestBattleEnd(t *testing.T) {
	rs, env, _ := newTestRace(4, Spec{Mode: ModeBattle, Anticheat: true})
	goAt := startRace(t, rs)
	for i, h := range []uint8{3, 5, 1, 0} {
		setState(rs, rs.seats[i], proto.KartState{Health: h})
	}
	rs.onEliminatedSelf(rs.seats[2], &proto.EliminatedSelf{AttackerSlot: 0, Tick: 600}, goAt.Add(10*time.Second))
	if e := env.of(0, proto.PushEliminated); len(e) != 4 || e[0].(*proto.Eliminated).Slot != 2 {
		t.Fatalf("Eliminated %v", e)
	}
	// Slot 3 sits at health 0 without reporting: eliminated with a strike after 2 s.
	rs.tick(goAt.Add(11 * time.Second))
	rs.tick(goAt.Add(12*time.Second + 999*time.Millisecond))
	if rs.seats[3].eliminated {
		t.Fatal("eliminated before 2 s at health 0")
	}
	rs.tick(goAt.Add(13 * time.Second))
	if !rs.seats[3].eliminated || len(rs.seats[3].strikes) != 1 || env.finishedN != 0 {
		t.Fatal("silent zero health not eliminated")
	}
	rs.onEliminatedSelf(rs.seats[0], &proto.EliminatedSelf{AttackerSlot: 1}, goAt.Add(20*time.Second))
	if env.finishedN != 1 {
		t.Fatal("battle with one survivor did not end")
	}
	slots := []uint8{}
	for _, p := range env.placements {
		slots = append(slots, p.Slot)
		if p.Outcome != OutcomeFinished {
			t.Fatalf("battle outcome %+v", p)
		}
	}
	if !slices.Equal(slots, []uint8{1, 0, 3, 2}) || env.placements[3].FinishMs != 10000 {
		t.Fatalf("battle placements %+v", env.placements)
	}

	rs, env, _ = newTestRace(3, Spec{Mode: ModeBattle})
	goAt = startRace(t, rs)
	rs.tick(goAt.Add(battleTimeLimit))
	if env.finishedN != 1 {
		t.Fatal("battle time limit")
	}
}

func TestSoloRace(t *testing.T) {
	rs, env, _ := newTestRace(1, Spec{Solo: true, AICount: 6, Items: true, Anticheat: true})
	goAt := startRace(t, rs)
	rs.onPickup(rs.seats[0], &proto.ItemPickup{BoxID: 0}, goAt.Add(time.Second))
	if len(env.of(0, proto.PushItemDenied))+len(env.of(0, proto.PushItemGranted)) != 0 {
		t.Fatal("solo race arbitrated an item")
	}
	rs.onFinished(rs.seats[0], &proto.Finished{FinishMs: 70000, Position: 3}, goAt.Add(70*time.Second))
	want := []Placement{{Slot: 0, AccountID: acc(0), Username: "r", Position: 3, FinishMs: 70000,
		Outcome: OutcomeFinished}}
	if !slices.Equal(env.placements, want) {
		t.Fatalf("%+v", env.placements)
	}

	rs, env, _ = newTestRace(1, Spec{Solo: true, AICount: 6})
	startRace(t, rs)
	rs.end(t0.Add(10 * time.Second))
	if len(env.placements) != 1 || env.placements[0].Outcome != OutcomeDNF || env.placements[0].Position != 7 {
		t.Fatalf("%+v", env.placements)
	}
}

func TestUnknownContent(t *testing.T) {
	uc := func(rs *raceState, from, about int, id string, at time.Time) {
		rs.onUnknownContent(rs.seats[from], &proto.UnknownContent{Slot: uint8(about), Kind: 2, ID: id}, at)
	}
	rs, env, _ := newTestRace(4, Spec{Anticheat: true})
	startRace(t, rs)
	uc(rs, 0, 3, "mod.kart", t0)
	uc(rs, 0, 3, "mod.kart", t0) // same reporter twice
	if rs.seats[3].out {
		t.Fatal("one reporter removed the racer")
	}
	uc(rs, 1, 3, "mod.kart", t0)
	if !rs.seats[3].out || rs.seats[3].outcome != OutcomeDesync || len(env.kicks) != 0 {
		t.Fatal("two reporters: not a desync leave")
	}

	rs, env, cat := newTestRace(4, Spec{Anticheat: true})
	startRace(t, rs)
	cat.notInMajor["mhk2.fake"] = true
	uc(rs, 0, 3, "mhk2.fake", t0)
	uc(rs, 2, 3, "mhk2.fake", t0)
	if rs.seats[3].outcome != OutcomeCheatKick || len(env.kicks) != 1 || env.kicks[0].Reporters != "100,102" {
		t.Fatalf("fabricated: %v %+v", rs.seats[3].outcome, env.kicks)
	}

	rs, _, _ = newTestRace(4, Spec{Anticheat: true})
	goAt := startRace(t, rs)
	uc(rs, 1, 2, "mod.kart", goAt)
	rs.tick(goAt.Add(unknownWindow - time.Millisecond))
	if rs.seats[1].out {
		t.Fatal("reporter left before 2 s")
	}
	rs.tick(goAt.Add(unknownWindow))
	if !rs.seats[1].out || rs.seats[1].outcome != OutcomeDesync || rs.seats[2].out {
		t.Fatal("single reporter did not leave")
	}
}

func TestBridgeLowestTickWins(t *testing.T) {
	rs, env, _ := newTestRace(3, Spec{})
	goAt := startRace(t, rs)
	now := goAt.Add(time.Minute)
	rs.onBridgeTrigger(rs.seats[0], &proto.BridgeTrigger{BridgeID: 4, Tick: 500}, now)
	rs.onBridgeTrigger(rs.seats[1], &proto.BridgeTrigger{BridgeID: 4, Tick: 480}, now.Add(50*time.Millisecond))
	rs.tickBridges(now.Add(100 * time.Millisecond))
	if len(env.of(0, proto.PushBridgeTriggered)) != 0 {
		t.Fatal("broadcast before the window")
	}
	rs.tickBridges(now.Add(bridgeWindow))
	rs.onBridgeTrigger(rs.seats[2], &proto.BridgeTrigger{BridgeID: 4, Tick: 1}, now.Add(time.Second))
	rs.tickBridges(now.Add(2 * time.Second))
	b := env.of(0, proto.PushBridgeTriggered)
	if len(b) != 3 || b[0].(*proto.BridgeTriggered).Tick != 480 {
		t.Fatalf("%v", b)
	}
}

func TestWorldSplitsAtDatagramLimit(t *testing.T) {
	rs, env, _ := newTestRace(10, Spec{})
	startRace(t, rs)
	big := make([]uint16, 120) // 40 + 240 bytes per state: 9 others do not fit one datagram
	for _, s := range rs.seats {
		setState(rs, s, proto.KartState{Effects: big})
	}
	addr := netip.MustParseAddrPort("127.0.0.1:9")
	rs.seats[0].addr.Store(&addr)
	rs.world(t0)
	if env.udp != 3 { // 4 + 4 + 1 entries
		t.Fatalf("%d datagrams", env.udp)
	}
}
