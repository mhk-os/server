package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mhk-os/server/internal/blobs"
	"github.com/mhk-os/server/internal/geo"
	"github.com/mhk-os/server/internal/proto"
)

// ErrGhostExists means a run with the same ghost id is already stored.
var ErrGhostExists = errors.New("store: ghost already uploaded")

// Board is a row of boards with the statistics of its visible runs.
type Board struct {
	ID             uint64
	TrackID        string
	EngineClass    uint8
	Laps           uint8
	ContentVersion int32
	TrackHash      string
	CreatedAt      time.Time
	Players        uint32     // accounts with a visible run
	BestMs         int32      // fastest visible finish, 0 = none
	LastUpload     *time.Time // newest visible upload
}

// BoardFilter narrows ListBoards; zero fields match everything.
type BoardFilter struct {
	TrackID        string
	EngineClass    *uint8
	ContentVersion int32
	Limit          int
}

// Run is a row of runs: one uploaded ghost and the summary the server validated.
type Run struct {
	ID             uint64
	BoardID        uint64
	AccountID      uint64
	GhostID        string
	TrackID        string
	TrackPackID    string
	KartID         string
	CharacterID    string
	EngineClass    uint8
	Laps           uint8
	ContentVersion int32
	TrackHash      string
	SetupHash      string
	GameBuild      string
	FinishMs       int32
	BestLapMs      int32
	LapMs          []int32
	FrameCount     int32
	RecordedAt     time.Time
	UploadedAt     time.Time
	Blob           blobs.Hash
	FileSize       uint32
	Flags          uint32
	HiddenAt       *time.Time
	HiddenBy       *uint64
	HiddenReason   string
	DeletedByOwner bool // hidden because the player deleted their account and runs
}

// RunView is a run with what clients see next to it: owner, board, rank and setup mismatch.
type RunView struct {
	Run
	Username        string
	Country         string // the owner's effective country
	AccountDisabled bool
	BoardTrackID    string
	Rank            uint32 // rank on the board (or in the requested list), 0 = not ranked
	SetupMismatch   bool
}

// Hidden reports whether players no longer see the run (hidden by a moderator or the owner is disabled).
func (v *RunView) Hidden() bool { return v.HiddenAt != nil || v.AccountDisabled }

// RunFilter narrows ListRuns (newest upload first); zero fields match everything.
type RunFilter struct {
	AccountID uint64
	TrackID   string
	Limit     int
}

// LeaderboardQuery asks for one page of a board: the best visible run per account matching the filter.
type LeaderboardQuery struct {
	BoardID       uint64
	Offset, Limit int
	Username      string // case-insensitive substring
	Country       string
	Continent     string
	KartID        string
	CharacterID   string
	IncludeHidden bool   // add every hidden run (rank 0) to the list; moderators only
	Mine          uint64 // account whose own best row is returned separately, 0 = none
}

// LeaderboardPage is the answer to a LeaderboardQuery.
type LeaderboardPage struct {
	Total   int
	Entries []RunView
	Mine    *RunView
}

// Report is a row of run_reports with the reporter's name.
type Report struct {
	ID         uint64
	RunID      uint64
	ReporterID uint64
	Reporter   string
	Reason     proto.FlagReason
	AtSeconds  float32
	Detail     string
	CreatedAt  time.Time
	Counted    bool
}

// CheatKick is a row of cheat_kicks (phase 3 writes them; reports of kicked accounts are never counted).
type CheatKick struct {
	ID        uint64
	AccountID uint64
	LobbyRef  string
	RaceRef   string
	Reason    string
	Reporters string
	CreatedAt time.Time
}

// AccountStats counts what moderators see about an account.
type AccountStats struct {
	Runs       int
	HiddenRuns int
	CheatKicks int
}

