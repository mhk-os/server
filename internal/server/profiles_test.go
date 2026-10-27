package server_test

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
)

// Profile tests name their accounts after one unique base, so searching for it lists only this test's accounts.

func register(t *testing.T, ts *testServer, name string) *player {
	t.Helper()
	c := connect(t, ts)
	s, err := c.Register(ctxT(t), name, password)
	if err != nil {
		t.Fatal(err)
	}
	return &player{c: c, acct: s.Account, name: name}
}

func summaryNames(players []proto.PlayerSummary) []string {
	out := make([]string, len(players))
	for i, p := range players {
		out[i] = p.Username
	}
	return out
}

func TestScoreRanking(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	base := uniqueName("s")
	a, b, c, d, e := register(t, ts, base+"_a"), register(t, ts, base+"_b"), register(t, ts, base+"_c"),
		register(t, ts, base+"_d"), register(t, ts, base+"_e")
	// Scores far above what other tests record; nobody else holds exactly s+1 or s, so a and b tie and c is two
	// places behind them.
	s := uint32(1_000_000 + rand.IntN(2_000_000_000))
	for _, race := range [][]store.RaceResult{
		{{AccountID: a.acct.ID, Points: s, Win: true, Podium: true}, {AccountID: c.acct.ID, Points: s - 12},
			{AccountID: e.acct.ID, Points: s + 5}},
		{{AccountID: a.acct.ID, Points: 1}, {AccountID: b.acct.ID, Points: s + 1, Podium: true},
			{AccountID: c.acct.ID, Points: 12, Podium: true}},
	} {
		if err := ts.st.RecordRace(ctx, race); err != nil {
			t.Fatal(err)
		}
	}
	ranking := func(p *player) *proto.ScoreRanking {
		t.Helper()
		r, err := p.c.ScoreLeaderboard(ctx, &proto.ScoreLeaderboard{Limit: 100, Search: base})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := ranking(a)
	if strings.Join(summaryNames(r.Players), ",") != strings.Join([]string{e.name, a.name, b.name, c.name}, ",") ||
		r.Total != 4 {
		t.Fatalf("ranking %v (total %d)", summaryNames(r.Players), r.Total)
	}
	top := r.Players[1].Rank
	if r.Players[0].Rank != top-1 || r.Players[2].Rank != top || r.Players[3].Rank != top+2 ||
		r.Players[1].Score != s+1 || r.Players[3].Score != s || !r.Players[1].Online {
		t.Fatalf("ranks %+v", r.Players)
	}
	if r.Mine == nil || r.Mine.AccountID != a.acct.ID || r.Mine.Rank != top {
		t.Fatalf("mine %+v", r.Mine)
	}
	if r := ranking(d); r.Mine != nil {
		t.Fatalf("unranked mine %+v", r.Mine)
	}
	page, err := connect(t, ts).ScoreLeaderboard(ctx, &proto.ScoreLeaderboard{Offset: 3, Limit: 2, Search: base})
	if err != nil || page.Total != 4 || len(page.Players) != 1 || page.Players[0].AccountID != c.acct.ID ||
		page.Mine != nil {
		t.Fatalf("page 2 logged out %+v %v", page, err)
	}
	_, err = a.c.ScoreLeaderboard(ctx, &proto.ScoreLeaderboard{Limit: 101})
	wantStatus(t, err, proto.StatusBadRequest)

	// A disabled account leaves the ranking; everybody behind it moves up.
	runAdmin(t, ts, "account", "disable", e.name, "--reason", "boosting")
	r = ranking(a)
	if strings.Join(summaryNames(r.Players), ",") != strings.Join([]string{a.name, b.name, c.name}, ",") ||
		r.Players[0].Rank != top-1 || r.Players[2].Rank != top+1 {
		t.Fatalf("ranking without the disabled account %+v", r.Players)
	}

	p, err := d.c.GetProfile(ctx, a.acct.ID)
	if err != nil || p.Player.Score != s+1 || p.Player.Rank != top-1 || p.Races != 2 || p.Wins != 1 || p.Podiums != 1 ||
		!p.Listed || p.Player.LastActive.IsZero() || !p.Player.Online || p.Player.Username != a.name {
		t.Fatalf("profile %+v %v", p, err)
	}
	if p, _ := d.c.GetProfile(ctx, d.acct.ID); p.Player.Rank != 0 || p.Player.Score != 0 || p.Races != 0 {
		t.Fatalf("unranked profile %+v", p)
	}
	out := runAdmin(t, ts, "account", "show", a.name)
	for _, want := range []string{"Score", strconv.FormatUint(uint64(s+1), 10), "Rank", strconv.Itoa(int(top - 1)),
		"2 (1 wins, 1 podiums)", "Last active"} {
		if !strings.Contains(out, want) {
			t.Fatalf("account show lacks %q:\n%s", want, out)
		}
	}
}

func TestPlayerDirectory(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	base := uniqueName("d")
	listed, hidden, disabled, newest := register(t, ts, base+"_b"), register(t, ts, base+"_a"),
		register(t, ts, base+"_c"), register(t, ts, base+"_d")
	mod := newModerator(t, ts)
	if err := hidden.c.SetProfileListed(ctx, false); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, connect(t, ts).SetProfileListed(ctx, false), proto.StatusNotLoggedIn)
	if err := mod.c.ModDisableAccount(ctx, disabled.acct.ID, "cheating"); err != nil {
		t.Fatal(err)
	}
	if err := ts.st.AddCheatKick(ctx, &store.CheatKick{AccountID: listed.acct.ID, CreatedAt: store.Now()}); err != nil {
		t.Fatal(err)
	}

	list := func(sort uint8) []string {
		t.Helper()
		page, err := connect(t, ts).ListPlayers(ctx, &proto.ListPlayers{Limit: 100, Search: strings.ToUpper(base),
			Sort: sort})
		if err != nil {
			t.Fatal(err)
		}
		if int(page.Total) != len(page.Players) {
			t.Fatalf("total %d for %d players", page.Total, len(page.Players))
		}
		return summaryNames(page.Players)
	}
	if got := list(proto.PlayerSortNewest); !slices.Equal(got, []string{newest.name, listed.name}) {
		t.Fatalf("newest first %v", got)
	}
	if got := list(proto.PlayerSortName); !slices.Equal(got, []string{listed.name, newest.name}) {
		t.Fatalf("by name %v", got)
	}
	_, err := listed.c.ListPlayers(ctx, &proto.ListPlayers{Limit: 10, Sort: 2})
	wantStatus(t, err, proto.StatusBadRequest)

	ids := []uint64{listed.acct.ID, hidden.acct.ID, disabled.acct.ID, 1 << 60}
	sorted := func(players []proto.PlayerSummary) []string {
		names := summaryNames(players)
		slices.Sort(names)
		return names
	}
	if got, err := listed.c.GetProfiles(ctx, ids...); err != nil ||
		!slices.Equal(sorted(got), []string{hidden.name, listed.name}) {
		t.Fatalf("player GetProfiles %v %v", sorted(got), err)
	}
	if got, err := mod.c.GetProfiles(ctx, ids...); err != nil ||
		!slices.Equal(sorted(got), []string{hidden.name, listed.name, disabled.name}) {
		t.Fatalf("moderator GetProfiles %v %v", sorted(got), err)
	}

	// Unlisted profiles stay reachable; disabled ones exist for moderators only.
	if p, err := listed.c.GetProfile(ctx, hidden.acct.ID); err != nil || p.Listed {
		t.Fatalf("unlisted profile %+v %v", p, err)
	}
	for _, id := range []uint64{disabled.acct.ID, 1 << 60} {
		_, err := listed.c.GetProfile(ctx, id)
		wantStatus(t, err, proto.StatusNotFound)
		_, err = connect(t, ts).PlayerRuns(ctx, id, 0, 10)
		wantStatus(t, err, proto.StatusNotFound)
	}
	p, err := mod.c.GetProfile(ctx, disabled.acct.ID)
	if err != nil || !p.Disabled || p.DisabledReason != "cheating" || p.Player.AccountID != disabled.acct.ID {
		t.Fatalf("moderator view of a disabled profile %+v %v", p, err)
	}
	if _, err := mod.c.PlayerRuns(ctx, disabled.acct.ID, 0, 10); err != nil {
		t.Fatalf("moderator PlayerRuns of a disabled account: %v", err)
	}
	// Moderator-only counters.
	if p, _ := hidden.c.GetProfile(ctx, listed.acct.ID); p.CheatKicks != 0 {
		t.Fatalf("player sees cheat kicks %+v", p)
	}
	if p, _ := mod.c.GetProfile(ctx, listed.acct.ID); p.CheatKicks != 1 {
		t.Fatalf("moderator does not see cheat kicks %+v", p)
	}

	// Online follows the connections; their end is the last activity.
	before, _ := mod.c.GetProfiles(ctx, newest.acct.ID)
	time.Sleep(5 * time.Millisecond)
	newest.c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := mod.c.GetProfiles(ctx, newest.acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got[0].Online {
			// A profile reads the last activity before the online flag; the close records the activity before it
			// drops the flag, so the next read has it.
			if got, err = mod.c.GetProfiles(ctx, newest.acct.ID); err != nil {
				t.Fatal(err)
			}
			if !got[0].LastActive.After(before[0].LastActive) || !before[0].Online {
				t.Fatalf("last active %v before %+v", got[0].LastActive, before[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still online after closing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPlayerRunsAndTimeTrialStats(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, track2, kart := uniqueID("trk"), uniqueID("trk"), uniqueID("kart")
	owner, rival := newPlayer(t, ts, "pr_"), newPlayer(t, ts, "pr_")
	mod := newModerator(t, ts)
	upload(t, rival, run(track, kart, 29, 29, 29))
	best := upload(t, owner, run(track, kart, 30, 30, 30))
	slow := upload(t, owner, run(track, kart, 33, 33, 33))
	mid := upload(t, owner, run(track, kart, 31, 31, 31))
	other := upload(t, owner, run(track2, kart, 30, 30, 30))

	page, err := rival.c.PlayerRuns(ctx, owner.acct.ID, 0, 100)
	if err != nil || page.Total != 4 || len(page.Runs) != 4 {
		t.Fatalf("player runs %+v %v", page, err)
	}
	ranks := map[uint64]uint32{}
	for i, r := range page.Runs {
		ranks[r.RunID] = r.Rank
		if i > 0 && r.Uploaded.After(page.Runs[i-1].Uploaded) {
			t.Fatal("not newest first")
		}
	}
	if page.Runs[0].RunID != other.Run.RunID || ranks[best.Run.RunID] != 2 || ranks[slow.Run.RunID] != 0 ||
		ranks[mid.Run.RunID] != 0 || ranks[other.Run.RunID] != 1 {
		t.Fatalf("ranks %v", ranks)
	}
	if page, err := connect(t, ts).PlayerRuns(ctx, owner.acct.ID, 1, 2); err != nil || page.Total != 4 ||
		len(page.Runs) != 2 || page.Runs[0].RunID != mid.Run.RunID {
		t.Fatalf("second page %+v %v", page, err)
	}
	_, err = rival.c.PlayerRuns(ctx, owner.acct.ID, 0, 0)
	wantStatus(t, err, proto.StatusBadRequest)

	if err := mod.c.ModHideRun(ctx, slow.Run.RunID, "x"); err != nil {
		t.Fatal(err)
	}
	if page, _ := rival.c.PlayerRuns(ctx, owner.acct.ID, 0, 100); page.Total != 3 || containsRun(page.Runs, slow.Run.RunID) {
		t.Fatalf("hidden run listed for a player %+v", page)
	}
	page, err = mod.c.PlayerRuns(ctx, owner.acct.ID, 0, 100)
	if err != nil || page.Total != 4 {
		t.Fatalf("moderator player runs %+v %v", page, err)
	}
	for _, r := range page.Runs {
		if r.Hidden != (r.RunID == slow.Run.RunID) {
			t.Fatalf("hidden flag %+v", r)
		}
	}

	p, err := rival.c.GetProfile(ctx, owner.acct.ID)
	if err != nil || p.TTRuns != 3 || p.TTBoards != 2 || p.TTBestRank != 1 || p.HiddenRuns != 0 {
		t.Fatalf("time-trial stats %+v %v", p, err)
	}
	if p, _ := mod.c.GetProfile(ctx, owner.acct.ID); p.HiddenRuns != 1 {
		t.Fatalf("moderator hidden runs %+v", p)
	}
}

func TestModListHidden(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kart := uniqueID("trk"), uniqueID("kart")
	hider, cheater, clean := newPlayer(t, ts, "hid_"), newPlayer(t, ts, "hid_"), newPlayer(t, ts, "hid_")
	mod := newModerator(t, ts)
	hiddenRun := upload(t, hider, run(track, kart, 30, 30, 30))
	cheaterRun := upload(t, cheater, run(track, kart, 31, 31, 31))
	upload(t, clean, run(track, kart, 32, 32, 32))
	boardID := hiddenRun.Run.BoardID
	if err := mod.c.ModHideRun(ctx, hiddenRun.Run.RunID, "impossible lap"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := mod.c.ModDisableAccount(ctx, cheater.acct.ID, "cheating"); err != nil {
		t.Fatal(err)
	}

	page, err := mod.c.ModListHidden(ctx, boardID, 0, 100)
	if err != nil || page.Total != 2 || len(page.Runs) != 2 || page.Runs[0].RunID != cheaterRun.Run.RunID ||
		page.Runs[1].RunID != hiddenRun.Run.RunID || !page.Runs[0].Hidden || !page.Runs[1].Hidden {
		t.Fatalf("hidden runs %+v %v", page, err)
	}
	if page, err := mod.c.ModListHidden(ctx, 0, 0, 100); err != nil || page.Total < 2 ||
		!containsRun(page.Runs, hiddenRun.Run.RunID) || !containsRun(page.Runs, cheaterRun.Run.RunID) {
		t.Fatalf("hidden runs of every board (total %d): %v", page.Total, err)
	}
	_, err = mod.c.ModListHidden(ctx, boardID, 0, 0)
	wantStatus(t, err, proto.StatusBadRequest)
	_, err = clean.c.ModListHidden(ctx, boardID, 0, 10)
	wantStatus(t, err, proto.StatusForbidden)
}
