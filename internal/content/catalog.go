package content

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
)

// StrikeThreshold is how many distinct accounts must back a majority before differing copies get strikes and are
// kept out of anticheat lobbies.
const StrikeThreshold = 3

// lookupTimeout bounds the database reads lookups may need (a majority manifest not in memory yet).
const lookupTimeout = 5 * time.Second

// Store is what the catalog persists.
type Store interface {
	HasManifest(ctx context.Context, v store.PackVersion, hash string) (bool, error)
	PutManifest(ctx context.Context, m *store.Manifest, at time.Time) error
	ManifestByHash(ctx context.Context, v store.PackVersion, hash string) (*store.Manifest, error)
	PackVotes(ctx context.Context, v store.PackVersion) (map[uint64]string, error)
	SetPackVote(ctx context.Context, v store.PackVersion, accountID uint64, hash string, at time.Time) error
	AddContentStrike(ctx context.Context, k *store.ContentStrike, once bool) error
}

// Announcement is what a connection announced in ContentHello once every requested manifest arrived.
type Announcement struct {
	SimVersion int32
	Packs      map[string]proto.PackRef // by lowercase pack id; hashes lowercase
}

// Pack returns the announced version of packID (case-insensitive).
func (a *Announcement) Pack(packID string) (proto.PackRef, bool) {
	p, ok := a.Packs[strings.ToLower(packID)]
	return p, ok
}

type versionKey struct {
	pack, commit string // lowercase
	sim          int32
}

func keyOf(p proto.PackRef, sim int32) versionKey {
	return versionKey{strings.ToLower(p.PackID), strings.ToLower(p.Commit), sim}
}

func (k versionKey) storeVersion() store.PackVersion {
	return store.PackVersion{PackID: k.pack, Commit: k.commit, SimVersion: k.sim}
}

// itemKey is a manifest item by kind and lowercase id.
type itemKey struct {
	kind uint8
	id   string
}

// index is a stored manifest prepared for lookups.
type index struct {
	items  map[itemKey]struct{}
	byKind map[uint8][]string // ids as reported, in manifest order
}

func newIndex(items []proto.ManifestItem) *index {
	ix := &index{items: make(map[itemKey]struct{}, len(items)), byKind: map[uint8][]string{}}
	for _, it := range items {
		ix.items[itemKey{it.Kind, strings.ToLower(it.ID)}] = struct{}{}
		ix.byKind[it.Kind] = append(ix.byKind[it.Kind], it.ID)
	}
	return ix
}

// version holds the votes of one pack version. Its mutex serializes loading and database writes so the
// in-memory tally always matches the stored one.
type version struct {
	key       versionKey
	mu        sync.Mutex
	votes     map[uint64]string // nil until loaded
	manifests map[string]*index // by hash; only manifests needed so far
}

// majority returns the hash reported by the most accounts and their number; "" when nobody reported or the top is
// a tie. The caller holds v.mu.
func (v *version) majority() (string, int) {
	counts := map[string]int{}
	for _, h := range v.votes {
		counts[h]++
	}
	best, n, tie := "", 0, false
	for h, c := range counts {
		switch {
		case c > n:
			best, n, tie = h, c, false
		case c == n:
			tie = true
		}
	}
	if tie {
		return "", n
	}
	return best, n
}

// connState is a connection's announcement; missing holds the lowercase pack ids whose manifests are still owed.
type connState struct {
	accountID uint64
	ann       Announcement
	missing   map[string]bool
	seq       uint64
}

// Catalog is the content catalog. Connection keys are opaque (the server passes its *server.Conn).
type Catalog struct {
	st  Store
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	versions map[versionKey]*version
	conns    map[any]*connState
	seq      uint64
}

var _ race.Catalog = (*Catalog)(nil)

// New returns an empty catalog over st; votes and manifests load on demand.
func New(st Store, log *slog.Logger) *Catalog {
	if log == nil {
		log = slog.Default()
	}
	return &Catalog{st: st, log: log, now: store.Now, versions: map[versionKey]*version{}, conns: map[any]*connState{}}
}

