package server_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/admin"
	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/content"
	"github.com/mhk-os/server/internal/ghost/ghosttest"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/testclient"
)

// Time-trial tests use a unique track and kart per test, so boards and setup votes of earlier runs against the
// same database do not interfere.

type player struct {
	c    *testclient.Client
	acct proto.Account
	name string
}

func newPlayer(t *testing.T, ts *testServer, prefix string) *player {
	t.Helper()
	c := connect(t, ts)
	name := uniqueName(prefix)
	s, err := c.Register(ctxT(t), name, password)
	if err != nil {
		t.Fatal(err)
	}
	announce(t, c, nil)
	return &player{c: c, acct: s.Account, name: name}
}

// announce sends the connection's content (uploads need it) and every manifest the server asks for.
func announce(t *testing.T, c *testclient.Client, ms []proto.PackManifest) {
	t.Helper()
	refs := make([]proto.PackRef, len(ms))
	for i, m := range ms {
		refs[i] = proto.PackRef{PackID: m.PackID, Commit: m.Commit, ManifestHash: m.ManifestHash}
	}
	res, err := c.ContentHello(ctxT(t), testclient.DefaultHello().ContentVersion, refs)
	if err != nil {
		t.Fatal(err)
	}
	var send []proto.PackManifest
	for _, m := range ms {
		if slices.Contains(res.Missing, m.ManifestHash) {
			send = append(send, m)
		}
	}
	if len(send) > 0 {
		if err := c.ContentManifests(ctxT(t), send); err != nil {
			t.Fatal(err)
		}
	}
}

// newModerator registers an account, promotes it with the CLI and logs in again so the connection has the role.
func newModerator(t *testing.T, ts *testServer) *player {
	t.Helper()
	p := newPlayer(t, ts, "mod_")
	runAdmin(t, ts, "account", "set-role", p.name, "moderator")
	c := connect(t, ts)
	s, err := c.Login(ctxT(t), p.name, password)
	if err != nil || s.Account.Role != proto.RoleModerator {
		t.Fatalf("moderator login %+v %v", s, err)
	}
	announce(t, c, nil)
	return &player{c: c, acct: s.Account, name: p.name}
}

func uniqueID(prefix string) string { return uniqueName(prefix) }

// run returns a valid ghost on track/kart whose laps take the given times (0.5 s run-up).
func run(track, kart string, laps ...float32) *ghosttest.Ghost {
	g := ghosttest.New()
	g.Header.TrackID, g.Header.KartID = track, kart
	g.SetTimes(0.5, laps...)
	return g
}

func upload(t *testing.T, p *player, g *ghosttest.Ghost) *proto.UploadResult {
	t.Helper()
	res, err := p.c.UploadRun(ctxT(t), g.Encode())
	if err != nil {
		t.Fatalf("upload by %s: %v", p.name, err)
	}
	return res
}

