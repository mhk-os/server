package race

import (
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/wire"
)

// aiProgress feeds an AiProgress payload from s.
func aiProgress(rs *raceState, s *seat, entries ...proto.AiProgressEntry) {
	w := wire.NewWriter(64)
	(&proto.UDPAiProgressPayload{Entries: entries}).Encode(w)
	rs.onAiProgress(s, w.Bytes(), t0)
}

func TestAIHostElection(t *testing.T) {
	rs, env, _ := newAIRace(3, 3, Spec{})
	if rs.aiHost != rs.seats[3] || rs.aiHostSlot() != 3 {
		t.Fatalf("host %v", rs.aiHostSlot())
	}
	// Loading counts only humans: AI karts never send RaceLoaded.
	startRace(t, rs)
	// Leaving hands the AI karts to the next human; everybody still in hears it, AI karts get nothing.
	rs.leave(rs.seats[3], OutcomeLeft, "", t0)
	hosts := env.of(0, proto.PushAiHost)
	if len(hosts) != 2 || hosts[0].(*proto.AiHost).Slot != 4 || rs.aiHost != rs.seats[4] ||
		len(env.of(acc(4), proto.PushAiHost)) != 1 || len(env.of(acc(5), proto.PushAiHost)) != 1 {
		t.Fatalf("AiHost pushes %v", hosts)
	}
	for _, p := range env.pushes {
		if p.acc < 100 {
			t.Fatalf("push %d to account %d (an AI kart)", p.typ, p.acc)
		}
	}
	env.reset()
	// A non-host leaving changes nothing; finishing does not end hosting.
	rs.leave(rs.seats[5], OutcomeLeft, "", t0)
	if len(env.of(0, proto.PushAiHost)) != 0 || rs.aiHost != rs.seats[4] {
		t.Fatal("non-host leave re-elected")
	}
	// AI karts never leave and are never struck, kicked or eligible.
	rs.leave(rs.seats[0], OutcomeLeft, "", t0)
	rs.strike(rs.seats[0], "x", t0)
	rs.kickCheater(rs.seats[0], "x", "y", t0)
	if rs.seats[0].out || len(rs.seats[0].strikes) != 0 || len(env.kicks) != 0 || rs.eligible(rs.seats[0]) {
		t.Fatal("AI seat left, struck, kicked or eligible")
	}
	if rs.seatOf(0) != nil {
		t.Fatal("account 0 resolved to an AI seat")
	}
	// The last human leaving ends the race: AI alone does not race.
	rs.leave(rs.seats[4], OutcomeLeft, "", t0)
	if env.finishedN != 1 || rs.aiHost != nil {
		t.Fatalf("no human left: finished %d", env.finishedN)
	}

	// Re-election during loading is pushed too, and loading completes over the remaining humans.
	rs, env, _ = newAIRace(2, 2, Spec{})
	rs.onLoaded(rs.seats[3], testLoaded(), t0)
	rs.leave(rs.seats[2], OutcomeLeft, "", t0)
	if h := env.of(acc(3), proto.PushAiHost); len(h) != 1 || h[0].(*proto.AiHost).Slot != 3 || rs.phase != phaseRacing {
		t.Fatalf("loading re-election %v phase %d", h, rs.phase)
	}

	// Without AI nobody hosts.
	rs, _, _ = newTestRace(3, Spec{})
	if rs.aiHost != nil || rs.aiHostSlot() != proto.NoSlot {
		t.Fatal("host without AI")
	}
}

