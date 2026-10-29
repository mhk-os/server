package lobby

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/store"
)

func aiOf(racers []race.Racer) []race.Racer {
	return slices.DeleteFunc(slices.Clone(racers), func(r race.Racer) bool { return !r.AI })
}

func humansOf(racers []race.Racer) []race.Racer {
	return slices.DeleteFunc(slices.Clone(racers), func(r race.Racer) bool { return r.AI })
}

// placed finishes every kart of a race in grid order (slot n at position n + 1): AI karts lead.
func placed(call startCall) []race.Placement {
	out := make([]race.Placement, len(call.racers))
	for i, r := range call.racers {
		out[i] = race.Placement{Slot: uint8(i), AccountID: r.AccountID, Username: r.Username, Position: uint8(i + 1),
			FinishMs: int32(60_000 + i), AI: r.AI, CharacterID: r.CharacterID}
	}
	return out
}

// waitRecorded waits for n RecordRace calls.
func (e *env) waitRecorded(t *testing.T, n int) [][]store.RaceResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.scores.recorded()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d races recorded, want %d", len(e.scores.recorded()), n)
		}
		time.Sleep(time.Millisecond)
	}
	return e.scores.recorded()
}

func aiRoom(maxPlayers, maxAI uint8) proto.RoomSettings {
	s := room()
	s.MaxPlayers, s.MaxAI, s.ItemOdds = maxPlayers, maxAI, nil
	return s
}

// TestAICounts: a race has min(maxAi, maxPlayers − humans) AI karts in front of the humans.
func TestAICounts(t *testing.T) {
	e := newEnv(t)
	owner := e.player(1)
	st := e.ok(e.m.CreateRoom(owner, aiRoom(10, 5)))
	for id := uint64(1); id <= 6; id++ {
		if id > 1 {
			e.ok(e.m.Join(e.player(id), 0, st.Code))
		}
		if id <= 4 {
			must(t, e.m.SetLoadout(Player{AccountID: id}, "game.huhn", "game.kart"))
		}
	}
	l := e.lobbyOf(1)
	// Members without a loadout sit the race out: 4 humans race 5 AI.
	must(t, e.m.StartRace(owner, "mhk2.rome"))
	call := e.races.last()
	if len(aiOf(call.racers)) != 5 || len(humansOf(call.racers)) != 4 || !call.racers[4].AI || call.racers[5].AI {
		t.Fatalf("4 humans: racers %+v", call.racers)
	}
	finish(t, e, l, call, placed(call), proto.PhaseResults)
	e.clock.Advance(10 * time.Second)
	for id := uint64(5); id <= 6; id++ {
		must(t, e.m.SetLoadout(Player{AccountID: id}, "game.huhn", "game.kart"))
	}
	must(t, e.m.StartRace(owner, "mhk2.rome"))
	call2 := e.races.last()
	if len(aiOf(call2.racers)) != 4 || len(humansOf(call2.racers)) != 6 {
		t.Fatalf("6 humans: racers %+v", call2.racers)
	}
	// The set's drivers race again in the same order.
	for i, d := range aiOf(call2.racers) {
		if d.CharacterID != call.racers[i].CharacterID || d.KartID != call.racers[i].KartID {
			t.Fatalf("driver %d changed: %+v, was %+v", i, d, call.racers[i])
		}
	}
}