func (c *Catalog) version(k versionKey) *version {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.versions[k]
	if v == nil {
		v = &version{key: k, manifests: map[string]*index{}}
		c.versions[k] = v
	}
	return v
}

// load reads the version's votes once. The caller holds v.mu.
func (c *Catalog) load(ctx context.Context, v *version) error {
	if v.votes != nil {
		return nil
	}
	votes, err := c.st.PackVotes(ctx, v.key.storeVersion())
	if err != nil {
		return err
	}
	v.votes = votes
	return nil
}

// manifest returns the index of a stored manifest, nil when the server does not hold it. The caller holds v.mu.
func (c *Catalog) manifest(ctx context.Context, v *version, hash string) (*index, error) {
	if ix := v.manifests[hash]; ix != nil {
		return ix, nil
	}
	m, err := c.st.ManifestByHash(ctx, v.key.storeVersion(), hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ix := newIndex(m.Items)
	v.manifests[hash] = ix
	return ix, nil
}

// vote records the account's hash (its manifest is stored). The caller holds v.mu and loaded the votes.
func (c *Catalog) vote(ctx context.Context, v *version, accountID uint64, hash string) error {
	if v.votes[accountID] == hash {
		return nil
	}
	if err := c.st.SetPackVote(ctx, v.key.storeVersion(), accountID, hash, c.now()); err != nil {
		return err
	}
	v.votes[accountID] = hash
	return nil
}

func strikeMessage(packID string) string {
	return fmt.Sprintf("Your copy of %s differs from other players'.", packID)
}

// Hello handles ContentHello for the connection conn of accountID: it records the votes of packs whose manifests
// the server holds, returns the manifests it lacks and the packs that differ from a majority backed by at least
// StrikeThreshold accounts (stored as content strikes). Invalid announcements are *server.StatusError.
func (c *Catalog) Hello(ctx context.Context, conn any, accountID uint64, sim int32, packs []proto.PackRef) (
	*proto.ContentHelloResult, error) {
	st := &connState{accountID: accountID, ann: Announcement{SimVersion: sim, Packs: map[string]proto.PackRef{}},
		missing: map[string]bool{}}
	for _, p := range packs {
		switch {
		case !validPackID(p.PackID):
			return nil, server.Errorf(proto.StatusBadRequest, "Pack id %q is invalid.", p.PackID)
		case !printable(p.Commit):
			return nil, server.Errorf(proto.StatusBadRequest, "The commit of pack %s is invalid.", p.PackID)
		case !validHash(p.ManifestHash):
			return nil, server.Errorf(proto.StatusBadRequest, "The manifest hash of pack %s is invalid.", p.PackID)
		}
		lower := strings.ToLower(p.PackID)
		if _, dup := st.ann.Packs[lower]; dup {
			return nil, server.Errorf(proto.StatusBadRequest, "Pack %s is announced twice.", p.PackID)
		}
		p.ManifestHash = strings.ToLower(p.ManifestHash)
		st.ann.Packs[lower] = p
	}

	res := &proto.ContentHelloResult{Missing: []string{}, Strikes: []proto.ContentStrike{}}
	for _, p := range packs {
		ref := st.ann.Packs[strings.ToLower(p.PackID)]
		v := c.version(keyOf(ref, sim))
		known, differs, err := c.helloPack(ctx, v, accountID, ref)
		if err != nil {
			return nil, err
		}
		if !known {
			st.missing[strings.ToLower(ref.PackID)] = true
			res.Missing = append(res.Missing, p.ManifestHash)
		}
		if differs != "" {
			res.Strikes = append(res.Strikes, proto.ContentStrike{PackID: ref.PackID, Message: strikeMessage(ref.PackID)})
			err := c.st.AddContentStrike(ctx, &store.ContentStrike{AccountID: accountID,
				PackVersion: v.key.storeVersion(), ManifestHash: ref.ManifestHash, MajorityHash: differs,
				Reason: "announced copy differs from the catalog majority", CreatedAt: c.now()}, true)
			if err != nil {
				return nil, err
			}
		}
	}
	c.mu.Lock()
	c.seq++
	st.seq = c.seq
	c.conns[conn] = st
	c.mu.Unlock()
	return res, nil
}

// helloPack records one announced pack: known reports whether its manifest is stored (the vote is then recorded),
// differs is the majority hash when it is backed by StrikeThreshold accounts and is not the announced one.
func (c *Catalog) helloPack(ctx context.Context, v *version, accountID uint64, ref proto.PackRef) (
	known bool, differs string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := c.load(ctx, v); err != nil {
		return false, "", err
	}
	if v.manifests[ref.ManifestHash] != nil {
		known = true
	} else if known, err = c.st.HasManifest(ctx, v.key.storeVersion(), ref.ManifestHash); err != nil {
		return false, "", err
	}
	if known {
		if _, err := c.manifest(ctx, v, ref.ManifestHash); err != nil {
			return false, "", err
		}
		if err := c.vote(ctx, v, accountID, ref.ManifestHash); err != nil {
			return false, "", err
		}
	}
	if maj, n := v.majority(); maj != "" && n >= StrikeThreshold && maj != ref.ManifestHash {
		differs = maj
	}
	return known, differs, nil
}

// Manifests handles ContentManifests: every manifest the connection still owes is verified, stored and counted.
// Manifests nobody asked for are ignored, so clients cannot fill the database with arbitrary ones.
func (c *Catalog) Manifests(ctx context.Context, conn any, accountID uint64, ms []proto.PackManifest) error {
	c.mu.Lock()
	st := c.conns[conn]
	var owed map[string]proto.PackRef
	var sim int32
	if st != nil && st.accountID == accountID {
		sim = st.ann.SimVersion
		owed = map[string]proto.PackRef{}
		for id := range st.missing {
			owed[id] = st.ann.Packs[id]
		}
	}
	c.mu.Unlock()
	if owed == nil {
		return server.Errorf(proto.StatusBadRequest, "Send ContentHello first.")
	}
	for i := range ms {
		m := &ms[i]
		lower := strings.ToLower(m.PackID)
		ref, ok := owed[lower]
		if !ok || !strings.EqualFold(ref.Commit, m.Commit) || m.SimVersion != sim ||
			!strings.EqualFold(ref.ManifestHash, m.ManifestHash) {
			continue
		}
		hash, err := ManifestHash(m.Items)
		if err != nil {
			return server.Errorf(proto.StatusBadRequest, "Manifest of %s: %v.", m.PackID, err)
		}
		if hash != ref.ManifestHash {
			return server.Errorf(proto.StatusBadRequest, "The manifest of %s does not match its hash.", m.PackID)
		}
		v := c.version(keyOf(ref, sim))
		if err := c.storeManifest(ctx, v, accountID, ref, m.Items); err != nil {
			return err
		}
		delete(owed, lower)
		c.mu.Lock()
		if cur := c.conns[conn]; cur == st {
			delete(st.missing, lower)
		}
		c.mu.Unlock()
	}
	return nil
}

func (c *Catalog) storeManifest(ctx context.Context, v *version, accountID uint64, ref proto.PackRef,
	items []proto.ManifestItem) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := c.load(ctx, v); err != nil {
		return err
	}
	k := v.key.storeVersion()
	if err := c.st.PutManifest(ctx, &store.Manifest{PackVersion: store.PackVersion{PackID: ref.PackID,
		Commit: ref.Commit, SimVersion: k.SimVersion}, Hash: ref.ManifestHash, Items: items}, c.now()); err != nil {
		return err
	}
	v.manifests[ref.ManifestHash] = newIndex(items)
	return c.vote(ctx, v, accountID, ref.ManifestHash)
}

