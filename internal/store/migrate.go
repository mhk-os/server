package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrations embed.FS

const migrateLock = "mhk_schema_migrate"

// Migrate applies the embedded migrations not yet recorded in schema_migrations, in file name order. A MySQL named
// lock serializes concurrent servers and CLI runs. MySQL DDL commits implicitly, so a migration that fails halfway
// must be repaired by hand; keep each file small.
func (s *Store) Migrate(ctx context.Context, log *slog.Logger) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var got int
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 60)", migrateLock).Scan(&got); err != nil {
		return fmt.Errorf("store: migration lock: %w", err)
	}
	if got != 1 {
		return fmt.Errorf("store: migration lock %q held by another process", migrateLock)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK(?)", migrateLock)

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    VARCHAR(255) NOT NULL PRIMARY KEY,
		applied_at DATETIME(3)  NOT NULL
	) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := conn.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		if applied[version] {
			continue
		}
		src, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(string(src)) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("store: migration %s: %w", version, err)
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
			version, Now()); err != nil {
			return fmt.Errorf("store: record migration %s: %w", version, err)
		}
		if log != nil {
			log.Info("applied migration", "version", version)
		}
	}
	return nil
}

// splitStatements drops "--" comment lines and splits on ";". Migrations must not put semicolons in literals.
func splitStatements(src string) []string {
	var b strings.Builder
	for line := range strings.Lines(src) {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
	}
	var out []string
	for stmt := range strings.SplitSeq(b.String(), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
