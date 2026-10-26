package store

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/mhk-os/server/internal/proto"
)

func (s *Store) CreateSession(ctx context.Context, se *Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(token_hash, account_id, created_at, last_used_at, expires_at, ip) VALUES (?, ?, ?, ?, ?, ?)`,
		se.TokenHash[:], se.AccountID, se.CreatedAt, se.LastUsedAt, se.ExpiresAt, ipBytes(se.IP))
	return err
}

func (s *Store) SessionByHash(ctx context.Context, h TokenHash) (*Session, error) {
	var (
		se      Session
		hash    []byte
		ip      []byte
		revoked sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `SELECT token_hash, account_id, created_at, last_used_at, expires_at, ip,
		revoked_at FROM sessions WHERE token_hash = ?`, h[:]).
		Scan(&hash, &se.AccountID, &se.CreatedAt, &se.LastUsedAt, &se.ExpiresAt, &ip, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	copy(se.TokenHash[:], hash)
	se.IP = ipFromBytes(ip)
	se.CreatedAt, se.LastUsedAt, se.ExpiresAt = se.CreatedAt.UTC(), se.LastUsedAt.UTC(), se.ExpiresAt.UTC()
	se.RevokedAt = nullTime(revoked)
	return &se, nil
}

// TouchSession records a use of the session and slides its expiry.
func (s *Store) TouchSession(ctx context.Context, h TokenHash, ip netip.Addr, usedAt, expiresAt time.Time) error {
	return s.execOne(ctx, "UPDATE sessions SET last_used_at = ?, expires_at = ?, ip = ? WHERE token_hash = ?",
		usedAt, expiresAt, ipBytes(ip), h[:])
}

func (s *Store) RevokeSession(ctx context.Context, h TokenHash, at time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL",
		at, h[:])
	return err
}

func (s *Store) CheckSessions(ctx context.Context, hashes []TokenHash, now time.Time) (map[TokenHash]SessionStatus, error) {
	out := make(map[TokenHash]SessionStatus, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(hashes)+1)
	args = append(args, now)
	for _, h := range hashes {
		args = append(args, h[:])
	}
	query := `SELECT s.token_hash, s.revoked_at IS NULL AND s.expires_at > ?, a.disabled_at IS NOT NULL,
		a.disabled_reason, a.role
		FROM sessions s JOIN accounts a ON a.id = s.account_id
		WHERE s.token_hash IN (?` + strings.Repeat(",?", len(hashes)-1) + ")"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			hash []byte
			st   SessionStatus
			role uint8
		)
		if err := rows.Scan(&hash, &st.Valid, &st.Disabled, &st.DisabledReason, &role); err != nil {
			return nil, err
		}
		st.Role = proto.Role(role)
		var h TokenHash
		copy(h[:], hash)
		out[h] = st
	}
	return out, rows.Err()
}

// CountActiveSessions counts unrevoked, unexpired sessions of an account (shown by the admin CLI).
func (s *Store) CountActiveSessions(ctx context.Context, accountID uint64, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions WHERE account_id = ? AND revoked_at IS NULL AND expires_at > ?",
		accountID, now).Scan(&n)
	return n, err
}

// DeleteExpiredSessions removes sessions that expired or were revoked before cutoff, keeping the table small.
func (s *Store) DeleteExpiredSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at < ? OR revoked_at < ?", cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
