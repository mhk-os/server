// Package store keeps accounts, sessions, the moderation audit log and the time-trial boards, runs and reports in
// MariaDB (through the MySQL protocol driver). Callers depend on the small interfaces below rather than on *Store so
// later phases (and tests) can compose or replace them.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/proto"
)

var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrUsernameTaken means another account already uses the username (case-insensitively).
	ErrUsernameTaken = errors.New("store: username taken")
	// ErrRenameLimited means the account already changed its username as often as RenameAccount allows.
	ErrRenameLimited = errors.New("store: username change limit reached")
)

// TokenHash is the SHA-256 of a session token; the token itself is never stored.
type TokenHash [32]byte

// Account is a row of accounts.
type Account struct {
	ID             uint64
	Username       string
	PasswordHash   string
	CountryIP      string
	CountryUser    *string // nil = follow the IP-detected country
	Role           proto.Role
	RegistrationIP netip.Addr
	CreatedAt      time.Time
	LastLoginAt    *time.Time
	DisabledAt     *time.Time
	DisabledReason string
	DisabledBy     *uint64 // nil with DisabledAt set = disabled by the sysadmin CLI
}

// Country returns the effective country and whether it is the IP-detected one.
func (a *Account) Country() (code string, fromIP bool) {
	if a.CountryUser != nil {
		return *a.CountryUser, false
	}
	return a.CountryIP, true
}

// Disabled reports whether a moderator or the sysadmin disabled the account.
func (a *Account) Disabled() bool { return a.DisabledAt != nil }

// Proto returns the public view sent to clients.
func (a *Account) Proto() proto.Account {
	country, fromIP := a.Country()
	return proto.Account{
		ID:            a.ID,
		Username:      a.Username,
		Country:       country,
		CountryFromIP: fromIP,
		Role:          a.Role,
		Created:       a.CreatedAt,
	}
}

// Session is a row of sessions.
type Session struct {
	TokenHash  TokenHash
	AccountID  uint64
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
	IP         netip.Addr
	RevokedAt  *time.Time
}

// Valid reports whether the session can still be used at now.
func (s *Session) Valid(now time.Time) bool { return s.RevokedAt == nil && now.Before(s.ExpiresAt) }

// SessionStatus is the result of re-validating a live connection's session.
type SessionStatus struct {
	Valid          bool // exists, not revoked, not expired
	Disabled       bool // the account is disabled
	DisabledReason string
	Role           proto.Role
}

// Action is a row of moderation_actions, the audit log of every moderator and sysadmin action.
type Action struct {
	ID            uint64
	Actor         *uint64 // nil = sysadmin CLI
	Action        string
	TargetAccount *uint64
	TargetRef     string // "" = none; phase 2 uses it for run ids
	Reason        string
	CreatedAt     time.Time
}

// Audit action names.
const (
	ActionSetRole        = "account.set-role"
	ActionDisableAccount = "account.disable"
	ActionEnableAccount  = "account.enable"
	ActionDeleteAccount  = "account.delete"
	ActionRenameAccount  = "account.rename"
)

// AccountFilter narrows ListAccounts.
type AccountFilter struct {
	Role  *proto.Role
	Limit int // 0 = no limit
}

// Accounts is what the login flow and the player's own account requests need.
type Accounts interface {
	CreateAccount(ctx context.Context, a *Account) error
	AccountByID(ctx context.Context, id uint64) (*Account, error)
	AccountByUsername(ctx context.Context, username string) (*Account, error)
	SetCountryUser(ctx context.Context, id uint64, country *string) error
	TouchLogin(ctx context.Context, id uint64, at time.Time) error
	TouchActive(ctx context.Context, id uint64, at time.Time) error
	DeleteOwnAccount(ctx context.Context, id uint64, hideRuns bool, act Action) (anonymous string, err error)
	RenameAccount(ctx context.Context, id uint64, username string, limit RenameLimit, act Action) (*Account, error)
}

// Sessions persists login sessions.
type Sessions interface {
	CreateSession(ctx context.Context, s *Session) error
	SessionByHash(ctx context.Context, h TokenHash) (*Session, error)
	TouchSession(ctx context.Context, h TokenHash, ip netip.Addr, usedAt, expiresAt time.Time) error
	RevokeSession(ctx context.Context, h TokenHash, at time.Time) error
	// CheckSessions re-validates many sessions in one query; hashes missing from the result no longer exist.
	CheckSessions(ctx context.Context, hashes []TokenHash, now time.Time) (map[TokenHash]SessionStatus, error)
}

// Moderation mutates accounts on behalf of a moderator or the sysadmin. Every method writes the audit log in the
// same transaction as the change.
type Moderation interface {
	ListAccounts(ctx context.Context, f AccountFilter) ([]Account, error)
	SetRole(ctx context.Context, id uint64, role proto.Role, act Action) error
	DisableAccount(ctx context.Context, id uint64, act Action) error
	EnableAccount(ctx context.Context, id uint64, act Action) error
	// DeleteAccount returns the blobs of the account's runs that no other run references.
	DeleteAccount(ctx context.Context, id uint64, act Action) ([]blobs.Hash, error)
	AuditLog(ctx context.Context, limit int) ([]Action, error)
	CountActiveSessions(ctx context.Context, accountID uint64, now time.Time) (int, error)
}

// Store implements every interface on one MariaDB database.
type Store struct {
	db *sql.DB
}

var (
	_ Accounts   = (*Store)(nil)
	_ Sessions   = (*Store)(nil)
	_ Moderation = (*Store)(nil)
)

// Open connects to MariaDB. The DSN is normalized to parseTime=true, UTC and utf8mb4 so callers cannot get time
// zones or character sets wrong.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.ClientFoundRows = true
	cfg.Collation = "utf8mb4_unicode_ci"
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect to %s/%s: %w", cfg.Addr, cfg.DBName, err)
	}
	return &Store{db: db}, nil
}

// Close closes the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the pool for tests and maintenance.
func (s *Store) DB() *sql.DB { return s.db }

func ipBytes(ip netip.Addr) []byte {
	if !ip.IsValid() {
		return []byte{}
	}
	return ip.Unmap().AsSlice()
}

func ipFromBytes(b []byte) netip.Addr {
	ip, _ := netip.AddrFromSlice(b)
	return ip
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func nullUint(v sql.Null[uint64]) *uint64 {
	if !v.Valid {
		return nil
	}
	return &v.V
}

func toNull[T any](p *T) sql.Null[T] {
	if p == nil {
		return sql.Null[T]{}
	}
	return sql.Null[T]{V: *p, Valid: true}
}

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// inTx runs fn in a transaction, rolling back on error.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Now returns the current time at the millisecond precision the database keeps.
func Now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