func board(t *testing.T, p *player, id uint64, filter proto.LeaderboardFilter) *proto.Leaderboard {
	t.Helper()
	lb, err := p.c.GetLeaderboard(ctxT(t), &proto.GetLeaderboard{BoardID: id, Limit: 100, Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	return lb
}

func names(entries []proto.RunEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Username
	}
	return out
}

func wantNames(t *testing.T, lb *proto.Leaderboard, want ...string) {
	t.Helper()
	got := names(lb.Entries)
	if strings.Join(got, ",") != strings.Join(want, ",") || int(lb.Total) != len(want) {
		t.Fatalf("leaderboard %v (total %d), want %v", got, lb.Total, want)
	}
	for i, e := range lb.Entries {
		if e.Rank != uint32(i+1) {
			t.Fatalf("%s has rank %d, want %d", e.Username, e.Rank, i+1)
		}
	}
}

func TestUploadAndLeaderboard(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kart := uniqueID("trk"), uniqueID("kart")
	a, b := newPlayer(t, ts, "lbA_"), newPlayer(t, ts, "lbB_")

	_, err := connect(t, ts).UploadRun(ctx, run(track, kart, 30, 30, 30).Encode())
	wantStatus(t, err, proto.StatusNotLoggedIn)

	r1 := upload(t, a, run(track, kart, 31, 30, 31)) // 92.5 s
	if !r1.PersonalBest || r1.Duplicate || r1.Run.Rank != 1 || r1.Run.FinishMs != 92_500 ||
		r1.Run.BestLapMs != 30_000 || len(r1.Run.LapMs) != 3 || r1.Run.TrackID != track ||
		r1.Run.EngineClass != 2 || r1.Run.Username != a.name || r1.Run.AccountID != a.acct.ID || r1.Run.Hidden {
		t.Fatalf("first upload %+v", r1)
	}
	boards, err := a.c.ListBoards(ctx, strings.ToUpper(track), proto.AnyEngineClass, proto.AnyContentVersion)
	if err != nil || len(boards) != 1 {
		t.Fatalf("boards %+v %v", boards, err)
	}
	bd := boards[0]
	if bd.BoardID != r1.Run.BoardID || bd.TrackID != track || bd.Laps != 3 || bd.EngineClass != 2 ||
		bd.ContentVersion != 4 || bd.Players != 1 || bd.BestMs != 92_500 || bd.LastUpload.IsZero() {
		t.Fatalf("board %+v", bd)
	}
	if bs, _ := a.c.ListBoards(ctx, track, 1, 0); len(bs) != 0 {
		t.Fatalf("engine class filter: %+v", bs)
	}
	if bs, _ := a.c.ListBoards(ctx, track, 2, 5); len(bs) != 0 {
		t.Fatalf("content version filter: %+v", bs)
	}

	r2 := upload(t, a, run(track, kart, 30, 30, 30)) // 90.5 s: new best
	if !r2.PersonalBest || r2.Run.Rank != 1 || r2.Run.RunID == r1.Run.RunID || r2.Run.BoardID != bd.BoardID {
		t.Fatalf("second upload %+v", r2)
	}
	r3 := upload(t, a, run(track, kart, 32, 32, 32)) // slower
	if r3.PersonalBest || r3.Run.Rank != 0 {
		t.Fatalf("slower upload %+v", r3)
	}
	upload(t, b, run(track, kart, 31, 31, 30))         // 92.5 s
	rb := upload(t, b, run(track, kart, 30, 30.5, 30)) // 91 s
	if !rb.PersonalBest || rb.Run.Rank != 2 {
		t.Fatalf("b's best %+v", rb)
	}
	// Another track case and the same identity: same board.
	g := run(strings.ToUpper(track), kart, 31, 31, 31.2)
	if res := upload(t, b, g); res.Run.BoardID != bd.BoardID || res.Run.Rank != 0 {
		t.Fatalf("upper-case track %+v", res)
	}

	lb := board(t, a, bd.BoardID, proto.LeaderboardFilter{})
	wantNames(t, lb, a.name, b.name)
	if lb.Entries[0].RunID != r2.Run.RunID || lb.Entries[1].RunID != rb.Run.RunID {
		t.Fatalf("best runs %d %d", lb.Entries[0].RunID, lb.Entries[1].RunID)
	}
	if lb.Mine == nil || lb.Mine.RunID != r2.Run.RunID || lb.Mine.Rank != 1 {
		t.Fatalf("mine %+v", lb.Mine)
	}
	page, err := connect(t, ts).GetLeaderboard(ctx, &proto.GetLeaderboard{BoardID: bd.BoardID, Offset: 1, Limit: 1})
	if err != nil || page.Total != 2 || len(page.Entries) != 1 || page.Entries[0].Rank != 2 || page.Mine != nil {
		t.Fatalf("page 2 logged out %+v %v", page, err)
	}
	page, err = b.c.GetLeaderboard(ctx, &proto.GetLeaderboard{BoardID: bd.BoardID, Offset: 5, Limit: 10})
	if err != nil || page.Total != 2 || len(page.Entries) != 0 || page.Mine == nil || page.Mine.Rank != 2 {
		t.Fatalf("past the end %+v %v", page, err)
	}
	_, err = a.c.GetLeaderboard(ctx, &proto.GetLeaderboard{BoardID: bd.BoardID, Limit: 0})
	wantStatus(t, err, proto.StatusBadRequest)
	_, err = a.c.GetLeaderboard(ctx, &proto.GetLeaderboard{BoardID: bd.BoardID, Limit: 101})
	wantStatus(t, err, proto.StatusBadRequest)
	_, err = a.c.GetLeaderboard(ctx, &proto.GetLeaderboard{BoardID: 1 << 60, Limit: 10})
	wantStatus(t, err, proto.StatusNotFound)

	boards, _ = a.c.ListBoards(ctx, track, proto.AnyEngineClass, proto.AnyContentVersion)
	if boards[0].Players != 2 || boards[0].BestMs != 90_500 {
		t.Fatalf("board stats %+v", boards[0])
	}

	// Duplicates: the owner gets the run back, anybody else Forbidden.
	dup := run(track, kart, 30, 30, 30)
	dup.Header.ID = ghostIDOf(t, a, r2.Run.RunID)
	file := dup.Encode()
	again, err := a.c.UploadRun(ctx, file)
	if err != nil || !again.Duplicate || again.Run.RunID != r2.Run.RunID || !again.PersonalBest {
		t.Fatalf("duplicate %+v %v", again, err)
	}
	_, err = b.c.UploadRun(ctx, file)
	wantStatus(t, err, proto.StatusForbidden)

	// Download returns the exact bytes and the entry.
	orig := run(track, kart, 33, 33, 33)
	origFile := orig.Encode()
	up, err := b.c.UploadRun(ctx, origFile)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := connect(t, ts).DownloadRun(ctx, up.Run.RunID)
	if err != nil || !bytes.Equal(dl.File, origFile) || dl.Run.RunID != up.Run.RunID ||
		dl.Run.FileSize != uint32(len(origFile)) || dl.Run.GhostID != orig.Header.ID {
		t.Fatalf("download %+v %v", dl.Run, err)
	}
	_, err = a.c.DownloadRun(ctx, 1<<60)
	wantStatus(t, err, proto.StatusNotFound)

	mine, err := a.c.MyRuns(ctx, track)
	if err != nil || len(mine) != 3 || mine[0].RunID != r3.Run.RunID || mine[1].Rank != 1 || mine[2].Rank != 0 {
		t.Fatalf("my runs %+v %v", mine, err)
	}
	if other, _ := a.c.MyRuns(ctx, uniqueID("none")); len(other) != 0 {
		t.Fatalf("other track %+v", other)
	}
	_, err = connect(t, ts).MyRuns(ctx, track)
	wantStatus(t, err, proto.StatusNotLoggedIn)
}

func ghostIDOf(t *testing.T, p *player, runID uint64) string {
	t.Helper()
	dl, err := p.c.DownloadRun(ctxT(t), runID)
	if err != nil {
		t.Fatal(err)
	}
	return dl.Run.GhostID
}

func TestUploadValidation(t *testing.T) {
	ts := start(t, server.Config{MalformedLimit: auth.Rate{N: 1000, Per: time.Minute}}, generous)
	ctx := ctxT(t)
	p := newPlayer(t, ts, "val_")
	track, kart := uniqueID("trk"), uniqueID("kart")
	cases := []struct {
		name   string
		mutate func(g *ghosttest.Ghost)
		file   func(g *ghosttest.Ghost) []byte
		want   string
	}{
		{"not gzip", nil, func(g *ghosttest.Ghost) []byte { return g.Raw() }, "not a GZip file"},
		{"inflate bomb", nil, func(*ghosttest.Ghost) []byte { return ghosttest.Gzip(make([]byte, 49<<20)) }, "inflated size"},
		{"version", func(g *ghosttest.Ghost) { g.Version = 9 }, nil, "format version 9"},
		{"trailing", func(g *ghosttest.Ghost) { g.Trailing = []byte{7} }, nil, "after the body block"},
		{"id", func(g *ghosttest.Ghost) { g.Header.ID = "nothex" }, nil, "ghost id"},
		{"track id", func(g *ghosttest.Ghost) { g.Header.TrackID = "../etc" }, nil, "track id"},
		{"hash", func(g *ghosttest.Ghost) { g.Header.SetupHash = "" }, nil, "setup hash"},
		{"engine class", func(g *ghosttest.Ghost) { g.Header.EngineClass = 7 }, nil, "engine class"},
		{"laps", func(g *ghosttest.Ghost) { g.Header.Laps = 4 }, nil, "3 lap times for 4 laps"},
		{"tick", func(g *ghosttest.Ghost) { g.Header.TickSeconds = 0.5 }, nil, "tick"},
		{"finish", func(g *ghosttest.Ghost) { g.Header.FinishTime = -1 }, nil, "finish time"},
		{"lap sum", func(g *ghosttest.Ghost) { g.Header.LapTimes[2] = 40 }, nil, "more than the finish"},
		{"frames", func(g *ghosttest.Ghost) { g.SetFrames(100) }, nil, "end before the finish"},
		{"position", func(g *ghosttest.Ghost) { g.Channels[2][50] = -900_000_000 }, nil, "position z"},
		{"event kind", func(g *ghosttest.Ghost) { g.Events[0].Kind = 99 }, nil, "unknown kind 99"},
		{"effect index", func(g *ghosttest.Ghost) { g.Events[2].Effect = 9 }, nil, "uses effect 9"},
		{"event time", func(g *ghosttest.Ghost) { g.Events[1].Time = 500 }, nil, "event 1 time"},
	}
	for _, tc := range cases {
		g := run(track, kart, 30, 30, 30)
		if tc.mutate != nil {
			tc.mutate(g)
		}
		file := g.Encode()
		if tc.file != nil {
			file = tc.file(g)
		}
		_, err := p.c.UploadRun(ctx, file)
		var se *testclient.StatusError
		if !errors.As(err, &se) || se.Status != proto.StatusBadRequest ||
			!strings.HasPrefix(se.Message, "Invalid ghost file: ") || !strings.Contains(se.Message, tc.want) {
			t.Errorf("%s: %v, want BadRequest mentioning %q", tc.name, err, tc.want)
		}
	}
	// Over the 8 MiB field limit: rejected by the codec.
	_, err := p.c.UploadRun(ctx, make([]byte, proto.MaxGhostFile+1))
	wantStatus(t, err, proto.StatusBadRequest)
	if mine, _ := p.c.MyRuns(ctx, track); len(mine) != 0 {
		t.Fatalf("invalid uploads stored: %+v", mine)
	}
}

func TestUploadRateLimit(t *testing.T) {
	ts := start(t, server.Config{UploadLimit: auth.Rate{N: 3, Per: time.Hour}}, generous)
	ctx := ctxT(t)
	p := newPlayer(t, ts, "rate_")
	track, kart := uniqueID("trk"), uniqueID("kart")
	first := upload(t, p, run(track, kart, 30, 30, 30))
	upload(t, p, run(track, kart, 31, 30, 30))
	upload(t, p, run(track, kart, 32, 30, 30))
	_, err := p.c.UploadRun(ctx, run(track, kart, 33, 30, 30).Encode())
	var se *testclient.StatusError
	if !errors.As(err, &se) || se.Status != proto.StatusRateLimited || !strings.Contains(se.Message, "last hour") {
		t.Fatalf("fourth upload: %v", err)
	}
	// Other accounts are not affected.
	upload(t, newPlayer(t, ts, "rate_"), run(track, kart, 30, 30, 30))
	if first.Run.RunID == 0 {
		t.Fatal("no run id")
	}
}

func TestUploadContentChecks(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	pack, mod, track := uniqueID("pk"), uniqueID("mod"), uniqueID("trk")
	kart := pack + ".kart"
	manifest := func(packID, fingerprint string, ids ...string) proto.PackManifest {
		m := proto.PackManifest{PackID: packID, Commit: "c1", ContentVersion: testclient.DefaultHello().ContentVersion}
		for _, id := range ids {
			m.Items = append(m.Items, proto.ManifestItem{Kind: proto.ContentKart, ID: id, Fingerprint: fingerprint})
		}
		m.ManifestHash, _ = content.ManifestHash(m.Items)
		return m
	}
	wantRefused := func(p *player, status proto.Status, msg string) {
		t.Helper()
		_, err := p.c.UploadRun(ctx, run(track, kart, 30, 30, 30).Encode())
		wantStatus(t, err, status)
		if !strings.Contains(err.Error(), msg) {
			t.Fatalf("%v does not mention %q", err, msg)
		}
	}

	// Nothing announced on this connection yet.
	c := connect(t, ts)
	s, err := c.Register(ctx, uniqueName("cc_"), password)
	if err != nil {
		t.Fatal(err)
	}
	wantRefused(&player{c: c, acct: s.Account}, proto.StatusBadRequest, "hasn't sent its content")

	// Three accounts back the kart's fingerprint; a fourth copy of the same commit differs.
	var honest []*player
	for range 3 {
		p := newPlayer(t, ts, "cc_")
		announce(t, p.c, []proto.PackManifest{manifest(pack, "aa", kart)})
		honest = append(honest, p)
	}
	tampered := newPlayer(t, ts, "cc_")
	announce(t, tampered.c, []proto.PackManifest{manifest(pack, "bb", kart)})
	wantRefused(tampered, proto.StatusForbidden, "Your copy of "+pack+" differs from other players' ("+kart+")")
	upload(t, honest[0], run(track, kart, 30, 30, 30))

	// A mod replacing the kart by load order.
	modded := newPlayer(t, ts, "cc_")
	announce(t, modded.c, []proto.PackManifest{manifest(pack, "aa"), manifest(mod, "ff", kart)})
	wantRefused(modded, proto.StatusForbidden, "uses "+kart+" replaced by "+mod)
}

func TestLeaderboardFilters(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kartX, kartY := uniqueID("trk"), uniqueID("kx"), uniqueID("ky")
	type spec struct {
		prefix, country, char string
		lap                   float32
	}
	specs := []spec{
		{"Huhn_de", "DE", "chicken", 30},
		{"skier_", "SK", "frog", 31},
		{"yankee", "US", "chicken", 32},
		{"tokyo_", "JP", "frog", 33},
	}
	var ps []*player
	var boardID uint64
	for _, s := range specs {
		p := newPlayer(t, ts, s.prefix)
		if _, err := p.c.SetCountry(ctx, s.country); err != nil {
			t.Fatal(err)
		}
		g := run(track, kartX, s.lap, s.lap, s.lap)
		g.Header.CharacterID = s.char
		boardID = upload(t, p, g).Run.BoardID
		ps = append(ps, p)
	}
	// The DE player also has a slower run with kart Y.
	upload(t, ps[0], run(track, kartY, 35, 35, 35))

	wantNames(t, board(t, ps[0], boardID, proto.LeaderboardFilter{}), ps[0].name, ps[1].name, ps[2].name, ps[3].name)
	wantNames(t, board(t, ps[0], boardID, proto.LeaderboardFilter{Username: strings.ToUpper(ps[1].name[2:9])}),
		ps[1].name)
	wantNames(t, board(t, ps[0], boardID, proto.LeaderboardFilter{Username: "%"}))
	// "_" is a literal character, not a LIKE wildcard: only names containing "n_" match.
	lb := board(t, ps[0], boardID, proto.LeaderboardFilter{Username: "N_"})
	wantNames(t, lb, ps[0].name)
	wantNames(t, board(t, ps[0], boardID, proto.LeaderboardFilter{Country: "US"}), ps[2].name)
	lb = board(t, ps[3], boardID, proto.LeaderboardFilter{Continent: "EU"})
	wantNames(t, lb, ps[0].name, ps[1].name)
	if lb.Mine != nil {
		t.Fatalf("JP player in the EU list: %+v", lb.Mine)
	}
	wantNames(t, board(t, ps[0], boardID, proto.LeaderboardFilter{Continent: "AS"}), ps[3].name)
	wantNames(t, board(t, ps[0], boardID, proto.LeaderboardFilter{Continent: "EU", Country: "US"}))
	wantNames(t, board(t, ps[0], boardID, proto.LeaderboardFilter{CharacterID: "FROG"}), ps[1].name, ps[3].name)
	// With a kart filter the DE player's best is the kart Y run.
	lb = board(t, ps[0], boardID, proto.LeaderboardFilter{KartID: kartY})
	wantNames(t, lb, ps[0].name)
	if lb.Entries[0].FinishMs != 105_500 || lb.Mine == nil || lb.Mine.Rank != 1 {
		t.Fatalf("kart filter %+v", lb.Entries[0])
	}
	lb = board(t, ps[2], boardID, proto.LeaderboardFilter{KartID: kartX})
	if len(lb.Entries) != 4 || lb.Mine == nil || lb.Mine.Rank != 3 || lb.Mine.Username != ps[2].name {
		t.Fatalf("mine with kart filter %+v", lb.Mine)
	}
	for _, f := range []proto.LeaderboardFilter{{Continent: "XX"}, {Country: "ZZ"}} {
		_, err := ps[0].c.GetLeaderboard(ctx, &proto.GetLeaderboard{BoardID: boardID, Limit: 10, Filter: f})
		wantStatus(t, err, proto.StatusBadRequest)
	}
}

func TestSetupMismatch(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	track, kart := uniqueID("trk"), uniqueID("kart")
	const common, odd = "11111111111111111111111111111111", "22222222222222222222222222222222"
	withSetup := func(hash string) *ghosttest.Ghost {
		g := run(track, kart, 30, 30, 30)
		g.Header.SetupHash = hash
		return g
	}
	a, b := newPlayer(t, ts, "su_"), newPlayer(t, ts, "su_")
	ra := upload(t, a, withSetup(common))
	rb := upload(t, b, withSetup(odd))
	if ra.Run.SetupMismatch || rb.Run.SetupMismatch {
		t.Fatal("mismatch before 3 accounts uploaded")
	}
	upload(t, newPlayer(t, ts, "su_"), withSetup(common))
	rc := upload(t, newPlayer(t, ts, "su_"), withSetup(common))
	if rc.Run.SetupMismatch {
		t.Fatal("majority run marked")
	}
	lb := board(t, a, ra.Run.BoardID, proto.LeaderboardFilter{})
	for _, e := range lb.Entries {
		if e.SetupMismatch != (e.RunID == rb.Run.RunID) {
			t.Fatalf("run %d by %s: mismatch %t", e.RunID, e.Username, e.SetupMismatch)
		}
	}
	// Another track with the same kart, class and laps shares the vote.
	other := withSetup(odd)
	other.Header.TrackID = uniqueID("trk")
	if res := upload(t, b, other); !res.Run.SetupMismatch {
		t.Fatal("odd setup on another track not marked")
	}
}

func TestReports(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kart := uniqueID("trk"), uniqueID("kart")
	owner, r1, r2, kicked := newPlayer(t, ts, "own_"), newPlayer(t, ts, "rep_"), newPlayer(t, ts, "rep_"),
		newPlayer(t, ts, "kick_")
	if err := ts.st.AddCheatKick(ctx, &store.CheatKick{AccountID: kicked.acct.ID, RaceRef: "race 1",
		Reason: "teleporting", CreatedAt: store.Now()}); err != nil {
		t.Fatal(err)
	}
	res := upload(t, owner, run(track, kart, 30, 30, 30))
	id := res.Run.RunID
	mod := newModerator(t, ts)
	// Only moderators see flags: a player's answer carries 0, the moderator's download the count.
	report := func(p *player, reason proto.FlagReason, detail string) uint32 {
		t.Helper()
		flags, err := p.c.ReportRun(ctx, &proto.ReportRun{RunID: id, Reason: reason, AtSeconds: 12.5, Detail: detail})
		if err != nil || flags != 0 {
			t.Fatalf("report by %s: flags %d, %v", p.name, flags, err)
		}
		dl, err := mod.c.DownloadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return dl.Run.Flags
	}
	if f := report(r1, proto.FlagTeleport, "jumped 40 m"); f != 1 {
		t.Fatalf("first report: %d", f)
	}
	if f := report(r1, proto.FlagSpeed, "again"); f != 1 {
		t.Fatalf("repeated report: %d", f)
	}
	if f := report(owner, proto.FlagOther, "my own"); f != 1 {
		t.Fatalf("own report: %d", f)
	}
	if f := report(kicked, proto.FlagWallClip, "kicked"); f != 1 {
		t.Fatalf("kicked reporter: %d", f)
	}
	if f := report(r2, proto.FlagDivergence, ""); f != 2 {
		t.Fatalf("second reporter: %d", f)
	}
	_, err := r1.c.ReportRun(ctx, &proto.ReportRun{RunID: id, Reason: 42})
	wantStatus(t, err, proto.StatusBadRequest)
	_, err = r1.c.ReportRun(ctx, &proto.ReportRun{RunID: 1 << 60, Reason: proto.FlagData})
	wantStatus(t, err, proto.StatusNotFound)
	_, err = connect(t, ts).ReportRun(ctx, &proto.ReportRun{RunID: id, Reason: proto.FlagData})
	wantStatus(t, err, proto.StatusNotLoggedIn)

	if dl, err := r1.c.DownloadRun(ctx, id); err != nil || dl.Run.Flags != 0 {
		t.Fatalf("flags for a player %+v %v", dl.Run.Flags, err)
	}
	for _, p := range []*player{r1, mod} {
		want := uint32(0)
		if p == mod {
			want = 2
		}
		lb := board(t, p, res.Run.BoardID, proto.LeaderboardFilter{})
		if lb.Entries[0].Flags != want {
			t.Fatalf("leaderboard flags for %s: %d", p.name, lb.Entries[0].Flags)
		}
		page, err := p.c.PlayerRuns(ctx, owner.acct.ID, 0, 10)
		if err != nil || page.Runs[0].Flags != want {
			t.Fatalf("player runs flags for %s: %+v %v", p.name, page.Runs, err)
		}
	}
	if mine, _ := owner.c.MyRuns(ctx, track); mine[0].Flags != 0 {
		t.Fatalf("own run flags %d", mine[0].Flags)
	}

	reports, err := mod.c.ModRunReports(ctx, id)
	if err != nil || len(reports) != 4 {
		t.Fatalf("reports %+v %v", reports, err)
	}
	counted := map[string]bool{}
	for _, r := range reports {
		counted[r.Reporter] = r.Counted
		if r.Reporter == r1.name && (r.Reason != proto.FlagTeleport || r.Detail != "jumped 40 m" || r.AtSeconds != 12.5) {
			t.Fatalf("first report changed: %+v", r)
		}
	}
	if !counted[r1.name] || !counted[r2.name] || counted[owner.name] || counted[kicked.name] {
		t.Fatalf("counted %v", counted)
	}
	flagged, err := mod.c.ModListFlagged(ctx, 0, 100, false)
	if err != nil || flagged.Total < 1 || !containsRun(flagged.Runs, id) {
		t.Fatalf("flagged list (total %d) lacks run %d: %v", flagged.Total, id, err)
	}
	_, err = mod.c.ModListFlagged(ctx, 0, 0, false)
	wantStatus(t, err, proto.StatusBadRequest)
	_, err = mod.c.ModRunReports(ctx, 1<<60)
	wantStatus(t, err, proto.StatusNotFound)

	// A later kick stops counting the reporter's earlier reports.
	if err := ts.st.AddCheatKick(ctx, &store.CheatKick{AccountID: r2.acct.ID, CreatedAt: store.Now()}); err != nil {
		t.Fatal(err)
	}
	if dl, _ := mod.c.DownloadRun(ctx, id); dl.Run.Flags != 1 {
		t.Fatalf("flags after kicking a reporter: %d", dl.Run.Flags)
	}
	if flags, err := mod.c.ReportRun(ctx, &proto.ReportRun{RunID: id, Reason: proto.FlagSpeed}); err != nil ||
		flags != 2 {
		t.Fatalf("moderator's report answer: %d %v", flags, err)
	}
	info, err := mod.c.ModGetAccount(ctx, r2.acct.ID)
	if err != nil || info.CheatKicks != 1 {
		t.Fatalf("cheat kicks %+v %v", info, err)
	}
}

func containsRun(runs []proto.RunEntry, id uint64) bool {
	for _, r := range runs {
		if r.RunID == id {
			return true
		}
	}
	return false
}

func TestModeration(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kart := uniqueID("trk"), uniqueID("kart")
	p, other := newPlayer(t, ts, "bad_"), newPlayer(t, ts, "good_")
	mod := newModerator(t, ts)
	res := upload(t, p, run(track, kart, 30, 30, 30))
	upload(t, other, run(track, kart, 31, 31, 31))
	id, boardID := res.Run.RunID, res.Run.BoardID

	// Players get Forbidden for every moderation request.
	for _, call := range []func() error{
		func() error { return other.c.ModHideRun(ctx, id, "x") },
		func() error { return other.c.ModUnhideRun(ctx, id) },
		func() error { return other.c.ModDisableAccount(ctx, p.acct.ID, "x") },
		func() error { return other.c.ModEnableAccount(ctx, p.acct.ID) },
		func() error { _, err := other.c.ModListFlagged(ctx, 0, 10, false); return err },
		func() error { _, err := other.c.ModRunReports(ctx, id); return err },
		func() error { _, err := other.c.ModGetAccount(ctx, p.acct.ID); return err },
	} {
		wantStatus(t, call(), proto.StatusForbidden)
	}
	wantStatus(t, connect(t, ts).ModHideRun(ctx, id, "x"), proto.StatusNotLoggedIn)

	// Hide: gone for players, visible with hidden set for moderators asking for it.
	if err := mod.c.ModHideRun(ctx, id, "impossible lap"); err != nil {
		t.Fatal(err)
	}
	wantNames(t, board(t, other, boardID, proto.LeaderboardFilter{}), other.name)
	// The hidden run sorts by its time but has rank 0; the visible ranks stay what players see.
	if lb := board(t, mod, boardID, proto.LeaderboardFilter{IncludeHidden: true}); lb.Entries[0].Rank != 0 ||
		!lb.Entries[0].Hidden || lb.Entries[1].Rank != 1 || lb.Entries[1].Hidden {
		t.Fatalf("moderator view %+v", lb.Entries)
	}
	if lb := board(t, other, boardID, proto.LeaderboardFilter{IncludeHidden: true}); len(lb.Entries) != 1 {
		t.Fatal("includeHidden honoured for a player")
	}
	_, err := other.c.DownloadRun(ctx, id)
	wantStatus(t, err, proto.StatusNotFound)
	_, err = connect(t, ts).DownloadRun(ctx, id)
	wantStatus(t, err, proto.StatusNotFound)
	if dl, err := mod.c.DownloadRun(ctx, id); err != nil || !dl.Run.Hidden {
		t.Fatalf("moderator download %+v %v", dl, err)
	}
	_, err = other.c.ReportRun(ctx, &proto.ReportRun{RunID: id, Reason: proto.FlagSpeed})
	wantStatus(t, err, proto.StatusNotFound)
	if mine, _ := p.c.MyRuns(ctx, track); len(mine) != 1 || !mine[0].Hidden || mine[0].Rank != 0 {
		t.Fatalf("own hidden run %+v", mine)
	}
	if bs, _ := other.c.ListBoards(ctx, track, proto.AnyEngineClass, 0); bs[0].Players != 1 || bs[0].BestMs != 93_500 {
		t.Fatalf("board stats with a hidden run %+v", bs[0])
	}
	if err := mod.c.ModUnhideRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	wantNames(t, board(t, other, boardID, proto.LeaderboardFilter{}), p.name, other.name)
	wantStatus(t, mod.c.ModHideRun(ctx, 1<<60, "x"), proto.StatusNotFound)

	// Disable: live connections get Disconnect(AccountDisabled, reason), runs vanish for players.
	if err := mod.c.ModDisableAccount(ctx, p.acct.ID, "Cheating in time trials"); err != nil {
		t.Fatal(err)
	}
	d, err := p.c.WaitDisconnect(ctx)
	if err != nil || d.Status != proto.StatusAccountDisabled || d.Message != "Cheating in time trials" {
		t.Fatalf("disconnect %+v %v", d, err)
	}
	waitClosed(t, p.c)
	_, err = connect(t, ts).Login(ctx, p.name, password)
	wantStatus(t, err, proto.StatusAccountDisabled)
	wantNames(t, board(t, other, boardID, proto.LeaderboardFilter{}), other.name)
	_, err = other.c.DownloadRun(ctx, id)
	wantStatus(t, err, proto.StatusNotFound)
	if lb := board(t, mod, boardID, proto.LeaderboardFilter{IncludeHidden: true}); len(lb.Entries) != 2 ||
		!lb.Entries[0].Hidden {
		t.Fatalf("moderator view of a disabled account %+v", lb.Entries)
	}
	info, err := mod.c.ModGetAccount(ctx, p.acct.ID)
	if err != nil || !info.Disabled || info.DisabledReason != "Cheating in time trials" || info.Runs != 1 ||
		info.HiddenRuns != 0 || info.Account.Username != p.name || info.LastLogin.IsZero() {
		t.Fatalf("account info %+v %v", info, err)
	}
	if err := mod.c.ModEnableAccount(ctx, p.acct.ID); err != nil {
		t.Fatal(err)
	}
	wantNames(t, board(t, other, boardID, proto.LeaderboardFilter{}), p.name, other.name)
	if _, err := connect(t, ts).Login(ctx, p.name, password); err != nil {
		t.Fatalf("login after enable: %v", err)
	}

	mod2 := newModerator(t, ts)
	wantStatus(t, mod.c.ModDisableAccount(ctx, mod2.acct.ID, "x"), proto.StatusForbidden)
	wantStatus(t, mod.c.ModDisableAccount(ctx, 1<<60, "x"), proto.StatusNotFound)
	_, err = mod.c.ModGetAccount(ctx, 1<<60)
	wantStatus(t, err, proto.StatusNotFound)
	if _, err := mod2.c.Ping(ctx); err != nil {
		t.Fatalf("moderator dropped: %v", err)
	}

	audit := runAdmin(t, ts, "audit", "--limit", "500")
	for _, want := range []string{store.ActionHideRun, store.ActionUnhideRun, store.ActionDisableAccount,
		store.ActionEnableAccount, "impossible lap"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit log lacks %q", want)
		}
	}
	if !strings.Contains(audit, "account "+strconv.FormatUint(mod.acct.ID, 10)) {
		t.Error("audit log lacks the moderator as actor")
	}
}

