package content

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/admin"
	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/store"
)

// TestCatalogOnMariaDB runs the catalog on the real store (MHK_TEST_DSN) and lists cheat kicks and reports with the
// admin CLI.
func TestCatalogOnMariaDB(t *testing.T) {
	dsn := os.Getenv("MHK_TEST_DSN")
	if dsn == "" {
		t.Skip("MHK_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var rb [5]byte
	rand.Read(rb[:])
	suffix := hex.EncodeToString(rb[:])
	ids := make([]uint64, 2)
	names := make([]string, 2)
	for i := range ids {
		a := &store.Account{Username: "ct" + suffix + string(rune('a'+i)), PasswordHash: "x", CreatedAt: store.Now()}
		if err := st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		ids[i], names[i] = a.ID, a.Username
	}

	commit := "it-" + suffix // a fresh pack version for this run
	first := manifest("mhk2", it(proto.ContentTrack, "mhk2.Rome", 3), it(proto.ContentKart, "mhk2.kart", 1))
	first.Commit = commit
	second := manifest("mhk2", it(proto.ContentKart, "mhk2.turbo", 1))
	second.Commit = commit

	c := New(st, nil)
	if res := announce(t, c, ids[0], first); len(res.Missing) != 1 {
		t.Fatalf("first announce %+v", res)
	}
	// The stored manifest keeps the ids as reported; storing another one for the version changes nothing.
	if err := st.PutManifest(ctx, &store.Manifest{PackVersion: store.PackVersion{PackID: "mhk2", Commit: commit,
		RulesVersion: rulesVersion}, Items: second.Items}, store.Now()); err != nil {
		t.Fatal(err)
	}
	m, err := st.Manifest(ctx, store.PackVersion{PackID: "MHK2", Commit: strings.ToUpper(commit),
		RulesVersion: rulesVersion})
	if err != nil || len(m.Items) != 2 || m.Items[0] != it(proto.ContentTrack, "mhk2.Rome", 3) {
		t.Fatalf("stored manifest %+v %v", m, err)
	}
	if _, err := st.Manifest(ctx, store.PackVersion{PackID: "mhk2", Commit: commit,
		RulesVersion: rulesVersion + 1}); err != store.ErrNotFound {
		t.Fatalf("other rules version: %v", err)
	}
	if latest, err := st.LatestManifest(ctx, "mhk2", rulesVersion); err != nil || latest.RulesVersion != rulesVersion {
		t.Fatalf("latest manifest %+v %v", latest, err)
	}
	// A fresh catalog reads it back and asks for nothing.
	c2 := New(st, nil)
	if res := announce(t, c2, ids[1], second); len(res.Missing) != 0 {
		t.Fatalf("reloaded catalog asked for %v", res.Missing)
	}
	if c2.Has(ids[1], "mhk2", proto.ContentKart, "mhk2.turbo") || !c2.Has(ids[1], "mhk2", proto.ContentTrack, "MHK2.ROME") {
		t.Fatal("reloaded catalog disagrees")
	}

	if err := st.AddCheatKick(ctx, &store.CheatKick{AccountID: ids[1], LobbyRef: "lobby 1", RaceRef: "race 2",
		Reason: "speed", Reporters: names[0], CreatedAt: store.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		var out bytes.Buffer
		if err := admin.Run(ctx, &admin.Env{Store: st, Out: &out}, args); err != nil {
			t.Fatalf("admin %v: %v", args, err)
		}
		return out.String()
	}
	out := run("kicks", "--account", names[1])
	if !strings.Contains(out, "1 kick(s)") || !strings.Contains(out, "speed") || !strings.Contains(out, "race 2") {
		t.Fatalf("kicks:\n%s", out)
	}
	if out := run("kicks", "--limit", "1"); !strings.Contains(out, "1 kick(s)") {
		t.Fatalf("kicks --limit:\n%s", out)
	}

	if err := st.AddCheatReport(ctx, &store.CheatReport{LobbyRef: "lobby 1", RaceRef: "race 3", ReporterID: ids[0],
		SuspectID: ids[1], Reason: "Teleport", Detail: "jumped 40 m", Note: "suspect ping unknown or over 200 ms",
		CreatedAt: store.Now()}); err != nil {
		t.Fatal(err)
	}
	out = run("cheat-reports", "--account", names[1])
	if !strings.Contains(out, "1 report(s)") || !strings.Contains(out, names[0]) ||
		!strings.Contains(out, "no: suspect ping") || !strings.Contains(out, "jumped 40 m") {
		t.Fatalf("cheat-reports:\n%s", out)
	}
}
