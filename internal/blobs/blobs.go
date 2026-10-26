// Package blobs stores ghost files on disk by content: <data dir>/ghosts/<first 2 hex>/<sha256>.mhkghost.
// Identical files share one blob; the database references blobs by hash and only sysadmin deletes remove them.
package blobs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Hash is the SHA-256 of a blob.
type Hash [32]byte

// Sum returns the hash of b.
func Sum(b []byte) Hash { return sha256.Sum256(b) }

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// ErrNotFound means no blob has the hash.
var ErrNotFound = errors.New("blobs: not found")

const ext = ".mhkghost"

// Store is a directory of blobs. Its methods are safe for concurrent use, also across processes (the server and
// the admin CLI): writes are atomic renames of complete files.
type Store struct {
	dir string
}

// Open returns the store under dataDir, creating <dataDir>/ghosts.
func Open(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "ghosts")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blobs: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Path returns where the blob with hash h lives.
func (s *Store) Path(h Hash) string {
	name := h.String()
	return filepath.Join(s.dir, name[:2], name+ext)
}

// Put stores b and returns its hash. A blob that already exists with the right size is kept as it is.
func (s *Store) Put(b []byte) (Hash, error) {
	h := Sum(b)
	path := s.Path(h)
	if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(b)) {
		return h, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return h, fmt.Errorf("blobs: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return h, fmt.Errorf("blobs: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return h, fmt.Errorf("blobs: write %s: %w", h, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return h, fmt.Errorf("blobs: sync %s: %w", h, err)
	}
	if err := tmp.Close(); err != nil {
		return h, fmt.Errorf("blobs: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return h, fmt.Errorf("blobs: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return h, fmt.Errorf("blobs: store %s: %w", h, err)
	}
	return h, nil
}

// Get reads the blob with hash h.
func (s *Store) Get(h Hash) ([]byte, error) {
	b, err := os.ReadFile(s.Path(h))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("blobs: %w", err)
	}
	return b, nil
}

// Delete removes the blob with hash h; a missing blob is not an error.
func (s *Store) Delete(h Hash) error {
	err := os.Remove(s.Path(h))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blobs: %w", err)
	}
	return nil
}
