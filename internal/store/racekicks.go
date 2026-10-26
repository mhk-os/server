package store

import "context"

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