func TestAIPickup(t *testing.T) {
	rs, env, _ := newAIRace(2, 2, Spec{Items: true, Anticheat: true})
	goAt := startRace(t, rs)
	host, other := rs.seats[2], rs.seats[3]
	now := goAt.Add(time.Second)
	// No position check for AI karts: the host has no state near the box.
	rs.onPickup(host, &proto.ItemPickup{BoxID: 0, Tick: 60, Slot: 0}, now)
	g := env.of(0, proto.PushItemGranted)
	if len(g) != 2 || len(env.of(acc(2), proto.PushItemGranted)) != 1 || len(env.of(acc(3), proto.PushItemGranted)) != 1 {
		t.Fatalf("AI grant not sent to every human: %v", g)
	}
	if gr := g[0].(*proto.ItemGranted); gr.Slot != 0 || gr.PowerupID != "mhk2.rocket" || gr.Tick != 60 {
		t.Fatalf("grant %+v", gr)
	}
	if len(env.of(0, proto.PushItemBoxTaken)) != 0 || rs.seats[0].held != "mhk2.rocket" {
		t.Fatal("AI grant sent ItemBoxTaken or did not hold")
	}
	env.reset()
	// Held item: denied to the host only, with the AI slot; no strikes.
	rs.onPickup(host, &proto.ItemPickup{BoxID: 2, Slot: 0}, now)
	d := env.of(0, proto.PushItemDenied)
	if len(d) != 1 || len(env.of(acc(2), proto.PushItemDenied)) != 1 || d[0].(*proto.ItemDenied).Slot != 0 ||
		d[0].(*proto.ItemDenied).Reason != proto.DenyCannotReceive {
		t.Fatalf("AI deny %v", d)
	}
	// The box is taken for humans too.
	env.reset()
	setState(rs, other, proto.KartState{})
	rs.onPickup(other, &proto.ItemPickup{BoxID: 0, Slot: proto.OwnSlot}, now)
	if d := env.of(acc(3), proto.PushItemDenied); len(d) != 1 || d[0].(*proto.ItemDenied).Slot != 3 ||
		d[0].(*proto.ItemDenied).Reason != proto.DenyNotAvailable {
		t.Fatalf("taken by AI: %v", d)
	}
	env.reset()
	// Notifies about AI karts from a non-host, and about a human's slot from the host, are ignored.
	rs.onPickup(other, &proto.ItemPickup{BoxID: 1, Slot: 1}, now)
	rs.onPickup(host, &proto.ItemPickup{BoxID: 1, Slot: 3}, now)
	rs.onPickup(host, &proto.ItemPickup{BoxID: 1, Slot: 2}, now) // its own slot number, not OwnSlot
	rs.onPickup(host, &proto.ItemPickup{BoxID: 1, Slot: 9}, now)
	if len(env.pushes) != 0 || rs.seats[1].held != "" {
		t.Fatalf("ignored pickups pushed %v", env.pushes)
	}
	// Special boxes grant the AI slot's special.
	rs.agreed.Specials = append(rs.agreed.Specials, proto.Special{Slot: 1, PowerupID: "mhk2.aisuper"})
	rs.onPickup(host, &proto.ItemPickup{BoxID: 1, Slot: 1}, now)
	if g := env.of(acc(3), proto.PushItemGranted); len(g) != 1 || g[0].(*proto.ItemGranted).PowerupID != "mhk2.aisuper" {
		t.Fatalf("AI special %v", g)
	}
	for _, s := range rs.seats {
		if len(s.strikes) != 0 {
			t.Fatalf("slot %d struck", s.slot)
		}
	}
}