func TestPublicAILobby(t *testing.T) {
	e := newEnv(t)
	// One human is enough to start the cycle in an AI category.
	s := e.ok(e.m.QuickJoin(e.player(1), 11))
	l := e.lobbyOf(1)
	if s.Summary.Phase != proto.PhaseVoting || s.Settings.MaxAI != 9 || s.Summary.MaxAI != 9 ||
		s.Summary.Name != "Races + AI #1" || len(s.AIDrivers) != 0 {
		t.Fatalf("AI lobby %+v", s.Summary)
	}
	wantStatus(t, e.m.StartRace(Player{AccountID: 1}, ""), proto.StatusForbidden, "voting")
	must(t, e.m.SetLoadout(Player{AccountID: 1}, "game.huhn", "game.kart"))
	must(t, e.m.Vote(Player{AccountID: 1}, "mhk2.rome"))
	call := e.races.last()
	ai := aiOf(call.racers)
	if len(ai) != 9 || len(call.racers) != 10 || call.racers[9].AccountID != 1 || call.spec.SetRace != 1 {
		t.Fatalf("1 human: racers %+v", call.racers)
	}
	// Characters nobody races with come first (the human drives game.huhn), then repeat.
	if c := []string{ai[0].CharacterID, ai[1].CharacterID, ai[2].CharacterID}; slices.Contains(c, "game.huhn") ||
		c[0] == c[1] || c[1] == c[2] || c[0] == c[2] || ai[3].CharacterID != "game.huhn" ||
		ai[4].CharacterID != ai[0].CharacterID {
		t.Fatalf("driver characters %+v", ai)
	}
	for _, d := range ai {
		if d.KartID != "game.kart" && d.KartID != "mhk2.speedy" || d.AccountID != 0 {
			t.Fatalf("driver %+v", d)
		}
	}
	st := e.push.lastState(t, 1)
	if len(st.AIDrivers) != 9 || st.AIDrivers[2].CharacterID != ai[2].CharacterID || st.AIDrivers[2].KartID != ai[2].KartID {
		t.Fatalf("LobbyState.aiDrivers %+v", st.AIDrivers)
	}
	// Race 1: the AI karts lead; one human races, so nothing is scored.
	finish(t, e, l, call, placed(call), proto.PhaseResults)
	res := e.push.of(1, proto.PushRaceResults)[0].(*proto.RaceResults)
	if len(res.Rows) != 10 || !res.Rows[0].AI || res.Rows[0].Points != 15 || res.Rows[0].SetPoints != 15 ||
		res.Rows[0].CharacterID != ai[0].CharacterID || res.Rows[0].AccountID != 0 || res.Rows[9].Points != 2 {
		t.Fatalf("results %+v", res.Rows)
	}
	if st := e.push.lastState(t, 1); st.AIDrivers[0].SetPoints != 15 || st.AIDrivers[8].SetPoints != 3 ||
		st.Members[0].SetPoints != 2 || len(st.LastResults) != 10 {
		t.Fatalf("set points %+v %+v", st.AIDrivers, st.Members)
	}

	// A second human joins during the results: race 2 has 8 AI, the set's first 8 drivers.
	e.ok(e.m.QuickJoin(e.player(2), 11))
	must(t, e.m.SetLoadout(Player{AccountID: 2}, "game.hank", "mhk2.speedy"))
	e.clock.Advance(10 * time.Second)
	if e.phase(l) != proto.PhaseVoting {
		t.Fatal("results did not end in Voting")
	}
	must(t, e.m.Vote(Player{AccountID: 1}, "mhk2.rome"))
	must(t, e.m.Vote(Player{AccountID: 2}, "mhk2.rome"))
	call = e.races.last()
	ai2 := aiOf(call.racers)
	if len(ai2) != 8 || len(humansOf(call.racers)) != 2 || call.spec.SetRace != 2 {
		t.Fatalf("2 humans: racers %+v", call.racers)
	}
	for i, d := range ai2 {
		if d != ai[i] {
			t.Fatalf("driver %d %+v, was %+v", i, d, ai[i])
		}
	}
	finish(t, e, l, call, placed(call), proto.PhaseResults)
	// Two humans: the account scores of the humans (only) are recorded.
	rec := e.waitRecorded(t, 1)
	if len(rec[0]) != 2 || rec[0][0].AccountID == 0 || rec[0][1].AccountID == 0 || rec[0][0].Points != 3 ||
		rec[0][1].Points != 2 {
		t.Fatalf("recorded %+v", rec)
	}
	if st := e.push.lastState(t, 1); st.AIDrivers[0].SetPoints != 30 || st.AIDrivers[8].SetPoints != 3 {
		t.Fatalf("AI set points %+v", st.AIDrivers)
	}

	// Race 3: account 2's catalog lacks mhk2.lola: drivers with it are drawn again, their points stay.
	lola := slices.IndexFunc(ai, func(r race.Racer) bool { return r.CharacterID == "mhk2.lola" })
	before := e.push.lastState(t, 1).AIDrivers
	e.cat.mu.Lock()
	e.cat.hidden[2] = map[string]bool{"mhk2.lola": true}
	e.cat.mu.Unlock()
	e.clock.Advance(10 * time.Second)
	must(t, e.m.Vote(Player{AccountID: 1}, "mhk2.rome"))
	must(t, e.m.Vote(Player{AccountID: 2}, "mhk2.rome"))
	call = e.races.last()
	for _, d := range aiOf(call.racers) {
		if d.CharacterID == "mhk2.lola" {
			t.Fatalf("invalid driver kept: %+v", call.racers)
		}
	}
	after := e.push.lastState(t, 1).AIDrivers
	if lola < 0 || after[lola].CharacterID == "mhk2.lola" || after[lola].SetPoints != before[lola].SetPoints {
		t.Fatalf("redraw of driver %d: before %+v after %+v", lola, before, after)
	}
	finish(t, e, l, call, placed(call), proto.PhaseResults)
	e.clock.Advance(10 * time.Second)
	must(t, e.m.Vote(Player{AccountID: 1}, "mhk2.rome"))
	must(t, e.m.Vote(Player{AccountID: 2}, "mhk2.rome"))
	finish(t, e, l, e.races.last(), placed(e.races.last()), proto.PhaseSetResults)
	// The set ends: the drivers keep their points through SetResults, then the next set draws new ones.
	if st := e.push.lastState(t, 1); len(st.AIDrivers) != 9 || st.AIDrivers[0].SetPoints != 60 {
		t.Fatalf("SetResults drivers %+v", st.AIDrivers)
	}
	e.clock.Advance(15 * time.Second)
	if st := e.push.lastState(t, 1); len(st.AIDrivers) != 0 || st.SetRace != 0 || st.Summary.Phase != proto.PhaseVoting {
		t.Fatalf("after the set %+v", st)
	}
	// Dropping to one human keeps the AI lobby voting; with nobody it waits.
	e.m.Leave(2)
	if e.phase(l) != proto.PhaseVoting {
		t.Fatal("one human stopped the AI lobby's voting")
	}
	e.m.Leave(1)
	if e.phase(l) != proto.PhaseWaiting {
		t.Fatal("empty AI lobby not waiting")
	}
}

