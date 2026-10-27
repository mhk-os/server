package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/mhk-os/server/internal/proto"
)

// ErrAchievementLimit means a sync would leave the account with more achievement data than proto.MaxAccountStats,
// MaxAccountSetItems or MaxAccountUnlocked allow; nothing of it was stored.
var ErrAchievementLimit = errors.New("store: achievement limit reached")

// AchievementData is an account's achievement progress. As SyncAchievements input, Counters hold deltas and Sets
// the items to add.
type AchievementData struct {
	Counters []StatCount
	Maxima   []StatCount
	Sets     []StatItems
	Unlocked []Unlock
}

// StatCount is the value of a counter or maximum stat.
type StatCount struct {
	Stat  string
	Value uint64
}

// StatItems is the items of a set stat.
type StatItems struct {
	Stat  string
	Items []string
}

// Unlock is an unlocked achievement.
type Unlock struct {
	ID string
	At time.Time
}

// AchievementShare is how many players unlocked an achievement.
type AchievementShare struct {
	ID       string
	Unlocked int
}

// AchievementSummary is what the sysadmin sees of an account's achievements.
type AchievementSummary struct {
	Stats    int // counters, maxima and set stats
	SetItems int
	Unlocked int
	SyncedAt *time.Time
}

// Achievements stores the progress the game syncs.
type Achievements interface {
	// SyncAchievements merges in into the account (counters add, maxima keep the highest, sets unite, unlocks keep
	// the earliest time) and returns the merged progress. ErrNotFound for an unknown account, ErrAchievementLimit
	// when the result would exceed the caps.
	SyncAchievements(ctx context.Context, accountID uint64, in *AchievementData, now time.Time) (*AchievementData,
		error)
	// AchievementShares counts the players (accounts that synced, neither disabled nor anonymous) and how many of
	// them unlocked each of ids, in the order asked (duplicates once, 0 when nobody did). Without ids it returns
	// the limit most unlocked achievements.
	AchievementShares(ctx context.Context, ids []string, limit int) (players int, shares []AchievementShare,
		err error)
	AchievementSummary(ctx context.Context, accountID uint64) (*AchievementSummary, error)
}

var _ Achievements = (*Store)(nil)

// insertChunk bounds the rows of one multi-row INSERT.
const insertChunk = 500