// Forget drops the announcement of a closed connection.
func (c *Catalog) Forget(conn any) {
	c.mu.Lock()
	delete(c.conns, conn)
	c.mu.Unlock()
}

// Ready reports whether conn, logged in as accountID, announced its content and sent every requested manifest.
func (c *Catalog) Ready(conn any, accountID uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.conns[conn]
	return st != nil && st.accountID == accountID && len(st.missing) == 0
}

// Announced returns the most recent complete announcement among the account's connections.
func (c *Catalog) Announced(accountID uint64) (Announcement, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *connState
	for _, st := range c.conns {
		if st.accountID == accountID && len(st.missing) == 0 && (best == nil || st.seq > best.seq) {
			best = st
		}
	}
	if best == nil {
		return Announcement{}, false
	}
	return best.ann, true
}

// announcedVersion returns the version of packID the account announced.
func (c *Catalog) announcedVersion(accountID uint64, packID string) (*version, proto.PackRef, bool) {
	ann, ok := c.Announced(accountID)
	if !ok {
		return nil, proto.PackRef{}, false
	}
	ref, ok := ann.Pack(packID)
	if !ok {
		return nil, proto.PackRef{}, false
	}
	return c.version(keyOf(ref, ann.SimVersion)), ref, true
}

// Differs reports whether the account's copy of packID differs from a majority backed by at least
// StrikeThreshold accounts: such players cannot join anticheat lobbies with that pack.
func (c *Catalog) Differs(accountID uint64, packID string) bool {
	v, ref, ok := c.announcedVersion(accountID, packID)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := c.load(ctx, v); err != nil {
		c.log.Error("loading pack votes", "pack", v.key.pack, "err", err)
		return false
	}
	maj, n := v.majority()
	return maj != "" && n >= StrikeThreshold && maj != ref.ManifestHash
}

