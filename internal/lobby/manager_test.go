package lobby

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/store"
)

func instances(e *env, cat uint8, sim int32) []uint16 {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	var out []uint16
	for _, l := range e.m.instances[instanceKey{cat, sim}] {
		out = append(out, l.instance)
	}
	return out
}

func TestListOpensOneInstancePerCategory(t *testing.T) {
	e := newEnv(t)
	list, err := e.m.List(e.player(1))
	must(t, err)
	if len(list) != maxCategory {
		t.Fatalf("%d lobbies listed", len(list))
	}
	for i, s := range list {
		c := categories[i+1]
		if s.Category != uint8(i+1) || s.Instance != 1 || s.Kind != proto.LobbyPublic || s.SimVersion != testSim ||
			s.MaxPlayers != 10 || s.Mode != c.mode || s.Anticheat != c.anticheat || !slices.Equal(s.Packs, c.packs) ||
			s.Phase != proto.PhaseWaiting || s.Name != c.name+" #1" {
			t.Fatalf("summary %d: %+v", i, s)
		}
	}
	// Listing again does not open more; another simulation version gets its own instances.
	list2, _ := e.m.List(e.player(1))
	if len(list2) != maxCategory || list2[0].LobbyID != list[0].LobbyID {
		t.Fatal("second list changed the instances")
	}
	e.cat.announce(2, testSim+1, allPacks...)
	list3, _ := e.m.List(Player{AccountID: 2, Username: "p2"})
	if list3[0].SimVersion != testSim+1 || list3[0].LobbyID == list[0].LobbyID {
		t.Fatal("simulation versions share instances")
	}
}

func TestInstancesOpenAndClose(t *testing.T) {
	e := newEnv(t)
	first := e.ok(e.m.QuickJoin(e.player(1), 1))
	for id := uint64(2); id <= 10; id++ {
		s := e.ok(e.m.QuickJoin(e.player(id), 1))
		if s.Summary.LobbyID != first.Summary.LobbyID {
			t.Fatalf("player %d not put into the fullest instance", id)
		}
	}
	// #1 is full: #2 opened.
	if got := instances(e, 1, testSim); !slices.Equal(got, []uint16{1, 2}) {
		t.Fatalf("instances %v", got)
	}
	s11 := e.ok(e.m.QuickJoin(e.player(11), 1))
	if s11.Summary.Instance != 2 {
		t.Fatalf("11th player in instance %d", s11.Summary.Instance)
	}
	// #1 regains room, #2 still has a player: nothing closes.
	e.m.Leave(3)
	if got := instances(e, 1, testSim); !slices.Equal(got, []uint16{1, 2}) {
		t.Fatalf("instances %v", got)
	}
	// #2 empties while #1 has room: #2 closes.
	e.m.Leave(11)
	if got := instances(e, 1, testSim); !slices.Equal(got, []uint16{1}) {
		t.Fatalf("instances %v after #2 emptied", got)
	}
	// Fill #1 again: the new instance reuses number 2.
	e.ok(e.m.QuickJoin(e.player(12), 1))
	if got := instances(e, 1, testSim); !slices.Equal(got, []uint16{1, 2}) {
		t.Fatalf("instances %v", got)
	}
	// The last instance with room stays even when empty; Join by id works and full lobbies refuse.
	wantStatus(t, errOnly(e.m.Join(e.player(13), first.Summary.LobbyID, "")), proto.StatusForbidden, "full")
	_, err := e.m.Join(e.player(13), 999, "")
	wantStatus(t, err, proto.StatusNotFound, "")
}

func errOnly(_ *proto.LobbyState, err error) error { return err }

