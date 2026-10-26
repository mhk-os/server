package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/content"
	"github.com/mhk-os/server/internal/ghost"
	"github.com/mhk-os/server/internal/ghost/ghosttest"
	"github.com/mhk-os/server/internal/lobby"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/testclient"
)

// shortGhost is a valid 3-lap ghost of 3.5 s (0.5 s run-up, 1 s laps) on the ghosttest circle.
func shortGhost(t *testing.T) []byte {
	t.Helper()
	g := ghosttest.New()
	g.Events = nil
	g.SetTimes(0.5, 1, 1, 1)
	for i := range g.Channels[16] { // ghosttest fills flags and keys with noise: controls on, throttle
		g.Channels[16][i], g.Channels[17][i] = ghost.FrameControlsEnabled, 4
	}
	return g.Encode()
}

func TestReplay(t *testing.T) {
	g, frames, err := ghost.ParseFrames(shortGhost(t))
	if err != nil {
		t.Fatal(err)
	}
	r := newReplay(g, frames)
	if r.finish() != 3500*time.Millisecond {
		t.Fatalf("finish %v", r.finish())
	}
	before := r.state(-time.Second)
	if before.Tick != 0 || before.Pos != proto.Vec3(frames[0].Pos) || before.Flags&proto.KartControls != 0 ||
		before.Lap != 1 || before.RaceDistance != 0 {
		t.Fatalf("before GO %+v", before)
	}
	// 1.25 s: frame 75, in lap 1 (the run-up ends at 0.5 s, lap 1 at 1.5 s).
	st := r.state(1250 * time.Millisecond)
	if st.Tick != 75 || st.Pos != proto.Vec3(frames[75].Pos) || st.Lap != 1 {
		t.Fatalf("1.25 s %+v", st)
	}
	// The ghosttest circle: radius 200 m, 30 s per lap, so 1.25 s drive 2π·200·1.25/30 m.
	if want := 2 * 3.14159265 * 200 * 1.25 / 30; st.RaceDistance < float32(want)-0.1 || st.RaceDistance > float32(want)+0.1 {
		t.Fatalf("distance %v, want %v", st.RaceDistance, want)
	}
	if l := r.state(2600 * time.Millisecond).Lap; l != 3 {
		t.Fatalf("lap at 2.6 s: %d", l)
	}
	end := r.state(time.Hour)
	if end.Lap != 3 || end.Flags&proto.KartFinished == 0 || end.Pos != proto.Vec3(frames[len(frames)-1].Pos) {
		t.Fatalf("after the end %+v", end)
	}
	var norm float64
	for _, c := range st.Rot {
		norm += float64(c) * float64(c)
	}
	if norm < 32700*32700 || norm > 32768*32768 {
		t.Fatalf("rotation %v", st.Rot)
	}
}

func testManifests(sim int32) *manifestFile {
	f := &manifestFile{SimVersion: sim}
	add := func(id string, items ...manifestItem) {
		p := manifestPack{PackID: id, Commit: "botcommit", Items: items}
		p.ManifestHash, _ = content.ManifestHash(p.items())
		f.Packs = append(f.Packs, p)
	}
	add("game", manifestItem{proto.ContentCharacter, "game.huhn", "f1"}, manifestItem{proto.ContentKart, "game.kart", "f2"})
	add("mhk2", manifestItem{proto.ContentTrack, "mhk2.rome", "f3"})
	return f
}

func writeJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "file.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManifestFile(t *testing.T) {
	f := testManifests(7)
	got, err := loadManifests(writeJSON(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if refs := got.refs(); len(refs) != 2 || refs[1].PackID != "mhk2" || refs[1].ManifestHash != f.Packs[1].ManifestHash {
		t.Fatalf("refs %+v", refs)
	}
	ms := got.manifests([]string{f.Packs[1].ManifestHash})
	if len(ms) != 1 || ms[0].PackID != "mhk2" || ms[0].SimVersion != 7 || len(ms[0].Items) != 1 {
		t.Fatalf("manifests %+v", ms)
	}
	f.Packs[0].Items[0].Fingerprint = "changed"
	if _, err := loadManifests(writeJSON(t, f)); err == nil {
		t.Fatal("a manifest whose items do not match its hash was accepted")
	}
}

func TestParseServer(t *testing.T) {
	addr, host, conf, err := parseServer("tls://mhk-os.svit.ac:7350")
	if err != nil || addr != "mhk-os.svit.ac:7350" || host != "mhk-os.svit.ac" || conf == nil || conf.ServerName != host {
		t.Fatalf("%q %q %v %v", addr, host, conf, err)
	}
	if addr, _, conf, err = parseServer("tcp://127.0.0.1:7351"); err != nil || addr != "127.0.0.1:7351" || conf != nil {
		t.Fatalf("%q %v %v", addr, conf, err)
	}
	if _, _, _, err = parseServer("tcp://nohost"); err == nil {
		t.Fatal("missing port accepted")
	}
}

// stack is an in-process server (content catalog, race engine, lobbies) on ephemeral ports with a fresh catalog
// (its own simVersion) and the files a bot needs.
type stack struct {
	addr      string
	st        *store.Store
	sim       int32
	mf        *manifestFile
	dir       string
	ghost     string // path of the short ghost
	manifests string // path of the manifest file
}

// startStack starts the server with the given idle timeout (0 = the protocol's 45 s).
func startStack(t *testing.T, idle time.Duration) *stack {
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
	slog.SetDefault(log)
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
	srv := server.New(server.Config{Version: "test", UDPPort: uint16(udp.LocalAddr().(*net.UDPAddr).Port),
		ConnLimit: auth.Rate{N: 1000, Per: time.Minute}, IdleTimeout: idle},
		server.Deps{Log: log, Auth: auth.NewService(st, st, auth.NewHasher(auth.DefaultParams, 2), limits),
			Accounts: st, Sessions: st, Moderation: st, Runs: st, Blobs: ghosts})
	catalog := content.New(st, log)
	catalog.Register(srv)
	races := race.NewEngine(race.Config{Log: log, Hub: srv.Hub(), Kicks: st, UDP: udp, Catalog: catalog})
	races.Register(srv)
	lobbies := lobby.New(lobby.Config{Races: races, Catalog: catalog, Pusher: srv.Hub(), Audit: st, Log: log})
	lobbies.Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(sctx, ln) }()
	t.Cleanup(func() {
		stop()
		<-served
		lobbies.Close()
		races.Close()
	})

	var simBytes [4]byte
	rand.Read(simBytes[:])
	s := &stack{addr: ln.Addr().String(), st: st, dir: t.TempDir(),
		sim: int32(binary.LittleEndian.Uint32(simBytes[:])&0xFFFFFF) + 1_000_000}
	s.mf = testManifests(s.sim)
	s.manifests = writeJSON(t, s.mf)
	s.ghost = filepath.Join(s.dir, "run.mhkghost")
	if err := os.WriteFile(s.ghost, shortGhost(t), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

func randomName(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// human connects a scripted player that pings like the game, announces the stack's content and creates a
// private room with a loadout.
func (s *stack) human(t *testing.T, ctx context.Context) (*testclient.Client, *proto.LobbyState) {
	t.Helper()
	c, err := testclient.Dial(ctx, s.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	hello := testclient.DefaultHello()
	hello.SimVersion = s.sim
	if _, err := c.Hello(ctx, hello); err != nil {
		t.Fatal(err)
	}
	c.KeepAlive(ctx, 200*time.Millisecond)
	if _, err := c.Register(ctx, randomName("hu"), "kart-racer-2026"); err != nil {
		t.Fatal(err)
	}
	res, err := c.ContentHello(ctx, s.sim, s.mf.refs())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ContentManifests(ctx, s.mf.manifests(res.Missing)); err != nil {
		t.Fatal(err)
	}
	room, err := c.CreateRoom(ctx, &proto.RoomSettings{Mode: proto.RoomRaceSet, Packs: []string{"mhk2"},
		Anticheat: true, EngineClass: 2, MaxPlayers: 4, TrackPick: proto.PickOwner, RacesPerSet: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetLoadout(ctx, "game.huhn", "game.kart"); err != nil {
		t.Fatal(err)
	}
	return c, room
}

// bot runs mhk-bot into the room; the returned channel yields run's result.
func (s *stack) bot(ctx context.Context, code string, keepAlive time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-server", "tcp://" + s.addr, "-user", randomName("bt"), "-pass", "kart-racer-2026",
			"-join", code, "-ghost", s.ghost, "-manifests", s.manifests, "-character", "game.huhn", "-kart", "game.kart",
			"-keepalive", keepAlive.String()})
	}()
	return done
}

// TestBotRacesHuman: a human creates a private room, the bot joins by code with a manifest file and a ghost,
// the human starts a race and the bot replays the ghost: its states reach the human through World and it
// finishes first. The 1 s idle timeout is shorter than the countdown plus the race, which the bot spends without
// any other TCP traffic: its keepalive pings hold the connection.
func TestBotRacesHuman(t *testing.T) {
	s := startStack(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	human, room := s.human(t, ctx)
	botDone := s.bot(ctx, room.Code, 200*time.Millisecond)
	if _, err := human.WaitLobbyState(ctx, func(s *proto.LobbyState) bool {
		return len(s.Members) == 2 && s.Members[1].KartID == "game.kart" && s.Members[1].Ready
	}); err != nil {
		t.Fatal(err)
	}
	if err := human.StartRace(ctx, "mhk2.rome"); err != nil {
		t.Fatal(err)
	}
	var setup proto.RaceSetup
	if err := human.WaitPushMsg(ctx, proto.PushRaceSetup, &setup); err != nil {
		t.Fatal(err)
	}
	u, err := testclient.DialUDP(fmt.Sprintf("127.0.0.1:%d", setup.UDPPort), setup.UDPToken, setup.UDPKey)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	u.Run(ctx, true)
	if err := human.Notify(proto.NotifyRaceLoaded, &proto.RaceLoaded{RaceID: setup.RaceID,
		TrackHash: "00112233445566778899aabbccddeeff", Laps: 3}); err != nil {
		t.Fatal(err)
	}
	var goMsg proto.RaceGo
	if err := human.WaitPushMsg(ctx, proto.PushRaceGo, &goMsg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(goMsg.Go) + 2*time.Second)
	w, _ := u.World()
	if w == nil || len(w.Entries) != 1 || w.Entries[0].State.RaceDistance < 10 || w.Entries[0].State.Tick < 60 {
		t.Fatalf("World 2 s after GO: %+v", w)
	}
	botSlot := w.Entries[0].Slot
	var pf proto.PlayerFinished
	if err := human.WaitPushMsg(ctx, proto.PushPlayerFinished, &pf); err != nil || pf.Slot != botSlot || pf.FinishMs != 3500 {
		t.Fatalf("PlayerFinished %+v %v", pf, err)
	}
	ms := int32(time.Since(goMsg.Go).Milliseconds())
	if err := human.Notify(proto.NotifyFinished, &proto.Finished{RaceID: setup.RaceID, FinishMs: ms,
		LapMs: []int32{ms / 3, ms / 3, ms - 2*(ms/3)}}); err != nil {
		t.Fatal(err)
	}
	var results proto.RaceResults
	if err := human.WaitPushMsg(ctx, proto.PushRaceResults, &results); err != nil {
		t.Fatal(err)
	}
	if len(results.Rows) != 2 || results.Rows[0].Slot != botSlot || results.Rows[0].Outcome != 0 ||
		results.Rows[1].Outcome != 0 {
		t.Fatalf("results %+v", results.Rows)
	}
	cancel()
	if err := <-botDone; err != nil {
		t.Fatal(err)
	}

	// Both announced the same manifests, so a dump reproduces the file the bot used.
	out := filepath.Join(s.dir, "dump.json")
	if err := dumpManifests(context.Background(), s.st, s.sim, []string{"game", "mhk2"}, out); err != nil {
		t.Fatal(err)
	}
	dumped, err := loadManifests(out)
	if err != nil {
		t.Fatal(err)
	}
	if dumped.SimVersion != s.sim || len(dumped.Packs) != 2 || dumped.Packs[1].ManifestHash != s.mf.Packs[1].ManifestHash ||
		dumped.Packs[0].Commit != "botcommit" {
		t.Fatalf("dump %+v", dumped)
	}
}

// TestBotSurvivesIdleTimeout: with a 1 s idle timeout, a bot idling in a room for 4 s stays thanks to its pings,
// while one that pings too rarely is dropped (and leaves the lobby), which shows the test can see the failure.
func TestBotSurvivesIdleTimeout(t *testing.T) {
	s := startStack(t, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	human, room := s.human(t, ctx)
	members := func(n int) func(*proto.LobbyState) bool {
		return func(st *proto.LobbyState) bool { return len(st.Members) == n }
	}

	quiet, stopQuiet := context.WithCancel(ctx)
	defer stopQuiet()
	s.bot(quiet, room.Code, time.Hour)
	if _, err := human.WaitLobbyState(ctx, members(2)); err != nil {
		t.Fatal(err)
	}
	dropCtx, dropCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dropCancel()
	if _, err := human.WaitLobbyState(dropCtx, members(1)); err != nil {
		t.Fatalf("a bot without keepalive was not dropped: %v", err)
	}

	s.bot(ctx, room.Code, 200*time.Millisecond)
	if _, err := human.WaitLobbyState(ctx, members(2)); err != nil {
		t.Fatal(err)
	}
	idleCtx, idleCancel := context.WithTimeout(ctx, 4*time.Second)
	defer idleCancel()
	if st, err := human.WaitLobbyState(idleCtx, members(1)); err == nil {
		t.Fatalf("the pinging bot was dropped: %+v", st.Members)
	}
}