// bulkInsert runs head + "VALUES (?,…),…" + tail for rows of width values each, insertChunk rows per statement.
func bulkInsert(ctx context.Context, tx *sql.Tx, head, tail string, width int, rows [][]any) error {
	tuple := "(" + placeholders(width) + ")"
	for len(rows) > 0 {
		n := min(len(rows), insertChunk)
		args := make([]any, 0, n*width)
		for _, r := range rows[:n] {
			args = append(args, r...)
		}
		query := head + " VALUES " + tuple + strings.Repeat(","+tuple, n-1) + " " + tail
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}

func (s *Store) SyncAchievements(ctx context.Context, accountID uint64, in *AchievementData, now time.Time) (
	*AchievementData, error) {
	var out *AchievementData
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Locks the account row, so syncs of one account (two connections) apply one after the other.
		res, err := tx.ExecContext(ctx, "UPDATE accounts SET achievements_synced_at = ? WHERE id = ?", now, accountID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		if err := mergeAchievements(ctx, tx, accountID, in); err != nil {
			return err
		}
		var stats, items, unlocked int
		if err := tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM achievement_counters WHERE account_id = ?) +
			(SELECT COUNT(*) FROM achievement_maxima WHERE account_id = ?) +
			(SELECT COUNT(DISTINCT stat) FROM achievement_set_items WHERE account_id = ?),
			(SELECT COUNT(*) FROM achievement_set_items WHERE account_id = ?),
			(SELECT COUNT(*) FROM achievement_unlocks WHERE account_id = ?)`,
			accountID, accountID, accountID, accountID, accountID).Scan(&stats, &items, &unlocked); err != nil {
			return err
		}
		if stats > proto.MaxAccountStats || items > proto.MaxAccountSetItems || unlocked > proto.MaxAccountUnlocked {
			return ErrAchievementLimit
		}
		out, err = loadAchievements(ctx, tx, accountID)
		return err
	})
	return out, err
}

func mergeAchievements(ctx context.Context, tx *sql.Tx, accountID uint64, in *AchievementData) error {
	var rows [][]any
	for _, c := range in.Counters {
		if c.Value > 0 {
			rows = append(rows, []any{accountID, c.Stat, c.Value})
		}
	}
	if err := bulkInsert(ctx, tx, "INSERT INTO achievement_counters (account_id, stat, value)",
		"ON DUPLICATE KEY UPDATE value = value + VALUES(value)", 3, rows); err != nil {
		return err
	}
	rows = rows[:0]
	for _, m := range in.Maxima {
		rows = append(rows, []any{accountID, m.Stat, m.Value})
	}
	if err := bulkInsert(ctx, tx, "INSERT INTO achievement_maxima (account_id, stat, value)",
		"ON DUPLICATE KEY UPDATE value = GREATEST(value, VALUES(value))", 3, rows); err != nil {
		return err
	}
	rows = rows[:0]
	for _, set := range in.Sets {
		for _, item := range set.Items {
			rows = append(rows, []any{accountID, set.Stat, item})
		}
	}
	if err := bulkInsert(ctx, tx, "INSERT INTO achievement_set_items (account_id, stat, item)",
		"ON DUPLICATE KEY UPDATE item = item", 3, rows); err != nil {
		return err
	}
	rows = rows[:0]
	for _, u := range in.Unlocked {
		rows = append(rows, []any{accountID, u.ID, u.At})
	}
	return bulkInsert(ctx, tx, "INSERT INTO achievement_unlocks (account_id, achievement_id, unlocked_at)",
		"ON DUPLICATE KEY UPDATE unlocked_at = LEAST(unlocked_at, VALUES(unlocked_at))", 3, rows)
}

// loadAchievements reads the account's progress, every list ordered by id.
func loadAchievements(ctx context.Context, tx *sql.Tx, accountID uint64) (*AchievementData, error) {
	out := &AchievementData{}
	counts := func(table string) ([]StatCount, error) {
		rows, err := tx.QueryContext(ctx, "SELECT stat, value FROM "+table+" WHERE account_id = ? ORDER BY stat",
			accountID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []StatCount{}
		for rows.Next() {
			var c StatCount
			if err := rows.Scan(&c.Stat, &c.Value); err != nil {
				return nil, err
			}
			list = append(list, c)
		}
		return list, rows.Err()
	}
	var err error
	if out.Counters, err = counts("achievement_counters"); err != nil {
		return nil, err
	}
	if out.Maxima, err = counts("achievement_maxima"); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT stat, item FROM achievement_set_items WHERE account_id = ? ORDER BY stat, item", accountID)
	if err != nil {
		return nil, err
	}
	out.Sets = []StatItems{}
	for rows.Next() {
		var stat, item string
		if err := rows.Scan(&stat, &item); err != nil {
			rows.Close()
			return nil, err
		}
		if n := len(out.Sets); n == 0 || out.Sets[n-1].Stat != stat {
			out.Sets = append(out.Sets, StatItems{Stat: stat})
		}
		last := &out.Sets[len(out.Sets)-1]
		last.Items = append(last.Items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT achievement_id, unlocked_at FROM achievement_unlocks
		WHERE account_id = ? ORDER BY achievement_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out.Unlocked = []Unlock{}
	for rows.Next() {
		var u Unlock
		if err := rows.Scan(&u.ID, &u.At); err != nil {
			return nil, err
		}
		u.At = u.At.UTC()
		out.Unlocked = append(out.Unlocked, u)
	}
	return out, rows.Err()
}

// sharePlayer is the condition for accounts counted by AchievementShares; queries alias accounts a.
const sharePlayer = "a.disabled_at IS NULL AND a.role <> 2"

func (s *Store) AchievementShares(ctx context.Context, ids []string, limit int) (int, []AchievementShare, error) {
	var players int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts a WHERE a.achievements_synced_at IS NOT NULL AND "+
		sharePlayer).Scan(&players); err != nil {
		return 0, nil, err
	}
	var asked []string
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			asked = append(asked, id)
		}
	}
	query := "SELECT u.achievement_id, COUNT(*) FROM achievement_unlocks u JOIN accounts a ON a.id = u.account_id WHERE " +
		sharePlayer
	var args []any
	if len(asked) > 0 {
		query += " AND u.achievement_id IN (" + placeholders(len(asked)) + ") GROUP BY u.achievement_id"
		for _, id := range asked {
			args = append(args, id)
		}
	} else {
		query += " GROUP BY u.achievement_id ORDER BY COUNT(*) DESC, u.achievement_id LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	shares := []AchievementShare{}
	counts := map[string]int{}
	for rows.Next() {
		var sh AchievementShare
		if err := rows.Scan(&sh.ID, &sh.Unlocked); err != nil {
			return 0, nil, err
		}
		shares = append(shares, sh)
		counts[sh.ID] = sh.Unlocked
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(asked) == 0 {
		return players, shares, nil
	}
	shares = shares[:0]
	for _, id := range asked {
		shares = append(shares, AchievementShare{ID: id, Unlocked: counts[id]})
	}
	return players, shares, nil
}

func (s *Store) AchievementSummary(ctx context.Context, accountID uint64) (*AchievementSummary, error) {
	var (
		sum    AchievementSummary
		synced sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `SELECT a.achievements_synced_at,
		(SELECT COUNT(*) FROM achievement_counters WHERE account_id = a.id) +
		(SELECT COUNT(*) FROM achievement_maxima WHERE account_id = a.id) +
		(SELECT COUNT(DISTINCT stat) FROM achievement_set_items WHERE account_id = a.id),
		(SELECT COUNT(*) FROM achievement_set_items WHERE account_id = a.id),
		(SELECT COUNT(*) FROM achievement_unlocks WHERE account_id = a.id)
		FROM accounts a WHERE a.id = ?`, accountID).Scan(&synced, &sum.Stats, &sum.SetItems, &sum.Unlocked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sum.SyncedAt = nullTime(synced)
	return &sum, nil
}