func TestQuickJoinPicksFullest(t *testing.T) {
	e := newEnv(t)
	for id := uint64(1); id <= 10; id++ {
		e.ok(e.m.QuickJoin(e.player(id), 2))
	}
	e.ok(e.m.QuickJoin(e.player(11), 2)) // #2
	e.m.Leave(1)
	e.m.Leave(2) // #1: 8 players, #2: 1 player
	s := e.ok(e.m.QuickJoin(e.player(20), 2))
	if s.Summary.Instance != 1 || s.Summary.Players != 9 {
		t.Fatalf("joined %+v", s.Summary)
	}
	// QuickJoin while already in an instance of the category returns that instance.
	s = e.ok(e.m.QuickJoin(e.player(11), 2))
	if s.Summary.Instance != 2 {
		t.Fatal("QuickJoin moved a member")
	}
	wantStatus(t, errOnly(e.m.QuickJoin(e.player(21), 11)), proto.StatusBadRequest, "category")
}

func TestJoinErrors(t *testing.T) {
	e := newEnv(t)
	// Not announced.
	_, err := e.m.QuickJoin(Player{AccountID: 50, Username: "x"}, 1)
	wantStatus(t, err, proto.StatusBadRequest, "content not announced")

	// Missing packs are listed.
	e.cat.announce(51, testSim, "game", "mhk2")
	_, err = e.m.QuickJoin(Player{AccountID: 51, Username: "x"}, 1)
	wantStatus(t, err, proto.StatusBadRequest, "Missing content packs: mhk3, mhke, mhkt.")
	if _, err := e.m.QuickJoin(Player{AccountID: 51, Username: "x"}, 9); err != nil {
		t.Fatal("mhk2-only category refused:", err)
	}

	// Another simulation version cannot join by id.
	host := e.ok(e.m.QuickJoin(e.player(1), 1))
	e.cat.announce(52, testSim+1, allPacks...)
	_, err = e.m.Join(Player{AccountID: 52, Username: "x"}, host.Summary.LobbyID, "")
	wantStatus(t, err, proto.StatusBadRequest, "update the game")

	// A differing copy keeps a player out of anticheat lobbies only.
	p := e.player(53)
	e.cat.differs[53] = "mhk3"
	_, err = e.m.QuickJoin(p, 1)
	wantStatus(t, err, proto.StatusForbidden, "Your copy of mhk3 differs")
	e.ok(e.m.QuickJoin(p, 2))

	// A refused join keeps the player where they are (category 4 needs tincan).
	_, err = e.m.QuickJoin(p, 4)
	wantStatus(t, err, proto.StatusBadRequest, "tincan")
	if e.lobbyOf(53) == nil || e.lobbyOf(53).category != 2 {
		t.Fatal("a refused join left the previous lobby")
	}
}

func TestOneLobbyPerAccount(t *testing.T) {
	e := newEnv(t)
	p := e.player(1)
	e.ok(e.m.QuickJoin(p, 1))
	e.ok(e.m.QuickJoin(p, 2))
	if e.lobbyOf(1).category != 2 {
		t.Fatal("not moved")
	}
	// Switching is the player's own request: no LobbyLeft, which the client could mistake for the new lobby.
	if left := e.push.of(1, proto.PushLobbyLeft); len(left) != 0 {
		t.Fatalf("LobbyLeft %v", left)
	}
	if got := instances(e, 1, testSim); len(got) != 1 {
		t.Fatalf("category 1 instances %v", got)
	}
}

// joinTwo puts players 1 and 2 with loadouts into category 1.
func joinTwo(t *testing.T, e *env) *lobby {
	t.Helper()
	for id := uint64(1); id <= 2; id++ {
		e.ok(e.m.QuickJoin(e.player(id), 1))
		must(t, e.m.SetLoadout(Player{AccountID: id}, "game.huhn", "mhk2.speedy"))
	}
	return e.lobbyOf(1)
}

func finish(t *testing.T, e *env, l *lobby, call startCall, placements []race.Placement, want uint8) {
	t.Helper()
	call.cb.Going(call.id)
	e.waitPhase(t, l, proto.PhaseCountdown)
	e.clock.Advance(DefaultTiming.Countdown)
	if e.phase(l) != proto.PhaseRacing {
		t.Fatal("countdown did not end in Racing")
	}
	call.cb.Finished(call.id, placements)
	e.waitPhase(t, l, want)
}

