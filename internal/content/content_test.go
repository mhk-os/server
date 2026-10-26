package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestManifestHashCanonicalForm(t *testing.T) {
	items := []proto.ManifestItem{
		{Kind: proto.ContentTrack, ID: "mhk2.Rome", Fingerprint: "ab"},
		{Kind: proto.ContentKart, ID: "mhk2.kart", Fingerprint: "cd"},
		{Kind: proto.ContentTrack, ID: "mhk2.alps", Fingerprint: "ef"},
	}
	// Sorted by kind, then lowercase id; ids lowercased, fingerprints as given.
	canonical := "1\nmhk2.alps\nef\n1\nmhk2.rome\nab\n2\nmhk2.kart\ncd\n"
	sum := sha256.Sum256([]byte(canonical))
	want := hex.EncodeToString(sum[:16])
	if want != "0cf67de748fbeb1d772512d45c3b24af" {
		t.Fatalf("pinned hash changed: %s", want)
	}
	got, err := ManifestHash(items)
	if err != nil || got != want {
		t.Fatalf("ManifestHash = %s, %v; want %s", got, err, want)
	}
	slices.Reverse(items)
	if got2, _ := ManifestHash(items); got2 != want {
		t.Fatal("hash depends on item order")
	}
	if empty, _ := ManifestHash(nil); empty != "e3b0c44298fc1c149afbf4c8996fb924" {
		t.Fatalf("empty manifest hash %s", empty)
	}
	// The order uses the lowercased id: "p.AC" sorts before "p.ab" bytewise but after it lowercased.
	a, _ := ManifestHash(items2(it(1, "p.AC", ""), it(1, "p.ab", "")))
	b, _ := ManifestHash(items2(it(1, "p.ab", ""), it(1, "p.ac", "")))
	if a != b {
		t.Fatal("ordering is not by lowercased id")
	}
}

func it(kind uint8, id, fpr string) proto.ManifestItem {
	return proto.ManifestItem{Kind: kind, ID: id, Fingerprint: fpr}
}

func items2(list ...proto.ManifestItem) []proto.ManifestItem { return list }

func TestManifestHashRejects(t *testing.T) {
	for name, items := range map[string][]proto.ManifestItem{
		"kind 0":            items2(it(0, "p.a", "")),
		"empty id":          items2(it(1, "", "")),
		"space in id":       items2(it(1, "p.a b", "")),
		"umlaut in id":      items2(it(1, "p.hühn", "")),
		"97-byte id":        items2(it(1, strings.Repeat("a", 97), "")),
		"newline in fpr":    items2(it(1, "p.a", "x\ny")),
		"duplicate":         items2(it(1, "p.a", "1"), it(1, "p.a", "2")),
		"duplicate by case": items2(it(2, "p.Kart", ""), it(2, "p.kart", "")),
	} {
		if _, err := ManifestHash(items); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ManifestHash(items2(it(1, "p.a", ""), it(2, "p.a", ""))); err != nil {
		t.Errorf("same id, different kinds: %v", err)
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
	votes     map[string]map[uint64]string
	strikes   []store.ContentStrike
	fail      error
}

func newMemStore() *memStore {
	return &memStore{manifests: map[string]*store.Manifest{}, votes: map[string]map[uint64]string{}}
}

func vkey(v store.PackVersion) string {
	return strings.ToLower(v.PackID) + "\x00" + strings.ToLower(v.Commit) + "\x00" + string(rune(v.SimVersion))
}

func (s *memStore) HasManifest(_ context.Context, v store.PackVersion, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.manifests[vkey(v)+hash] != nil, s.fail
}

func (s *memStore) PutManifest(_ context.Context, m *store.Manifest, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifests[vkey(m.PackVersion)+m.Hash] == nil {
		s.manifests[vkey(m.PackVersion)+m.Hash] = m
	}
	return s.fail
}

func (s *memStore) ManifestByHash(_ context.Context, v store.PackVersion, hash string) (*store.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.manifests[vkey(v)+hash]; m != nil {
		return m, nil
	}
	return nil, store.ErrNotFound
}

func (s *memStore) PackVotes(_ context.Context, v store.PackVersion) (map[uint64]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uint64]string{}
	for id, h := range s.votes[vkey(v)] {
		out[id] = h
	}
	return out, s.fail
}

func (s *memStore) SetPackVote(_ context.Context, v store.PackVersion, id uint64, hash string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.votes[vkey(v)] == nil {
		s.votes[vkey(v)] = map[uint64]string{}
	}
	s.votes[vkey(v)][id] = hash
	return s.fail
}

func (s *memStore) AddContentStrike(_ context.Context, k *store.ContentStrike, once bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if once {
		for _, x := range s.strikes {
			if x.AccountID == k.AccountID && vkey(x.PackVersion) == vkey(k.PackVersion) &&
				x.ManifestHash == k.ManifestHash && x.MajorityHash == k.MajorityHash {
				return nil
			}
		}
	}
	s.strikes = append(s.strikes, *k)
	return s.fail
}

func (s *memStore) strikeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.strikes)
}

