package lobby

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/content"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/testclient"
)

// Integration tests run the real server, content catalog and lobby manager over TCP against MySQL
// (MHK_TEST_DSN, see internal/server tests); races are faked so the test decides when they go and finish.

const itPassword = "kart-racer-2026"

type itServer struct {
	addr  string
	st    *store.Store
	races *fakeRaces
	sim   int32
}

// startServer runs the server with faked races, or with the real race engine and UDP when realRaces is set.
func startServer(t *testing.T, realRaces bool) *itServer {
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
	limits := auth.Limits{Register: auth.Rate{N: 1000, Per: time.Hour}, LoginFailIP: auth.Rate{N: 1000, Per: time.Hour},
		LoginFailAccount: auth.Rate{N: 1000, Per: time.Hour}}
	svc := auth.NewService(st, st, auth.NewHasher(auth.DefaultParams, auth.DefaultHashConcurrency), limits)
	ghosts, err := blobs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.Config{Version: "test", RevalidateInterval: 100 * time.Millisecond,
		ConnLimit: auth.Rate{N: 1000, Per: time.Minute}}
	var udp *net.UDPConn
	if realRaces {
		if udp, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
			t.Fatal(err)
		}
		cfg.UDPPort = uint16(udp.LocalAddr().(*net.UDPAddr).Port)
	}
	srv := server.New(cfg, server.Deps{Log: log, Auth: svc, Accounts: st, Sessions: st, Moderation: st, Runs: st,
		Blobs: ghosts})
	catalog := content.New(st, log)
	catalog.Register(srv)
	its := &itServer{st: st}
	var races race.Races
	var engine *race.Engine
	if udp != nil {
		engine = race.NewEngine(race.Config{Log: log, Hub: srv.Hub(), Kicks: st, UDP: udp, Catalog: catalog})
		engine.Register(srv)
		races = engine
	} else {
		its.races = &fakeRaces{rtt: map[uint64]uint16{}}
		races = its.races
	}
	m := New(Config{Races: races, Catalog: catalog, Pusher: srv.Hub(), Audit: st, Log: log})
	m.Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
		m.Close()
		if engine != nil {
			engine.Close()
		}
	})
	// A random simulation version keeps this run's catalog votes apart from earlier runs on the same database.
	var b [3]byte
	rand.Read(b[:])
	its.addr = ln.Addr().String()
	its.sim = 1_000_000 + int32(b[0])<<16 | int32(b[1])<<8 | int32(b[2])
	return its
}

func itCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type itPlayer struct {
	*testclient.Client
	id   uint64
	name string
}

