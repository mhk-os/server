package content

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
)

var _ race.Catalog = (*Catalog)(nil)

func it(kind uint8, id string, version int32) proto.ManifestItem {
	return proto.ManifestItem{Kind: kind, ID: id, Version: version}
}

func items(list ...proto.ManifestItem) []proto.ManifestItem { return list }

func TestValidateItems(t *testing.T) {
	for name, list := range map[string][]proto.ManifestItem{
		"kind 0":               items(it(0, "p.a", 0)),
		"empty id":             items(it(proto.ContentCharacter, "", 0)),
		"space in id":          items(it(proto.ContentCharacter, "p.a b", 0)),
		"umlaut in id":         items(it(proto.ContentCharacter, "p.hühn", 0)),
		"97-byte id":           items(it(proto.ContentCharacter, strings.Repeat("a", 97), 0)),
		"duplicate":            items(it(proto.ContentTrack, "p.a", 1), it(proto.ContentTrack, "p.a", 2)),
		"duplicate by case":    items(it(proto.ContentKart, "p.Kart", 1), it(proto.ContentKart, "p.kart", 1)),
		"track version 0":      items(it(proto.ContentTrack, "p.a", 0)),
		"kart version 0":       items(it(proto.ContentKart, "p.a", 0)),
		"arena version 0":      items(it(proto.ContentArena, "p.a", 0)),
		"negative cup version": items(it(proto.ContentCup, "p.a", -1)),
	} {
		if err := ValidateItems(list); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	ok := items(it(proto.ContentTrack, "p.a", 1), it(proto.ContentArena, "p.a", 3), it(proto.ContentCharacter, "p.c", 0),
		it(proto.ContentPowerup, "p.egg", 0), it(proto.ContentCup, "p.cup", 2))
	if err := ValidateItems(ok); err != nil {
		t.Errorf("valid items: %v", err)
	}
}

func TestPackOf(t *testing.T) {
	for id, want := range map[string]string{"mhk2.kart.speedy": "mhk2", "game.huhn": "game", "nodot": "", ".x": ""} {
		if got := PackOf(id); got != want {
			t.Errorf("PackOf(%q) = %q, want %q", id, got, want)
		}
	}
}

// memStore is an in-memory Store.
type memStore struct {
	mu        sync.Mutex
	manifests map[string]*store.Manifest
	reads     int
}

func newMemStore() *memStore { return &memStore{manifests: map[string]*store.Manifest{}} }

func vkey(v store.PackVersion) string {
	return strings.ToLower(v.PackID) + "\x00" + strings.ToLower(v.Commit) + "\x00" + string(rune(v.RulesVersion))
}

func (s *memStore) PutManifest(_ context.Context, m *store.Manifest, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifests[vkey(m.PackVersion)] == nil {
		s.manifests[vkey(m.PackVersion)] = m
	}
	return nil
}

func (s *memStore) Manifest(_ context.Context, v store.PackVersion) (*store.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if m := s.manifests[vkey(v)]; m != nil {
		return m, nil
	}
	return nil, store.ErrNotFound
}

const rulesVersion = 7

// manifest builds a manifest of packID at commit "v1".
func manifest(packID string, list ...proto.ManifestItem) proto.PackManifest {
	return proto.PackManifest{PackID: packID, Commit: "v1", RulesVersion: rulesVersion, Items: list}
}

// pack builds a manifest of pack "mhk2" whose items are the given track ids (kind 1, version 1).
func pack(tracks ...string) proto.PackManifest {
	m := manifest("mhk2")
	for _, t := range tracks {
		m.Items = append(m.Items, it(proto.ContentTrack, t, 1))
	}
	return m
}

func ref(m proto.PackManifest) proto.PackRef {
	return proto.PackRef{PackID: m.PackID, Commit: m.Commit}
}

// announce runs the full ContentHello (+ manifests when asked) for account id on connection key id.
func announce(t *testing.T, c *Catalog, id uint64, ms ...proto.PackManifest) *proto.ContentHelloResult {
	t.Helper()
	ctx := context.Background()
	refs := make([]proto.PackRef, len(ms))
	for i, m := range ms {
		refs[i] = ref(m)
	}
	res, err := c.Hello(ctx, id, id, rulesVersion, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) > 0 {
		if c.Ready(id, id) {
			t.Fatal("ready with missing manifests")
		}
		if err := c.Manifests(ctx, id, id, ms); err != nil {
			t.Fatal(err)
		}
	}
	if !c.Ready(id, id) {
		t.Fatal("not ready after manifests")
	}
	return res
}

func TestFirstManifestIsKept(t *testing.T) {
	st := newMemStore()
	c := New(st, nil)
	first, second := pack("mhk2.rome", "mhk2.alps"), pack("mhk2.rome", "mhk2.alps", "mhk2.fake")

	res := announce(t, c, 1, first)
	if !slices.Equal(res.Missing, []string{"mhk2"}) {
		t.Fatalf("first announce should ask for the manifest: %+v", res)
	}
	// The same pack version again (any case): known, never asked for again; the stored manifest decides.
	upper := second
	upper.PackID, upper.Commit = "MHK2", "V1"
	if res := announce(t, c, 2, upper); len(res.Missing) != 0 {
		t.Fatalf("known manifest requested again: %+v", res)
	}
	if c.Has(2, "mhk2", proto.ContentTrack, "mhk2.fake") || !c.Has(2, "MHK2", proto.ContentTrack, "MHK2.Rome") {
		t.Fatal("the first stored manifest does not decide")
	}
	if got := c.Items(1, "mhk2", proto.ContentTrack); !slices.Equal(got, []string{"mhk2.rome", "mhk2.alps"}) {
		t.Fatalf("Items %v", got)
	}
	// Another commit or rules version is another manifest.
	next := pack("mhk2.paris")
	next.Commit = "v2"
	if res := announce(t, c, 3, next); len(res.Missing) != 1 || !c.Has(3, "mhk2", proto.ContentTrack, "mhk2.paris") {
		t.Fatalf("new commit: %+v", res)
	}
	// Two connections owing the same manifest: the first one stored wins for both.
	ctx := context.Background()
	race1, race2 := pack("mhk2.a"), pack("mhk2.b")
	race1.Commit, race2.Commit = "v3", "v3"
	for _, id := range []uint64{4, 5} {
		if res, err := c.Hello(ctx, id, id, rulesVersion, []proto.PackRef{ref(race1)}); err != nil ||
			len(res.Missing) != 1 {
			t.Fatal(res, err)
		}
	}
	if err := c.Manifests(ctx, uint64(4), 4, []proto.PackManifest{race1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Manifests(ctx, uint64(5), 5, []proto.PackManifest{race2}); err != nil || !c.Ready(uint64(5), 5) {
		t.Fatal("second manifest of a version refused", err)
	}
	if !c.Has(5, "mhk2", proto.ContentTrack, "mhk2.a") || c.Has(5, "mhk2", proto.ContentTrack, "mhk2.b") {
		t.Fatal("the later manifest replaced the first one")
	}
	// An incomplete announcement is invisible.
	if _, err := c.Hello(ctx, 6, 6, rulesVersion, []proto.PackRef{{PackID: "mhk3", Commit: "x"}}); err != nil {
		t.Fatal(err)
	}
	if c.Ready(6, 6) {
		t.Fatal("ready while a manifest is owed")
	}
	if _, ok := c.Announced(6); ok {
		t.Fatal("incomplete announcement visible")
	}
}

func TestManifestsPersist(t *testing.T) {
	st := newMemStore()
	announce(t, New(st, nil), 1, pack("mhk2.rome"))
	// A restarted server finds the stored manifest and asks for nothing; later lookups are cached.
	c := New(st, nil)
	if res := announce(t, c, 2, pack("mhk2.alps")); len(res.Missing) != 0 {
		t.Fatalf("after restart: %+v", res)
	}
	reads := st.reads
	for range 3 {
		if !c.Has(2, "mhk2", proto.ContentTrack, "mhk2.rome") {
			t.Fatal("stored manifest not used")
		}
	}
	if st.reads != reads {
		t.Fatal("lookups read the store again")
	}
}

func wantStatus(t *testing.T, err error, want proto.Status, msg string) {
	t.Helper()
	var se *server.StatusError
	if !errors.As(err, &se) || se.Status != want || !strings.Contains(se.Message, msg) {
		t.Fatalf("error %v, want status %s with %q", err, want, msg)
	}
}

func TestHelloAndManifestValidation(t *testing.T) {
	ctx := context.Background()
	c := New(newMemStore(), nil)
	m := pack("mhk2.rome")
	for name, refs := range map[string][]proto.PackRef{
		"dotted pack id": {{PackID: "mhk.2"}},
		"empty pack id":  {{PackID: ""}},
		"duplicate pack": {ref(m), {PackID: "MHK2", Commit: "other"}},
		"control commit": {{PackID: "mhk2", Commit: "a\nb"}},
	} {
		_, err := c.Hello(ctx, 1, 1, rulesVersion, refs)
		if err == nil {
			t.Errorf("%s accepted", name)
			continue
		}
		wantStatus(t, err, proto.StatusBadRequest, "")
	}
	wantStatus(t, c.Manifests(ctx, 1, 1, []proto.PackManifest{m}), proto.StatusBadRequest, "ContentHello") // before hello

	res, err := c.Hello(ctx, 1, 1, rulesVersion, []proto.PackRef{ref(m)})
	if err != nil || !slices.Equal(res.Missing, []string{"mhk2"}) {
		t.Fatal(res, err)
	}
	// Invalid items are refused.
	bad := pack("mhk2.rome")
	bad.Items[0].Version = 0
	wantStatus(t, c.Manifests(ctx, 1, 1, []proto.PackManifest{bad}), proto.StatusBadRequest, "version 0")
	// Manifests nobody asked for (another pack, commit or rules version) are ignored: not stored, no error.
	other := pack("mhk2.x")
	other.PackID = "mhk3"
	commit := pack("mhk2.x")
	commit.Commit = "v9"
	rules := pack("mhk2.x")
	rules.RulesVersion = rulesVersion + 1
	if err := c.Manifests(ctx, 1, 1, []proto.PackManifest{other, commit, rules}); err != nil || c.Ready(1, 1) {
		t.Fatal("unrequested manifest changed the state", err)
	}
	if err := c.Manifests(ctx, 1, 1, []proto.PackManifest{m}); err != nil || !c.Ready(1, 1) {
		t.Fatal("manifest not accepted", err)
	}
	// Readiness belongs to the connection and its account.
	if c.Ready(1, 2) || c.Ready(2, 1) {
		t.Fatal("ready for another account or connection")
	}
	c.Forget(1)
	if c.Ready(1, 1) {
		t.Fatal("forgotten connection still ready")
	}
	if _, ok := c.Announced(1); ok {
		t.Fatal("forgotten announcement still visible")
	}
}

func TestOverrides(t *testing.T) {
	c := New(newMemStore(), nil)
	announce(t, c, 1, pack("mhk2.rome"), manifest("tuning", it(proto.ContentKart, "mhk2.speedy", 2),
		it(proto.ContentKart, "tuning.kart", 1)))
	ann, ok := c.Announced(1)
	if !ok {
		t.Fatal("not announced")
	}
	got := c.Overrides(ann)
	if len(got) != 1 || got[0] != (Override{Kind: proto.ContentKart, ID: "mhk2.speedy", Pack: "tuning"}) {
		t.Fatalf("overrides %+v", got)
	}
}

func TestRunContent(t *testing.T) {
	ctx := context.Background()
	c := New(newMemStore(), nil)
	game := manifest("game", it(proto.ContentCharacter, "game.huhn", 0), it(proto.ContentKart, "game.kart", 2))
	mhk2 := manifest("mhk2", it(proto.ContentTrack, "mhk2.rome", 3))
	run := server.RunContent{RulesVersion: rulesVersion, TrackID: "MHK2.Rome", TrackVersion: 3, KartID: "game.kart",
		KartVersion: 2, CharacterID: "game.huhn"}

	wantStatus(t, c.RunContent(ctx, 1, 1, run), proto.StatusBadRequest, "hasn't sent its content")
	announce(t, c, 1, game, mhk2)
	if err := c.RunContent(ctx, uint64(1), 1, run); err != nil {
		t.Fatal(err)
	}
	// Overlay bumps make a variant's version higher than the track's: accepted.
	bumped := run
	bumped.TrackVersion = 5
	if err := c.RunContent(ctx, uint64(1), 1, bumped); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*server.RunContent)
		msg    string
	}{
		{"rules", func(r *server.RunContent) { r.RulesVersion++ }, "other game rules"},
		{"old track", func(r *server.RunContent) { r.TrackVersion = 2 }, "older version of MHK2.Rome"},
		{"unknown track", func(r *server.RunContent) { r.TrackID = "mhk2.paris" }, "isn't installed"},
		{"track of an unannounced pack", func(r *server.RunContent) { r.TrackID = "mhk3.paris" }, "isn't installed"},
		{"kart version", func(r *server.RunContent) { r.KartVersion = 3 }, "another version of game.kart"},
		{"older kart", func(r *server.RunContent) { r.KartVersion = 1 }, "another version of game.kart"},
		{"unknown kart", func(r *server.RunContent) { r.KartID = "game.other" }, "isn't installed"},
	} {
		r := run
		tc.change(&r)
		wantStatus(t, c.RunContent(ctx, uint64(1), 1, r), proto.StatusForbidden, tc.msg)
	}
	// Another connection of the account has its own announcement.
	wantStatus(t, c.RunContent(ctx, uint64(2), 1, run), proto.StatusBadRequest, "hasn't sent its content")

	// A pack replacing the kart, character or track refuses the run.
	for _, item := range []proto.ManifestItem{it(proto.ContentKart, "game.kart", 2),
		it(proto.ContentCharacter, "game.huhn", 0), it(proto.ContentTrack, "mhk2.rome", 3)} {
		mod := manifest("mod", item)
		mod.Commit = item.ID
		announce(t, c, 3, game, mhk2, mod)
		wantStatus(t, c.RunContent(ctx, uint64(3), 3, run), proto.StatusForbidden, "replaced by mod")
	}
}
