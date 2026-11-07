package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/wire"
)

// PackVersion identifies a pack manifest: a pack at a commit for a rules version.
type PackVersion struct {
	PackID       string
	Commit       string
	RulesVersion int32
}

// Manifest is a row of pack_manifests.
type Manifest struct {
	PackVersion
	Items []proto.ManifestItem
}

// CheatKickView is a cheat_kicks row with the account's name.
type CheatKickView struct {
	CheatKick
	Username string
}

// Audit action written when a moderator kicks a player from a lobby.
const ActionLobbyKick = "lobby.kick"

// PutManifest stores a validated manifest. The first manifest stored for a pack version is kept: storing another
// one for the same version is a no-op.
func (s *Store) PutManifest(ctx context.Context, m *Manifest, at time.Time) error {
	w := wire.NewWriter(16 + 40*len(m.Items))
	proto.EncodeManifestItems(w, m.Items)
	_, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO pack_manifests (pack_id, pack_commit, rules_version, items,
		item_count, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		m.PackID, m.Commit, m.RulesVersion, w.Bytes(), len(m.Items), at)
	return err
}

// Manifest returns the stored manifest of a pack version, ErrNotFound when there is none.
func (s *Store) Manifest(ctx context.Context, v PackVersion) (*Manifest, error) {
	return s.scanManifest(s.db.QueryRowContext(ctx, `SELECT pack_id, pack_commit, rules_version, items
		FROM pack_manifests WHERE pack_id = ? AND pack_commit = ? AND rules_version = ?`,
		v.PackID, v.Commit, v.RulesVersion), v.PackID)
}

// LatestManifest returns the most recently stored manifest of packID for rulesVersion over every commit. Dev bots
// announce it to look like a current client of the server.
func (s *Store) LatestManifest(ctx context.Context, packID string, rulesVersion int32) (*Manifest, error) {
	return s.scanManifest(s.db.QueryRowContext(ctx, `SELECT pack_id, pack_commit, rules_version, items
		FROM pack_manifests WHERE pack_id = ? AND rules_version = ? ORDER BY created_at DESC LIMIT 1`,
		packID, rulesVersion), packID)
}

func (s *Store) scanManifest(row *sql.Row, packID string) (*Manifest, error) {
	var blob []byte
	m := &Manifest{}
	err := row.Scan(&m.PackID, &m.Commit, &m.RulesVersion, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r := wire.NewReader(blob)
	m.Items = proto.DecodeManifestItems(r)
	if err := r.Err(); err != nil {
		return nil, fmt.Errorf("store: manifest of %s: %w", packID, err)
	}
	return m, nil
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