// connect registers a fresh account; moderator accounts get the role first and log in afterwards.
func (s *itServer) connect(t *testing.T, moderator bool) *itPlayer {
	t.Helper()
	ctx := itCtx(t)
	dial := func() *testclient.Client {
		c, err := testclient.Dial(ctx, s.addr, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		h := testclient.DefaultHello()
		h.SimVersion, h.Platform = s.sim, "OSXEditor" // LobbyMember/RaceSlot.platform 2
		if _, err := c.Hello(ctx, h); err != nil {
			t.Fatal(err)
		}
		return c
	}
	var nb [4]byte
	rand.Read(nb[:])
	name := "lb" + hex.EncodeToString(nb[:])
	c := dial()
	sess, err := c.Register(ctx, name, itPassword)
	if err != nil {
		t.Fatal(err)
	}
	if moderator {
		if err := s.st.SetRole(ctx, sess.Account.ID, proto.RoleModerator,
			store.Action{Action: store.ActionSetRole, CreatedAt: store.Now()}); err != nil {
			t.Fatal(err)
		}
		c.Close()
		c = dial()
		if _, err := c.Login(ctx, name, itPassword); err != nil {
			t.Fatal(err)
		}
	}
	return &itPlayer{Client: c, id: sess.Account.ID, name: name}
}

func manifest(id, commit string, sim int32, items ...proto.ManifestItem) proto.PackManifest {
	m := proto.PackManifest{PackID: id, Commit: commit, SimVersion: sim, Items: items}
	m.ManifestHash, _ = content.ManifestHash(items)
	return m
}

func item(kind uint8, id string) proto.ManifestItem {
	return proto.ManifestItem{Kind: kind, ID: id, Fingerprint: "0123456789abcdef"}
}

// packs are the built-in pack and the classic packs of category 1, at a commit unique to the test run.
func (s *itServer) packs(commit string) []proto.PackManifest {
	return []proto.PackManifest{
		manifest("game", commit, s.sim, item(proto.ContentCharacter, "game.huhn"), item(proto.ContentKart, "game.kart")),
		manifest("mhk2", commit, s.sim, item(proto.ContentTrack, "mhk2.rome"), item(proto.ContentTrack, "mhk2.alps"),
			item(proto.ContentKart, "mhk2.speedy")),
		manifest("mhk3", commit, s.sim, item(proto.ContentTrack, "mhk3.paris")),
		manifest("mhke", commit, s.sim),
		manifest("mhkt", commit, s.sim, item(proto.ContentArena, "mhkt.arena")),
	}
}

// announce runs ContentHello and sends what the server asks for.
func (p *itPlayer) announce(t *testing.T, sim int32, ms []proto.PackManifest) *proto.ContentHelloResult {
	t.Helper()
	ctx := itCtx(t)
	refs := make([]proto.PackRef, len(ms))
	for i, m := range ms {
		refs[i] = proto.PackRef{PackID: m.PackID, Commit: m.Commit, ManifestHash: m.ManifestHash}
	}
	res, err := p.ContentHello(ctx, sim, refs)
	if err != nil {
		t.Fatal(err)
	}
	var send []proto.PackManifest
	for _, m := range ms {
		for _, h := range res.Missing {
			if h == m.ManifestHash {
				send = append(send, m)
			}
		}
	}
	if len(send) > 0 {
		if err := p.ContentManifests(ctx, send); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

func itStatus(t *testing.T, err error, want proto.Status, contains string) {
	t.Helper()
	if got := testclient.StatusOf(err); got != want {
		t.Fatalf("status %s (%v), want %s", got, err, want)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("%v does not mention %q", err, contains)
	}
}

func (p *itPlayer) waitState(t *testing.T, ok func(*proto.LobbyState) bool) *proto.LobbyState {
	t.Helper()
	st, err := p.WaitLobbyState(itCtx(t), ok)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (s *itServer) waitStart(t *testing.T, n int) startCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.races.startCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("race %d not started", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.races.last()
}

func TestPublicLobbyOverTCP(t *testing.T) {
	s := startServer(t, false)
	ctx := itCtx(t)
	commit := "c" + hex.EncodeToString([]byte{byte(s.sim), byte(s.sim >> 8)})
	packs := s.packs(commit)

	a, b, mod := s.connect(t, false), s.connect(t, false), s.connect(t, true)

	// Lobby requests need the content announced first.
	_, err := a.ListLobbies(ctx)
	itStatus(t, err, proto.StatusBadRequest, "content not announced")
	res := a.announce(t, s.sim, packs)
	if len(res.Missing) != len(packs) || len(res.Strikes) != 0 {
		t.Fatalf("first ContentHello %+v", res)
	}
	if res := b.announce(t, s.sim, packs); len(res.Missing) != 0 {
		t.Fatalf("known manifests requested again: %+v", res)
	}
	mod.announce(t, s.sim, packs)
	// ContentHello must carry the Hello's simulation version.
	_, err = a.ContentHello(ctx, s.sim+1, nil)
	itStatus(t, err, proto.StatusBadRequest, "simVersion")
	a.announce(t, s.sim, packs)

	list, err := a.ListLobbies(ctx)
	if err != nil || len(list) != maxCategory {
		t.Fatalf("ListLobbies %d %v", len(list), err)
	}
	// Alone: the lobby waits; a solo AI race only on request, no points, back to Waiting.
	stA, err := a.QuickJoin(ctx, 1)
	if err != nil || stA.Summary.Phase != proto.PhaseWaiting || len(stA.Members) != 1 {
		t.Fatalf("QuickJoin %+v %v", stA, err)
	}
	if err := a.SetLoadout(ctx, "game.huhn", "mhk2.speedy"); err != nil {
		t.Fatal(err)
	}
	if err := a.StartRace(ctx, "mhk2.rome"); err != nil {
		t.Fatal(err)
	}
	solo := s.waitStart(t, 1)
	if !solo.spec.Solo || solo.spec.AICount != 6 || solo.spec.TrackID != "mhk2.rome" {
		t.Fatalf("solo race %+v", solo.spec)
	}
	solo.cb.Finished(solo.id, []race.Placement{{AccountID: a.id, Username: a.name, Position: 2, FinishMs: 80_000}})
	var soloRes proto.RaceResults
	if err := a.WaitPushMsg(ctx, proto.PushRaceResults, &soloRes); err != nil || soloRes.Rows[0].Points != 0 {
		t.Fatalf("solo results %+v %v", soloRes, err)
	}
	a.waitState(t, func(st *proto.LobbyState) bool { return st.Summary.Phase == proto.PhaseWaiting && st.RaceID == 0 })

	// The second human starts the voting.
	stB, err := b.QuickJoin(ctx, 1)
	if err != nil || stB.Summary.LobbyID != stA.Summary.LobbyID || len(stB.Members) != 2 ||
		stB.Summary.Phase != proto.PhaseVoting {
		t.Fatalf("second QuickJoin %+v %v", stB, err)
	}
	a.waitState(t, func(st *proto.LobbyState) bool { return len(st.Members) == 2 })
	itStatus(t, a.StartRace(ctx, ""), proto.StatusForbidden, "voting")

	// Missing packs are refused with a list.
	c := s.connect(t, false)
	c.announce(t, s.sim, packs[:2])
	_, err = c.QuickJoin(ctx, 1)
	itStatus(t, err, proto.StatusBadRequest, "mhk3, mhke, mhkt")

	err = a.SetLoadout(ctx, "game.huhn", "mhk3.paris")
	itStatus(t, err, proto.StatusBadRequest, "not a kart")
	for _, p := range []*itPlayer{a, b} {
		if err := p.SetLoadout(ctx, "game.huhn", "mhk2.speedy"); err != nil {
			t.Fatal(err)
		}
	}
	itStatus(t, a.Vote(ctx, "mhkt.arena"), proto.StatusBadRequest, "not a track")
	if err := a.Vote(ctx, "mhk2.alps"); err != nil {
		t.Fatal(err)
	}
	b.waitState(t, func(st *proto.LobbyState) bool { return len(st.Votes) == 1 && st.Votes[0].TrackID == "mhk2.alps" })
	if err := b.Vote(ctx, "mhk2.alps"); err != nil {
		t.Fatal(err)
	}
	call := s.waitStart(t, 2)
	if call.spec.TrackID != "mhk2.alps" || len(call.racers) != 2 || call.spec.LobbyID != stA.Summary.LobbyID {
		t.Fatalf("race %+v", call.spec)
	}
	a.waitState(t, func(st *proto.LobbyState) bool {
		return st.Summary.Phase == proto.PhaseLoading && st.RaceID == call.id && st.TrackID == "mhk2.alps" &&
			st.Members[0].InRace && st.Members[1].InRace
	})
	call.cb.Going(call.id)
	a.waitState(t, func(st *proto.LobbyState) bool { return st.Summary.Phase == proto.PhaseCountdown })
	call.cb.Finished(call.id, []race.Placement{
		{Slot: 0, AccountID: b.id, Username: b.name, Position: 1, FinishMs: 90_000},
		{Slot: 1, AccountID: a.id, Username: a.name, Position: 2, FinishMs: 91_000},
	})
	var results proto.RaceResults
	if err := a.WaitPushMsg(ctx, proto.PushRaceResults, &results); err != nil {
		t.Fatal(err)
	}
	if results.RaceID != call.id || results.SetDone || len(results.Rows) != 2 || results.Rows[0].Points != 15 ||
		results.Rows[1].Points != 12 || results.Rows[1].SetPoints != 12 {
		t.Fatalf("RaceResults %+v", results)
	}
	st := a.waitState(t, func(st *proto.LobbyState) bool { return st.Summary.Phase == proto.PhaseResults })
	if st.SetRace != 1 || st.SetSize != 4 || len(st.LastResults) != 2 || st.PhaseEnds.IsZero() {
		t.Fatalf("results state %+v", st)
	}

	// A moderator kicks b out of the lobby: b hears why, a sees one member, the audit log has the row.
	if err := mod.KickMember(ctx, b.id, "testing kicks"); err != nil {
		t.Fatal(err)
	}
	var left proto.LobbyLeft
	if err := b.WaitPushMsg(ctx, proto.PushLobbyLeft, &left); err != nil || left.Reason != proto.LeftModKick ||
		left.Message != "testing kicks" {
		t.Fatalf("LobbyLeft %+v %v", left, err)
	}
	a.waitState(t, func(st *proto.LobbyState) bool { return len(st.Members) == 1 })
	_, err = b.JoinLobby(ctx, stA.Summary.LobbyID, "")
	itStatus(t, err, proto.StatusForbidden, "kicked")
	log, err := s.st.AuditLog(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, act := range log {
		if act.Action == store.ActionLobbyKick && act.Actor != nil && *act.Actor == mod.id &&
			act.TargetAccount != nil && *act.TargetAccount == b.id && act.Reason == "testing kicks" {
			found = true
		}
	}
	if !found {
		t.Fatal("no audit row for the moderator kick")
	}
	// Players cannot kick in public lobbies.
	itStatus(t, b.KickMember(ctx, a.id, "x"), proto.StatusForbidden, "owner")

	// Closing a's only connection takes it out of the lobby.
	a.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := mod.ListLobbies(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if list[0].Players == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed connection still in the lobby")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPrivateRoomOverTCP(t *testing.T) {
	s := startServer(t, false)
	ctx := itCtx(t)
	packs := s.packs("room")
	owner, guest := s.connect(t, false), s.connect(t, false)
	owner.announce(t, s.sim, packs)
	guest.announce(t, s.sim, packs)

	settings := proto.RoomSettings{Name: "Hühnerstall", Mode: proto.RoomRaceSet, Packs: []string{"mhk2", "mhk3"},
		Anticheat: true, EngineClass: 1, Laps: 2, Items: true, MaxPlayers: 4, TrackPick: proto.PickOwner,
		RacesPerSet: 3}
	st, err := owner.CreateRoom(ctx, &settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Code) != 6 || st.Summary.Kind != proto.LobbyPrivate || !st.Members[0].Owner || st.SetSize != 3 {
		t.Fatalf("room %+v", st)
	}
	joined, err := guest.JoinLobby(ctx, 0, st.Code)
	if err != nil || joined.Summary.LobbyID != st.Summary.LobbyID || len(joined.Members) != 2 {
		t.Fatalf("join by code %+v %v", joined, err)
	}
	owner.waitState(t, func(x *proto.LobbyState) bool { return len(x.Members) == 2 })

	settings.Name, settings.Laps = "Neu", 4
	itStatus(t, guest.UpdateRoom(ctx, &settings), proto.StatusForbidden, "owner")
	if err := owner.UpdateRoom(ctx, &settings); err != nil {
		t.Fatal(err)
	}
	guest.waitState(t, func(x *proto.LobbyState) bool { return x.Settings.Name == "Neu" && x.Settings.Laps == 4 })
	if err := guest.SetReady(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := guest.SetLoadout(ctx, "game.huhn", "game.kart"); err != nil {
		t.Fatal(err)
	}
	itStatus(t, guest.StartRace(ctx, "mhk2.rome"), proto.StatusForbidden, "owner")
	if err := owner.StartRace(ctx, "mhk3.paris"); err != nil {
		t.Fatal(err)
	}
	call := s.waitStart(t, 1)
	if call.spec.TrackID != "mhk3.paris" || call.spec.Laps != 4 || call.spec.EngineClass != 1 || len(call.racers) != 1 ||
		call.racers[0].AccountID != guest.id || call.spec.Solo {
		t.Fatalf("private race %+v %+v", call.spec, call.racers)
	}
	call.cb.Finished(call.id, []race.Placement{{AccountID: guest.id, Username: guest.name, Position: 1, FinishMs: 5}})
	guest.waitState(t, func(x *proto.LobbyState) bool { return x.Summary.Phase == proto.PhaseResults })

	// The owner leaves: the guest owns the room; when the guest leaves too, the code is gone.
	if err := owner.LeaveLobby(ctx); err != nil {
		t.Fatal(err)
	}
	guest.waitState(t, func(x *proto.LobbyState) bool {
		return len(x.Members) == 1 && x.Members[0].AccountID == guest.id && x.Members[0].Owner
	})
	if err := guest.LeaveLobby(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = owner.JoinLobby(ctx, 0, st.Code)
	itStatus(t, err, proto.StatusNotFound, "")
}
