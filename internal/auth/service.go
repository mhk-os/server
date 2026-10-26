package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/mhk-os/server/internal/store"
)

// SessionTTL is the sliding session lifetime: every Resume extends it to now + SessionTTL.
const SessionTTL = 90 * 24 * time.Hour

var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrUsernameTaken      = errors.New("auth: username taken")
	ErrRateLimited        = errors.New("auth: rate limited")
	ErrSessionInvalid     = errors.New("auth: session invalid")
)

// DisabledError means the account is disabled; Reason is shown to the player.
type DisabledError struct{ Reason string }

func (e *DisabledError) Error() string { return "auth: account disabled: " + e.Reason }

// Limits are the rate limits of the account flows.
type Limits struct {
	Register         Rate // successful registrations per IP
	LoginFailIP      Rate // failed logins per IP
	LoginFailAccount Rate // failed logins per username (case-insensitive)
}

// DefaultLimits are the limits from docs/protocol.md.
var DefaultLimits = Limits{
	Register:         Rate{3, time.Hour},
	LoginFailIP:      Rate{10, 10 * time.Minute},
	LoginFailAccount: Rate{10, 10 * time.Minute},
}

// Service implements register, login, resume and logout.
type Service struct {
	accounts store.Accounts
	sessions store.Sessions
	hasher   *Hasher

	register      *Limiter
	loginFailIP   *Limiter
	loginFailUser *Limiter
}

// NewService wires the flows to their storage.
func NewService(accounts store.Accounts, sessions store.Sessions, hasher *Hasher, limits Limits) *Service {
	return &Service{
		accounts:      accounts,
		sessions:      sessions,
		hasher:        hasher,
		register:      NewLimiter(limits.Register),
		loginFailIP:   NewLimiter(limits.LoginFailIP),
		loginFailUser: NewLimiter(limits.LoginFailAccount),
	}
}

// Sweep drops idle rate limiter entries; call it periodically.
func (s *Service) Sweep() {
	s.register.Sweep()
	s.loginFailIP.Sweep()
	s.loginFailUser.Sweep()
}

// Login is the result of a successful register, login or resume.
type Login struct {
	Account *store.Account
	Token   string
	Hash    store.TokenHash
	Expires time.Time
}

// NewToken returns a random session token (32 bytes, base64url) and its SHA-256, the only form stored.
func NewToken() (string, store.TokenHash) {
	var raw [32]byte
	rand.Read(raw[:])
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	return token, HashToken(token)
}

// HashToken returns the stored form of a token.
func HashToken(token string) store.TokenHash { return sha256.Sum256([]byte(token)) }

func ipKey(ip netip.Addr) string { return ip.Unmap().String() }

// Register creates an account and logs it in. country is the IP-detected country ("" when unknown).
func (s *Service) Register(ctx context.Context, ip netip.Addr, country, username, password string) (*Login, error) {
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	if err := ValidatePassword(password, username); err != nil {
		return nil, err
	}
	if s.register.Blocked(ipKey(ip)) {
		return nil, ErrRateLimited
	}
	// Cheap pre-check so a taken name does not cost a hash; CreateAccount still catches races.
	if _, err := s.accounts.AccountByUsername(ctx, username); err == nil {
		return nil, ErrUsernameTaken
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return nil, err
	}
	now := store.Now()
	acct := &store.Account{
		Username:       username,
		PasswordHash:   hash,
		CountryIP:      country,
		RegistrationIP: ip,
		CreatedAt:      now,
		LastLoginAt:    &now,
	}
	if err := s.accounts.CreateAccount(ctx, acct); err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	s.register.Add(ipKey(ip))
	return s.newSession(ctx, acct, ip, now)
}

// Login checks credentials and creates a new session.
func (s *Service) Login(ctx context.Context, ip netip.Addr, username, password string) (*Login, error) {
	userKey := strings.ToLower(username)
	if s.loginFailIP.Blocked(ipKey(ip)) || s.loginFailUser.Blocked(userKey) {
		return nil, ErrRateLimited
	}
	fail := func() (*Login, error) {
		s.loginFailIP.Add(ipKey(ip))
		s.loginFailUser.Add(userKey)
		return nil, ErrInvalidCredentials
	}
	acct, err := s.accounts.AccountByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		s.hasher.DummyVerify(ctx, password)
		return fail()
	}
	if err != nil {
		return nil, err
	}
	ok, err := s.hasher.Verify(ctx, password, acct.PasswordHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return fail()
	}
	if acct.Disabled() {
		return nil, &DisabledError{Reason: acct.DisabledReason}
	}
	now := store.Now()
	if err := s.accounts.TouchLogin(ctx, acct.ID, now); err != nil {
		return nil, err
	}
	acct.LastLoginAt = &now
	return s.newSession(ctx, acct, ip, now)
}

func (s *Service) newSession(ctx context.Context, acct *store.Account, ip netip.Addr, now time.Time) (*Login, error) {
	token, hash := NewToken()
	se := &store.Session{
		TokenHash:  hash,
		AccountID:  acct.ID,
		CreatedAt:  now,
		LastUsedAt: now,
		ExpiresAt:  now.Add(SessionTTL),
		IP:         ip,
	}
	if err := s.sessions.CreateSession(ctx, se); err != nil {
		return nil, err
	}
	return &Login{Account: acct, Token: token, Hash: hash, Expires: se.ExpiresAt}, nil
}

// Resume continues a session from its token and slides its expiry.
func (s *Service) Resume(ctx context.Context, ip netip.Addr, token string) (*Login, error) {
	hash := HashToken(token)
	se, err := s.sessions.SessionByHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	now := store.Now()
	if !se.Valid(now) {
		return nil, ErrSessionInvalid
	}
	acct, err := s.accounts.AccountByID(ctx, se.AccountID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if acct.Disabled() {
		return nil, &DisabledError{Reason: acct.DisabledReason}
	}
	expires := now.Add(SessionTTL)
	if err := s.sessions.TouchSession(ctx, hash, ip, now, expires); err != nil {
		return nil, err
	}
	return &Login{Account: acct, Token: token, Hash: hash, Expires: expires}, nil
}

// Logout revokes a session.
func (s *Service) Logout(ctx context.Context, hash store.TokenHash) error {
	return s.sessions.RevokeSession(ctx, hash, store.Now())
}