func TestPublicSet(t *testing.T) {
	e := newEnv(t)
	l := joinTwo(t, e)
	if e.phase(l) != proto.PhaseVoting {
		t.Fatal("first member did not open the voting")
	}
	st := e.push.lastState(t, 1)
	if st.Summary.Phase != proto.PhaseVoting || !st.PhaseEnds.Equal(e.clock.Now().Add(20*time.Second)) {
		t.Fatalf("voting state %+v", st)
	}
	var p1Total int32
	for n := uint8(1); n <= 4; n++ {
		must(t, e.m.Vote(Player{AccountID: 1}, "mhk2.rome"))
		if e.races.startCount() != int(n)-1 {
			t.Fatal("race started before everyone voted")
		}
		must(t, e.m.Vote(Player{AccountID: 2}, "mhk2.rome"))
		call := e.races.last()
		if call.spec.TrackID != "mhk2.rome" || call.spec.EngineClass != 2 || !call.spec.Items || !call.spec.Anticheat ||
			call.spec.Solo || call.spec.SetRace != n || call.spec.SetSize != 4 || len(call.racers) != 2 ||
			call.spec.Mode != race.ModeRace || call.spec.Laps != 0 {
			t.Fatalf("race %d spec %+v racers %+v", n, call.spec, call.racers)
		}
		if e.phase(l) != proto.PhaseLoading {
			t.Fatal("not loading")
		}
		// Player 2 wins every race; player 1 is second, except a DNF in race 2.
		p1 := race.Placement{Slot: 1, AccountID: 1, Username: "p1", Position: 2, FinishMs: 61_000}
		if n == 2 {
			p1.Outcome, p1.FinishMs = race.OutcomeDNF, 0
		} else {
			p1Total += 12
		}
		want := proto.PhaseResults
		if n == 4 {
			want = proto.PhaseSetResults
		}
		finish(t, e, l, call, []race.Placement{
			{Slot: 0, AccountID: 2, Username: "p2", Position: 1, FinishMs: 60_000}, p1}, want)

		results := e.push.of(1, proto.PushRaceResults)
		res := results[len(results)-1].(*proto.RaceResults)
		if res.RaceID != call.id || res.SetDone != (n == 4) || res.Rows[0].Points != 15 ||
			res.Rows[0].SetPoints != 15*int32(n) || res.Rows[1].SetPoints != p1Total ||
			res.Rows[1].Outcome != uint8(p1.Outcome) {
			t.Fatalf("race %d results %+v", n, res)
		}
		st := e.push.lastState(t, 2)
		if st.RaceID != 0 || st.SetRace != n || len(st.LastResults) != 2 || st.Members[0].InRace {
			t.Fatalf("state after race %d: %+v", n, st)
		}
		if n < 4 {
			e.clock.Advance(10 * time.Second)
			if e.phase(l) != proto.PhaseVoting {
				t.Fatal("results did not end in Voting")
			}
		}
	}
	// After the set: points stay through SetResults, then reset with the next voting.
	if st := e.push.lastState(t, 1); st.Members[1].SetPoints != 60 || st.Members[0].SetPoints != 36 {
		t.Fatalf("set points %+v", st.Members)
	}
	e.clock.Advance(15 * time.Second)
	st = e.push.lastState(t, 1)
	if st.Summary.Phase != proto.PhaseVoting || st.SetRace != 0 || st.Members[0].SetPoints != 0 ||
		st.Members[1].SetPoints != 0 || len(st.LastResults) != 0 {
		t.Fatalf("after the set: %+v", st)
	}
}

func TestPointsTable(t *testing.T) {
	want := []int32{15, 12, 10, 8, 7, 6, 5, 4, 3, 2, 0}
	for i, w := range want {
		if got := points(race.Placement{Position: uint8(i + 1)}); got != w {
			t.Errorf("position %d: %d points", i+1, got)
		}
	}
	for _, o := range []race.Outcome{race.OutcomeDNF, race.OutcomeLeft, race.OutcomeCheatKick, race.OutcomeModKick,
		race.OutcomeDesync} {
		if points(race.Placement{Position: 1, Outcome: o}) != 0 {
			t.Errorf("outcome %d scores", o)
		}
	}
	if points(race.Placement{Position: 1, AI: true}) != 0 {
		t.Error("AI scores")
	}
}