// Runs is the time-trial storage.
type Runs interface {
	CreateRun(ctx context.Context, r *Run) error
	RunByID(ctx context.Context, id uint64) (*RunView, error)
	RunByGhostID(ctx context.Context, ghostID string) (*RunView, error)
	CountUploadsSince(ctx context.Context, accountID uint64, since time.Time) (int, error)
	ListBoards(ctx context.Context, f BoardFilter) ([]Board, error)
	BoardByID(ctx context.Context, id uint64) (*Board, error)
	Leaderboard(ctx context.Context, q LeaderboardQuery) (*LeaderboardPage, error)
	ListRuns(ctx context.Context, f RunFilter) ([]RunView, error)
	FlaggedRuns(ctx context.Context, offset, limit int, includeHidden bool) (total int, runs []RunView, err error)
	PlayerRuns(ctx context.Context, accountID uint64, offset, limit int, includeHidden bool) (total int,
		runs []RunView, err error)
	// ListHiddenRuns pages the runs players do not see; boardID 0 = every board.
	ListHiddenRuns(ctx context.Context, boardID uint64, offset, limit int) (total int, runs []RunView, err error)
	ReportRun(ctx context.Context, rep *Report) (flags uint32, inserted bool, err error)
	RunReports(ctx context.Context, runID uint64, limit int) ([]Report, error)
	HideRun(ctx context.Context, runID uint64, act Action) error
	UnhideRun(ctx context.Context, runID uint64, act Action) error
	// DeleteRun removes the run and its reports and returns its blob when no other run references it.
	DeleteRun(ctx context.Context, runID uint64, act Action) (orphans []blobs.Hash, err error)
	AccountStats(ctx context.Context, accountID uint64) (AccountStats, error)
	AddCheatKick(ctx context.Context, k *CheatKick) error
	// RunHiddenInfo returns the run's owner and why it is hidden, if it is.
	RunHiddenInfo(ctx context.Context, runID uint64) (*HiddenInfo, error)
}

var _ Runs = (*Store)(nil)

// Audit action names for runs.
const (
	ActionHideRun   = "run.hide"
	ActionUnhideRun = "run.unhide"
	ActionDeleteRun = "run.delete"
)

// visible is the condition for runs players see; queries alias runs r and accounts a.
const visible = "r.hidden_at IS NULL AND a.disabled_at IS NULL"

// shownName and shownCountry are the owner's name and country as clients see them: anonymous accounts (role 2)
// show as AnonymousName without a country. Queries alias accounts a.
const (
	shownName    = "IF(a.role = 2, '" + AnonymousName + "', a.username)"
	shownCountry = "IF(a.role = 2, '', COALESCE(a.country_user, a.country_ip))"
)

// rankOrder orders runs of a board: fastest first, ties by earlier upload.
const rankOrder = "finish_ms, uploaded_at, id"

const runSelect = `SELECT r.id, r.board_id, r.account_id, r.ghost_id, r.track_id, r.track_pack_id, r.kart_id,
	r.character_id, r.engine_class, r.laps, r.content_version, r.track_hash, r.setup_hash, r.game_build, r.finish_ms,
	r.best_lap_ms, r.lap_ms, r.frame_count, r.recorded_at, r.uploaded_at, r.blob_sha256, r.file_size, r.flags,
	r.hidden_at, r.hidden_by, r.hidden_reason, r.deleted_by_owner, ` + shownName + `, ` + shownCountry + `,
	a.disabled_at IS NOT NULL, b.track_id
	FROM runs r JOIN accounts a ON a.id = r.account_id JOIN boards b ON b.id = r.board_id`

func scanRun(row rowScanner) (*RunView, error) {
	var (
		v        RunView
		lapMs    string
		blob     []byte
		hiddenAt sql.NullTime
		hiddenBy sql.Null[uint64]
	)
	err := row.Scan(&v.ID, &v.BoardID, &v.AccountID, &v.GhostID, &v.TrackID, &v.TrackPackID, &v.KartID,
		&v.CharacterID, &v.EngineClass, &v.Laps, &v.ContentVersion, &v.TrackHash, &v.SetupHash, &v.GameBuild, &v.FinishMs,
		&v.BestLapMs, &lapMs, &v.FrameCount, &v.RecordedAt, &v.UploadedAt, &blob, &v.FileSize, &v.Flags, &hiddenAt,
		&hiddenBy, &v.HiddenReason, &v.DeletedByOwner, &v.Username, &v.Country, &v.AccountDisabled, &v.BoardTrackID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if v.LapMs, err = parseLapMs(lapMs); err != nil {
		return nil, fmt.Errorf("store: run %d: %w", v.ID, err)
	}
	copy(v.Blob[:], blob)
	v.RecordedAt, v.UploadedAt = v.RecordedAt.UTC(), v.UploadedAt.UTC()
	v.HiddenAt = nullTime(hiddenAt)
	v.HiddenBy = nullUint(hiddenBy)
	return &v, nil
}

func formatLapMs(laps []int32) string {
	parts := make([]string, len(laps))
	for i, ms := range laps {
		parts[i] = strconv.Itoa(int(ms))
	}
	return strings.Join(parts, ",")
}

func parseLapMs(s string) ([]int32, error) {
	if s == "" {
		return []int32{}, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int32, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad lap_ms %q", s)
		}
		out[i] = int32(n)
	}
	return out, nil
}

