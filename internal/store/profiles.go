package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/mhk-os/server/internal/proto"
)

// PlayerSummary is an account's public mini-profile (docs/protocol.md PlayerSummary) without online, which only
// the server's live connections know.
type PlayerSummary struct {
	AccountID  uint64
	Username   string
	Country    string // the effective country
	Role       proto.Role
	CreatedAt  time.Time
	LastActive *time.Time
	Score      uint32
	Rank       uint32 // 0 = unranked (score 0 or disabled)
	Disabled   bool
}

// Profile is an account's full profile. The moderator-only fields are always filled; callers blank them for players.
type Profile struct {
	PlayerSummary
	Listed         bool
	DisabledReason string
	Races          uint32
	Wins           uint32
	Podiums        uint32
	TTRuns         uint32 // runs a moderator did not hide (all visible unless the account is disabled)
	TTBoards       uint32 // boards with such a run
	TTBestRank     uint32 // best board rank, 0 = none (always 0 while disabled)
	CheatKicks     uint32
	ContentStrikes uint32
	HiddenRuns     uint32 // runs hidden by a moderator
	FlaggedRuns    uint32 // runs with at least one counted report
}

// PlayerQuery asks for one page of the players list.
type PlayerQuery struct {
	Offset, Limit int
	Search        string // case-insensitive username substring, "" = all
	ByName        bool   // username A–Z instead of newest registration first
}

// RaceResult is one human racer's result of an online race.
type RaceResult struct {
	AccountID uint64
	Points    uint32
	Win       bool // finished first
	Podium    bool // finished in the first three
}

// Profiles is the player directory and the online race score.
type Profiles interface {
	SetProfileListed(ctx context.Context, id uint64, listed bool) error
	// ListPlayers pages listed accounts that are not disabled.
	ListPlayers(ctx context.Context, q PlayerQuery) (total int, players []PlayerSummary, err error)
	// PlayerSummaries returns the summaries of the ids that exist, in no particular order.
	PlayerSummaries(ctx context.Context, ids []uint64, includeDisabled bool) ([]PlayerSummary, error)
	Profile(ctx context.Context, id uint64) (*Profile, error)
	// ScoreLeaderboard pages ranked accounts by rank, then username; total counts the rows matching search.
	ScoreLeaderboard(ctx context.Context, offset, limit int, search string) (total int, players []PlayerSummary,
		err error)
	// RecordRace adds one online race to the score and counters of its human racers, in one transaction.
	RecordRace(ctx context.Context, results []RaceResult) error
}

var _ Profiles = (*Store)(nil)

// ranked is the condition for accounts on the score ranking; queries alias accounts a.
const ranked = "a.score > 0 AND a.disabled_at IS NULL"

// summarySelect computes the rank per row; accounts_score (score, disabled_at) makes the count an index range scan.
const summarySelect = `SELECT a.id, a.username, COALESCE(a.country_user, a.country_ip), a.role, a.created_at,
	a.last_active_at, a.score, IF(` + ranked + `, 1 + (SELECT COUNT(*) FROM accounts h
		WHERE h.score > a.score AND h.disabled_at IS NULL), 0),
	a.disabled_at IS NOT NULL`