func TestVotingTimeoutAndRandom(t *testing.T) {
	e := newEnv(t)
	l := joinTwo(t, e)
	must(t, e.m.Vote(Player{AccountID: 1}, ""))
	wantStatus(t, e.m.Vote(Player{AccountID: 2}, "mhk2.arena"), proto.StatusBadRequest, "not a track")
	wantStatus(t, e.m.Vote(Player{AccountID: 2}, "tincan.track"), proto.StatusBadRequest, "not a track")
	e.clock.Advance(19 * time.Second)
	if e.races.startCount() != 0 {
		t.Fatal("voting ended early")
	}
	e.clock.Advance(time.Second)
	// Only a Random vote: a track every racer's catalog lists, from the lobby packs.
	call := e.races.last()
	if !slices.Contains([]string{"mhk2.rome", "mhk2.alps", "mhk3.paris"}, call.spec.TrackID) {
		t.Fatalf("random track %q", call.spec.TrackID)
	}
	if e.phase(l) != proto.PhaseLoading {
		t.Fatal("not loading")
	}
	wantStatus(t, e.m.Vote(Player{AccountID: 1}, ""), proto.StatusBadRequest, "Voting is not open")
}

func TestBattleArenas(t *testing.T) {
	e := newEnv(t)
	for id := uint64(1); id <= 2; id++ {
		e.ok(e.m.QuickJoin(e.player(id), 5))
		must(t, e.m.SetLoadout(Player{AccountID: id}, "game.huhn", "game.kart"))
	}
	wantStatus(t, e.m.Vote(Player{AccountID: 1}, "mhk2.rome"), proto.StatusBadRequest, "not a track")
	must(t, e.m.Vote(Player{AccountID: 1}, ""))
	must(t, e.m.Vote(Player{AccountID: 2}, ""))
	if call := e.races.last(); call.spec.TrackID != "mhk2.arena" || call.spec.Mode != race.ModeBattle {
		t.Fatalf("battle spec %+v", call.spec)
	}
}

func TestNoLoadoutsVotesAgain(t *testing.T) {
	e := newEnv(t)
	e.ok(e.m.QuickJoin(e.player(1), 1))
	l := e.lobbyOf(1)
	must(t, e.m.Vote(Player{AccountID: 1}, "mhk2.rome"))
	if e.races.startCount() != 0 || e.phase(l) != proto.PhaseVoting {
		t.Fatal("a race started without loadouts")
	}
	if st := e.push.lastState(t, 1); len(st.Votes) != 0 {
		t.Fatal("votes kept into the new round")
	}
	// A failing engine also returns to voting.
	must(t, e.m.SetLoadout(Player{AccountID: 1}, "game.huhn", "game.kart"))
	e.races.fail = errors.New("boom")
	must(t, e.m.Vote(Player{AccountID: 1}, "mhk2.rome"))
	if e.phase(l) != proto.PhaseVoting {
		t.Fatal("engine failure not handled")
	}
}