func placeholders(n int) string { return "?" + strings.Repeat(",?", n-1) }

func (s *Store) queryRuns(ctx context.Context, query string, args ...any) ([]RunView, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunView
	for rows.Next() {
		v, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// CreateRun stores r, creating its board on first use, and sets r.ID and r.BoardID. ErrGhostExists when the ghost
// id is taken.
func (s *Store) CreateRun(ctx context.Context, r *Run) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO boards (track_id, engine_class, laps, content_version, track_hash,
			created_at) VALUES (?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
			r.TrackID, r.EngineClass, r.Laps, r.ContentVersion, r.TrackHash, r.UploadedAt)
		if err != nil {
			return fmt.Errorf("store: board: %w", err)
		}
		boardID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		r.BoardID = uint64(boardID)
		res, err = tx.ExecContext(ctx, `INSERT INTO runs (board_id, account_id, ghost_id, track_id, track_pack_id,
			kart_id, character_id, engine_class, laps, content_version, track_hash, setup_hash, game_build, finish_ms,
			best_lap_ms, lap_ms, frame_count, recorded_at, uploaded_at, blob_sha256, file_size)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.BoardID, r.AccountID, r.GhostID, r.TrackID, r.TrackPackID, r.KartID, r.CharacterID, r.EngineClass,
			r.Laps, r.ContentVersion, r.TrackHash, r.SetupHash, r.GameBuild, r.FinishMs, r.BestLapMs, formatLapMs(r.LapMs),
			r.FrameCount, r.RecordedAt, r.UploadedAt, r.Blob[:], r.FileSize)
		if isDuplicate(err) {
			return ErrGhostExists
		}
		if err != nil {
			return fmt.Errorf("store: create run: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		r.ID = uint64(id)
		return nil
	})
}

func (s *Store) runWhere(ctx context.Context, cond string, arg any) (*RunView, error) {
	v, err := scanRun(s.db.QueryRowContext(ctx, runSelect+" WHERE "+cond, arg))
	if err != nil {
		return nil, err
	}
	views := []RunView{*v}
	if err := s.decorate(ctx, views); err != nil {
		return nil, err
	}
	return &views[0], nil
}

// RunByID returns the run with its board rank and setup mismatch.
func (s *Store) RunByID(ctx context.Context, id uint64) (*RunView, error) {
	return s.runWhere(ctx, "r.id = ?", id)
}

// RunByGhostID matches the ghost id case-insensitively.
func (s *Store) RunByGhostID(ctx context.Context, ghostID string) (*RunView, error) {
	return s.runWhere(ctx, "r.ghost_id = ?", ghostID)
}

func (s *Store) CountUploadsSince(ctx context.Context, accountID uint64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE account_id = ? AND uploaded_at > ?",
		accountID, since).Scan(&n)
	return n, err
}

// decorate fills Rank (on the unfiltered board) and SetupMismatch of views.
func (s *Store) decorate(ctx context.Context, views []RunView) error {
	if len(views) == 0 {
		return nil
	}
	if err := s.rankOnBoards(ctx, views); err != nil {
		return err
	}
	return s.markSetupMismatch(ctx, views)
}

// rankOnBoards sets Rank to the position of each run on its board when it is its owner's best visible run.
func (s *Store) rankOnBoards(ctx context.Context, views []RunView) error {
	boardSet := map[uint64]bool{}
	var boards, ids []any
	for i := range views {
		views[i].Rank = 0
		ids = append(ids, views[i].ID)
		if !boardSet[views[i].BoardID] {
			boardSet[views[i].BoardID] = true
			boards = append(boards, views[i].BoardID)
		}
	}
	query := `WITH best AS (
			SELECT r.id, r.board_id, r.finish_ms, r.uploaded_at,
				ROW_NUMBER() OVER (PARTITION BY r.board_id, r.account_id ORDER BY ` + rankOrder + `) AS rn
			FROM runs r JOIN accounts a ON a.id = r.account_id
			WHERE r.board_id IN (` + placeholders(len(boards)) + `) AND ` + visible + `
		), ranked AS (
			SELECT id, ROW_NUMBER() OVER (PARTITION BY board_id ORDER BY ` + rankOrder + `) AS rnk
			FROM best WHERE rn = 1
		)
		SELECT id, rnk FROM ranked WHERE id IN (` + placeholders(len(ids)) + `)`
	rows, err := s.db.QueryContext(ctx, query, append(boards, ids...)...)
	if err != nil {
		return fmt.Errorf("store: ranks: %w", err)
	}
	defer rows.Close()
	ranks := map[uint64]uint32{}
	for rows.Next() {
		var (
			id   uint64
			rank uint32
		)
		if err := rows.Scan(&id, &rank); err != nil {
			return err
		}
		ranks[id] = rank
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range views {
		views[i].Rank = ranks[views[i].ID]
	}
	return nil
}

type setupKey struct {
	kart           string
	engineClass    uint8
	laps           uint8
	contentVersion int32
}

func setupKeyOf(r *Run) setupKey {
	return setupKey{strings.ToLower(r.KartID), r.EngineClass, r.Laps, r.ContentVersion}
}

// markSetupMismatch applies the majority rule: for every (kart, engine class, laps, content version) used by at least
// 3 accounts, runs whose setup hash is used by fewer accounts than the most common one are mismatched.
func (s *Store) markSetupMismatch(ctx context.Context, views []RunView) error {
	keys := map[setupKey]bool{}
	var args []any
	for i := range views {
		k := setupKeyOf(&views[i].Run)
		if !keys[k] {
			keys[k] = true
			args = append(args, k.kart, k.engineClass, k.laps, k.contentVersion)
		}
	}
	in := "(kart_id, engine_class, laps, content_version) IN ((?,?,?,?)" + strings.Repeat(",(?,?,?,?)", len(keys)-1) + ")"

	type hashKey struct {
		setupKey
		hash string
	}
	perHash := map[hashKey]int{}
	top := map[setupKey]int{}
	rows, err := s.db.QueryContext(ctx, `SELECT kart_id, engine_class, laps, content_version, setup_hash,
		COUNT(DISTINCT account_id) FROM runs WHERE `+in+` GROUP BY kart_id, engine_class, laps, content_version, setup_hash`,
		args...)
	if err != nil {
		return fmt.Errorf("store: setup votes: %w", err)
	}
	for rows.Next() {
		var (
			k    setupKey
			hash string
			n    int
		)
		if err := rows.Scan(&k.kart, &k.engineClass, &k.laps, &k.contentVersion, &hash, &n); err != nil {
			rows.Close()
			return err
		}
		k.kart = strings.ToLower(k.kart)
		perHash[hashKey{k, strings.ToLower(hash)}] = n
		top[k] = max(top[k], n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	totals := map[setupKey]int{}
	rows, err = s.db.QueryContext(ctx, `SELECT kart_id, engine_class, laps, content_version, COUNT(DISTINCT account_id)
		FROM runs WHERE `+in+` GROUP BY kart_id, engine_class, laps, content_version`, args...)
	if err != nil {
		return fmt.Errorf("store: setup votes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k setupKey
			n int
		)
		if err := rows.Scan(&k.kart, &k.engineClass, &k.laps, &k.contentVersion, &n); err != nil {
			return err
		}
		k.kart = strings.ToLower(k.kart)
		totals[k] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range views {
		k := setupKeyOf(&views[i].Run)
		votes := perHash[hashKey{k, strings.ToLower(views[i].SetupHash)}]
		views[i].SetupMismatch = totals[k] >= 3 && votes < top[k]
	}
	return nil
}

const boardSelect = `SELECT b.id, b.track_id, b.engine_class, b.laps, b.content_version, b.track_hash, b.created_at,
	COUNT(DISTINCT v.account_id), COALESCE(MIN(v.finish_ms), 0), MAX(v.uploaded_at)
	FROM boards b LEFT JOIN (
		SELECT r.board_id, r.account_id, r.finish_ms, r.uploaded_at
		FROM runs r JOIN accounts a ON a.id = r.account_id WHERE ` + visible + `
	) v ON v.board_id = b.id`

func scanBoard(row rowScanner) (*Board, error) {
	var (
		b    Board
		last sql.NullTime
	)
	err := row.Scan(&b.ID, &b.TrackID, &b.EngineClass, &b.Laps, &b.ContentVersion, &b.TrackHash, &b.CreatedAt,
		&b.Players, &b.BestMs, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.CreatedAt = b.CreatedAt.UTC()
	b.LastUpload = nullTime(last)
	return &b, nil
}

// ListBoards returns boards with their visible statistics, most recent visible upload first.
func (s *Store) ListBoards(ctx context.Context, f BoardFilter) ([]Board, error) {
	var (
		conds []string
		args  []any
	)
	if f.TrackID != "" {
		conds = append(conds, "b.track_id = ?")
		args = append(args, f.TrackID)
	}
	if f.EngineClass != nil {
		conds = append(conds, "b.engine_class = ?")
		args = append(args, *f.EngineClass)
	}
	if f.ContentVersion != 0 {
		conds = append(conds, "b.content_version = ?")
		args = append(args, f.ContentVersion)
	}
	query := boardSelect
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " GROUP BY b.id ORDER BY MAX(v.uploaded_at) DESC, b.id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (s *Store) BoardByID(ctx context.Context, id uint64) (*Board, error) {
	return scanBoard(s.db.QueryRowContext(ctx, boardSelect+" WHERE b.id = ? GROUP BY b.id", id))
}

// likeEscape escapes LIKE wildcards so a username filter matches literally.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Leaderboard ranks the best visible run per account on the board after filtering. With IncludeHidden every hidden
// run matching the filter joins the list with rank 0, so moderators see what players do not, in finish order.
func (s *Store) Leaderboard(ctx context.Context, q LeaderboardQuery) (*LeaderboardPage, error) {
	conds := []string{"r.board_id = ?"}
	args := []any{q.BoardID}
	if !q.IncludeHidden {
		conds = append(conds, visible)
	}
	if q.Username != "" {
		// Matches the name clients see: anonymous accounts by AnonymousName, never by their internal name.
		conds = append(conds, "IF(a.role = 2, '"+strings.ToLower(AnonymousName)+"', a.username_lower) LIKE ?")
		args = append(args, "%"+likeEscape(strings.ToLower(q.Username))+"%")
	}
	if q.Country != "" {
		conds = append(conds, shownCountry+" = ?")
		args = append(args, q.Country)
	}
	if q.Continent != "" {
		countries := geo.CountriesIn(q.Continent)
		if len(countries) == 0 {
			return &LeaderboardPage{}, nil
		}
		conds = append(conds, shownCountry+" IN ("+placeholders(len(countries))+")")
		for _, c := range countries {
			args = append(args, c)
		}
	}
	if q.KartID != "" {
		conds = append(conds, "r.kart_id = ?")
		args = append(args, q.KartID)
	}
	if q.CharacterID != "" {
		conds = append(conds, "r.character_id = ?")
		args = append(args, q.CharacterID)
	}
	ctes := `WITH cand AS (
			SELECT r.id, r.account_id, r.finish_ms, r.uploaded_at,
				(r.hidden_at IS NOT NULL OR a.disabled_at IS NOT NULL) AS hid
			FROM runs r JOIN accounts a ON a.id = r.account_id
			WHERE ` + strings.Join(conds, " AND ") + `
		), best AS (
			SELECT id, account_id, finish_ms, uploaded_at, hid,
				ROW_NUMBER() OVER (PARTITION BY account_id, hid ORDER BY ` + rankOrder + `) AS rn
			FROM cand
		), shown AS (
			SELECT id, account_id, hid,
				IF(hid, 0, ROW_NUMBER() OVER (PARTITION BY hid ORDER BY ` + rankOrder + `)) AS rnk,
				ROW_NUMBER() OVER (ORDER BY ` + rankOrder + `) AS pos,
				COUNT(*) OVER () AS total
			FROM best WHERE hid OR rn = 1
		) `
	rows, err := s.db.QueryContext(ctx, ctes+`SELECT id, account_id, hid, rnk, pos, total FROM shown
		WHERE (pos > ? AND pos <= ?) OR (account_id = ? AND NOT hid) ORDER BY pos`,
		append(args, q.Offset, q.Offset+q.Limit, q.Mine)...)
	if err != nil {
		return nil, fmt.Errorf("store: leaderboard: %w", err)
	}
	type row struct {
		id, account uint64
		hid         bool
		rank        uint32
		pos         int
	}
	var (
		page   LeaderboardPage
		result []row
	)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.account, &r.hid, &r.rank, &r.pos, &page.Total); err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 && q.Offset > 0 {
		// The page lies past the end: count separately.
		if err := s.db.QueryRowContext(ctx, ctes+"SELECT COUNT(*) FROM shown", args...).Scan(&page.Total); err != nil {
			return nil, err
		}
	}
	if len(result) == 0 {
		return &page, nil
	}
	ids := make([]any, len(result))
	for i, r := range result {
		ids[i] = r.id
	}
	views, err := s.queryRuns(ctx, runSelect+" WHERE r.id IN ("+placeholders(len(ids))+")", ids...)
	if err != nil {
		return nil, err
	}
	if err := s.markSetupMismatch(ctx, views); err != nil {
		return nil, err
	}
	byID := make(map[uint64]*RunView, len(views))
	for i := range views {
		byID[views[i].ID] = &views[i]
	}
	for _, r := range result {
		v, ok := byID[r.id]
		if !ok {
			continue // deleted between the two queries
		}
		v.Rank = r.rank
		if r.pos > q.Offset && r.pos <= q.Offset+q.Limit {
			page.Entries = append(page.Entries, *v)
		}
		if q.Mine != 0 && r.account == q.Mine && !r.hid {
			mine := *v
			page.Mine = &mine
		}
	}
	return &page, nil
}

// ListRuns returns runs, hidden ones included, newest upload first, with board ranks.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]RunView, error) {
	var (
		conds []string
		args  []any
	)
	if f.AccountID != 0 {
		conds = append(conds, "r.account_id = ?")
		args = append(args, f.AccountID)
	}
	if f.TrackID != "" {
		conds = append(conds, "r.track_id = ?")
		args = append(args, f.TrackID)
	}
	query := runSelect
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY r.uploaded_at DESC, r.id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	views, err := s.queryRuns(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return views, s.decorate(ctx, views)
}

// FlaggedRuns lists runs with at least one counted report, most flags first, then newest.
func (s *Store) FlaggedRuns(ctx context.Context, offset, limit int, includeHidden bool) (int, []RunView, error) {
	where := " WHERE r.flags > 0"
	if !includeHidden {
		where += " AND " + visible
	}
	return s.runPage(ctx, where, "r.flags DESC, r.uploaded_at DESC, r.id DESC", offset, limit)
}

// PlayerRuns lists the account's visible runs (all of them with includeHidden), newest upload first, with board
// ranks: only the account's best run on a board has one.
func (s *Store) PlayerRuns(ctx context.Context, accountID uint64, offset, limit int, includeHidden bool) (int,
	[]RunView, error) {
	where := " WHERE r.account_id = ?"
	if !includeHidden {
		where += " AND " + visible
	}
	return s.runPage(ctx, where, "r.uploaded_at DESC, r.id DESC", offset, limit, accountID)
}

// ListHiddenRuns lists hidden runs (by a moderator or because the owner is disabled), most recently hidden first.
func (s *Store) ListHiddenRuns(ctx context.Context, boardID uint64, offset, limit int) (int, []RunView, error) {
	where := " WHERE (r.hidden_at IS NOT NULL OR a.disabled_at IS NOT NULL)"
	var args []any
	if boardID != 0 {
		where += " AND r.board_id = ?"
		args = append(args, boardID)
	}
	return s.runPage(ctx, where, "COALESCE(r.hidden_at, a.disabled_at) DESC, r.id DESC", offset, limit, args...)
}

// runPage counts the runs matching where (aliases r, a) and returns the requested page in order, decorated.
func (s *Store) runPage(ctx context.Context, where, order string, offset, limit int, args ...any) (int, []RunView,
	error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs r JOIN accounts a ON a.id = r.account_id`+
		where, args...).Scan(&total); err != nil {
		return 0, nil, err
	}
	views, err := s.queryRuns(ctx, runSelect+where+" ORDER BY "+order+" LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return 0, nil, err
	}
	return total, views, s.decorate(ctx, views)
}

// recountFlags sets the cached flag count of the runs ids from their counted reports.
func recountFlags(ctx context.Context, tx *sql.Tx, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := tx.ExecContext(ctx, `UPDATE runs SET flags = (SELECT COUNT(*) FROM run_reports
		WHERE run_reports.run_id = runs.id AND run_reports.counted) WHERE id IN (`+placeholders(len(ids))+")", args...)
	return err
}

func hasCheatKick(ctx context.Context, q queryer, accountID uint64) (bool, error) {
	var kicked bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM cheat_kicks WHERE account_id = ?)", accountID).
		Scan(&kicked)
	return kicked, err
}

