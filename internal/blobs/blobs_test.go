package blobs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutGetDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("ghost file bytes")
	h, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	name := h.String()
	want := filepath.Join(dir, "ghosts", name[:2], name+".mhkghost")
	if s.Path(h) != want {
		t.Fatalf("path %s, want %s", s.Path(h), want)
	}
	got, err := s.Get(h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get %q %v", got, err)
	}
	// Deduplicated: a second Put keeps the file and leaves no temporary files behind.
	if h2, err := s.Put(append([]byte(nil), data...)); err != nil || h2 != h {
		t.Fatalf("second put %v %v", h2, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(want))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") || len(entries) != 1 {
			t.Fatalf("directory holds %v", entries)
		}
	}
	// A truncated blob (crash of an older writer) is replaced.
	os.WriteFile(want, data[:3], 0o640)
	if _, err := s.Put(data); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(h); !bytes.Equal(got, data) {
		t.Fatalf("not repaired: %q", got)
	}
	if err := s.Delete(h); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.Delete(h); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
