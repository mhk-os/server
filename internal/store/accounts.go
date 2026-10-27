package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/proto"
)

const accountColumns = `id, username, password_hash, country_ip, country_user, role, registration_ip, created_at,
	last_login_at, disabled_at, disabled_reason, disabled_by`

type rowScanner interface{ Scan(dest ...any) error }

func scanAccount(row rowScanner) (*Account, error) {
	var (
		a                     Account
		countryUser           sql.NullString
		regIP                 []byte
		lastLogin, disabledAt sql.NullTime
		disabledBy            sql.Null[uint64]
		role                  uint8
	)
	err := row.Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CountryIP, &countryUser, &role, &regIP, &a.CreatedAt,
		&lastLogin, &disabledAt, &a.DisabledReason, &disabledBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if countryUser.Valid {
		a.CountryUser = &countryUser.String
	}
	a.Role = proto.Role(role)
	a.RegistrationIP = ipFromBytes(regIP)
	a.CreatedAt = a.CreatedAt.UTC()
	a.LastLoginAt = nullTime(lastLogin)
	a.DisabledAt = nullTime(disabledAt)
	a.DisabledBy = nullUint(disabledBy)
	return &a, nil
}

// CreateAccount inserts a and sets its ID. ErrUsernameTaken on a case-insensitive clash.
func (s *Store) CreateAccount(ctx context.Context, a *Account) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO accounts (username, username_lower, password_hash, country_ip,
		country_user, role, registration_ip, created_at, last_login_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Username, strings.ToLower(a.Username), a.PasswordHash, a.CountryIP, toNull(a.CountryUser), uint8(a.Role),
		ipBytes(a.RegistrationIP), a.CreatedAt, toNull(a.LastLoginAt))
	if isDuplicate(err) {
		return ErrUsernameTaken
	}
	if err != nil {
		return fmt.Errorf("store: create account: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	a.ID = uint64(id)
	return nil
}

func (s *Store) AccountByID(ctx context.Context, id uint64) (*Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM accounts WHERE id = ?", id))
}

// AccountByUsername matches case-insensitively. Usernames are ASCII, so strings.ToLower equals the stored key.
func (s *Store) AccountByUsername(ctx context.Context, username string) (*Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx,
		"SELECT "+accountColumns+" FROM accounts WHERE username_lower = ?", strings.ToLower(username)))
}

// SetCountryUser sets the player's chosen country; nil returns to the IP-detected one.
func (s *Store) SetCountryUser(ctx context.Context, id uint64, country *string) error {
	return s.execOne(ctx, "UPDATE accounts SET country_user = ? WHERE id = ?", toNull(country), id)
}

func (s *Store) TouchLogin(ctx context.Context, id uint64, at time.Time) error {
	return s.execOne(ctx, "UPDATE accounts SET last_login_at = ? WHERE id = ?", at, id)
}

// TouchActive sets the account's last activity (login, resume, end of a logged-in connection).
func (s *Store) TouchActive(ctx context.Context, id uint64, at time.Time) error {
	return s.execOne(ctx, "UPDATE accounts SET last_active_at = ? WHERE id = ?", at, id)
}

// execOne runs a statement that must match exactly one row (Open enables ClientFoundRows, so unchanged rows count).
func (s *Store) execOne(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) accountExists(ctx context.Context, q queryer, id uint64) error {
	var one int
	err := q.QueryRowContext(ctx, "SELECT 1 FROM accounts WHERE id = ?", id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) ListAccounts(ctx context.Context, f AccountFilter) ([]Account, error) {
	query := "SELECT " + accountColumns + " FROM accounts"
	var args []any
	if f.Role != nil {
		query += " WHERE role = ?"
		args = append(args, uint8(*f.Role))
	}
	query += " ORDER BY id"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) SetRole(ctx context.Context, id uint64, role proto.Role, act Action) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.accountExists(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET role = ? WHERE id = ?", uint8(role), id); err != nil {
			return err
		}
		return recordAction(ctx, tx, act)
	})
}

// DisableAccount marks the account disabled with act.Reason and revokes all its sessions; connected clients are
// dropped by the server's periodic session check.
func (s *Store) DisableAccount(ctx context.Context, id uint64, act Action) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.accountExists(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE accounts SET disabled_at = ?, disabled_reason = ?, disabled_by = ? WHERE id = ?",
			act.CreatedAt, act.Reason, toNull(act.Actor), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET revoked_at = ? WHERE account_id = ? AND revoked_at IS NULL",
			act.CreatedAt, id); err != nil {
			return err
		}
		return recordAction(ctx, tx, act)
	})
}

func (s *Store) EnableAccount(ctx context.Context, id uint64, act Action) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.accountExists(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE accounts SET disabled_at = NULL, disabled_reason = '', disabled_by = NULL WHERE id = ?",
			id); err != nil {
			return err
		}
		return recordAction(ctx, tx, act)
	})
}

// DeleteAccount removes the account permanently; its sessions, runs, reports and cheat kicks cascade, the audit log
// keeps the entry. Runs the account reported get their flag counts recomputed. It returns the blobs of the deleted
// runs that no other run references, for the caller to remove after the commit.
func (s *Store) DeleteAccount(ctx context.Context, id uint64, act Action) ([]blobs.Hash, error) {
	var orphans []blobs.Hash
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.accountExists(ctx, tx, id); err != nil {
			return err
		}
		hashes, err := accountBlobs(ctx, tx, id)
		if err != nil {
			return err
		}
		reported, err := countedReportsBy(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id); err != nil {
			return err
		}
		if err := recountFlags(ctx, tx, reported); err != nil {
			return err
		}
		if orphans, err = unreferenced(ctx, tx, hashes); err != nil {
			return err
		}
		return recordAction(ctx, tx, act)
	})
	return orphans, err
}

func accountBlobs(ctx context.Context, tx *sql.Tx, accountID uint64) ([]blobs.Hash, error) {
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT blob_sha256 FROM runs WHERE account_id = ?", accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []blobs.Hash
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var h blobs.Hash
		copy(h[:], b)
		out = append(out, h)
	}
	return out, rows.Err()
}
