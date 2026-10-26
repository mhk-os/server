// Package content keeps the content catalog (docs/protocol.md, "Content agreement"): which pack manifests exist,
// which manifest every account reported for each pack version, the catalog majority, content strikes, and what
// each connection announced. Lobbies ask it which ids are lobby content; races ask it through race.Catalog.
package content

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mhk-os/server/internal/proto"
)

// validID reports whether s is a content id: 1–96 characters of [A-Za-z0-9_.-]. The restriction keeps
// lowercasing ASCII-only, so the Go server and the C# client agree on the canonical form.
func validID(s string) bool {
	if len(s) == 0 || len(s) > proto.MaxContentID {
		return false
	}
	for i := range len(s) {
		if c := s[i]; !isAlnum(c) && c != '_' && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// validPackID is a content id without dots: item ids name their pack as the text before the first dot.
func validPackID(s string) bool {
	return len(s) <= proto.MaxPackID && validID(s) && !strings.Contains(s, ".")
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// printable rejects control characters; they would make the newline-separated canonical form ambiguous.
func printable(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

func validHash(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// PackOf returns the pack an item id belongs to: the text before its first dot ("" when there is none).
func PackOf(id string) string {
	pack, _, ok := strings.Cut(id, ".")
	if !ok {
		return ""
	}
	return pack
}

// ManifestHash computes the canonical manifest hash: items sorted by kind, then by lowercased id (ordinal), each
// contributing the UTF-8 text "<kind>\n<id lowercase>\n<fingerprint>\n"; SHA-256 of the concatenation, first 16
// bytes as lowercase hex. Invalid ids, kind 0, control characters and duplicate (kind, lowercase id) are errors.
func ManifestHash(items []proto.ManifestItem) (string, error) {
	type entry struct {
		kind    uint8
		id, fpr string
	}
	sorted := make([]entry, len(items))
	for i, it := range items {
		if it.Kind == 0 {
			return "", fmt.Errorf("item %q has kind 0", it.ID)
		}
		if !validID(it.ID) {
			return "", fmt.Errorf("item id %q is not 1-96 characters of A-Z a-z 0-9 _ . -", it.ID)
		}
		if !printable(it.Fingerprint) {
			return "", fmt.Errorf("fingerprint of %q has control characters", it.ID)
		}
		sorted[i] = entry{it.Kind, strings.ToLower(it.ID), it.Fingerprint}
	}
	slices.SortFunc(sorted, func(a, b entry) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), strings.Compare(a.id, b.id))
	})
	h := sha256.New()
	buf := make([]byte, 0, 160)
	for i, e := range sorted {
		if i > 0 && e.kind == sorted[i-1].kind && e.id == sorted[i-1].id {
			return "", fmt.Errorf("item %q of kind %d is listed twice", e.id, e.kind)
		}
		buf = strconv.AppendUint(buf[:0], uint64(e.kind), 10)
		buf = append(buf, '\n')
		buf = append(buf, e.id...)
		buf = append(buf, '\n')
		buf = append(buf, e.fpr...)
		buf = append(buf, '\n')
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}