func TestFiftyMemberRoom(t *testing.T) {
	e := newEnv(t)
	owner := e.player(1)
	st := e.ok(e.m.CreateRoom(owner, aiRoom(50, 49)))
	for id := uint64(2); id <= 50; id++ {
		e.ok(e.m.Join(e.player(id), 0, st.Code))
	}
	_, err := e.m.Join(e.player(51), 0, st.Code)
	wantStatus(t, err, proto.StatusForbidden, "full")
	// Alone with a loadout, the owner races 49 AI karts.
	must(t, e.m.SetLoadout(owner, "game.huhn", "game.kart"))
	must(t, e.m.StartRace(owner, "mhk2.rome"))
	call := e.races.last()
	if len(aiOf(call.racers)) != 49 || len(call.racers) != proto.MaxRacers {
		t.Fatalf("%d racers", len(call.racers))
	}
	if st := e.push.lastState(t, 50); len(st.Members) != 50 || len(st.AIDrivers) != 49 {
		t.Fatalf("state with %d members, %d drivers", len(st.Members), len(st.AIDrivers))
	}
	// Everybody racing: 50 humans, no room for AI.
	l := e.lobbyOf(1)
	finish(t, e, l, call, placed(call), proto.PhaseResults)
	e.clock.Advance(10 * time.Second)
	for id := uint64(2); id <= 50; id++ {
		must(t, e.m.SetLoadout(Player{AccountID: id}, "game.hank", "mhk2.speedy"))
	}
	must(t, e.m.StartRace(owner, "mhk2.rome"))
	if call := e.races.last(); len(call.racers) != 50 || len(aiOf(call.racers)) != 0 {
		t.Fatalf("full room: %d racers, %d AI", len(call.racers), len(aiOf(call.racers)))
	}
}

func TestCustomChampionship(t *testing.T) {
	e := newEnv(t)
	owner := e.player(1)
	s := aiRoom(4, 2)
	s.Mode, s.TrackPick, s.RacesPerSet = proto.RoomChampionship, proto.PickVote, 0
	s.TrackIDs = []string{"mhk2.rome", "mhk2.alps", "mhk2.rome"}
	st := e.ok(e.m.CreateRoom(owner, s))
	if st.Settings.RacesPerSet != 3 || st.SetSize != 3 || st.Settings.TrackPick != proto.PickOwner ||
		!slices.Equal(st.Settings.TrackIDs, s.TrackIDs) || st.Settings.CupID != "" {
		t.Fatalf("custom championship %+v", st.Settings)
	}
	e.ok(e.m.Join(e.player(2), 0, st.Code))
	for id := uint64(1); id <= 2; id++ {
		must(t, e.m.SetLoadout(Player{AccountID: id}, "game.huhn", "game.kart"))
	}
	l := e.lobbyOf(1)
	run := func(track, want string) {
		t.Helper()
		must(t, e.m.StartRace(owner, track))
		call := e.races.last()
		if call.spec.TrackID != want {
			t.Fatalf("race on %s, want %s", call.spec.TrackID, want)
		}
		phase := proto.PhaseResults
		if call.spec.SetRace == 3 {
			phase = proto.PhaseSetResults
		}
		finish(t, e, l, call, placed(call), phase)
		e.clock.Advance(15 * time.Second)
	}
	run("", "mhk2.rome")
	wantStatus(t, e.m.StartRace(owner, "mhk2.rome"), proto.StatusBadRequest, "mhk2.alps")
	wantStatus(t, e.m.StartRace(owner, "mhk3.paris"), proto.StatusBadRequest, "mhk2.alps")
	run("mhk2.alps", "mhk2.alps")
	run("", "mhk2.rome") // duplicates are fine; the third race ends the set
	if st := e.push.lastState(t, 1); st.SetRace != 0 || e.phase(l) != proto.PhaseWaiting {
		t.Fatalf("after the set %+v", st)
	}
	run("", "mhk2.rome") // the next set starts over

	// Settings changes draw the AI drivers again; other modes drop the list.
	if st := e.push.lastState(t, 1); len(st.AIDrivers) != 2 {
		t.Fatalf("drivers %+v", st.AIDrivers)
	}
	upd := aiRoom(4, 2)
	upd.TrackIDs = []string{"mhk2.rome"}
	must(t, e.m.UpdateRoom(owner, upd))
	if st := e.push.lastState(t, 1); len(st.AIDrivers) != 0 || len(st.Settings.TrackIDs) != 0 ||
		st.Settings.RacesPerSet != 2 {
		t.Fatalf("after UpdateRoom %+v", st)
	}
}