func TestAIUseHitsAndProjectiles(t *testing.T) {
	rs, env, _ := newAIRace(2, 3, Spec{Items: true, Anticheat: true})
	goAt := startRace(t, rs)
	host := rs.seats[2]
	now := goAt.Add(5 * time.Second)
	ai := rs.seats[1]
	ai.held, ai.charges = "mhk2.egg", 2
	useItem := func(from *seat, id uint32, slot uint8) uint8 {
		env.reset()
		rs.onUse(from, &proto.ItemUse{UseID: id, Tick: 9, TargetSlot: proto.NoSlot, Slot: slot}, now)
		if r := env.of(from.racer.AccountID, proto.PushItemUseRejected); len(r) == 1 {
			return r[0].(*proto.ItemUseRejected).Reason
		}
		return 0
	}
	// The use id must carry the AI slot; a wrong one is rejected to the host without a strike.
	if r := useItem(host, 3<<24|1, 1); r != proto.RejectNotHeld || len(host.strikes) != 0 {
		t.Fatalf("host-slot use id for AI: %d strikes %d", r, len(host.strikes))
	}
	if r := useItem(host, 2<<24|1, 1); r != 0 {
		t.Fatalf("AI use rejected: %d", r)
	}
	u := env.of(0, proto.PushItemUsed)
	if len(u) != 2 || len(env.of(acc(2), proto.PushItemUsed)) != 0 || u[0].(*proto.ItemUsed).Slot != 1 ||
		u[0].(*proto.ItemUsed).PowerupID != "mhk2.egg" || ai.charges != 1 {
		t.Fatalf("AI ItemUsed %v", u)
	}
	if r := useItem(rs.seats[3], 2<<24|2, 1); r != 0 || len(env.pushes) != 0 || ai.charges != 1 {
		t.Fatal("non-host AI use not ignored")
	}

	// ProjectileGone from the host ends tracking of the AI's use (the host counts as its owner).
	if rs.live[2<<24|1] == nil {
		t.Fatal("AI use not tracked")
	}
	for _, s := range rs.seats[3:] {
		setState(rs, s, proto.KartState{}, proto.ProjectileReport{UseID: 2<<24 | 1})
	}
	rs.onProjectileGone(rs.seats[4], &proto.ProjectileGone{UseID: 2<<24 | 1}, now)
	if rs.live[2<<24|1] == nil {
		t.Fatal("a non-owner ended tracking alone")
	}
	rs.onProjectileGone(host, &proto.ProjectileGone{UseID: 2<<24 | 1}, now)
	if rs.live[2<<24|1] != nil || rs.uses[2<<24|1].proj != nil {
		t.Fatal("host's ProjectileGone did not end the AI use")
	}

	// HitTaken about an AI kart from the host: HitApplied to everyone but the host, once per use.
	env.reset()
	rs.onHitTaken(host, &proto.HitTaken{AttackerSlot: 3, UseID: 4<<24 | 1, EffectID: "hit", Slot: 0}, now)
	rs.onHitTaken(host, &proto.HitTaken{AttackerSlot: 3, UseID: 4<<24 | 1, EffectID: "hit", Slot: 0}, now)
	h := env.of(0, proto.PushHitApplied)
	if len(h) != 2 || len(env.of(acc(2), proto.PushHitApplied)) != 0 || h[0].(*proto.HitApplied).VictimSlot != 0 {
		t.Fatalf("AI HitApplied %v", h)
	}
	env.reset()
	rs.onHitTaken(rs.seats[3], &proto.HitTaken{UseID: 4<<24 | 2, Slot: 0}, now)
	if len(env.pushes) != 0 {
		t.Fatal("non-host AI hit relayed")
	}
	// Hit claims, cheat reports and unknown-content reports about AI karts are ignored.
	rs.uses[4<<24|3] = &use{owner: 3}
	rs.onHitSeen(rs.seats[3], &proto.HitSeen{VictimSlot: 0, AttackerSlot: 3, UseID: 4<<24 | 3}, now)
	rs.onHitSeen(rs.seats[4], &proto.HitSeen{VictimSlot: 0, AttackerSlot: 3, UseID: 4<<24 | 3}, now)
	rs.onCheatReport(rs.seats[3], &proto.CheatReport{SuspectSlot: 0, Reason: proto.FlagSpeed}, now)
	rs.onUnknownContent(rs.seats[3], &proto.UnknownContent{Slot: 0, Kind: 2, ID: "x.y"}, now)
	rs.onUnknownContent(rs.seats[4], &proto.UnknownContent{Slot: 0, Kind: 2, ID: "x.y"}, now)
	rs.tickHits(now.Add(time.Second))
	if len(rs.claims) != 0 || len(env.reports) != 0 || len(rs.unknown) != 0 || len(env.pushes) != 0 {
		t.Fatalf("claims %d reports %d unknown %d pushes %d", len(rs.claims), len(env.reports), len(rs.unknown),
			len(env.pushes))
	}
}

