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

// TestCatalogOnMariaDB runs the catalog on the real store (MHK_TEST_DSN) and lists the results with the admin CLI.
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
	ids := make([]uint64, 4)
	names := make([]string, 4)
	for i := range ids {
		a := &store.Account{Username: "ct" + suffix + string(rune('a'+i)), PasswordHash: "x", CreatedAt: store.Now()}
		if err := st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		ids[i], names[i] = a.ID, a.Username
	}

	commit := "it-" + suffix // a fresh pack version for this run
	good := proto.PackManifest{PackID: "mhk2", Commit: commit, SimVersion: sim,
		Items: []proto.ManifestItem{it(proto.ContentTrack, "mhk2.Rome", "f"), it(proto.ContentKart, "mhk2.kart", "")}}
	good.ManifestHash, _ = ManifestHash(good.Items)
	bad := good
	bad.Items = append([]proto.ManifestItem{it(proto.ContentKart, "mhk2.turbo", "")}, good.Items...)
	bad.ManifestHash, _ = ManifestHash(bad.Items)

	c := New(st, nil)
	for _, id := range ids[:3] {
		announce(t, c, id, good)
	}
	res := announce(t, c, ids[3], bad)
	if len(res.Strikes) != 1 || !c.Differs(ids[3], "MHK2") {
		t.Fatalf("no strike on MariaDB: %+v", res)
	}
	announce(t, c, ids[3], bad)

	// The stored manifest keeps the ids as reported; a fresh catalog reads votes and manifests back.
	m, err := st.ManifestByHash(ctx, store.PackVersion{PackID: "MHK2", Commit: commit, SimVersion: sim}, good.ManifestHash)
	if err != nil || len(m.Items) != 2 || m.Items[0].ID != "mhk2.Rome" {
		t.Fatalf("stored manifest %+v %v", m, err)
	}
	c2 := New(st, nil)
	announce(t, c2, ids[3], bad)
	if !c2.Differs(ids[3], "mhk2") || c2.Has(ids[3], "mhk2", proto.ContentKart, "mhk2.turbo") ||
		!c2.Has(ids[3], "mhk2", proto.ContentTrack, "MHK2.ROME") {
		t.Fatal("reloaded catalog disagrees")
	}
	c2.Strike(ids[3], "mhk2", "race content desync")

	strikes, err := st.ContentStrikes(ctx, ids[3], 0)
	if err != nil || len(strikes) != 2 {
		t.Fatalf("strikes %+v %v", strikes, err)
	}
	if s := strikes[1]; s.ManifestHash != bad.ManifestHash || s.MajorityHash != good.ManifestHash ||
		!strings.EqualFold(s.Commit, commit) || s.Username != names[3] {
		t.Fatalf("hello strike %+v", s)
	}
	if strikes[0].Reason != "race content desync" {
		t.Fatalf("race strike %+v", strikes[0])
	}

	if err := st.AddCheatKick(ctx, &store.CheatKick{AccountID: ids[3], LobbyRef: "lobby 1", RaceRef: "race 2",
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
	out := run("content", "strikes", "--account", names[3])
	if !strings.Contains(out, "2 strike(s)") || !strings.Contains(out, bad.ManifestHash) ||
		!strings.Contains(out, "race content desync") {
		t.Fatalf("content strikes:\n%s", out)
	}
	if out := run("content", "strikes", "--account", names[0]); !strings.Contains(out, "0 strike(s)") {
		t.Fatalf("strikes of a good account:\n%s", out)
	}
	out = run("kicks", "--account", names[3])
	if !strings.Contains(out, "1 kick(s)") || !strings.Contains(out, "speed") || !strings.Contains(out, "race 2") {
		t.Fatalf("kicks:\n%s", out)
	}
	if out := run("kicks", "--limit", "1"); !strings.Contains(out, "1 kick(s)") {
		t.Fatalf("kicks --limit:\n%s", out)
	}
}