func TestSoloRace(t *testing.T) {
	e := newEnv(t)
	e.ok(e.m.QuickJoin(e.player(1), 1))
	must(t, e.m.SetLoadout(Player{AccountID: 1}, "game.huhn", "game.kart"))
	l := e.lobbyOf(1)
	must(t, e.m.Vote(Player{AccountID: 1}, "mhk2.alps"))
	call := e.races.last()
	if !call.spec.Solo || call.spec.AICount != 6 || len(call.racers) != 1 {
		t.Fatalf("solo spec %+v", call.spec)
	}
	call.cb.Going(call.id)
	e.waitPhase(t, l, proto.PhaseCountdown)

	// Someone joins the solo race: they wait, the racer hears about it.
	s := e.ok(e.m.QuickJoin(e.player(2), 1))
	if s.Members[1].InRace || !s.Members[0].InRace {
		t.Fatalf("members %+v", s.Members)
	}
	waiting := e.push.of(1, proto.PushSoloRaceJoinWaiting)
	if len(waiting) != 1 || waiting[0].(*proto.SoloRaceJoinWaiting).Username != "p2" {
		t.Fatalf("SoloRaceJoinWaiting %v", waiting)
	}
	wantStatus(t, e.m.EndRaceEarly(Player{AccountID: 2}), proto.StatusBadRequest, "alone")
	must(t, e.m.EndRaceEarly(Player{AccountID: 1}))
	if !slices.Equal(e.races.ended, []uint64{call.id}) {
		t.Fatalf("EndEarly calls %v", e.races.ended)
	}
	call.cb.Finished(call.id, []race.Placement{{AccountID: 1, Username: "p1", Position: 3, Outcome: race.OutcomeDNF}})
	e.waitPhase(t, l, proto.PhaseResults)
	e.clock.Advance(10 * time.Second)

	// The next race starts with the humans only.
	must(t, e.m.SetLoadout(Player{AccountID: 2}, "game.huhn", "game.kart"))
	must(t, e.m.Vote(Player{AccountID: 1}, ""))
	must(t, e.m.Vote(Player{AccountID: 2}, ""))
	if call := e.races.last(); call.spec.Solo || call.spec.AICount != 0 || len(call.racers) != 2 {
		t.Fatalf("second race %+v", call.spec)
	}
	wantStatus(t, e.m.EndRaceEarly(Player{AccountID: 1}), proto.StatusBadRequest, "alone")
}

func TestLeaveDuringRace(t *testing.T) {
	e := newEnv(t)
	l := joinTwo(t, e)
	must(t, e.m.Vote(Player{AccountID: 1}, ""))
	must(t, e.m.Vote(Player{AccountID: 2}, ""))
	call := e.races.last()
	e.m.Leave(2)
	if len(e.races.removes) != 1 || e.races.removes[0] != (removeCall{call.id, 2, race.OutcomeLeft, ""}) {
		t.Fatalf("removes %+v", e.races.removes)
	}
	// Late callbacks of a race whose lobby emptied are fine; the lobby idles once it ends.
	e.m.Leave(1)
	call.cb.Finished(call.id, nil)
	e.waitPhase(t, l, proto.PhaseWaiting)
}

func room() proto.RoomSettings {
	return proto.RoomSettings{Name: "  ", Mode: proto.RoomRaceSet, Packs: []string{"mhk2"}, Anticheat: true,
		EngineClass: 1, Items: true, ItemOdds: []proto.ItemOdds{{PowerupID: "mhk2.rocket", Percent: 100}},
		AIWhenAlone: true, MaxPlayers: 4, TrackPick: proto.PickOwner, RacesPerSet: 2}
}

