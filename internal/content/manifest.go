// Package content keeps the content catalog (docs/protocol.md, "Content agreement"): the stored pack manifests and
// what each connection announced. Lobbies ask it which ids are lobby content, races ask it through race.Catalog and
// time-trial uploads through server.Content.
package content

import (
	"fmt"
	"strings"

	"github.com/mhk-os/server/internal/proto"
)

// validID reports whether s is a content id: 1–96 characters of [A-Za-z0-9_.-]. The restriction keeps
// lowercasing ASCII-only, so the Go server and the C# client agree on the canonical form.
func validID(s string) bool { return proto.ValidID(s, proto.MaxContentID) }

// validPackID is a content id without dots: item ids name their pack as the text before the first dot.
func validPackID(s string) bool {
	return len(s) <= proto.MaxPackID && validID(s) && !strings.Contains(s, ".")
}

// printable rejects control characters in commits.
func printable(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// PackOf returns the pack an item id belongs to: the text before its first dot ("" when there is none).
func PackOf(id string) string {
	pack, _, ok := strings.Cut(id, ".")
	if !ok {
		return ""
	}
	return pack
}

// versioned reports whether items of kind carry a content version (at least 1): tracks, karts and arenas.
func versioned(kind uint8) bool {
	return kind == proto.ContentTrack || kind == proto.ContentKart || kind == proto.ContentArena
}

// ValidateItems checks a manifest's item list: kind not 0, valid ids, no (kind, lowercase id) twice, version at
// least 1 for tracks, karts and arenas and at least 0 for the other kinds.
func ValidateItems(items []proto.ManifestItem) error {
	type key struct {
		kind uint8
		id   string
	}
	seen := make(map[key]bool, len(items))
	for _, it := range items {
		if it.Kind == 0 {
			return fmt.Errorf("item %q has kind 0", it.ID)
		}
		if !validID(it.ID) {
			return fmt.Errorf("item id %q is not 1-96 characters of A-Z a-z 0-9 _ . -", it.ID)
		}
		if it.Version < 0 || versioned(it.Kind) && it.Version < 1 {
			return fmt.Errorf("item %q has version %d", it.ID, it.Version)
		}
		k := key{it.Kind, strings.ToLower(it.ID)}
		if seen[k] {
			return fmt.Errorf("item %q of kind %d is listed twice", it.ID, it.Kind)
		}
		seen[k] = true
	}
	return nil
}