func TestCLIRuns(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kart := uniqueID("trk"), uniqueID("kart")
	p, reporter := newPlayer(t, ts, "cli_"), newPlayer(t, ts, "cli_")
	res := upload(t, p, run(track, kart, 30, 30, 30))
	other := upload(t, reporter, run(track, kart, 31, 31, 31))
	id := strconv.FormatUint(res.Run.RunID, 10)
	if _, err := reporter.c.ReportRun(ctx, &proto.ReportRun{RunID: res.Run.RunID, Reason: proto.FlagFlying,
		Detail: "airborne"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.c.ReportRun(ctx, &proto.ReportRun{RunID: other.Run.RunID, Reason: proto.FlagSpeed}); err != nil {
		t.Fatal(err)
	}

	out := runAdmin(t, ts, "run", "list", "--account", p.name, "--track", track)
	if !strings.Contains(out, id) || !strings.Contains(out, "1:30.500") || !strings.Contains(out, "1 run(s)") {
		t.Fatalf("run list:\n%s", out)
	}
	out = runAdmin(t, ts, "run", "show", id)
	for _, want := range []string{p.name, track, kart, "1:30.500", "visible", res.Run.GhostID} {
		if !strings.Contains(out, want) {
			t.Fatalf("run show lacks %q:\n%s", want, out)
		}
	}
	if out := runAdmin(t, ts, "run", "reports", id); !strings.Contains(out, reporter.name) ||
		!strings.Contains(out, "Flying") || !strings.Contains(out, "airborne") {
		t.Fatalf("run reports:\n%s", out)
	}
	if out := runAdmin(t, ts, "board", "list", "--track", track); !strings.Contains(out, track) ||
		!strings.Contains(out, "1 board(s)") {
		t.Fatalf("board list:\n%s", out)
	}
	runAdmin(t, ts, "run", "hide", id, "--reason", "sysadmin test")
	if _, err := reporter.c.DownloadRun(ctx, res.Run.RunID); testclient.StatusOf(err) != proto.StatusNotFound {
		t.Fatalf("download after CLI hide: %v", err)
	}
	if out := runAdmin(t, ts, "run", "show", id); !strings.Contains(out, "sysadmin test") {
		t.Fatalf("hidden run show:\n%s", out)
	}
	runAdmin(t, ts, "run", "unhide", id)
	if _, err := reporter.c.DownloadRun(ctx, res.Run.RunID); err != nil {
		t.Fatalf("download after CLI unhide: %v", err)
	}

	var ue *admin.UsageError
	if err := admin.Run(ctx, &admin.Env{Store: ts.st, Blobs: ts.blobs, Out: &bytes.Buffer{}},
		[]string{"run", "delete", id}); !errors.As(err, &ue) {
		t.Fatalf("delete without --yes: %v", err)
	}
	if err := admin.Run(ctx, &admin.Env{Store: ts.st, Blobs: ts.blobs, Out: &bytes.Buffer{}},
		[]string{"run", "hide", id}); !errors.As(err, &ue) {
		t.Fatalf("hide without reason: %v", err)
	}

	// Permanent delete removes the blob.
	view, err := ts.st.RunByID(ctx, res.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	path := ts.blobs.Path(view.Blob)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("blob missing before delete: %v", err)
	}
	out = runAdmin(t, ts, "run", "delete", id, "--yes")
	if !strings.Contains(out, "1 ghost file(s) removed") {
		t.Fatalf("run delete:\n%s", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("blob still there: %v", err)
	}
	_, err = reporter.c.DownloadRun(ctx, res.Run.RunID)
	wantStatus(t, err, proto.StatusNotFound)

	// Account delete removes the account's runs, blobs and reports (the report on the other run stops counting).
	if v, _ := ts.st.RunByID(ctx, other.Run.RunID); v.Flags != 1 {
		t.Fatalf("flags before delete: %d", v.Flags)
	}
	again := upload(t, p, run(track, kart, 29, 29, 29))
	view, err = ts.st.RunByID(ctx, again.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	out = runAdmin(t, ts, "account", "delete", p.name, "--yes")
	if !strings.Contains(out, "1 ghost file(s) removed") {
		t.Fatalf("account delete:\n%s", out)
	}
	if _, err := os.Stat(ts.blobs.Path(view.Blob)); !os.IsNotExist(err) {
		t.Fatalf("account blob still there: %v", err)
	}
	if _, err := ts.st.RunByID(ctx, again.Run.RunID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run of deleted account: %v", err)
	}
	if v, _ := ts.st.RunByID(ctx, other.Run.RunID); v.Flags != 0 {
		t.Fatalf("flags after the reporter was deleted: %d", v.Flags)
	}
	audit := runAdmin(t, ts, "audit", "--limit", "500")
	for _, action := range []string{store.ActionHideRun, store.ActionUnhideRun, store.ActionDeleteRun} {
		if !strings.Contains(audit, action) {
			t.Errorf("audit log lacks %s", action)
		}
	}
}

func TestDedupBlobs(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := context.Background()
	p := newPlayer(t, ts, "blob_")
	g := run(uniqueID("trk"), uniqueID("kart"), 30, 30, 30)
	file := g.Encode()
	res := upload(t, p, g)
	runAdmin(t, ts, "run", "delete", strconv.FormatUint(res.Run.RunID, 10), "--yes")
	// The same bytes uploaded again after the delete are stored again.
	again, err := p.c.UploadRun(ctx, file)
	if err != nil || again.Duplicate {
		t.Fatalf("re-upload %+v %v", again, err)
	}
	dl, err := p.c.DownloadRun(ctx, again.Run.RunID)
	if err != nil || !bytes.Equal(dl.File, file) {
		t.Fatalf("download %v", err)
	}
}