func TestPrivateRoom(t *testing.T) {
	e := newEnv(t)
	owner := e.player(1)
	s := e.ok(e.m.CreateRoom(owner, room()))
	if len(s.Code) != 6 || strings.Trim(s.Code, codeAlphabet) != "" || s.Summary.Kind != proto.LobbyPrivate ||
		s.Settings.Name != "p1's room" || !s.Members[0].Owner || s.Summary.Phase != proto.PhaseWaiting ||
		s.SetSize != 2 {
		t.Fatalf("room %+v", s)
	}
	// Private rooms are not listed; they are joined by code (any case) or id.
	list, _ := e.m.List(owner)
	for _, x := range list {
		if x.LobbyID == s.Summary.LobbyID {
			t.Fatal("private room listed")
		}
	}
	j := e.ok(e.m.Join(e.player(2), 0, strings.ToLower(s.Code)))
	if j.Summary.LobbyID != s.Summary.LobbyID || j.Code != s.Code || j.Members[1].Owner {
		t.Fatalf("joined %+v", j)
	}
	e.ok(e.m.Join(e.player(3), s.Summary.LobbyID, ""))
	_, err := e.m.Join(e.player(4), 0, "ZZZZZZ")
	wantStatus(t, err, proto.StatusNotFound, "")

	// Only the owner starts, changes settings or kicks; ready is informational.
	must(t, e.m.SetReady(Player{AccountID: 2}, true))
	wantStatus(t, e.m.StartRace(Player{AccountID: 2}, "mhk2.rome"), proto.StatusForbidden, "owner")
	upd := room()
	upd.MaxPlayers = 2
	wantStatus(t, e.m.UpdateRoom(owner, upd), proto.StatusBadRequest, "3 players")
	wantStatus(t, e.m.UpdateRoom(Player{AccountID: 2}, room()), proto.StatusForbidden, "owner")
	wantStatus(t, e.m.StartRace(owner, "mhk2.rome"), proto.StatusBadRequest, "character")
	must(t, e.m.SetLoadout(Player{AccountID: 2}, "game.huhn", "mhk2.speedy"))
	wantStatus(t, e.m.StartRace(owner, "mhk3.paris"), proto.StatusBadRequest, "not a track")
	must(t, e.m.StartRace(owner, "mhk2.rome"))
	call := e.races.last()
	if call.spec.TrackID != "mhk2.rome" || call.spec.EngineClass != 1 || len(call.racers) != 1 ||
		call.racers[0].AccountID != 2 || call.spec.SetSize != 2 || len(call.spec.ItemOdds) != 1 || call.spec.Solo {
		t.Fatalf("private race %+v %+v", call.spec, call.racers)
	}
	wantStatus(t, e.m.UpdateRoom(owner, room()), proto.StatusBadRequest, "waits")
	l := e.lobbyOf(1)
	finish(t, e, l, call, []race.Placement{{AccountID: 2, Username: "p2", Position: 1}}, proto.PhaseResults)
	e.clock.Advance(10 * time.Second)
	if e.phase(l) != proto.PhaseWaiting || e.push.lastState(t, 3).Members[1].Ready {
		t.Fatal("private room did not return to Waiting (with ready reset)")
	}

	// Settings change: switching packs clears loadouts that are no longer room content.
	upd = room()
	upd.Name, upd.Packs, upd.ItemOdds, upd.TrackPick = "Tin", []string{"mhk3"}, nil, proto.PickVote
	must(t, e.m.UpdateRoom(owner, upd))
	st := e.push.lastState(t, 2)
	if st.Settings.Name != "Tin" || st.Members[1].KartID != "" || st.Members[1].CharacterID != "game.huhn" {
		t.Fatalf("after update %+v", st)
	}
	must(t, e.m.SetLoadout(Player{AccountID: 2}, "game.huhn", "game.kart"))
	// Vote mode: StartRace opens the voting.
	wantStatus(t, e.m.StartRace(owner, "mhk3.paris"), proto.StatusBadRequest, "votes")
	must(t, e.m.StartRace(owner, ""))
	if e.phase(l) != proto.PhaseVoting {
		t.Fatal("vote room did not open the voting")
	}

	// Owner transfer to the longest-present member; the empty room closes.
	e.m.Leave(1)
	if st := e.push.lastState(t, 2); !st.Members[0].Owner || st.Members[0].AccountID != 2 {
		t.Fatalf("owner not transferred: %+v", st.Members)
	}
	e.m.Leave(2)
	e.m.Leave(3)
	_, err = e.m.Join(e.player(4), 0, s.Code)
	wantStatus(t, err, proto.StatusNotFound, "")
}

