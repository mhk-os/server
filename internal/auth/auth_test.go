package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestValidateUsername(t *testing.T) {
	good := []string{"abc", "Hans_Huhn", "a.b-c", "x12345678901234567z", strings.Repeat("a", 20), "007", "a_1",
		"Moderator1"}
	for _, name := range good {
		if err := ValidateUsername(name); err != nil {
			t.Errorf("%q rejected: %v", name, err)
		}
	}
	bad := []string{"", "ab", strings.Repeat("a", 21), "_abc", "abc_", ".ab", "ab-", "a b", "äbc", "ab@c",
		"Admin", "MODERATOR", "mod", "mhk-os", "MhkOs", "root"}
	for _, name := range bad {
		if err := ValidateUsername(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("12345678", "user"); err != nil {
		t.Error(err)
	}
	// 8 code points but 16 bytes: allowed. 129 code points: refused.
	if err := ValidatePassword("ääääääää", "user"); err != nil {
		t.Error(err)
	}
	if err := ValidatePassword(strings.Repeat("ä", 128), "user"); err != nil {
		t.Error(err)
	}
	for _, pw := range []string{"1234567", strings.Repeat("ä", 129), "SuperHuhn", "superhuhn"} {
		err := ValidatePassword(pw, "superHUHN")
		if err == nil {
			t.Errorf("%q accepted", pw)
		} else if re, ok := err.(*RuleError); !ok || !re.Password {
			t.Errorf("%q: %v is not a password rule error", pw, err)
		}
	}
}

func TestHasher(t *testing.T) {
	h := NewHasher(DefaultParams, 2)
	ctx := context.Background()
	phc, err := h.Hash(ctx, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC %q", phc)
	}
	if ok, err := h.Verify(ctx, "correct horse", phc); err != nil || !ok {
		t.Fatalf("verify correct: %v %v", ok, err)
	}
	if ok, _ := h.Verify(ctx, "wrong horse", phc); ok {
		t.Fatal("wrong password verified")
	}
	if _, err := h.Verify(ctx, "x", "$argon2id$v=19$garbage"); err == nil {
		t.Fatal("malformed hash accepted")
	}
	// Parameters come from the stored hash, so cheaper hashes from older settings still verify.
	cheap := NewHasher(Params{MemoryKiB: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 1)
	old, _ := cheap.Hash(ctx, "pw")
	if ok, err := h.Verify(ctx, "pw", old); err != nil || !ok {
		t.Fatalf("verify with stored params: %v %v", ok, err)
	}
	h.DummyVerify(ctx, "anything")
}

func TestToken(t *testing.T) {
	a, ha := NewToken()
	b, hb := NewToken()
	if len(a) != 43 || a == b || ha == hb || HashToken(a) != ha {
		t.Fatalf("tokens %q %q", a, b)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(Rate{N: 3, Per: time.Minute})
	l.now = func() time.Time { return now }
	for i := range 3 {
		if !l.Allow("ip") {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if l.Allow("ip") || !l.Blocked("ip") {
		t.Fatal("4th attempt allowed")
	}
	if l.Blocked("other") {
		t.Fatal("keys are not independent")
	}
	now = now.Add(time.Minute)
	if l.Blocked("ip") {
		t.Fatal("window did not slide")
	}
	l.Add("x")
	now = now.Add(2 * time.Minute)
	l.Sweep()
	if len(l.events) != 0 {
		t.Fatalf("sweep left %d keys", len(l.events))
	}
	if !NewLimiter(Rate{}).Allow("k") {
		t.Fatal("zero rate limits")
	}
}
