package race

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/testclient"
	"github.com/mhk-os/server/internal/wire"
)

// Integration tests need MariaDB (MHK_TEST_DSN, see internal/server); they drive real TCP and UDP through
// Engine.Start with bots.

type harness struct {
	addr string
	st   *store.Store
	eng  *Engine
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("MHK_TEST_DSN")
	if dsn == "" {
		t.Skip("MHK_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ghosts, err := blobs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	limits := auth.Limits{Register: auth.Rate{N: 1000, Per: time.Hour}, LoginFailIP: auth.Rate{N: 1000, Per: time.Hour},
		LoginFailAccount: auth.Rate{N: 1000, Per: time.Hour}}
	svc := auth.NewService(st, st, auth.NewHasher(auth.DefaultParams, auth.DefaultHashConcurrency), limits)
	srv := server.New(server.Config{Version: "test", UDPPort: uint16(udp.LocalAddr().(*net.UDPAddr).Port),
		ConnLimit: auth.Rate{N: 1000, Per: time.Minute}},
		server.Deps{Log: log, Auth: svc, Accounts: st, Sessions: st, Moderation: st, Runs: st, Blobs: ghosts})
	eng := NewEngine(Config{Log: log, Hub: srv.Hub(), Kicks: st, UDP: udp})
	eng.Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
		eng.Close()
	})
	return &harness{addr: ln.Addr().String(), st: st, eng: eng}
}

// bot is one racer: a logged-in control connection that collects its pushes, plus its UDP side once set up.
type bot struct {
	t     *testing.T
	c     *testclient.Client
	acc   uint64
	name  string
	setup proto.RaceSetup
	udp   *testclient.UDPBot

	mu     sync.Mutex
	pushes []testclient.Push
}

func newBot(t *testing.T, h *harness) *bot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := testclient.Connect(ctx, h.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	b := make([]byte, 5)
	rand.Read(b)
	name := "race" + hex.EncodeToString(b)
	s, err := c.Register(ctx, name, "kart-racer-2026")
	if err != nil {
		t.Fatal(err)
	}
	bt := &bot{t: t, c: c, acc: s.Account.ID, name: name}
	go func() {
		for p := range c.Pushes {
			bt.mu.Lock()
			bt.pushes = append(bt.pushes, p)
			bt.mu.Unlock()
		}
	}()
	return bt
}

func (b *bot) racer() Racer {
	return Racer{AccountID: b.acc, Username: b.name, Country: "DE", CharacterID: "mhk2.moorhuhn", KartID: "mhk2.kart"}
}

// take waits for the first unconsumed push of typ that match accepts, removes it and returns it decoded into m.
func (b *bot) take(typ proto.Type, m proto.Message, match func() bool) {
	b.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		for i, p := range b.pushes {
			if p.Type != typ || m.Decode(wire.NewReader(p.Body)) != nil || (match != nil && !match()) {
				continue
			}
			b.pushes = slices.Delete(b.pushes, i, i+1)
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	b.t.Fatalf("%s: no push %d", b.name, typ)
}

// none fails if a push of typ is waiting.
func (b *bot) none(typ proto.Type) {
	b.t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pushes {
		if p.Type == typ {
			b.t.Fatalf("%s: unexpected push %d", b.name, typ)
		}
	}
}

func (b *bot) notify(typ proto.Type, m proto.Message) {
	b.t.Helper()
	if err := b.c.Notify(typ, m); err != nil {
		b.t.Fatal(err)
	}
}

// joinUDP reads RaceSetup and starts pinging and sending states.
func (b *bot) joinUDP(ctx context.Context) {
	b.t.Helper()
	b.take(proto.PushRaceSetup, &b.setup, nil)
	u, err := testclient.DialUDP(fmt.Sprintf("127.0.0.1:%d", b.setup.UDPPort), b.setup.UDPToken, b.setup.UDPKey)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(u.Close)
	b.udp = u
	u.Run(ctx, true)
}

func integrationLoaded(raceID uint64) *proto.RaceLoaded {
	var flat [proto.PoolWeights]float32
	for i := range flat {
		flat[i] = 1
	}
	return &proto.RaceLoaded{
		RaceID: raceID, TrackHash: "00112233445566778899aabbccddeeff", Laps: 3,
		Boxes: []proto.ItemBoxInfo{
			{Pos: proto.Vec3{0, 0, 0}, RespawnTenths: 50, Fixed: "mhk2.egg"},
			{Pos: proto.Vec3{100, 0, 0}, RespawnTenths: 50},
		},
		Pool: []proto.PoolEntry{
			{PowerupID: "mhk2.egg", Charges: 3, BattleCharges: 3, Weights: flat},
			{PowerupID: "mhk2.rocket", Charges: 1, BattleCharges: 1, Weights: flat},
		},
		Rules:   proto.ItemRules{ReplacesHeld: false, RouletteMs: 0},
		Effects: []string{"mhk2.hit.shotdown"},
	}
}

type callbacks struct {
	going    chan uint64
	finished chan []Placement
}

func newCallbacks() (*callbacks, Callbacks) {
	c := &callbacks{going: make(chan uint64, 1), finished: make(chan []Placement, 1)}
	return c, Callbacks{
		Going:    func(id uint64) { c.going <- id },
		Finished: func(_ uint64, p []Placement) { c.finished <- p },
	}
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		t.Fatal("callback not called")
		panic("unreachable")
	}
}