// ReportRun stores rep once per (run, reporter). Reports on one's own run and from cheat-kicked accounts are kept
// with Counted false. It sets rep.Counted and returns the run's flag count and whether the report was new.
func (s *Store) ReportRun(ctx context.Context, rep *Report) (flags uint32, inserted bool, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var owner uint64
		err := tx.QueryRowContext(ctx, "SELECT account_id FROM runs WHERE id = ? FOR UPDATE", rep.RunID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		kicked, err := hasCheatKick(ctx, tx, rep.ReporterID)
		if err != nil {
			return err
		}
		rep.Counted = owner != rep.ReporterID && !kicked
		_, err = tx.ExecContext(ctx, `INSERT INTO run_reports (run_id, reporter_id, reason, at_seconds, detail,
			created_at, counted) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rep.RunID, rep.ReporterID, uint8(rep.Reason), rep.AtSeconds, rep.Detail, rep.CreatedAt, rep.Counted)
		switch {
		case isDuplicate(err):
		case err != nil:
			return err
		default:
			inserted = true
			if rep.Counted {
				if err := recountFlags(ctx, tx, []uint64{rep.RunID}); err != nil {
					return err
				}
			}
		}
		return tx.QueryRowContext(ctx, "SELECT flags FROM runs WHERE id = ?", rep.RunID).Scan(&flags)
	})
	return flags, inserted, err
}

// RunReports returns the reports on a run, oldest first.
func (s *Store) RunReports(ctx context.Context, runID uint64, limit int) ([]Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rr.id, rr.run_id, rr.reporter_id, a.username, rr.reason,
		rr.at_seconds, rr.detail, rr.created_at, rr.counted
		FROM run_reports rr JOIN accounts a ON a.id = rr.reporter_id
		WHERE rr.run_id = ? ORDER BY rr.created_at, rr.id LIMIT ?`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		var (
			r      Report
			reason uint8
		)
		if err := rows.Scan(&r.ID, &r.RunID, &r.ReporterID, &r.Reporter, &reason, &r.AtSeconds, &r.Detail,
			&r.CreatedAt, &r.Counted); err != nil {
			return nil, err
		}
		r.Reason = proto.FlagReason(reason)
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// runActionTarget completes act with the run's owner and reference and locks the run row.
func runActionTarget(ctx context.Context, tx *sql.Tx, runID uint64, act *Action) error {
	var owner uint64
	err := tx.QueryRowContext(ctx, "SELECT account_id FROM runs WHERE id = ? FOR UPDATE", runID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	act.TargetAccount = &owner
	if act.TargetRef == "" {
		act.TargetRef = fmt.Sprintf("run %d", runID)
	}
	return nil
}

// HideRun hides the run from players with act.Reason; act.Actor is the moderator (nil = sysadmin).
func (s *Store) HideRun(ctx context.Context, runID uint64, act Action) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := runActionTarget(ctx, tx, runID, &act); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET hidden_at = ?, hidden_by = ?, hidden_reason = ?,
			deleted_by_owner = FALSE WHERE id = ?`, act.CreatedAt, toNull(act.Actor), act.Reason, runID); err != nil {
			return err
		}
		return recordAction(ctx, tx, act)
	})
}

func (s *Store) UnhideRun(ctx context.Context, runID uint64, act Action) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := runActionTarget(ctx, tx, runID, &act); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET hidden_at = NULL, hidden_by = NULL, hidden_reason = '',
			deleted_by_owner = FALSE WHERE id = ?`, runID); err != nil {
			return err
		}
		return recordAction(ctx, tx, act)
	})
}