// TestLoneHumanRaces: solo races only for the lone member of a public lobby without AI (6 throwaway drivers,
// outside the set); EndRaceEarly and SoloRaceJoinWaiting whenever exactly one human races.
func TestLoneHumanRaces(t *testing.T) {
	e := newEnv(t)
	e.ok(e.m.QuickJoin(e.player(1), 1))
	must(t, e.m.SetLoadout(Player{AccountID: 1}, "game.huhn", "game.kart"))
	must(t, e.m.StartRace(Player{AccountID: 1}, ""))
	call := e.races.last()
	l := e.lobbyOf(1)
	e.m.mu.Lock()
	roster := l.roster
	e.m.mu.Unlock()
	if len(aiOf(call.racers)) != 6 || call.spec.SetRace != 0 || len(e.push.lastState(t, 1).AIDrivers) != 0 ||
		roster != nil {
		t.Fatalf("solo race %+v", call)
	}
	call.cb.Finished(call.id, placed(call))
	e.waitPhase(t, l, proto.PhaseWaiting)
	res := e.push.of(1, proto.PushRaceResults)[0].(*proto.RaceResults)
	for _, r := range res.Rows {
		if r.Points != 0 || r.SetPoints != 0 {
			t.Fatalf("solo race scored %+v", res.Rows)
		}
	}

	// A private room's owner racing alone against the room's AI: a set race, ended early for a newcomer.
	rm := e.ok(e.m.CreateRoom(e.player(10), aiRoom(6, 5)))
	must(t, e.m.SetLoadout(Player{AccountID: 10}, "game.huhn", "game.kart"))
	must(t, e.m.StartRace(Player{AccountID: 10}, "mhk2.rome"))
	call = e.races.last()
	if len(aiOf(call.racers)) != 5 || call.spec.SetRace != 1 {
		t.Fatalf("private race alone %+v", call.spec)
	}
	e.ok(e.m.Join(e.player(11), 0, rm.Code))
	if w := e.push.of(10, proto.PushSoloRaceJoinWaiting); len(w) != 1 || w[0].(*proto.SoloRaceJoinWaiting).Username != "p11" {
		t.Fatalf("SoloRaceJoinWaiting %v", w)
	}
	wantStatus(t, e.m.EndRaceEarly(Player{AccountID: 11}), proto.StatusBadRequest, "alone")
	must(t, e.m.EndRaceEarly(Player{AccountID: 10}))

	// The lone human of an AI lobby may end early too; with two humans nobody may.
	e.ok(e.m.QuickJoin(e.player(20), 12))
	must(t, e.m.SetLoadout(Player{AccountID: 20}, "game.huhn", "game.kart"))
	must(t, e.m.Vote(Player{AccountID: 20}, ""))
	aiRace := e.races.last()
	must(t, e.m.EndRaceEarly(Player{AccountID: 20}))
	if !slices.Contains(e.races.ended, aiRace.id) {
		t.Fatalf("ended %v", e.races.ended)
	}
	e.ok(e.m.QuickJoin(e.player(21), 12))
	if len(e.push.of(20, proto.PushSoloRaceJoinWaiting)) != 1 {
		t.Fatal("lone AI lobby racer not told")
	}

	cl := e.lobbyOf(20)
	aiRace.cb.Finished(aiRace.id, placed(aiRace))
	e.waitPhase(t, cl, proto.PhaseResults)
	must(t, e.m.SetLoadout(Player{AccountID: 21}, "game.huhn", "game.kart"))
	e.clock.Advance(10 * time.Second)
	must(t, e.m.Vote(Player{AccountID: 20}, ""))
	must(t, e.m.Vote(Player{AccountID: 21}, ""))
	wantStatus(t, e.m.EndRaceEarly(Player{AccountID: 20}), proto.StatusBadRequest, "alone")
	if got := fmt.Sprint(len(aiOf(e.races.last().racers))); got != "8" {
		t.Fatalf("%s AI with two humans", got)
	}
}