// inspect runs f on the race goroutine (for assertions on server-side state such as strikes).
func inspect[T any](t *testing.T, e *Engine, raceID uint64, f func(rs *raceState) T) T {
	t.Helper()
	ch := make(chan T, 1)
	e.call(raceID, func(rs *raceState, _ time.Time) { ch <- f(rs) })
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("race not running")
		panic("unreachable")
	}
}

// eventually polls cond for up to 10 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRaceIntegration(t *testing.T) {
	h := startHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bots := make([]*bot, 4)
	racers := make([]Racer, 4)
	for i := range bots {
		bots[i] = newBot(t, h)
		racers[i] = bots[i].racer()
	}
	cb, callbacks := newCallbacks()
	raceID, err := h.eng.Start(Spec{LobbyID: 77, TrackID: "mhk2.egypt", Mode: ModeRace, EngineClass: 2, Items: true,
		Anticheat: true, SetRace: 1, SetSize: 4}, racers, callbacks)
	if err != nil {
		t.Fatal(err)
	}

	// Setup: every bot gets its own seat, token and key.
	for i, b := range bots {
		b.joinUDP(ctx)
		if b.setup.RaceID != raceID || b.setup.YourSlot != uint8(i) || len(b.setup.Grid) != 4 ||
			b.setup.UDPPort != h.eng.UDPPort() || len(b.setup.UDPKey) != proto.UDPKeySize || b.setup.Seed == 0 {
			t.Fatalf("bot %d setup %+v", i, b.setup)
		}
	}
	if bots[0].setup.UDPToken == bots[1].setup.UDPToken {
		t.Fatal("shared UDP token")
	}

	// Loading: bot 3 has different content and leaves as desync.
	for i, b := range bots {
		m := integrationLoaded(raceID)
		if i == 3 {
			m.Boxes[1].RespawnTenths = 10
		}
		b.notify(proto.NotifyRaceLoaded, m)
	}
	recv(t, cb.going)
	var left proto.PlayerLeftRace
	for _, b := range bots {
		b.take(proto.PushPlayerLeftRace, &left, nil)
		if left.Slot != 3 || left.Outcome != uint8(OutcomeDesync) {
			t.Fatalf("left %+v", left)
		}
	}
	var goMsg proto.RaceGo
	for _, b := range bots[:3] {
		b.take(proto.PushRaceGo, &goMsg, nil)
	}
	bots[3].none(proto.PushRaceGo)
	if d := time.Until(goMsg.Go); d < 2*time.Second || d > 4*time.Second {
		t.Fatalf("GO in %v", d)
	}
	racing := bots[:3]

	// States are relayed in World with server-measured RTTs.
	for i, b := range racing {
		b.udp.SetState(proto.KartState{Tick: 0, Pos: proto.Vec3{float32(50 * i), 0, 0}, Lap: 1,
			RaceDistance: float32(10 * i), Health: 100})
	}
	for i, b := range racing {
		eventually(t, fmt.Sprintf("World for bot %d", i), func() bool {
			w, _ := b.udp.World()
			if w == nil || len(w.Entries) != 2 {
				return false
			}
			for _, e := range w.Entries {
				if e.Slot == uint8(i) || e.Slot == 3 || e.RTTMs == 0 || e.State.Pos[0] != float32(50*int(e.Slot)) {
					return false
				}
			}
			return true
		})
	}
	if rtt, ok := h.eng.RTT(bots[0].acc); !ok || rtt > 200 {
		t.Fatalf("RTT %d %v", rtt, ok)
	}
	if _, ok := h.eng.RTT(bots[3].acc); ok {
		t.Fatal("RTT of a racer who left")
	}
	time.Sleep(time.Until(goMsg.Go) + 50*time.Millisecond)

	// Pickups: bot 0 sits on box 0 and gets its fixed egg; bot 1 is denied the taken box and a far one.
	bots[0].notify(proto.NotifyItemPickup, &proto.ItemPickup{RaceID: raceID, BoxID: 0, Tick: 10})
	var granted proto.ItemGranted
	bots[0].take(proto.PushItemGranted, &granted, nil)
	if granted.PowerupID != "mhk2.egg" || granted.Charges != 3 || granted.Tick != 10 {
		t.Fatalf("granted %+v", granted)
	}
	var taken proto.ItemBoxTaken
	for _, b := range racing[1:] {
		b.take(proto.PushItemBoxTaken, &taken, nil)
		if taken.Slot != 0 || taken.BoxID != 0 {
			t.Fatalf("taken %+v", taken)
		}
	}
	var denied proto.ItemDenied
	bots[1].notify(proto.NotifyItemPickup, &proto.ItemPickup{RaceID: raceID, BoxID: 0})
	bots[1].take(proto.PushItemDenied, &denied, nil)
	if denied.Reason != proto.DenyNotAvailable {
		t.Fatalf("denied %+v", denied)
	}
	bots[1].notify(proto.NotifyItemPickup, &proto.ItemPickup{RaceID: raceID, BoxID: 1}) // 50 m from box 1
	bots[1].take(proto.PushItemDenied, &denied, nil)
	if denied.Reason != proto.DenyTooFar {
		t.Fatalf("denied %+v", denied)
	}

	// Uses: relayed to the others; bot 1 holds nothing.
	use1, use2 := uint32(1<<24|1), uint32(1<<24|2)
	bots[0].notify(proto.NotifyItemUse, &proto.ItemUse{RaceID: raceID, UseID: use1, Tick: 20, Rot: proto.Quat{0, 0, 0, 1},
		TargetSlot: proto.NoSlot})
	var used proto.ItemUsed
	for _, b := range racing[1:] {
		b.take(proto.PushItemUsed, &used, func() bool { return used.UseID == use1 })
		if used.Slot != 0 || used.PowerupID != "mhk2.egg" {
			t.Fatalf("used %+v", used)
		}
	}
	bots[0].none(proto.PushItemUsed)
	var rejected proto.ItemUseRejected
	bots[1].notify(proto.NotifyItemUse, &proto.ItemUse{RaceID: raceID, UseID: 2<<24 | 1, TargetSlot: proto.NoSlot})
	bots[1].take(proto.PushItemUseRejected, &rejected, nil)
	if rejected.Reason != proto.RejectNotHeld {
		t.Fatalf("rejected %+v", rejected)
	}
	if n := inspect(t, h.eng, raceID, func(rs *raceState) int { return len(rs.seats[1].strikes) }); n != 2 {
		t.Fatalf("bot 1 strikes %d, want 2 (pickup too far, use without item)", n)
	}

	// Projectiles: bot 2 lies about where the egg is; corrected for everyone, liar struck once.
	for i, x := range []float32{30, 30.4, 80} {
		racing[i].udp.Report(proto.ProjectileReport{UseID: use1, Tick: 40, Pos: proto.Vec3{x, 0, 0},
			Vel: proto.Vec3{10, 0, 0}})
	}
	var corr proto.ProjectileCorrect
	for _, b := range racing {
		b.take(proto.PushProjectileCorrect, &corr, nil)
		if corr.UseID != use1 || corr.Pos[0] != 30.4 {
			t.Fatalf("correct %+v", corr)
		}
	}
	strikes := inspect(t, h.eng, raceID, func(rs *raceState) [3]int {
		return [3]int{len(rs.seats[0].strikes), len(rs.seats[1].strikes), len(rs.seats[2].strikes)}
	})
	if strikes != [3]int{0, 2, 1} {
		t.Fatalf("strikes %v", strikes)
	}

	// Hits: two eligible observers see bot 1 hit, bot 1 stays silent → HitForce; it then confirms.
	bots[0].notify(proto.NotifyItemUse, &proto.ItemUse{RaceID: raceID, UseID: use2, Tick: 50, TargetSlot: 1})
	bots[1].take(proto.PushItemUsed, &used, func() bool { return used.UseID == use2 })
	for _, i := range []int{0, 2} {
		racing[i].notify(proto.NotifyHitSeen, &proto.HitSeen{RaceID: raceID, VictimSlot: 1, AttackerSlot: 0, UseID: use2,
			EffectID: "mhk2.hit.shotdown", Tick: 60})
	}
	var force proto.HitForce
	bots[1].take(proto.PushHitForce, &force, nil)
	if force.UseID != use2 || force.AttackerSlot != 0 || force.EffectID != "mhk2.hit.shotdown" {
		t.Fatalf("force %+v", force)
	}
	bots[1].notify(proto.NotifyHitTaken, &proto.HitTaken{RaceID: raceID, AttackerSlot: 0, UseID: use2,
		EffectID: "mhk2.hit.shotdown", Tick: 70})
	var applied proto.HitApplied
	for _, i := range []int{0, 2} {
		racing[i].take(proto.PushHitApplied, &applied, nil)
		if applied.VictimSlot != 1 || applied.UseID != use2 {
			t.Fatalf("applied %+v", applied)
		}
	}

	// Cheat reports: two eligible observers report bot 2 → kicked, cheat_kicks row, UDP seat gone.
	for _, i := range []int{0, 1} {
		racing[i].notify(proto.NotifyCheatReport, &proto.CheatReport{RaceID: raceID, SuspectSlot: 2,
			Reason: proto.FlagReason(1), Tick: 80, Detail: "speed"})
	}
	for _, b := range racing {
		b.take(proto.PushPlayerLeftRace, &left, func() bool { return left.Slot == 2 })
		if left.Outcome != uint8(OutcomeCheatKick) {
			t.Fatalf("kick %+v", left)
		}
	}
	eventually(t, "cheat_kicks row", func() bool {
		st, err := h.st.AccountStats(context.Background(), bots[2].acc)
		return err == nil && st.CheatKicks == 1
	})
	_, before := bots[0].udp.World()
	eventually(t, "World without the kicked racer", func() bool {
		w, n := bots[0].udp.World()
		return n > before+2 && len(w.Entries) == 1 && w.Entries[0].Slot == 1
	})

	// Finishes: the race ends when both remaining racers finished.
	laps := []int32{20000, 20000, 20000}
	bots[1].notify(proto.NotifyFinished, &proto.Finished{RaceID: raceID, FinishMs: int32(time.Since(goMsg.Go).Milliseconds()),
		LapMs: laps})
	time.Sleep(20 * time.Millisecond)
	bots[0].notify(proto.NotifyFinished, &proto.Finished{RaceID: raceID, FinishMs: int32(time.Since(goMsg.Go).Milliseconds()),
		LapMs: laps})
	var pf proto.PlayerFinished
	bots[0].take(proto.PushPlayerFinished, &pf, func() bool { return pf.Slot == 1 })
	placements := recv(t, cb.finished)
	var got []string
	for _, p := range placements {
		got = append(got, fmt.Sprintf("%d:%d:%d", p.Slot, p.Position, p.Outcome))
	}
	want := []string{"1:1:0", "0:2:0", "2:3:3", "3:4:5"}
	if !slices.Equal(got, want) || placements[0].FinishMs <= 0 || placements[0].AccountID != bots[1].acc {
		t.Fatalf("placements %v (%+v), want %v", got, placements, want)
	}
	if _, ok := h.eng.RTT(bots[0].acc); ok {
		t.Fatal("seat still registered after the race")
	}
}