func TestAIProgressRankingAndPlacements(t *testing.T) {
	rs, env, _ := newAIRace(2, 2, Spec{Items: true})
	goAt := startRace(t, rs)
	host, other := rs.seats[2], rs.seats[3]
	setState(rs, host, proto.KartState{RaceDistance: 500})
	setState(rs, other, proto.KartState{RaceDistance: 100})
	aiProgress(rs, host, proto.AiProgressEntry{Slot: 0, Lap: 2, RaceDistance: 900, Health: 50},
		proto.AiProgressEntry{Slot: 1, Lap: 1, RaceDistance: 300}, proto.AiProgressEntry{Slot: 3, RaceDistance: 9999},
		proto.AiProgressEntry{Slot: 40, RaceDistance: 1})
	if rs.seats[0].state.RaceDistance != 900 || rs.seats[0].state.Lap != 2 || rs.seats[0].state.Health != 50 ||
		other.state.RaceDistance != 100 || rs.seats[0].hasState {
		t.Fatal("AiProgress not applied to AI seats only")
	}
	// Ranking: AI 0 (900), host (500), AI 1 (300), other (100).
	for slot, want := range []float64{0, 2.0 / 3, 1.0 / 3, 1} {
		if got := rs.positionFraction(rs.seats[slot]); got != want {
			t.Fatalf("slot %d fraction %v, want %v", slot, got, want)
		}
	}
	// Progress from anybody but the host is ignored.
	aiProgress(rs, other, proto.AiProgressEntry{Slot: 1, RaceDistance: 2000})
	if rs.seats[1].state.RaceDistance != 300 {
		t.Fatal("non-host AiProgress applied")
	}
	// An AI finish the server cannot believe is taken at its clock, without a strike; everybody hears it.
	at := goAt.Add(60 * time.Second)
	rs.onFinished(host, &proto.Finished{FinishMs: 10_000, LapMs: []int32{1, 1, 1}, Slot: 1}, at)
	pf := env.of(0, proto.PushPlayerFinished)
	if len(pf) != 2 || pf[0].(*proto.PlayerFinished).Slot != 1 || pf[0].(*proto.PlayerFinished).FinishMs != 60_000 ||
		!rs.seats[1].finished || len(host.strikes) != 0 {
		t.Fatalf("AI finish %v", pf)
	}
	// The first finisher (an AI kart) starts the 30 s; the rest are DNF by distance.
	rs.tick(at.Add(afterFirstFinish - time.Millisecond))
	if env.finishedN != 0 {
		t.Fatal("ended early")
	}
	rs.tick(at.Add(afterFirstFinish))
	want := []Placement{
		{Slot: 1, Position: 1, FinishMs: 60_000, Outcome: OutcomeFinished, AI: true, CharacterID: "mhk2.char1"},
		{Slot: 0, Position: 2, Outcome: OutcomeDNF, AI: true, CharacterID: "mhk2.char0"},
		{Slot: 2, AccountID: acc(2), Username: "r", Position: 3, Outcome: OutcomeDNF},
		{Slot: 3, AccountID: acc(3), Username: "r", Position: 4, Outcome: OutcomeDNF},
	}
	if env.finishedN != 1 || !slices.Equal(env.placements, want) {
		t.Fatalf("placements\n%+v\nwant\n%+v", env.placements, want)
	}

	// Every kart finished: the race ends at once.
	rs, env, _ = newAIRace(1, 1, Spec{})
	goAt = startRace(t, rs)
	laps := []int32{1, 1, 1}
	rs.onFinished(rs.seats[1], &proto.Finished{FinishMs: 61_000, LapMs: laps, Slot: proto.OwnSlot}, goAt.Add(61*time.Second))
	if env.finishedN != 0 {
		t.Fatal("ended with the AI kart still driving")
	}
	rs.onFinished(rs.seats[1], &proto.Finished{FinishMs: 62_000, LapMs: laps, Slot: 0}, goAt.Add(62*time.Second))
	if env.finishedN != 1 || env.placements[1].Slot != 0 || !env.placements[1].AI {
		t.Fatalf("all finished %+v", env.placements)
	}
}

