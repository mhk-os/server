package content

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/store"
)

// lookupTimeout bounds the database reads lookups may need (a manifest not in memory yet).
const lookupTimeout = 5 * time.Second

// Store is what the catalog persists.
type Store interface {
	PutManifest(ctx context.Context, m *store.Manifest, at time.Time) error
	Manifest(ctx context.Context, v store.PackVersion) (*store.Manifest, error)
}

// Announcement is what a connection announced in ContentHello once every requested manifest arrived.
type Announcement struct {
	RulesVersion int32
	Packs        map[string]proto.PackRef // by lowercase pack id
}

// Pack returns the announced version of packID (case-insensitive).
func (a *Announcement) Pack(packID string) (proto.PackRef, bool) {
	p, ok := a.Packs[strings.ToLower(packID)]
	return p, ok
}

type versionKey struct {
	pack, commit string // lowercase
	rulesVersion int32
}

func keyOf(p proto.PackRef, rulesVersion int32) versionKey {
	return versionKey{strings.ToLower(p.PackID), strings.ToLower(p.Commit), rulesVersion}
}

// itemKey is a manifest item by kind and lowercase id.
type itemKey struct {
	kind uint8
	id   string
}

// index is a stored manifest prepared for lookups.
type index struct {
	items   map[itemKey]int32  // versions
	byKind  map[uint8][]string // ids as reported, in manifest order
	foreign []proto.ManifestItem
}

// newIndex indexes a manifest of pack; foreign holds its items under another pack's id (it replaces them).
func newIndex(pack string, items []proto.ManifestItem) *index {
	ix := &index{items: make(map[itemKey]int32, len(items)), byKind: map[uint8][]string{}}
	for _, it := range items {
		ix.items[itemKey{it.Kind, strings.ToLower(it.ID)}] = it.Version
		ix.byKind[it.Kind] = append(ix.byKind[it.Kind], it.ID)
		if !strings.EqualFold(PackOf(it.ID), pack) {
			ix.foreign = append(ix.foreign, it)
		}
	}
	return ix
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

	mu        sync.Mutex
	manifests map[versionKey]*index // stored manifests loaded so far
	conns     map[any]*connState
	seq       uint64
}

var (
	_ race.Catalog   = (*Catalog)(nil)
	_ server.Content = (*Catalog)(nil)
)

// New returns an empty catalog over st; manifests load on demand.
func New(st Store, log *slog.Logger) *Catalog {
	if log == nil {
		log = slog.Default()
	}
	return &Catalog{st: st, log: log, now: store.Now, manifests: map[versionKey]*index{}, conns: map[any]*connState{}}
}

// manifest returns the index of the stored manifest of a pack version, nil when the server does not hold it.
func (c *Catalog) manifest(ctx context.Context, k versionKey) (*index, error) {
	c.mu.Lock()
	ix := c.manifests[k]
	c.mu.Unlock()
	if ix != nil {
		return ix, nil
	}
	m, err := c.st.Manifest(ctx, store.PackVersion{PackID: k.pack, Commit: k.commit, RulesVersion: k.rulesVersion})
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ix = newIndex(k.pack, m.Items)
	c.mu.Lock()
	if cur := c.manifests[k]; cur != nil {
		ix = cur
	} else {
		c.manifests[k] = ix
	}
	c.mu.Unlock()
	return ix, nil
}

