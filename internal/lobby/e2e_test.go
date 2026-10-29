package lobby

import (
	"fmt"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/testclient"
)

// TestLobbyRaceResultsEndToEnd runs a public lobby race through the real race engine: voting starts the race,
// the racers load and finish over TCP while their UDP bots ping and send states, and the lobby turns the
// engine's placements into points.
func TestLobbyRaceResultsEndToEnd(t *testing.T) {
	s := startServer(t, true)
	ctx := itCtx(t)
	packs := s.packs("e2e")
	a, b := s.connect(t, false), s.connect(t, false)
	for _, p := range []*itPlayer{a, b} {
		p.announce(t, s.contentVersion, packs)
		if _, err := p.QuickJoin(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if err := p.SetLoadout(ctx, "game.huhn", "mhk2.speedy"); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []*itPlayer{a, b} {
		if err := p.Vote(ctx, "mhk2.rome"); err != nil {
			t.Fatal(err)
		}
	}

	setups := map[*itPlayer]*proto.RaceSetup{}
	for _, p := range []*itPlayer{a, b} {
		var setup proto.RaceSetup
		if err := p.WaitPushMsg(ctx, proto.PushRaceSetup, &setup); err != nil {
			t.Fatal(err)
		}
		if setup.TrackID != "mhk2.rome" || len(setup.Grid) != 2 || setup.AIHost != proto.NoSlot || setup.SetRace != 1 ||
			setup.SetSize != 4 || setup.EngineClass != 2 || !setup.Anticheat || setup.UDPPort == 0 ||
			setup.Grid[0].Platform != proto.PlatformMacOS || setup.Grid[1].Platform != proto.PlatformMacOS {
			t.Fatalf("RaceSetup %+v", setup)
		}
		setups[p] = &setup
		u, err := testclient.DialUDP(fmt.Sprintf("127.0.0.1:%d", setup.UDPPort), setup.UDPToken, setup.UDPKey)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(u.Close)
		u.Run(ctx, true)
	}
	raceID := setups[a].RaceID
	for _, p := range []*itPlayer{a, b} {
		loaded := &proto.RaceLoaded{RaceID: raceID, TrackHash: "00112233445566778899aabbccddeeff", Laps: 3,
			Rules: proto.ItemRules{RouletteMs: 0}}
		if err := p.Notify(proto.NotifyRaceLoaded, loaded); err != nil {
			t.Fatal(err)
		}
	}
	var goMsg proto.RaceGo
	for _, p := range []*itPlayer{a, b} {
		if err := p.WaitPushMsg(ctx, proto.PushRaceGo, &goMsg); err != nil || goMsg.RaceID != raceID {
			t.Fatalf("RaceGo %+v %v", goMsg, err)
		}
	}
	time.Sleep(time.Until(goMsg.Go) + 100*time.Millisecond)
	finish := func(p *itPlayer) {
		ms := int32(time.Since(goMsg.Go).Milliseconds())
		lap := ms / 3
		if err := p.Notify(proto.NotifyFinished, &proto.Finished{RaceID: raceID, FinishMs: ms,
			LapMs: []int32{lap, lap, ms - 2*lap}, Slot: proto.OwnSlot}); err != nil {
			t.Fatal(err)
		}
	}
	finish(b)
	time.Sleep(50 * time.Millisecond)
	finish(a)

	var res proto.RaceResults
	if err := a.WaitPushMsg(ctx, proto.PushRaceResults, &res); err != nil {
		t.Fatal(err)
	}
	if res.RaceID != raceID || res.SetDone || len(res.Rows) != 2 {
		t.Fatalf("RaceResults %+v", res)
	}
	first, second := res.Rows[0], res.Rows[1]
	if first.AccountID != b.id || first.Position != 1 || first.Points != 15 || first.SetPoints != 15 ||
		second.AccountID != a.id || second.Position != 2 || second.Points != 12 || first.FinishMs <= 0 {
		t.Fatalf("rows %+v", res.Rows)
	}
	st := a.waitState(t, func(st *proto.LobbyState) bool { return st.Summary.Phase == proto.PhaseResults })
	if st.RaceID != 0 || st.SetRace != 1 || len(st.LastResults) != 2 {
		t.Fatalf("lobby after the race %+v", st)
	}
	for _, m := range st.Members {
		if m.InRace || (m.AccountID == b.id && m.SetPoints != 15) || (m.AccountID == a.id && m.SetPoints != 12) ||
			m.Platform != proto.PlatformMacOS {
			t.Fatalf("members %+v", st.Members)
		}
	}
}

// TestRoomAIRaceEndToEnd: a private room for 10 with up to 8 AI drivers and two humans races 8 AI karts in front of
// the humans through the real engine. The first human hosts the AI: its AI finishes arrive as PlayerFinished, and
// the results give the AI rows points and set points, kept in LobbyState.aiDrivers.
func TestRoomAIRaceEndToEnd(t *testing.T) {
	s := startServer(t, true)
	ctx := itCtx(t)
	packs := s.packs("e2eai")
	a, b := s.connect(t, false), s.connect(t, false)
	for _, p := range []*itPlayer{a, b} {
		p.announce(t, s.contentVersion, packs)
	}
	room, err := a.CreateRoom(ctx, &proto.RoomSettings{Mode: proto.RoomRaceSet, Packs: []string{"mhk2"},
		Anticheat: true, EngineClass: 2, Items: true, MaxAI: 8, MaxPlayers: 10, TrackPick: proto.PickOwner,
		RacesPerSet: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.JoinLobby(ctx, 0, room.Code); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*itPlayer{a, b} {
		if err := p.SetLoadout(ctx, "game.huhn", "mhk2.speedy"); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.StartRace(ctx, "mhk2.rome"); err != nil {
		t.Fatal(err)
	}
	setups := map[*itPlayer]*proto.RaceSetup{}
	for _, p := range []*itPlayer{a, b} {
		var setup proto.RaceSetup
		if err := p.WaitPushMsg(ctx, proto.PushRaceSetup, &setup); err != nil {
			t.Fatal(err)
		}
		if len(setup.Grid) != 10 || setup.AIHost != 8 || setup.YourSlot < 8 || setup.SetRace != 1 {
			t.Fatalf("RaceSetup %+v", setup)
		}
		for i, g := range setup.Grid {
			if g.AI != (i < 8) || g.AI && (g.AccountID != 0 || g.Username != "" || g.CharacterID != "game.huhn" ||
				g.KartID != "game.kart" && g.KartID != "mhk2.speedy") {
				t.Fatalf("grid slot %d %+v", i, g)
			}
		}
		setups[p] = &setup
		u, err := testclient.DialUDP(fmt.Sprintf("127.0.0.1:%d", setup.UDPPort), setup.UDPToken, setup.UDPKey)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(u.Close)
		u.Run(ctx, true)
	}
	host := a
	if setups[b].YourSlot == 8 {
		host = b
	}
	raceID := setups[a].RaceID
	if st := a.waitState(t, func(st *proto.LobbyState) bool { return st.RaceID == raceID }); len(st.AIDrivers) != 8 {
		t.Fatalf("aiDrivers %+v", st.AIDrivers)
	}
	for _, p := range []*itPlayer{a, b} {
		if err := p.Notify(proto.NotifyRaceLoaded, &proto.RaceLoaded{RaceID: raceID,
			TrackHash: "00112233445566778899aabbccddeeff", Laps: 3}); err != nil {
			t.Fatal(err)
		}
	}
	var goMsg proto.RaceGo
	for _, p := range []*itPlayer{a, b} {
		if err := p.WaitPushMsg(ctx, proto.PushRaceGo, &goMsg); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(time.Until(goMsg.Go) + 100*time.Millisecond)
	finish := func(p *itPlayer, slot uint8) {
		ms := int32(time.Since(goMsg.Go).Milliseconds())
		lap := ms / 3
		if err := p.Notify(proto.NotifyFinished, &proto.Finished{RaceID: raceID, FinishMs: ms,
			LapMs: []int32{lap, lap, ms - 2*lap}, Slot: slot}); err != nil {
			t.Fatal(err)
		}
	}
	// AI slot 7 first, then 6 … 0 (reported by the host), then both humans.
	for slot := 7; slot >= 0; slot-- {
		finish(host, uint8(slot))
		var pf proto.PlayerFinished
		if err := b.WaitPushMsg(ctx, proto.PushPlayerFinished, &pf); err != nil || pf.Slot != uint8(slot) {
			t.Fatalf("PlayerFinished %+v %v", pf, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	finish(a, proto.OwnSlot)
	time.Sleep(5 * time.Millisecond)
	finish(b, proto.OwnSlot)

	var res proto.RaceResults
	if err := a.WaitPushMsg(ctx, proto.PushRaceResults, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 10 || res.SetDone {
		t.Fatalf("RaceResults %+v", res)
	}
	for i, r := range res.Rows {
		if want := uint8(7 - i); i < 8 && (!r.AI || r.Slot != want || r.Points != positionPoints[i] ||
			r.SetPoints != r.Points || r.CharacterID != "game.huhn" || r.AccountID != 0) {
			t.Fatalf("row %d %+v", i, r)
		}
	}
	if r := res.Rows[8]; r.AI || r.AccountID != a.id || r.Points != 3 || res.Rows[9].AccountID != b.id ||
		res.Rows[9].Points != 2 {
		t.Fatalf("human rows %+v", res.Rows[8:])
	}
	st := a.waitState(t, func(st *proto.LobbyState) bool { return st.Summary.Phase == proto.PhaseResults })
	if len(st.AIDrivers) != 8 || st.AIDrivers[7].SetPoints != 15 || st.AIDrivers[0].SetPoints != 4 ||
		len(st.LastResults) != 10 {
		t.Fatalf("lobby after the race %+v", st)
	}
}