func scanSummary(row rowScanner, extra ...any) (*PlayerSummary, error) {
	var (
		p          PlayerSummary
		role       uint8
		lastActive sql.NullTime
	)
	err := row.Scan(append([]any{&p.AccountID, &p.Username, &p.Country, &role, &p.CreatedAt, &lastActive, &p.Score,
		&p.Rank, &p.Disabled}, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Role = proto.Role(role)
	p.CreatedAt = p.CreatedAt.UTC()
	p.LastActive = nullTime(lastActive)
	return &p, nil
}

func (s *Store) querySummaries(ctx context.Context, query string, args ...any) ([]PlayerSummary, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayerSummary
	for rows.Next() {
		p, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// summaryPage counts the accounts matching where and returns the requested page of them in order.
func (s *Store) summaryPage(ctx context.Context, where, order string, offset, limit int, args ...any) (int,
	[]PlayerSummary, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts a WHERE "+where, args...).
		Scan(&total); err != nil {
		return 0, nil, err
	}
	players, err := s.querySummaries(ctx, summarySelect+" FROM accounts a WHERE "+where+" ORDER BY "+order+
		" LIMIT ? OFFSET ?", append(args, limit, offset)...)
	return total, players, err
}

// searchCond appends the username substring condition for a non-empty search.
func searchCond(where string, args []any, search string) (string, []any) {
	if search == "" {
		return where, args
	}
	return where + " AND a.username_lower LIKE ?", append(args, "%"+likeEscape(strings.ToLower(search))+"%")
}

func (s *Store) SetProfileListed(ctx context.Context, id uint64, listed bool) error {
	return s.execOne(ctx, "UPDATE accounts SET profile_listed = ? WHERE id = ?", listed, id)
}

func (s *Store) ListPlayers(ctx context.Context, q PlayerQuery) (int, []PlayerSummary, error) {
	where, args := searchCond("a.profile_listed AND a.disabled_at IS NULL", nil, q.Search)
	order := "a.id DESC"
	if q.ByName {
		order = "a.username_lower, a.id"
	}
	return s.summaryPage(ctx, where, order, q.Offset, q.Limit, args...)
}

func (s *Store) PlayerSummaries(ctx context.Context, ids []uint64, includeDisabled bool) ([]PlayerSummary, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := summarySelect + " FROM accounts a WHERE a.id IN (" + placeholders(len(ids)) + ")"
	if !includeDisabled {
		query += " AND a.disabled_at IS NULL"
	}
	return s.querySummaries(ctx, query, args...)
}

func (s *Store) Profile(ctx context.Context, id uint64) (*Profile, error) {
	var p Profile
	sum, err := scanSummary(s.db.QueryRowContext(ctx, summarySelect+`, a.profile_listed, a.disabled_reason,
		a.races, a.wins, a.podiums,
		(SELECT COUNT(*) FROM runs WHERE account_id = a.id AND hidden_at IS NULL),
		(SELECT COUNT(DISTINCT board_id) FROM runs WHERE account_id = a.id AND hidden_at IS NULL),
		(SELECT COUNT(*) FROM cheat_kicks WHERE account_id = a.id),
		(SELECT COUNT(*) FROM content_strikes WHERE account_id = a.id),
		(SELECT COUNT(*) FROM runs WHERE account_id = a.id AND hidden_at IS NOT NULL),
		(SELECT COUNT(*) FROM runs WHERE account_id = a.id AND flags > 0)
		FROM accounts a WHERE a.id = ?`, id),
		&p.Listed, &p.DisabledReason, &p.Races, &p.Wins, &p.Podiums, &p.TTRuns, &p.TTBoards, &p.CheatKicks,
		&p.ContentStrikes, &p.HiddenRuns, &p.FlaggedRuns)
	if err != nil {
		return nil, err
	}
	p.PlayerSummary = *sum
	if p.TTRuns > 0 && !p.Disabled {
		if p.TTBestRank, err = s.bestBoardRank(ctx, id); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// bestBoardRank returns the account's best rank over every board it has a visible run on, ranked as rankOnBoards
// does: each account's best visible run per board, fastest first, ties by earlier upload.
func (s *Store) bestBoardRank(ctx context.Context, accountID uint64) (uint32, error) {
	var rank uint32
	err := s.db.QueryRowContext(ctx, `WITH best AS (
			SELECT r.id, r.board_id, r.account_id, r.finish_ms, r.uploaded_at,
				ROW_NUMBER() OVER (PARTITION BY r.board_id, r.account_id ORDER BY `+rankOrder+`) AS rn
			FROM runs r JOIN accounts a ON a.id = r.account_id
			WHERE r.board_id IN (SELECT board_id FROM runs WHERE account_id = ? AND hidden_at IS NULL) AND `+visible+`
		), ranks AS (
			SELECT account_id, ROW_NUMBER() OVER (PARTITION BY board_id ORDER BY `+rankOrder+`) AS rnk
			FROM best WHERE rn = 1
		)
		SELECT COALESCE(MIN(rnk), 0) FROM ranks WHERE account_id = ?`, accountID, accountID).Scan(&rank)
	return rank, err
}

func (s *Store) ScoreLeaderboard(ctx context.Context, offset, limit int, search string) (int, []PlayerSummary,
	error) {
	where, args := searchCond(ranked, nil, search)
	return s.summaryPage(ctx, where, "a.score DESC, a.username_lower, a.id", offset, limit, args...)
}

func (s *Store) RecordRace(ctx context.Context, results []RaceResult) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, r := range results {
			// A racer deleted meanwhile matches no row: nothing to record.
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET score = score + ?, races = races + 1,
				wins = wins + ?, podiums = podiums + ? WHERE id = ?`,
				r.Points, r.Win, r.Podium, r.AccountID); err != nil {
				return err
			}
		}
		return nil
	})
}