const sim = 7

// pack builds a manifest of pack "mhk2" at commit "v1" whose items are the given track ids (kind 1).
func pack(tracks ...string) proto.PackManifest {
	m := proto.PackManifest{PackID: "mhk2", Commit: "v1", SimVersion: sim}
	for _, t := range tracks {
		m.Items = append(m.Items, proto.ManifestItem{Kind: proto.ContentTrack, ID: t, Fingerprint: "f"})
	}
	m.ManifestHash, _ = ManifestHash(m.Items)
	return m
}

func ref(m proto.PackManifest) proto.PackRef {
	return proto.PackRef{PackID: m.PackID, Commit: m.Commit, ManifestHash: m.ManifestHash}
}

// announce runs the full ContentHello (+ manifests when asked) for account id on connection key id.
func announce(t *testing.T, c *Catalog, id uint64, ms ...proto.PackManifest) *proto.ContentHelloResult {
	t.Helper()
	ctx := context.Background()
	refs := make([]proto.PackRef, len(ms))
	for i, m := range ms {
		refs[i] = ref(m)
	}
	res, err := c.Hello(ctx, id, id, sim, refs)
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

func TestMajorityAndStrikes(t *testing.T) {
	st := newMemStore()
	c := New(st, nil)
	good, bad := pack("mhk2.rome", "mhk2.alps"), pack("mhk2.rome", "mhk2.alps", "mhk2.fake")

	res := announce(t, c, 1, good)
	if len(res.Missing) != 1 || res.Missing[0] != good.ManifestHash {
		t.Fatalf("first announce should ask for the manifest: %+v", res)
	}
	if res := announce(t, c, 2, good); len(res.Missing) != 0 {
		t.Fatalf("known manifest requested again: %+v", res)
	}
	// One differing copy against two: a majority, but not backed by 3 accounts: no strike.
	if res := announce(t, c, 3, bad); len(res.Strikes) != 0 {
		t.Fatalf("strike below threshold: %+v", res)
	}
	if c.Differs(3, "mhk2") {
		t.Fatal("differs below threshold")
	}
	// The majority decides content even below the threshold: the fake track is not content for account 3.
	if c.Has(3, "mhk2", proto.ContentTrack, "mhk2.fake") || !c.Has(3, "MHK2", proto.ContentTrack, "MHK2.Rome") {
		t.Fatal("majority manifest does not decide")
	}
	announce(t, c, 4, good)
	if !c.Differs(3, "mhk2") || c.Differs(1, "mhk2") {
		t.Fatal("majority of 3 not applied")
	}
	// Account 3 announces again: now it gets a strike, stored once however often it announces.
	res = announce(t, c, 3, bad)
	if len(res.Strikes) != 1 || res.Strikes[0].PackID != "mhk2" ||
		res.Strikes[0].Message != "Your copy of mhk2 differs from other players'." {
		t.Fatalf("strikes %+v", res.Strikes)
	}
	announce(t, c, 3, bad)
	if n := st.strikeCount(); n != 1 {
		t.Fatalf("%d strikes stored", n)
	}
	if got := st.strikes[0]; got.ManifestHash != bad.ManifestHash || got.MajorityHash != good.ManifestHash ||
		got.AccountID != 3 {
		t.Fatalf("strike row %+v", got)
	}
	// A brand-new (unknown) hash is struck at once: its manifest is still owed.
	odd := pack("mhk2.odd")
	res, err := c.Hello(context.Background(), 5, 5, sim, []proto.PackRef{ref(odd)})
	if err != nil || len(res.Missing) != 1 || len(res.Strikes) != 1 {
		t.Fatalf("unknown hash: %+v %v", res, err)
	}
	if c.Ready(5, 5) {
		t.Fatal("ready while a manifest is owed")
	}
	if _, ok := c.Announced(5); ok {
		t.Fatal("incomplete announcement visible")
	}
	if got := c.Items(1, "mhk2", proto.ContentTrack); !slices.Equal(got, []string{"mhk2.rome", "mhk2.alps"}) {
		t.Fatalf("Items %v", got)
	}
	if c.PackCommit(1, "MHK2") != "v1" || c.PackCommit(1, "mhk3") != "" || c.PackCommit(99, "mhk2") != "" {
		t.Fatal("PackCommit")
	}
}

func TestTieUsesOwnManifest(t *testing.T) {
	c := New(newMemStore(), nil)
	a, b := pack("mhk2.rome"), pack("mhk2.alps")
	announce(t, c, 1, a)
	announce(t, c, 2, b)
	// 1:1 is no majority: each player's own manifest decides; nobody is struck or kept out.
	if !c.Has(1, "mhk2", proto.ContentTrack, "mhk2.rome") || c.Has(1, "mhk2", proto.ContentTrack, "mhk2.alps") {
		t.Fatal("account 1 does not use its own manifest")
	}
	if !c.Has(2, "mhk2", proto.ContentTrack, "mhk2.alps") || !c.InMajority(2, "mhk2", proto.ContentTrack, "mhk2.alps") {
		t.Fatal("account 2 does not use its own manifest")
	}
	if c.Differs(1, "mhk2") || c.Differs(2, "mhk2") {
		t.Fatal("tie differs")
	}
	// A third account breaks the tie for everybody.
	announce(t, c, 3, a)
	if c.Has(2, "mhk2", proto.ContentTrack, "mhk2.alps") || !c.Has(2, "mhk2", proto.ContentTrack, "mhk2.rome") {
		t.Fatal("majority after the tie broke not applied")
	}
	// Votes change with the account's latest report: 2 switches to a, 1 to b; a still leads 2:1.
	announce(t, c, 2, a)
	announce(t, c, 1, b)
	if !c.Has(1, "mhk2", proto.ContentTrack, "mhk2.rome") {
		t.Fatal("latest votes not counted")
	}
}

func TestVotesPersist(t *testing.T) {
	st := newMemStore()
	c := New(st, nil)
	a, b := pack("mhk2.rome"), pack("mhk2.alps")
	for id := uint64(1); id <= 3; id++ {
		announce(t, c, id, a)
	}
	announce(t, c, 4, b)
	// A restarted server loads the votes from the store and strikes at once.
	c2 := New(st, nil)
	res := announce(t, c2, 4, b)
	if len(res.Strikes) != 1 || !c2.Differs(4, "mhk2") || len(res.Missing) != 0 {
		t.Fatalf("after restart: %+v", res)
	}
}

func wantStatus(t *testing.T, err error, want proto.Status) {
	t.Helper()
	var se *server.StatusError
	if !errors.As(err, &se) || se.Status != want {
		t.Fatalf("error %v, want status %s", err, want)
	}
}

func TestHelloAndManifestValidation(t *testing.T) {
	ctx := context.Background()
	c := New(newMemStore(), nil)
	m := pack("mhk2.rome")
	for name, refs := range map[string][]proto.PackRef{
		"dotted pack id": {{PackID: "mhk.2", ManifestHash: m.ManifestHash}},
		"empty pack id":  {{PackID: "", ManifestHash: m.ManifestHash}},
		"short hash":     {{PackID: "mhk2", ManifestHash: "abc"}},
		"non-hex hash":   {{PackID: "mhk2", ManifestHash: strings.Repeat("g", 32)}},
		"duplicate pack": {ref(m), {PackID: "MHK2", ManifestHash: m.ManifestHash}},
		"control commit": {{PackID: "mhk2", Commit: "a\nb", ManifestHash: m.ManifestHash}},
	} {
		_, err := c.Hello(ctx, 1, 1, sim, refs)
		if err == nil {
			t.Errorf("%s accepted", name)
			continue
		}
		wantStatus(t, err, proto.StatusBadRequest)
	}
	wantStatus(t, c.Manifests(ctx, 1, 1, []proto.PackManifest{m}), proto.StatusBadRequest) // before hello

	upper := ref(m)
	upper.ManifestHash = strings.ToUpper(upper.ManifestHash)
	res, err := c.Hello(ctx, 1, 1, sim, []proto.PackRef{upper})
	if err != nil || len(res.Missing) != 1 {
		t.Fatal(res, err)
	}
	// A manifest whose items do not produce the announced hash is refused.
	tampered := m
	tampered.Items = []proto.ManifestItem{{Kind: proto.ContentTrack, ID: "mhk2.other", Fingerprint: "f"}}
	wantStatus(t, c.Manifests(ctx, 1, 1, []proto.PackManifest{tampered}), proto.StatusBadRequest)
	// A manifest nobody asked for is ignored (not stored, no error).
	other := pack("mhk2.x")
	other.PackID = "mhk3"
	other.ManifestHash, _ = ManifestHash(other.Items)
	if err := c.Manifests(ctx, 1, 1, []proto.PackManifest{other}); err != nil || c.Ready(1, 1) {
		t.Fatal("unrequested manifest changed the state", err)
	}
	// The uppercase announced hash matches the lowercase computed one.
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

func TestRaceStrike(t *testing.T) {
	st := newMemStore()
	c := New(st, nil)
	m := pack("mhk2.rome")
	announce(t, c, 1, m)
	c.Strike(1, "mhk2", "desync: "+strings.Repeat("x", 300))
	if len(st.strikes) != 1 {
		t.Fatal("no strike stored")
	}
	k := st.strikes[0]
	if k.Commit != "v1" || k.SimVersion != sim || k.ManifestHash != m.ManifestHash || len(k.Reason) != 255 {
		t.Fatalf("strike %+v", k)
	}
}
