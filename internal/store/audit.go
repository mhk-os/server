package store

import (
	"context"
	"database/sql"
)

func recordAction(ctx context.Context, tx *sql.Tx, a Action) error {
	var ref sql.NullString
	if a.TargetRef != "" {
		ref = sql.NullString{String: a.TargetRef, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO moderation_actions
		(actor_account_id, action, target_account_id, target_ref, reason, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		toNull(a.Actor), a.Action, toNull(a.TargetAccount), ref, a.Reason, a.CreatedAt)
	return err
}

// AuditLog returns the newest limit actions, newest first.
func (s *Store) AuditLog(ctx context.Context, limit int) ([]Action, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, actor_account_id, action, target_account_id, target_ref, reason,
		created_at FROM moderation_actions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var (
			a             Action
			actor, target sql.Null[uint64]
			ref           sql.NullString
		)
		if err := rows.Scan(&a.ID, &actor, &a.Action, &target, &ref, &a.Reason, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Actor, a.TargetAccount, a.TargetRef = nullUint(actor), nullUint(target), ref.String
		a.CreatedAt = a.CreatedAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
