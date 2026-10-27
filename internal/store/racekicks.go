package store

import (
	"context"
	"time"
)

// CheatReport is a row of cheat_reports: one CheatReport a race received.
type CheatReport struct {
	ID         uint64
	LobbyRef   string
	RaceRef    string
	ReporterID uint64
	SuspectID  uint64
	Reason     string // FlagReason name
	Detail     string
	Counted    bool   // counted towards a kick
	Note       string // why it did not count; "" when it did
	CreatedAt  time.Time
}

// CheatReportView is a cheat_reports row with the accounts' names.
type CheatReportView struct {
	CheatReport
	Reporter, Suspect string
}

// CheatKicked returns which of the accounts were ever kicked from a race for cheating: races ignore their reports.
func (s *Store) CheatKicked(ctx context.Context, accountIDs []uint64) (map[uint64]bool, error) {
	kicked := map[uint64]bool{}
	if len(accountIDs) == 0 {
		return kicked, nil
	}
	args := make([]any, len(accountIDs))
	for i, id := range accountIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT account_id FROM cheat_kicks WHERE account_id IN ("+
		placeholders(len(args))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		kicked[id] = true
	}
	return kicked, rows.Err()
}

// AddCheatReport stores a received cheat report.
func (s *Store) AddCheatReport(ctx context.Context, r *CheatReport) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO cheat_reports (lobby_ref, race_ref, reporter_id, suspect_id, reason,
		detail, counted, note, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.LobbyRef, r.RaceRef, r.ReporterID,
		r.SuspectID, r.Reason, r.Detail, r.Counted, r.Note, r.CreatedAt)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	r.ID = uint64(id)
	return err
}

// CheatReports lists cheat reports newest first: accountID 0 means every report, otherwise those by or about the
// account; limit 0 means no limit.
func (s *Store) CheatReports(ctx context.Context, accountID uint64, limit int) ([]CheatReportView, error) {
	q := `SELECT r.id, r.lobby_ref, r.race_ref, r.reporter_id, rep.username, r.suspect_id, sus.username, r.reason,
		r.detail, r.counted, r.note, r.created_at
		FROM cheat_reports r JOIN accounts rep ON rep.id = r.reporter_id JOIN accounts sus ON sus.id = r.suspect_id`
	var args []any
	if accountID != 0 {
		q += " WHERE r.reporter_id = ? OR r.suspect_id = ?"
		args = append(args, accountID, accountID)
	}
	q += " ORDER BY r.id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheatReportView
	for rows.Next() {
		var r CheatReportView
		if err := rows.Scan(&r.ID, &r.LobbyRef, &r.RaceRef, &r.ReporterID, &r.Reporter, &r.SuspectID, &r.Suspect,
			&r.Reason, &r.Detail, &r.Counted, &r.Note, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