// Hello handles ContentHello for the connection conn of accountID and returns the ids of the announced packs whose
// manifests the server does not hold yet. Invalid announcements are *server.StatusError.
func (c *Catalog) Hello(ctx context.Context, conn any, accountID uint64, rulesVersion int32, packs []proto.PackRef) (
	*proto.ContentHelloResult, error) {
	st := &connState{accountID: accountID,
		ann: Announcement{RulesVersion: rulesVersion, Packs: map[string]proto.PackRef{}}, missing: map[string]bool{}}
	for _, p := range packs {
		switch {
		case !validPackID(p.PackID):
			return nil, server.Errorf(proto.StatusBadRequest, "Pack id %q is invalid.", p.PackID)
		case !printable(p.Commit):
			return nil, server.Errorf(proto.StatusBadRequest, "The commit of pack %s is invalid.", p.PackID)
		}
		lower := strings.ToLower(p.PackID)
		if _, dup := st.ann.Packs[lower]; dup {
			return nil, server.Errorf(proto.StatusBadRequest, "Pack %s is announced twice.", p.PackID)
		}
		st.ann.Packs[lower] = p
	}

	res := &proto.ContentHelloResult{Missing: []string{}}
	for _, p := range packs {
		ix, err := c.manifest(ctx, keyOf(p, rulesVersion))
		if err != nil {
			return nil, err
		}
		if ix == nil {
			st.missing[strings.ToLower(p.PackID)] = true
			res.Missing = append(res.Missing, p.PackID)
		}
	}
	c.mu.Lock()
	c.seq++
	st.seq = c.seq
	c.conns[conn] = st
	c.mu.Unlock()
	return res, nil
}

// Manifests handles ContentManifests: every manifest the connection still owes is validated and stored, unless
// another connection stored one for the same pack version first (the first one is kept). Manifests nobody asked
// for are ignored, so clients cannot fill the database with arbitrary ones.
func (c *Catalog) Manifests(ctx context.Context, conn any, accountID uint64, ms []proto.PackManifest) error {
	c.mu.Lock()
	st := c.conns[conn]
	var owed map[string]proto.PackRef
	var rulesVersion int32
	if st != nil && st.accountID == accountID {
		rulesVersion = st.ann.RulesVersion
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
		if !ok || !strings.EqualFold(ref.Commit, m.Commit) || m.RulesVersion != rulesVersion {
			continue
		}
		if err := ValidateItems(m.Items); err != nil {
			return server.Errorf(proto.StatusBadRequest, "Manifest of %s: %v.", m.PackID, err)
		}
		k := keyOf(ref, rulesVersion)
		if err := c.st.PutManifest(ctx, &store.Manifest{PackVersion: store.PackVersion{PackID: ref.PackID,
			Commit: ref.Commit, RulesVersion: rulesVersion}, Items: m.Items}, c.now()); err != nil {
			return err
		}
		// Read back what is stored: a manifest stored first by another connection wins.
		ix, err := c.manifest(ctx, k)
		if err != nil {
			return err
		}
		if ix == nil {
			return errors.New("content: stored manifest not found")
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

// announcedManifest returns the manifest of the version of packID the account announced; nil when the pack is not
// announced or its manifest cannot be loaded (logged).
func (c *Catalog) announcedManifest(accountID uint64, packID string) *index {
	ann, ok := c.Announced(accountID)
	if !ok {
		return nil
	}
	ref, ok := ann.Pack(packID)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ix, err := c.manifest(ctx, keyOf(ref, ann.RulesVersion))
	if err != nil {
		c.log.Error("loading manifest", "pack", ref.PackID, "commit", ref.Commit, "err", err)
		return nil
	}
	return ix
}

// Has reports whether the manifest of the account's announced version of packID lists (kind, id). It implements
// race.Catalog.
func (c *Catalog) Has(accountID uint64, packID string, kind uint8, id string) bool {
	ix := c.announcedManifest(accountID, packID)
	if ix == nil {
		return false
	}
	_, ok := ix.items[itemKey{kind, strings.ToLower(id)}]
	return ok
}

// Items lists the ids of one kind in the manifest of the account's announced version of packID.
func (c *Catalog) Items(accountID uint64, packID string, kind uint8) []string {
	ix := c.announcedManifest(accountID, packID)
	if ix == nil {
		return nil
	}
	return ix.byKind[kind]
}

// Override is an item a pack lists under another pack's id: by load order it replaces that pack's definition.
type Override struct {
	Kind uint8
	ID   string // as the replacing pack lists it
	Pack string // the replacing pack, as announced
}

// Overrides lists what the announced packs replace in other packs, by id. Lookup failures are logged and leave
// out the pack concerned.
func (c *Catalog) Overrides(ann Announcement) []Override {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	out, err := c.overrides(ctx, ann)
	if err != nil {
		c.log.Error("loading announced manifests", "err", err)
	}
	return out
}

func (c *Catalog) overrides(ctx context.Context, ann Announcement) ([]Override, error) {
	var out []Override
	var firstErr error
	for _, ref := range ann.Packs {
		ix, err := c.manifest(ctx, keyOf(ref, ann.RulesVersion))
		if err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		if ix == nil {
			continue
		}
		for _, it := range ix.foreign {
			out = append(out, Override{Kind: it.Kind, ID: it.ID, Pack: ref.PackID})
		}
	}
	slices.SortFunc(out, func(a, b Override) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.ID), strings.ToLower(b.ID)), cmp.Compare(a.Kind, b.Kind),
			strings.Compare(a.Pack, b.Pack))
	})
	return out, firstErr
}

