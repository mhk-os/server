package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// MostReportedManifest returns the manifest of packID for contentVersion that the most accounts reported, over every
// commit (ties: the most recently reported). Dev bots announce it to look like a typical client of the server.
func (s *Store) MostReportedManifest(ctx context.Context, packID string, contentVersion int32) (*Manifest, error) {
	var commit, hash string
	err := s.db.QueryRowContext(ctx, `SELECT pack_commit, manifest_hash FROM pack_votes
		WHERE pack_id = ? AND content_version = ? GROUP BY pack_commit, manifest_hash
		ORDER BY COUNT(*) DESC, MAX(updated_at) DESC LIMIT 1`, packID, contentVersion).Scan(&commit, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.ManifestByHash(ctx, PackVersion{PackID: packID, Commit: commit, ContentVersion: contentVersion},
		strings.ToLower(hash))
}
