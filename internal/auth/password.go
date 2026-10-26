package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id cost parameters (OWASP minimum recommendation by default).
type Params struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
	SaltLen   int
	KeyLen    uint32
}

// DefaultParams: m=19456 KiB, t=2, p=1, 16-byte salt, 32-byte key.
var DefaultParams = Params{MemoryKiB: 19456, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// DefaultHashConcurrency bounds simultaneous hashes so a login burst cannot exhaust memory (19 MiB each).
const DefaultHashConcurrency = 4

var errBadHash = errors.New("auth: malformed password hash")

// Hasher hashes and verifies passwords with argon2id, at most a fixed number at a time.
type Hasher struct {
	params Params
	sem    chan struct{}

	dummyOnce sync.Once
	dummy     string
}

// NewHasher returns a Hasher running at most concurrency hashes at once.
func NewHasher(p Params, concurrency int) *Hasher {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Hasher{params: p, sem: make(chan struct{}, concurrency)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns the PHC string $argon2id$v=19$m=..,t=..,p=..$salt$hash (unpadded standard base64).
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	rand.Read(salt)
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.MemoryKiB, h.params.Threads, h.params.KeyLen)
	h.release()
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, h.params.MemoryKiB, h.params.Time,
		h.params.Threads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify reports whether password matches the PHC hash, using the parameters stored in the hash so cost changes
// do not invalidate existing accounts. The comparison is constant-time.
func (h *Hasher) Verify(ctx context.Context, password, phc string) (bool, error) {
	p, salt, want, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want)))
	h.release()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyVerify spends the same time as a real Verify so a login for an unknown username is indistinguishable by
// timing from a wrong password.
func (h *Hasher) DummyVerify(ctx context.Context, password string) {
	h.dummyOnce.Do(func() {
		random := make([]byte, 24)
		rand.Read(random)
		h.dummy, _ = h.Hash(context.Background(), base64.RawStdEncoding.EncodeToString(random))
	})
	h.Verify(ctx, password, h.dummy)
}

func parsePHC(s string) (Params, []byte, []byte, error) {
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, errBadHash
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil ||
		p.MemoryKiB == 0 || p.Time == 0 || p.Threads == 0 {
		return Params{}, nil, nil, errBadHash
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, errBadHash
	}
	key, err := enc.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return Params{}, nil, nil, errBadHash
	}
	return p, salt, key, nil
}