func TestRaceIntegrationAnticheatOff(t *testing.T) {
	h := startHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bots := make([]*bot, 3)
	racers := make([]Racer, 3)
	for i := range bots {
		bots[i] = newBot(t, h)
		racers[i] = bots[i].racer()
	}
	cb, callbacks := newCallbacks()
	raceID, err := h.eng.Start(Spec{LobbyID: 78, TrackID: "mhk2.egypt", Items: true}, racers, callbacks)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bots {
		b.joinUDP(ctx)
		b.notify(proto.NotifyRaceLoaded, integrationLoaded(raceID))
	}
	recv(t, cb.going)
	var goMsg proto.RaceGo
	for _, b := range bots {
		b.take(proto.PushRaceGo, &goMsg, nil)
	}
	eventually(t, "RTTs", func() bool {
		for _, b := range bots {
			if _, ok := h.eng.RTT(b.acc); !ok {
				return false
			}
		}
		return true
	})
	time.Sleep(time.Until(goMsg.Go) + 50*time.Millisecond)
	for _, b := range bots[:2] {
		b.notify(proto.NotifyCheatReport, &proto.CheatReport{RaceID: raceID, SuspectSlot: 2, Reason: proto.FlagReason(1)})
	}
	// Server checks still arbitrate but never strike.
	bots[1].notify(proto.NotifyItemUse, &proto.ItemUse{RaceID: raceID, UseID: 2<<24 | 1, TargetSlot: proto.NoSlot})
	var rejected proto.ItemUseRejected
	bots[1].take(proto.PushItemUseRejected, &rejected, nil)
	state := inspect(t, h.eng, raceID, func(rs *raceState) [2]int {
		out := 0
		if rs.seats[2].out {
			out = 1
		}
		return [2]int{out, len(rs.seats[1].strikes)}
	})
	if state != [2]int{0, 0} {
		t.Fatalf("anticheat off: kicked/strikes %v", state)
	}
	h.eng.EndEarly(raceID)
	placements := recv(t, cb.finished)
	for _, p := range placements {
		if p.Outcome != OutcomeDNF {
			t.Fatalf("%+v", placements)
		}
	}
	st, err := h.st.AccountStats(context.Background(), bots[2].acc)
	if err != nil || st.CheatKicks != 0 {
		t.Fatalf("cheat kicks %d %v", st.CheatKicks, err)
	}
}
