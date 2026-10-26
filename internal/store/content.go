package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/wire"
)

// PackVersion identifies what the catalog majority is computed over: a pack at a commit for a simulation version.
type PackVersion struct {
	PackID     string
	Commit     string
	SimVersion int32
}

// Manifest is a row of pack_manifests.
type Manifest struct {
	PackVersion
	Hash  string // lowercase hex
	Items []proto.ManifestItem
}

// ContentStrike is a row of content_strikes, with the account's name for listings.
type ContentStrike struct {
	ID        uint64
	AccountID uint64
	Username  string
	PackVersion
	ManifestHash string // "" = unknown
	MajorityHash string // "" = unknown
	Reason       string
	CreatedAt    time.Time
}

// CheatKickView is a cheat_kicks row with the account's name.
type CheatKickView struct {
	CheatKick
	Username string
}

// Audit action written when a moderator kicks a player from a lobby.
const ActionLobbyKick = "lobby.kick"

// HasManifest reports whether the manifest with hash is stored for the pack version.
func (s *Store) HasManifest(ctx context.Context, v PackVersion, hash string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pack_manifests
		WHERE pack_id = ? AND pack_commit = ? AND sim_version = ? AND manifest_hash = ?`,
		v.PackID, v.Commit, v.SimVersion, hash).Scan(&n)
	return n > 0, err
}

// PutManifest stores a verified manifest; storing one that exists already is a no-op.
func (s *Store) PutManifest(ctx context.Context, m *Manifest, at time.Time) error {
	w := wire.NewWriter(16 + 40*len(m.Items))
	proto.EncodeManifestItems(w, m.Items)
	_, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO pack_manifests (pack_id, pack_commit, sim_version,
		manifest_hash, items, item_count, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.PackID, m.Commit, m.SimVersion, m.Hash, w.Bytes(), len(m.Items), at)
	return err
}

// ManifestByHash returns a stored manifest, ErrNotFound when there is none.
func (s *Store) ManifestByHash(ctx context.Context, v PackVersion, hash string) (*Manifest, error) {
	var blob []byte
	m := &Manifest{}
	err := s.db.QueryRowContext(ctx, `SELECT pack_id, pack_commit, sim_version, manifest_hash, items
		FROM pack_manifests WHERE pack_id = ? AND pack_commit = ? AND sim_version = ? AND manifest_hash = ?`,
		v.PackID, v.Commit, v.SimVersion, hash).Scan(&m.PackID, &m.Commit, &m.SimVersion, &m.Hash, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r := wire.NewReader(blob)
	m.Items = proto.DecodeManifestItems(r)
	if err := r.Err(); err != nil {
		return nil, fmt.Errorf("store: manifest %s of %s: %w", hash, v.PackID, err)
	}
	return m, nil
}

// PackVotes returns the manifest hash every account reported for the pack version.
func (s *Store) PackVotes(ctx context.Context, v PackVersion) (map[uint64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, manifest_hash FROM pack_votes
		WHERE pack_id = ? AND pack_commit = ? AND sim_version = ?`, v.PackID, v.Commit, v.SimVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	votes := map[uint64]string{}
	for rows.Next() {
		var (
			id   uint64
			hash string
		)
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		votes[id] = strings.ToLower(hash)
	}
	return votes, rows.Err()
}

// SetPackVote records the account's latest manifest hash for the pack version.
func (s *Store) SetPackVote(ctx context.Context, v PackVersion, accountID uint64, hash string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pack_votes (pack_id, pack_commit, sim_version, account_id,
		manifest_hash, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE manifest_hash = VALUES(manifest_hash), updated_at = VALUES(updated_at)`,
		v.PackID, v.Commit, v.SimVersion, accountID, hash, at)
	return err
}

// AddContentStrike stores a strike. With once, a strike with the same account, pack version and both hashes is
// written only the first time, so announcing the same differing copy again does not pile up strikes.
func (s *Store) AddContentStrike(ctx context.Context, k *ContentStrike, once bool) error {
	q := `INSERT INTO content_strikes (account_id, pack_id, pack_commit, sim_version, manifest_hash, majority_hash,
		reason, created_at) SELECT ?, ?, ?, ?, ?, ?, ?, ? FROM DUAL`
	args := []any{k.AccountID, k.PackID, k.Commit, k.SimVersion, k.ManifestHash, k.MajorityHash, k.Reason, k.CreatedAt}
	if once {
		q += ` WHERE NOT EXISTS (SELECT 1 FROM content_strikes WHERE account_id = ? AND pack_id = ? AND
			pack_commit = ? AND sim_version = ? AND manifest_hash = ? AND majority_hash = ?)`
		args = append(args, k.AccountID, k.PackID, k.Commit, k.SimVersion, k.ManifestHash, k.MajorityHash)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// ContentStrikes lists strikes newest first; accountID 0 means every account, limit 0 no limit.
func (s *Store) ContentStrikes(ctx context.Context, accountID uint64, limit int) ([]ContentStrike, error) {
	q := `SELECT cs.id, cs.account_id, a.username, cs.pack_id, cs.pack_commit, cs.sim_version, cs.manifest_hash,
		cs.majority_hash, cs.reason, cs.created_at FROM content_strikes cs JOIN accounts a ON a.id = cs.account_id`
	var args []any
	if accountID != 0 {
		q += " WHERE cs.account_id = ?"
		args = append(args, accountID)
	}
	q += " ORDER BY cs.id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContentStrike
	for rows.Next() {
		var k ContentStrike
		if err := rows.Scan(&k.ID, &k.AccountID, &k.Username, &k.PackID, &k.Commit, &k.SimVersion, &k.ManifestHash,
			&k.MajorityHash, &k.Reason, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.CreatedAt = k.CreatedAt.UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}

// CheatKicks lists cheat kicks newest first; accountID 0 means every account, limit 0 no limit.
func (s *Store) CheatKicks(ctx context.Context, accountID uint64, limit int) ([]CheatKickView, error) {
	q := `SELECT k.id, k.account_id, a.username, k.lobby_ref, k.race_ref, k.reason, k.reporters, k.created_at
		FROM cheat_kicks k JOIN accounts a ON a.id = k.account_id`
	var args []any
	if accountID != 0 {
		q += " WHERE k.account_id = ?"
		args = append(args, accountID)
	}
	q += " ORDER BY k.id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheatKickView
	for rows.Next() {
		var k CheatKickView
		if err := rows.Scan(&k.ID, &k.AccountID, &k.Username, &k.LobbyRef, &k.RaceRef, &k.Reason, &k.Reporters,
			&k.CreatedAt); err != nil {
			return nil, err
		}
		k.CreatedAt = k.CreatedAt.UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}

// RecordAction writes one audit log row for an action that changes nothing in the database (lobby kicks).
func (s *Store) RecordAction(ctx context.Context, a Action) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return recordAction(ctx, tx, a) })
}