// HiddenInfo is a run's owner and why players do not see it.
type HiddenInfo struct {
	AccountID      uint64
	HiddenAt       *time.Time // nil = not hidden itself
	Moderator      string     // username of the hiding moderator, "" = sysadmin, the owner, or a deleted moderator
	Reason         string
	DeletedByOwner bool
	DisabledAt     *time.Time // the owner's account is disabled
	DisabledReason string
}

func (s *Store) RunHiddenInfo(ctx context.Context, runID uint64) (*HiddenInfo, error) {
	var (
		h                    HiddenInfo
		hiddenAt, disabledAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `SELECT r.account_id, r.hidden_at, COALESCE(m.username, ''), r.hidden_reason,
		r.deleted_by_owner, a.disabled_at, a.disabled_reason
		FROM runs r JOIN accounts a ON a.id = r.account_id LEFT JOIN accounts m ON m.id = r.hidden_by
		WHERE r.id = ?`, runID).Scan(&h.AccountID, &hiddenAt, &h.Moderator, &h.Reason, &h.DeletedByOwner, &disabledAt,
		&h.DisabledReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	h.HiddenAt, h.DisabledAt = nullTime(hiddenAt), nullTime(disabledAt)
	return &h, nil
}

// unreferenced returns the hashes no run references any more.
func unreferenced(ctx context.Context, tx *sql.Tx, hashes []blobs.Hash) ([]blobs.Hash, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	args := make([]any, len(hashes))
	for i := range hashes {
		args[i] = hashes[i][:]
	}
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT blob_sha256 FROM runs WHERE blob_sha256 IN ("+
		placeholders(len(hashes))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[blobs.Hash]bool{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var h blobs.Hash
		copy(h[:], b)
		used[h] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []blobs.Hash
	for _, h := range hashes {
		if !used[h] {
			out = append(out, h)
		}
	}
	return out, nil
}

func (s *Store) DeleteRun(ctx context.Context, runID uint64, act Action) ([]blobs.Hash, error) {
	var orphans []blobs.Hash
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := runActionTarget(ctx, tx, runID, &act); err != nil {
			return err
		}
		var b []byte
		if err := tx.QueryRowContext(ctx, "SELECT blob_sha256 FROM runs WHERE id = ?", runID).Scan(&b); err != nil {
			return err
		}
		var h blobs.Hash
		copy(h[:], b)
		if _, err := tx.ExecContext(ctx, "DELETE FROM runs WHERE id = ?", runID); err != nil {
			return err
		}
		var err error
		if orphans, err = unreferenced(ctx, tx, []blobs.Hash{h}); err != nil {
			return err
		}
		return recordAction(ctx, tx, act)
	})
	return orphans, err
}

func (s *Store) AccountStats(ctx context.Context, accountID uint64) (AccountStats, error) {
	var st AccountStats
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM runs WHERE account_id = ?),
		(SELECT COUNT(*) FROM runs WHERE account_id = ? AND hidden_at IS NOT NULL),
		(SELECT COUNT(*) FROM cheat_kicks WHERE account_id = ?)`, accountID, accountID, accountID).
		Scan(&st.Runs, &st.HiddenRuns, &st.CheatKicks)
	return st, err
}

// AddCheatKick records a kick and stops counting every report the account ever made.
func (s *Store) AddCheatKick(ctx context.Context, k *CheatKick) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO cheat_kicks (account_id, lobby_ref, race_ref, reason, reporters,
			created_at) VALUES (?, ?, ?, ?, ?, ?)`, k.AccountID, k.LobbyRef, k.RaceRef, k.Reason, k.Reporters, k.CreatedAt)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		k.ID = uint64(id)
		runs, err := countedReportsBy(ctx, tx, k.AccountID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE run_reports SET counted = FALSE WHERE reporter_id = ?",
			k.AccountID); err != nil {
			return err
		}
		return recountFlags(ctx, tx, runs)
	})
}

// countedReportsBy returns the runs the account reported with a counted report.
func countedReportsBy(ctx context.Context, tx *sql.Tx, accountID uint64) ([]uint64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT run_id FROM run_reports WHERE reporter_id = ? AND counted", accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