// deciding returns the manifest that decides what the account's version of packID contains: the catalog majority,
// or the account's own manifest when there is no majority (a tie), so one tampered copy cannot block a pack
// version while only two accounts reported it. nil when the pack is not announced or not loadable.
func (c *Catalog) deciding(accountID uint64, packID string) *index {
	v, ref, ok := c.announcedVersion(accountID, packID)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := c.load(ctx, v); err != nil {
		c.log.Error("loading pack votes", "pack", v.key.pack, "err", err)
		return nil
	}
	hash, _ := v.majority()
	if hash == "" {
		hash = ref.ManifestHash
	}
	ix, err := c.manifest(ctx, v, hash)
	if err != nil {
		c.log.Error("loading manifest", "pack", v.key.pack, "hash", hash, "err", err)
		return nil
	}
	return ix
}

// Has reports whether (kind, id) is content of the account's version of packID (see deciding).
func (c *Catalog) Has(accountID uint64, packID string, kind uint8, id string) bool {
	ix := c.deciding(accountID, packID)
	if ix == nil {
		return false
	}
	_, ok := ix.items[itemKey{kind, strings.ToLower(id)}]
	return ok
}

// Items lists the ids of one kind in the deciding manifest of the account's version of packID.
func (c *Catalog) Items(accountID uint64, packID string, kind uint8) []string {
	ix := c.deciding(accountID, packID)
	if ix == nil {
		return nil
	}
	return ix.byKind[kind]
}

// PackCommit implements race.Catalog.
func (c *Catalog) PackCommit(accountID uint64, packID string) string {
	ann, ok := c.Announced(accountID)
	if !ok {
		return ""
	}
	ref, _ := ann.Pack(packID)
	return ref.Commit
}

// InMajority implements race.Catalog with the same rule as Has.
func (c *Catalog) InMajority(accountID uint64, packID string, kind uint8, id string) bool {
	return c.Has(accountID, packID, kind, id)
}

// Strike implements race.Catalog: a race found the account's content disagreeing with the other racers.
func (c *Catalog) Strike(accountID uint64, packID, reason string) {
	k := &store.ContentStrike{AccountID: accountID, PackVersion: store.PackVersion{PackID: packID},
		Reason: truncate(reason, 255), CreatedAt: c.now()}
	if ann, ok := c.Announced(accountID); ok {
		k.SimVersion = ann.SimVersion
		if ref, ok := ann.Pack(packID); ok {
			k.Commit, k.ManifestHash = ref.Commit, ref.ManifestHash
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	if err := c.st.AddContentStrike(ctx, k, false); err != nil {
		c.log.Error("storing content strike", "account", accountID, "pack", packID, "err", err)
	}
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}