func TestRoomValidation(t *testing.T) {
	e := newEnv(t)
	p := e.player(1)
	for name, mutate := range map[string]func(*proto.RoomSettings){
		"mode 3":           func(s *proto.RoomSettings) { s.Mode = 3 },
		"engine class 3":   func(s *proto.RoomSettings) { s.EngineClass = 3 },
		"100 laps":         func(s *proto.RoomSettings) { s.Laps = 100 },
		"1 player":         func(s *proto.RoomSettings) { s.MaxPlayers = 1 },
		"11 players":       func(s *proto.RoomSettings) { s.MaxPlayers = 11 },
		"pick 2":           func(s *proto.RoomSettings) { s.TrackPick = 2 },
		"0 races":          func(s *proto.RoomSettings) { s.RacesPerSet = 0 },
		"17 races":         func(s *proto.RoomSettings) { s.RacesPerSet = 17 },
		"duplicate pack":   func(s *proto.RoomSettings) { s.Packs = []string{"mhk2", "MHK2"} },
		"missing pack":     func(s *proto.RoomSettings) { s.Packs = []string{"tincan"} },
		"foreign powerup":  func(s *proto.RoomSettings) { s.ItemOdds = []proto.ItemOdds{{PowerupID: "x.y"}} },
		"championship cup": func(s *proto.RoomSettings) { s.Mode = proto.RoomChampionship; s.CupID = "mhk3.cup" },
		"control name":     func(s *proto.RoomSettings) { s.Name = "a\x01" },
	} {
		s := room()
		mutate(&s)
		if _, err := e.m.CreateRoom(p, s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A differing copy of a room pack forbids anticheat rooms.
	e.cat.differs[1] = "mhk2"
	_, err := e.m.CreateRoom(p, room())
	wantStatus(t, err, proto.StatusForbidden, "differs")
	s := room()
	s.Anticheat = false
	s.Mode, s.CupID = proto.RoomChampionship, "mhk2.cup"
	st := e.ok(e.m.CreateRoom(p, s))
	if st.Settings.CupID != "mhk2.cup" {
		t.Fatal("cup lost")
	}
	// Championships need the next track from the owner.
	must(t, e.m.SetLoadout(p, "game.huhn", "game.kart"))
	wantStatus(t, e.m.StartRace(p, ""), proto.StatusBadRequest, "cup")
	// Alone with aiWhenAlone: AI fills the room.
	must(t, e.m.StartRace(p, "mhk2.alps"))
	if call := e.races.last(); !call.spec.Solo || call.spec.AICount != 3 {
		t.Fatalf("private solo %+v", call.spec)
	}
}

func TestPrivateSoloWithoutAI(t *testing.T) {
	e := newEnv(t)
	s := room()
	s.AIWhenAlone = false
	p := e.player(1)
	e.ok(e.m.CreateRoom(p, s))
	must(t, e.m.SetLoadout(p, "game.huhn", "game.kart"))
	must(t, e.m.StartRace(p, ""))
	if call := e.races.last(); !call.spec.Solo || call.spec.AICount != 0 || call.spec.TrackID != "mhk2.rome" &&
		call.spec.TrackID != "mhk2.alps" {
		t.Fatalf("spec %+v", call.spec)
	}
}

func TestOwnerKick(t *testing.T) {
	e := newEnv(t)
	owner := e.player(1)
	s := e.ok(e.m.CreateRoom(owner, room()))
	e.ok(e.m.Join(e.player(2), 0, s.Code))
	e.ok(e.m.Join(e.player(3), 0, s.Code))
	ctx := context.Background()
	wantStatus(t, e.m.Kick(ctx, Player{AccountID: 2}, 3, "x"), proto.StatusForbidden, "owner")
	wantStatus(t, e.m.Kick(ctx, owner, 1, "x"), proto.StatusBadRequest, "yourself")
	wantStatus(t, e.m.Kick(ctx, owner, 99, "x"), proto.StatusNotFound, "")
	must(t, e.m.Kick(ctx, owner, 3, "rude"))
	left := e.push.of(3, proto.PushLobbyLeft)
	if len(left) != 1 || *left[0].(*proto.LobbyLeft) != (proto.LobbyLeft{Reason: proto.LeftOwnerKick, Message: "rude"}) {
		t.Fatalf("LobbyLeft %+v", left)
	}
	if len(e.audit.actions) != 0 {
		t.Fatal("owner kick audited")
	}
	// Banned from that room for 10 minutes.
	_, err := e.m.Join(e.player(3), 0, s.Code)
	wantStatus(t, err, proto.StatusForbidden, "kicked")
	e.clock.Advance(10*time.Minute - time.Second)
	_, err = e.m.Join(e.player(3), 0, s.Code)
	wantStatus(t, err, proto.StatusForbidden, "kicked")
	e.clock.Advance(time.Second)
	e.ok(e.m.Join(e.player(3), 0, s.Code))

	// Owners cannot kick in public lobbies or in other rooms.
	e.ok(e.m.QuickJoin(e.player(4), 1))
	wantStatus(t, e.m.Kick(ctx, owner, 4, "x"), proto.StatusForbidden, "owner")
}

func TestModeratorKick(t *testing.T) {
	e := newEnv(t)
	l := joinTwo(t, e)
	must(t, e.m.Vote(Player{AccountID: 1}, ""))
	must(t, e.m.Vote(Player{AccountID: 2}, ""))
	call := e.races.last()
	mod := e.player(9)
	mod.Moderator = true
	// The moderator is in no lobby at all and still kicks out of the running race.
	must(t, e.m.Kick(context.Background(), mod, 2, "teleporting"))
	if len(e.races.removes) != 1 || e.races.removes[0] != (removeCall{call.id, 2, race.OutcomeModKick, "teleporting"}) {
		t.Fatalf("removes %+v", e.races.removes)
	}
	left := e.push.of(2, proto.PushLobbyLeft)
	if len(left) != 1 || left[0].(*proto.LobbyLeft).Reason != proto.LeftModKick {
		t.Fatalf("LobbyLeft %+v", left)
	}
	if len(e.audit.actions) != 1 {
		t.Fatal("no audit row")
	}
	a := e.audit.actions[0]
	if a.Action != store.ActionLobbyKick || *a.Actor != 9 || *a.TargetAccount != 2 || a.Reason != "teleporting" ||
		!strings.Contains(a.TargetRef, "p2") {
		t.Fatalf("audit %+v", a)
	}
	_, err := e.m.Join(Player{AccountID: 2, Username: "p2"}, l.id, "")
	wantStatus(t, err, proto.StatusForbidden, "kicked")
	// An audit failure means no kick.
	e.audit.fail = errors.New("db down")
	if err := e.m.Kick(context.Background(), mod, 1, "x"); err == nil || e.lobbyOf(1) == nil {
		t.Fatal("kick without audit row")
	}
}

func TestLoadoutValidation(t *testing.T) {
	e := newEnv(t)
	p := e.player(1)
	wantStatus(t, e.m.SetLoadout(p, "game.huhn", "game.kart"), proto.StatusBadRequest, "not in a lobby")
	e.ok(e.m.QuickJoin(p, 9)) // mhk2 only
	must(t, e.m.SetLoadout(p, "game.huhn", "mhk2.speedy"))
	wantStatus(t, e.m.SetLoadout(p, "game.huhn", "tincan.can"), proto.StatusBadRequest, "kart")
	wantStatus(t, e.m.SetLoadout(p, "mhk2.speedy", ""), proto.StatusBadRequest, "character")
	wantStatus(t, e.m.SetLoadout(p, "", "mhk2.nothing"), proto.StatusBadRequest, "kart")
	wantStatus(t, e.m.SetLoadout(p, "nodot", ""), proto.StatusBadRequest, "character")
	must(t, e.m.SetLoadout(p, "", ""))
	wantStatus(t, e.m.SetReady(p, true), proto.StatusBadRequest, "private")
}

func TestPingsArePushedWhenTheyChange(t *testing.T) {
	e := newEnv(t)
	e.ok(e.m.QuickJoin(e.player(1), 1))
	before := len(e.push.of(1, proto.PushLobbyState))
	e.clock.Advance(5 * time.Second)
	if len(e.push.of(1, proto.PushLobbyState)) != before {
		t.Fatal("pushed without a change")
	}
	e.races.mu.Lock()
	e.races.rtt[1] = 42
	e.races.mu.Unlock()
	e.clock.Advance(5 * time.Second)
	if st := e.push.lastState(t, 1); st.Members[0].PingMs != 42 {
		t.Fatalf("ping %d", st.Members[0].PingMs)
	}
}