func TestAIBattle(t *testing.T) {
	rs, env, _ := newAIRace(2, 2, Spec{Mode: ModeBattle})
	goAt := startRace(t, rs)
	host := rs.seats[2]
	aiProgress(rs, host, proto.AiProgressEntry{Slot: 0, Health: 3}, proto.AiProgressEntry{Slot: 1, Health: 7})
	setState(rs, host, proto.KartState{Health: 5})
	// AI karts never get the silent-zero-health treatment (they have no states).
	rs.tick(goAt.Add(time.Second))
	rs.tick(goAt.Add(4 * time.Second))
	if rs.seats[0].eliminated || rs.seats[1].eliminated {
		t.Fatal("AI kart eliminated without a report")
	}
	rs.onEliminatedSelf(rs.seats[3], &proto.EliminatedSelf{AttackerSlot: 1, Tick: 300, Slot: 0}, goAt.Add(5*time.Second))
	if rs.seats[0].eliminated {
		t.Fatal("non-host AI elimination accepted")
	}
	rs.onEliminatedSelf(host, &proto.EliminatedSelf{AttackerSlot: 1, Tick: 300, Slot: 0}, goAt.Add(5*time.Second))
	if e := env.of(0, proto.PushEliminated); len(e) != 2 || e[0].(*proto.Eliminated).Slot != 0 {
		t.Fatalf("AI Eliminated %v", e)
	}
	// No human alive ends the battle while an AI kart still drives; survivors rank by health (AI from AiProgress).
	rs.onEliminatedSelf(rs.seats[3], &proto.EliminatedSelf{AttackerSlot: 1, Slot: proto.OwnSlot}, goAt.Add(6*time.Second))
	if env.finishedN != 0 {
		t.Fatal("ended with a human alive")
	}
	rs.onEliminatedSelf(host, &proto.EliminatedSelf{AttackerSlot: 1, Slot: proto.OwnSlot}, goAt.Add(7*time.Second))
	if env.finishedN != 1 {
		t.Fatal("battle without a live human did not end")
	}
	var slots []uint8
	for _, p := range env.placements {
		slots = append(slots, p.Slot)
	}
	if !slices.Equal(slots, []uint8{1, 2, 3, 0}) || !env.placements[0].AI {
		t.Fatalf("battle placements %+v", env.placements)
	}

	// The last kart alive being an AI kart ends it too.
	rs, env, _ = newAIRace(1, 1, Spec{Mode: ModeBattle})
	goAt = startRace(t, rs)
	rs.onEliminatedSelf(rs.seats[1], &proto.EliminatedSelf{AttackerSlot: 0, Slot: proto.OwnSlot}, goAt.Add(time.Second))
	if env.finishedN != 1 || env.placements[0].Slot != 0 {
		t.Fatalf("AI survivor %+v", env.placements)
	}
}

// fiftyKarts fills a race of humans + ai = 50 with typical states (3 effects) and addresses.
func fiftyKarts(t testing.TB, ai int) (*raceState, *fakeEnv) {
	rs, env, _ := newAIRace(ai, proto.MaxRacers-ai, Spec{})
	for _, s := range rs.seats {
		if !s.ai {
			rs.onLoaded(s, testLoaded(), t0)
		}
	}
	if rs.phase != phaseRacing {
		t.Fatalf("phase %d", rs.phase)
	}
	for _, s := range rs.seats {
		if s.ai {
			continue
		}
		setState(rs, s, proto.KartState{Tick: 100, Pos: proto.Vec3{float32(s.slot), 1, 2}, Lap: 1,
			RaceDistance: float32(s.slot), Health: 100, Effects: []uint16{1, 2, 3}})
		addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 1000+uint16(s.slot))
		s.addr.Store(&addr)
	}
	if ai > 0 {
		entries := make([]proto.AiProgressEntry, ai)
		for i := range entries {
			entries[i] = proto.AiProgressEntry{Slot: uint8(i), Lap: 1, RaceDistance: float32(i), Health: 100}
		}
		aiProgress(rs, rs.aiHost, entries...)
	}
	return rs, env
}

