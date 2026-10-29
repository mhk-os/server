package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/auth"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
)

func TestChangeUsername(t *testing.T) {
	ts := start(t, server.Config{}, generous)
	ctx := ctxT(t)
	_, err := connect(t, ts).ChangeUsername(ctx, uniqueName("nobody"))
	wantStatus(t, err, proto.StatusNotLoggedIn)

	p := newPlayer(t, ts, "ren_")
	other := connect(t, ts)
	login, err := other.Login(ctx, p.name, password)
	if err != nil {
		t.Fatal(err)
	}
	track, kart := uniqueID("trk"), uniqueID("kart")
	r := upload(t, p, run(track, kart, 30, 30, 30))

	for _, bad := range []string{"ab", "_abc", "abc.", "has space", "Admin", "mhk-os", "anon-1234", "ANON-x",
		strings.Repeat("x", 21), "ünï"} {
		_, err := p.c.ChangeUsername(ctx, bad)
		wantStatus(t, err, proto.StatusInvalidUsername)
	}
	taken := register(t, ts, uniqueName("renT_"))
	_, err = p.c.ChangeUsername(ctx, strings.ToUpper(taken.name))
	wantStatus(t, err, proto.StatusUsernameTaken)

	// A valid name: the response, the account's other connection (push) and every list show it.
	renamed := uniqueName("renN_")
	a, err := p.c.ChangeUsername(ctx, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != p.acct.ID || a.Username != renamed {
		t.Fatalf("renamed account %+v", a)
	}
	pushed, err := other.WaitAccountUpdated(ctx)
	if err != nil || pushed.ID != p.acct.ID || pushed.Username != renamed {
		t.Fatalf("AccountUpdated push %+v %v", pushed, err)
	}

	// A case-only change of one's own name is allowed; the same name again changes nothing.
	upper := strings.ToUpper(renamed)
	if a, err = p.c.ChangeUsername(ctx, upper); err != nil || a.Username != upper {
		t.Fatalf("case-only change %+v %v", a, err)
	}
	if a, err = p.c.ChangeUsername(ctx, upper); err != nil || a.Username != upper {
		t.Fatalf("same name %+v %v", a, err)
	}

	viewer := connect(t, ts)
	prof, err := viewer.GetProfile(ctx, p.acct.ID)
	if err != nil || prof.Player.Username != upper {
		t.Fatalf("profile %+v %v", prof, err)
	}
	page, err := viewer.ListPlayers(ctx, &proto.ListPlayers{Limit: 10, Search: strings.ToLower(renamed)})
	if err != nil || page.Total != 1 || page.Players[0].Username != upper {
		t.Fatalf("players list %+v %v", page, err)
	}
	if page, err = viewer.ListPlayers(ctx, &proto.ListPlayers{Limit: 10, Search: p.name}); err != nil ||
		page.Total != 0 {
		t.Fatalf("old name still listed %+v %v", page, err)
	}
	// Runs belong to the account: the board lists the existing run under the new name.
	wantNames(t, board(t, p, r.Run.BoardID, proto.LeaderboardFilter{}), upper)

	// The old name no longer logs in, the new one does in any case; a resumed session has the new name.
	_, err = connect(t, ts).Login(ctx, p.name, password)
	wantStatus(t, err, proto.StatusInvalidCredentials)
	if s, err := connect(t, ts).Login(ctx, renamed, password); err != nil || s.Account.Username != upper {
		t.Fatalf("login with the new name %+v %v", s, err)
	}
	if s, err := connect(t, ts).Resume(ctx, login.Token); err != nil || s.Account.Username != upper {
		t.Fatalf("resume %+v %v", s, err)
	}

	// The old name is free again.
	register(t, ts, p.name)

	// The audit log has old and new name of both changes.
	log, err := ts.st.AuditLog(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, act := range log {
		if act.Action == store.ActionRenameAccount && act.TargetAccount != nil && *act.TargetAccount == p.acct.ID {
			if act.Actor == nil || *act.Actor != p.acct.ID {
				t.Fatalf("rename audit actor %v", act.Actor)
			}
			reasons = append(reasons, act.TargetRef+": "+act.Reason)
		}
	}
	want := []string{
		upper + ": username changed by the player from " + renamed + " to " + upper,
		renamed + ": username changed by the player from " + p.name + " to " + renamed,
	}
	if strings.Join(reasons, "|") != strings.Join(want, "|") {
		t.Fatalf("audit %q, want %q", reasons, want)
	}
}

func TestChangeUsernameRateLimit(t *testing.T) {
	limits := generous
	limits.Rename = auth.Rate{N: 2, Per: time.Hour}
	ts := start(t, server.Config{}, limits)
	ctx := ctxT(t)
	p := register(t, ts, uniqueName("rl_"))
	first, second := uniqueName("rlA_"), uniqueName("rlB_")
	for _, name := range []string{first, second} {
		if _, err := p.c.ChangeUsername(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	_, err := p.c.ChangeUsername(ctx, uniqueName("rlC_"))
	wantStatus(t, err, proto.StatusRateLimited)
	// Case-only changes count too; keeping the name is no change.
	_, err = p.c.ChangeUsername(ctx, strings.ToUpper(second))
	wantStatus(t, err, proto.StatusRateLimited)
	if a, err := p.c.ChangeUsername(ctx, second); err != nil || a.Username != second {
		t.Fatalf("same name while limited %+v %v", a, err)
	}

	// The count lives in the audit log, so a restarted server still refuses.
	ts2 := start(t, server.Config{}, limits)
	c := connect(t, ts2)
	if _, err := c.Login(ctx, second, password); err != nil {
		t.Fatal(err)
	}
	_, err = c.ChangeUsername(ctx, uniqueName("rlD_"))
	wantStatus(t, err, proto.StatusRateLimited)
}
