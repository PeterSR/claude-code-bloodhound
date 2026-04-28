// Package store opens the SQLite database, applies embedded migrations, and
// exposes typed helpers for the rest of the codebase.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const dbFilename = "bloodhound.db"

// Store wraps the SQLite handle. Safe for concurrent use (sql.DB is).
type Store struct {
	DB   *sql.DB
	Path string
}

// Open creates the data directory (if needed), opens the SQLite database,
// applies any pending migrations, and ensures the install_id row exists.
func Open(ctx context.Context) (*Store, error) {
	dir, err := config.DataDir()
	if err != nil {
		return nil, err
	}
	if err := config.EnsureDir(dir); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, dbFilename)

	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{DB: db, Path: dbPath}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := s.InstallID(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("install_id: %w", err)
	}
	return s, nil
}

// Close releases the underlying handle.
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}

// SchemaVersion returns the highest applied migration version, or 0 if none.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.DB.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`,
	).Scan(&v)
	return v, err
}

// InstallID returns the persistent random ID generated for this install.
// Created lazily on first call. Used as the prefix for community-insights
// session_hash so the same machine produces stable hashes across sessions.
func (s *Store) InstallID(ctx context.Context) (string, error) {
	const key = "install_id"
	var id string
	err := s.DB.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, key,
	).Scan(&id)
	if err == nil && id != "" {
		return id, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id, err = newInstallID()
	if err != nil {
		return "", err
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, id,
	); err != nil {
		return "", err
	}
	return id, nil
}

func newInstallID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// RFC 4122 v4 layout
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// migrate applies any embedded migrations whose version exceeds the highest
// already in schema_migrations. Each migration runs in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		)
	`); err != nil {
		return err
	}

	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	type mig struct {
		version int
		name    string
	}
	var migs []mig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := parseMigrationVersion(e.Name())
		if err != nil {
			return fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		migs = append(migs, mig{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return fmt.Errorf("read %s: %w", m.name, err)
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES (?)`, m.version,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", m.name, err)
		}
	}
	return nil
}

// parseMigrationVersion extracts the leading integer from filenames like
// "0001_initial.sql".
func parseMigrationVersion(name string) (int, error) {
	base := strings.TrimSuffix(name, ".sql")
	cut := strings.IndexAny(base, "_-")
	if cut < 0 {
		cut = len(base)
	}
	return strconv.Atoi(base[:cut])
}