// TestWorldFiftyRacers: with 50 humans every World datagram fits 1200 bytes and each racer gets all 49 others;
// with 48 AI karts the two humans get one entry each and nothing is sent for AI.
func TestWorldFiftyRacers(t *testing.T) {
	for _, ai := range []int{0, 48} {
		rs, env := fiftyKarts(t, ai)
		env.keepUDP = true
		rs.world(t0.Add(time.Second))
		got := map[uint16]map[uint8]bool{} // receiver port → slots
		for i, d := range env.datagrams {
			if len(d) > proto.UDPMaxDatagram {
				t.Fatalf("datagram of %d bytes", len(d))
			}
			h, payload, ok := proto.ParseUDP(d)
			to := rs.seats[env.udpTo[i]-1000]
			if !ok || h.Type != proto.UDPWorld || h.Token != to.token || !proto.NewUDPMac(to.key).Verify(d) {
				t.Fatalf("bad datagram %+v to slot %d", h, to.slot)
			}
			var w proto.UDPWorldPayload
			if err := w.Decode(wire.NewReader(payload)); err != nil {
				t.Fatal(err)
			}
			if got[env.udpTo[i]] == nil {
				got[env.udpTo[i]] = map[uint8]bool{}
			}
			for _, e := range w.Entries {
				if rs.seats[e.Slot].ai || e.Slot == to.slot || got[env.udpTo[i]][e.Slot] {
					t.Fatalf("entry for slot %d to slot %d", e.Slot, to.slot)
				}
				got[env.udpTo[i]][e.Slot] = true
			}
		}
		humans := proto.MaxRacers - ai
		if len(got) != humans {
			t.Fatalf("%d AI: %d receivers", ai, len(got))
		}
		for port, slots := range got {
			if len(slots) != humans-1 {
				t.Fatalf("%d AI: receiver %d got %d states", ai, port-1000, len(slots))
			}
		}
		t.Logf("%d AI: %d datagrams per World tick", ai, len(env.datagrams))
	}
}

// BenchmarkTick measures one 30 Hz race tick (World for every human plus the rule timers) of a full grid.
func BenchmarkTick(b *testing.B) {
	for _, c := range []struct {
		name string
		ai   int
	}{{"50humans", 0}, {"2humans48ai", 48}} {
		b.Run(c.name, func(b *testing.B) {
			rs, _ := fiftyKarts(b, c.ai)
			// Plain Info logging as in production: the per-tick paths must not pay for debug arguments.
			rs.log = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
			now := rs.goAt.Add(time.Second)
			b.ReportAllocs()
			for b.Loop() {
				rs.tick(now)
			}
		})
	}
}

// An AI kart's projectile flies in every client's own copy of the race: far reports are corrected, never struck.
func TestAIProjectileOutliersNotStruck(t *testing.T) {
	rs, env, _ := newAIRace(1, 4, Spec{Items: true, Anticheat: true})
	goAt := startRace(t, rs)
	ai := rs.seats[0]
	ai.held, ai.charges = "mhk2.rocket", 1
	id := uint32(1<<24 | 1)
	rs.onUse(rs.aiHost, &proto.ItemUse{UseID: id, TargetSlot: proto.NoSlot, Slot: 0}, goAt.Add(2*time.Second))
	for round := range 2 {
		for i, x := range []float32{10, 10.4, 9.8, 50} {
			setState(rs, rs.seats[1+i], proto.KartState{}, proto.ProjectileReport{UseID: id, Tick: 120,
				Pos: proto.Vec3{x, 0, 0}})
		}
		env.reset()
		rs.tickProjectiles(goAt.Add(time.Duration(3+round) * time.Second))
		if c := env.of(0, proto.PushProjectileCorrect); len(c) != 4 {
			t.Fatalf("round %d: %d corrections", round, len(c))
		}
	}
	for _, s := range rs.seats[1:] {
		if len(s.strikes) != 0 {
			t.Fatalf("slot %d struck over an AI projectile", s.slot)
		}
	}
}