// RunContent implements server.Content: conn may upload a run once its announcement is complete, for the rules
// version it announced, when the manifest of the track's pack lists the track with a version up to the run's (a
// variant adds its overlays' bumps to the track's version), the manifest of the kart's pack lists the kart with
// exactly the run's version, and no other pack replaces the track, kart or character.
func (c *Catalog) RunContent(ctx context.Context, conn any, accountID uint64, run server.RunContent) error {
	c.mu.Lock()
	st := c.conns[conn]
	ready := st != nil && st.accountID == accountID && len(st.missing) == 0
	var ann Announcement
	if ready {
		ann = st.ann
	}
	c.mu.Unlock()
	if !ready {
		return server.Errorf(proto.StatusBadRequest, "Your game hasn't sent its content yet. Try again in a moment.")
	}
	if run.RulesVersion != ann.RulesVersion {
		return server.Errorf(proto.StatusForbidden,
			"This run was recorded with other game rules (version %d, yours %d) and can't be uploaded.",
			run.RulesVersion, ann.RulesVersion)
	}
	ovs, err := c.overrides(ctx, ann)
	if err != nil {
		return err
	}
	items := []proto.ManifestItem{{Kind: proto.ContentKart, ID: run.KartID},
		{Kind: proto.ContentCharacter, ID: run.CharacterID}, {Kind: proto.ContentTrack, ID: run.TrackID}}
	for _, it := range items {
		for _, o := range ovs {
			if o.Kind == it.Kind && strings.EqualFold(o.ID, it.ID) {
				return server.Errorf(proto.StatusForbidden,
					"This run uses %s replaced by %s. Runs with modified content can't be uploaded.", it.ID, o.Pack)
			}
		}
	}
	track, err := c.itemVersion(ctx, ann, proto.ContentTrack, run.TrackID)
	if err != nil {
		return err
	}
	switch {
	case track < 0:
		return server.Errorf(proto.StatusForbidden, "Track %s isn't installed in your game.", run.TrackID)
	case run.TrackVersion < track:
		return server.Errorf(proto.StatusForbidden,
			"This run was recorded on an older version of %s and can't be uploaded.", run.TrackID)
	}
	kart, err := c.itemVersion(ctx, ann, proto.ContentKart, run.KartID)
	if err != nil {
		return err
	}
	switch {
	case kart < 0:
		return server.Errorf(proto.StatusForbidden, "Kart %s isn't installed in your game.", run.KartID)
	case run.KartVersion != kart:
		return server.Errorf(proto.StatusForbidden,
			"This run was recorded with another version of %s and can't be uploaded.", run.KartID)
	}
	return nil
}

// itemVersion returns the version of (kind, id) in the announced manifest of its pack, -1 when it is not listed
// there (or the pack is not announced).
func (c *Catalog) itemVersion(ctx context.Context, ann Announcement, kind uint8, id string) (int32, error) {
	ref, ok := ann.Pack(PackOf(id))
	if !ok {
		return -1, nil
	}
	ix, err := c.manifest(ctx, keyOf(ref, ann.RulesVersion))
	if err != nil || ix == nil {
		return -1, err
	}
	v, ok := ix.items[itemKey{kind, strings.ToLower(id)}]
	if !ok {
		return -1, nil
	}
	return v, nil
}
