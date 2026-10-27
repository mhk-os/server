package server_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
	"github.com/mhk-os/server/internal/testclient"
)

func syncAch(t *testing.T, c *testclient.Client, req *proto.SyncAchievements) *proto.AchievementState {
	t.Helper()
	st, err := c.SyncAchievements(ctxT(t), req)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSyncAchievementsMerge(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	p := register(t, ts, uniqueName("ach_"))
	other := connect(t, ts)
	if _, err := other.Login(ctx, p.name, password); err != nil {
		t.Fatal(err)
	}
	_, err := connect(t, ts).SyncAchievements(ctx, &proto.SyncAchievements{})
	wantStatus(t, err, proto.StatusNotLoggedIn)

	t1 := time.UnixMilli(1_700_000_000_000).UTC()
	t2 := t1.Add(24 * time.Hour)
	syncAch(t, p.c, &proto.SyncAchievements{
		Counters: []proto.StatValue{{Stat: "races.finished", Value: 5}, {Stat: "distance.m", Value: 3}},
		Maxima:   []proto.StatValue{{Stat: "use.mhk2.shrink.hits", Value: 4}},
		Sets:     []proto.StatSet{{Stat: "tracks.finished", Items: []string{"mhk2.farm", "mhk2.beach"}}},
		Unlocked: []proto.Unlock{{ID: "game.first_race", At: t1}, {ID: "game.top3_10", At: t2}},
	})
	got := syncAch(t, other, &proto.SyncAchievements{
		Counters: []proto.StatValue{{Stat: "races.finished", Value: 2}, {Stat: "races.finished", Value: 1},
			{Stat: "races.won", Value: 0}},
		Maxima: []proto.StatValue{{Stat: "use.mhk2.shrink.hits", Value: 2}, {Stat: "use.mhk2.hammer.hits", Value: 7}},
		Sets: []proto.StatSet{{Stat: "tracks.finished", Items: []string{"mhk2.beach", "mhk3.city", "mhk3.city"}},
			{Stat: "cups.perfect", Items: []string{"mhk2.cup1"}}},
		Unlocked: []proto.Unlock{{ID: "game.first_race", At: t1.Add(-time.Hour)}, {ID: "game.top3_10", At: t2.Add(time.Hour)},
			{ID: "game.future", At: time.Now().Add(48 * time.Hour)}},
	})
	if len(got.Unlocked) != 3 || got.Unlocked[0].ID != "game.first_race" || got.Unlocked[1].ID != "game.future" ||
		time.Since(got.Unlocked[1].At) > time.Minute || got.Unlocked[1].At.After(time.Now()) {
		t.Fatalf("unlocked %+v", got.Unlocked)
	}
	future := got.Unlocked[1].At
	want := &proto.AchievementState{
		Counters: []proto.StatTotal{{Stat: "distance.m", Value: 3}, {Stat: "races.finished", Value: 8}},
		Maxima:   []proto.StatValue{{Stat: "use.mhk2.hammer.hits", Value: 7}, {Stat: "use.mhk2.shrink.hits", Value: 4}},
		Sets: []proto.StatSet{{Stat: "cups.perfect", Items: []string{"mhk2.cup1"}},
			{Stat: "tracks.finished", Items: []string{"mhk2.beach", "mhk2.farm", "mhk3.city"}}},
		Unlocked: []proto.Unlock{{ID: "game.first_race", At: t1.Add(-time.Hour)}, {ID: "game.future", At: future},
			{ID: "game.top3_10", At: t2}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged\n%+v\nwant\n%+v", got, want)
	}
	if fetched := syncAch(t, p.c, &proto.SyncAchievements{}); !reflect.DeepEqual(fetched, want) {
		t.Fatalf("fetched %+v", fetched)
	}

	// A refused sync stores nothing, not even its valid parts.
	for name, req := range map[string]*proto.SyncAchievements{
		"bad stat id": {Counters: []proto.StatValue{{Stat: "races.finished", Value: 1}, {Stat: "bad stat", Value: 1}}},
		"bad item": {Counters: []proto.StatValue{{Stat: "races.finished", Value: 1}},
			Sets: []proto.StatSet{{Stat: "tracks.finished", Items: []string{"mhk2/farm"}}}},
		"empty id":         {Unlocked: []proto.Unlock{{ID: "", At: t1}}},
		"65-byte stat":     {Maxima: []proto.StatValue{{Stat: strings.Repeat("s", 65), Value: 1}}},
		"unlock time zero": {Unlocked: []proto.Unlock{{ID: "game.zero", At: time.UnixMilli(0)}}},
	} {
		_, err := p.c.SyncAchievements(ctx, req)
		if testclient.StatusOf(err) != proto.StatusBadRequest {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if fetched := syncAch(t, p.c, &proto.SyncAchievements{}); !reflect.DeepEqual(fetched, want) {
		t.Fatalf("after refused syncs %+v", fetched)
	}

	out := runAdmin(t, ts, "account", "show", p.name)
	if !strings.Contains(out, "3 unlocked (6 stats, 4 set items") {
		t.Fatalf("account show:\n%s", out)
	}
}

func TestAchievementCaps(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	stats := func(prefix string, n int) []proto.StatValue {
		out := make([]proto.StatValue, n)
		for i := range out {
			out[i] = proto.StatValue{Stat: fmt.Sprintf("%s%d", prefix, i), Value: 1}
		}
		return out
	}
	items := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%d", prefix, i)
		}
		return out
	}
	set := func(stat string, n int) proto.StatSet { return proto.StatSet{Stat: stat, Items: items(stat+".", n)} }

	a := register(t, ts, uniqueName("capS_"))
	_, err := a.c.SyncAchievements(ctx, &proto.SyncAchievements{Counters: stats("c", 256), Maxima: stats("m", 245)})
	wantStatus(t, err, proto.StatusBadRequest)
	if st := syncAch(t, a.c, &proto.SyncAchievements{Counters: stats("c", 256), Maxima: stats("m", 244)}); len(st.Counters) != 256 ||
		len(st.Maxima) != 244 {
		t.Fatalf("%d counters, %d maxima", len(st.Counters), len(st.Maxima))
	}
	_, err = a.c.SyncAchievements(ctx, &proto.SyncAchievements{Sets: []proto.StatSet{{Stat: "s", Items: []string{"x"}}}})
	wantStatus(t, err, proto.StatusBadRequest)
	// Existing stats keep syncing at the cap.
	if st := syncAch(t, a.c, &proto.SyncAchievements{Counters: stats("c", 1)}); st.Counters[0].Value != 2 {
		t.Fatalf("counter %+v", st.Counters[0])
	}

	b := register(t, ts, uniqueName("capI_"))
	_, err = b.c.SyncAchievements(ctx, &proto.SyncAchievements{Sets: []proto.StatSet{set("a", 2000), set("b", 2000),
		set("c", 1001)}})
	wantStatus(t, err, proto.StatusBadRequest)
	syncAch(t, b.c, &proto.SyncAchievements{Sets: []proto.StatSet{set("a", 2000), set("b", 2000)}})
	if st := syncAch(t, b.c, &proto.SyncAchievements{Sets: []proto.StatSet{set("a", 2000), set("c", 1000)}}); len(st.Sets) != 3 ||
		len(st.Sets[2].Items) != 1000 {
		t.Fatalf("sets %d", len(st.Sets))
	}
	_, err = b.c.SyncAchievements(ctx, &proto.SyncAchievements{Sets: []proto.StatSet{{Stat: "a", Items: []string{"new"}}}})
	wantStatus(t, err, proto.StatusBadRequest)

	c := register(t, ts, uniqueName("capU_"))
	unlocks := func(n int) []proto.Unlock {
		out := make([]proto.Unlock, n)
		for i, id := range items("u", n) {
			out[i] = proto.Unlock{ID: id, At: time.UnixMilli(1_700_000_000_000)}
		}
		return out
	}
	syncAch(t, c.c, &proto.SyncAchievements{Unlocked: unlocks(2000)})
	_, err = c.c.SyncAchievements(ctx, &proto.SyncAchievements{Unlocked: []proto.Unlock{{ID: "more", At: time.Now()}}})
	wantStatus(t, err, proto.StatusBadRequest)
	if st := syncAch(t, c.c, &proto.SyncAchievements{Unlocked: unlocks(10)}); len(st.Unlocked) != 2000 {
		t.Fatalf("%d unlocked", len(st.Unlocked))
	}
}

func TestAchievementShares(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	x, y, z := uniqueName("ach.x"), uniqueName("ach.y"), uniqueName("ach.z")
	before, err := connect(t, ts).AchievementShares(ctx, x)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := register(t, ts, uniqueName("shA_")), register(t, ts, uniqueName("shB_")),
		register(t, ts, uniqueName("shC_"))
	register(t, ts, uniqueName("shD_")) // never syncs: not a player of achievements
	now := time.Now()
	syncAch(t, a.c, &proto.SyncAchievements{Unlocked: []proto.Unlock{{ID: x, At: now}, {ID: y, At: now}}})
	syncAch(t, b.c, &proto.SyncAchievements{Unlocked: []proto.Unlock{{ID: x, At: now}}})
	syncAch(t, c.c, &proto.SyncAchievements{})

	shares := func(ids ...string) *proto.AchievementShares {
		t.Helper()
		s, err := connect(t, ts).AchievementShares(ctx, ids...)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	got := shares(y, x, z, x)
	want := []proto.AchievementShare{{ID: y, Unlocked: 1}, {ID: x, Unlocked: 2}, {ID: z, Unlocked: 0}}
	if got.Players != before.Players+3 || !reflect.DeepEqual(got.Shares, want) {
		t.Fatalf("shares %d %+v, want %d %+v", got.Players, got.Shares, before.Players+3, want)
	}

	runAdmin(t, ts, "account", "disable", b.name, "--reason", "test")
	got = shares(x)
	if got.Players != before.Players+2 || got.Shares[0].Unlocked != 1 {
		t.Fatalf("after disabling: %d %+v", got.Players, got.Shares)
	}

	all := shares()
	if len(all.Shares) == 0 || len(all.Shares) > proto.MaxShareIDs ||
		!slices.IsSortedFunc(all.Shares, func(p, q proto.AchievementShare) int { return int(q.Unlocked) - int(p.Unlocked) }) {
		t.Fatalf("all shares: %d, sorted %v", len(all.Shares), all.Shares[:min(len(all.Shares), 5)])
	}
}

// anonymousOwner returns the account now owning runID and checks it is a fresh anonymous account.
func anonymousOwner(t *testing.T, ts *testServer, runID, formerOwner uint64) *store.Account {
	t.Helper()
	ctx := ctxT(t)
	v, err := ts.st.RunByID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ts.st.AccountByID(ctx, v.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == formerOwner || a.Role != proto.RoleAnonymous || !strings.HasPrefix(a.Username, "anon-") ||
		len(a.Username) != 15 || v.Username != "Anonymous" || v.Country != "" {
		t.Fatalf("run %d belongs to %+v shown as %q/%q", runID, a, v.Username, v.Country)
	}
	return a
}

func TestDeleteAccountAnonymizesRuns(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kart := uniqueID("trk"), uniqueID("kart")
	p, q, mod := newPlayer(t, ts, "del_"), newPlayer(t, ts, "delQ_"), newModerator(t, ts)
	if _, err := p.c.SetCountry(ctx, "DE"); err != nil {
		t.Fatal(err)
	}
	fast := upload(t, p, run(track, kart, 30, 30, 30))
	upload(t, p, run(track, kart, 35, 35, 35))
	upload(t, q, run(track, kart, 32, 32, 32))
	boardID := fast.Run.BoardID
	syncAch(t, p.c, &proto.SyncAchievements{Counters: []proto.StatValue{{Stat: "races.finished", Value: 1}},
		Unlocked: []proto.Unlock{{ID: "game.first_race", At: time.Now()}}})

	other := connect(t, ts)
	session, err := other.Login(ctx, p.name, password)
	if err != nil {
		t.Fatal(err)
	}
	err = connect(t, ts).DeleteAccount(ctx, password, false)
	wantStatus(t, err, proto.StatusNotLoggedIn)
	err = p.c.DeleteAccount(ctx, "wrong-password-1", false)
	wantStatus(t, err, proto.StatusInvalidCredentials)
	if _, err := p.c.SetCountry(ctx, "SK"); err != nil {
		t.Fatalf("still logged in after a wrong password: %v", err)
	}

	if err := p.c.DeleteAccount(ctx, password, false); err != nil {
		t.Fatal(err)
	}
	d, err := other.WaitDisconnect(ctx)
	if err != nil || d.Status != proto.StatusSessionInvalid || d.Message != "This account was deleted." {
		t.Fatalf("other connection: %+v %v", d, err)
	}
	_, err = p.c.SetCountry(ctx, "DE")
	wantStatus(t, err, proto.StatusNotLoggedIn)
	if _, err := p.c.Ping(ctx); err != nil {
		t.Fatalf("connection should stay open: %v", err)
	}

	anon := anonymousOwner(t, ts, fast.Run.RunID, p.acct.ID)
	lb := board(t, q, boardID, proto.LeaderboardFilter{})
	wantNames(t, lb, "Anonymous", q.name)
	if e := lb.Entries[0]; e.RunID != fast.Run.RunID || e.AccountID != anon.ID || e.Country != "" {
		t.Fatalf("anonymous row %+v", e)
	}
	wantNames(t, board(t, q, boardID, proto.LeaderboardFilter{Username: "anon"}), "Anonymous")
	wantNames(t, board(t, q, boardID, proto.LeaderboardFilter{Username: anon.Username}))
	wantNames(t, board(t, q, boardID, proto.LeaderboardFilter{Country: "DE"}))

	_, err = q.c.GetProfile(ctx, anon.ID)
	wantStatus(t, err, proto.StatusNotFound)
	_, err = mod.c.GetProfile(ctx, anon.ID)
	wantStatus(t, err, proto.StatusNotFound)
	if got, err := q.c.GetProfiles(ctx, anon.ID, q.acct.ID); err != nil || len(got) != 1 || got[0].AccountID != q.acct.ID {
		t.Fatalf("GetProfiles %+v %v", got, err)
	}
	_, err = q.c.PlayerRuns(ctx, anon.ID, 0, 10)
	wantStatus(t, err, proto.StatusNotFound)
	if page, err := mod.c.PlayerRuns(ctx, anon.ID, 0, 10); err != nil || page.Total != 2 {
		t.Fatalf("moderator PlayerRuns %+v %v", page, err)
	}
	if page, err := q.c.ListPlayers(ctx, &proto.ListPlayers{Limit: 10, Search: anon.Username}); err != nil ||
		page.Total != 0 {
		t.Fatalf("ListPlayers %+v %v", page, err)
	}
	_, err = mod.c.RunHiddenReason(ctx, fast.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.c.RunHiddenReason(ctx, fast.Run.RunID)
	wantStatus(t, err, proto.StatusNotFound)

	// Everything else of the account is gone; the name is free and nobody can log in as the anonymous account.
	var left int
	if err := ts.st.DB().QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM accounts WHERE id = ?) +
		(SELECT COUNT(*) FROM sessions WHERE account_id = ?) +
		(SELECT COUNT(*) FROM achievement_counters WHERE account_id = ?) +
		(SELECT COUNT(*) FROM achievement_unlocks WHERE account_id = ?)`,
		p.acct.ID, p.acct.ID, p.acct.ID, p.acct.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d rows left (%v)", left, err)
	}
	_, err = connect(t, ts).Resume(ctx, session.Token)
	wantStatus(t, err, proto.StatusSessionInvalid)
	_, err = connect(t, ts).Login(ctx, p.name, password)
	wantStatus(t, err, proto.StatusInvalidCredentials)
	_, err = connect(t, ts).Login(ctx, anon.Username, "")
	wantStatus(t, err, proto.StatusInvalidCredentials)
	for _, name := range []string{"Anonymous", "anon-1234", "ANON-x"} {
		_, err = connect(t, ts).Register(ctx, name, password)
		wantStatus(t, err, proto.StatusInvalidUsername)
	}
	again := register(t, ts, p.name)
	if again.acct.ID == p.acct.ID {
		t.Fatal("same id after re-registering")
	}

	log, err := ts.st.AuditLog(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(log, func(a store.Action) bool {
		return a.Action == store.ActionDeleteAccount && a.TargetRef == p.name
	})
	if i < 0 || !strings.HasPrefix(log[i].Reason, "account deleted by the player; 2 run(s) moved to "+anon.Username) ||
		*log[i].TargetAccount != p.acct.ID {
		t.Fatalf("audit entry missing: %+v", log)
	}
}

func TestDeleteAccountDeletingRuns(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kart := uniqueID("trk"), uniqueID("kart")
	p, q, mod := newPlayer(t, ts, "delR_"), newPlayer(t, ts, "delRQ_"), newModerator(t, ts)
	mine := upload(t, p, run(track, kart, 30, 30, 30))
	upload(t, q, run(track, kart, 32, 32, 32))
	boardID := mine.Run.BoardID
	if err := p.c.DeleteAccount(ctx, password, true); err != nil {
		t.Fatal(err)
	}
	anonymousOwner(t, ts, mine.Run.RunID, p.acct.ID)
	wantNames(t, board(t, q, boardID, proto.LeaderboardFilter{}), q.name)
	_, err := q.c.DownloadRun(ctx, mine.Run.RunID)
	wantStatus(t, err, proto.StatusNotFound)

	lb := board(t, mod, boardID, proto.LeaderboardFilter{IncludeHidden: true})
	if len(lb.Entries) != 2 || lb.Entries[0].Username != "Anonymous" || !lb.Entries[0].Hidden {
		t.Fatalf("moderator board %+v", lb.Entries)
	}
	info, err := mod.c.RunHiddenReason(ctx, mine.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if info.HiddenAt.IsZero() || info.Moderator != "" || info.Reason != "Deleted by the player" ||
		!info.DeletedByOwner || info.AccountDisabled {
		t.Fatalf("hidden info %+v", info)
	}
	if err := mod.c.ModUnhideRun(ctx, mine.Run.RunID); err != nil {
		t.Fatal(err)
	}
	wantNames(t, board(t, q, boardID, proto.LeaderboardFilter{}), "Anonymous", q.name)
	if info, err := mod.c.RunHiddenReason(ctx, mine.Run.RunID); err != nil || !info.HiddenAt.IsZero() ||
		info.DeletedByOwner {
		t.Fatalf("after unhide %+v %v", info, err)
	}

	// Without runs no anonymous account is created.
	var anons int
	count := func() int {
		t.Helper()
		if err := ts.st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE role = 2").Scan(&anons); err != nil {
			t.Fatal(err)
		}
		return anons
	}
	n := count()
	if err := register(t, ts, uniqueName("delN_")).c.DeleteAccount(ctx, password, true); err != nil {
		t.Fatal(err)
	}
	if count() != n {
		t.Fatal("anonymous account created for an account without runs")
	}
}

func TestRunHiddenReason(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	track, kart := uniqueID("trk"), uniqueID("kart")
	owner, other, mod := newPlayer(t, ts, "hid_"), newPlayer(t, ts, "hidO_"), newModerator(t, ts)
	runID := upload(t, owner, run(track, kart, 30, 30, 30)).Run.RunID

	_, err := connect(t, ts).RunHiddenReason(ctx, runID)
	wantStatus(t, err, proto.StatusNotLoggedIn)
	if info, err := owner.c.RunHiddenReason(ctx, runID); err != nil || *info != (proto.RunHiddenInfo{}) {
		t.Fatalf("visible run %+v %v", info, err)
	}
	_, err = other.c.RunHiddenReason(ctx, runID)
	wantStatus(t, err, proto.StatusNotFound)
	_, err = mod.c.RunHiddenReason(ctx, 1<<62)
	wantStatus(t, err, proto.StatusNotFound)

	reason := "Drives through the wall at the bridge"
	if err := mod.c.ModHideRun(ctx, runID, reason); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*player{owner, mod} {
		info, err := p.c.RunHiddenReason(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(info.HiddenAt) > time.Minute || info.Moderator != mod.name || info.Reason != reason ||
			info.DeletedByOwner || info.AccountDisabled {
			t.Fatalf("%s sees %+v", p.name, info)
		}
	}
	_, err = other.c.RunHiddenReason(ctx, runID)
	wantStatus(t, err, proto.StatusNotFound)

	if err := mod.c.ModUnhideRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := mod.c.ModDisableAccount(ctx, owner.acct.ID, "cheating"); err != nil {
		t.Fatal(err)
	}
	info, err := mod.c.RunHiddenReason(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if info.HiddenAt.IsZero() || info.Moderator != "" || info.Reason != "cheating" || info.DeletedByOwner ||
		!info.AccountDisabled {
		t.Fatalf("disabled owner %+v", info)
	}
}
