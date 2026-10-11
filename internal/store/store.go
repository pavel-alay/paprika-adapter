// Package store is the local SQLite cache. It holds copies of what Paprika
// returned; Paprika stays the source of truth and nothing here is ever pushed back.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
)

const schema = `
CREATE TABLE IF NOT EXISTS recipes (
	uid        TEXT PRIMARY KEY,
	hash       TEXT NOT NULL,
	name       TEXT NOT NULL,
	in_trash   INTEGER NOT NULL,
	raw        TEXT NOT NULL,
	fetched_at TEXT NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS recipes_fts USING fts5(
	uid UNINDEXED, name, ingredients, categories, description, notes, source,
	tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TABLE IF NOT EXISTS entities (
	kind TEXT NOT NULL,
	uid  TEXT NOT NULL,
	raw  TEXT NOT NULL,
	PRIMARY KEY (kind, uid)
);
CREATE TABLE IF NOT EXISTS sync_state (
	collection      TEXT PRIMARY KEY,
	counter         INTEGER,
	last_attempt_at TEXT,
	last_checked_at TEXT,
	last_fetched_at TEXT,
	error_kind      TEXT,
	error_message   TEXT,
	drift           TEXT
);
CREATE TABLE IF NOT EXISTS proposals (
	id         TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	payload    TEXT NOT NULL,
	state      TEXT NOT NULL,
	result     TEXT,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
`

// Store wraps the cache database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the cache at path.
func Open(ctx context.Context, path string) (*Store, error) {
	// The cache holds household data: create it private. SQLite gives its WAL files the same mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open cache: %w", err)
	}
	f.Close()
	dsn := "file:" + url.PathEscape(path) + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open cache: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init cache schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ReplaceEntities swaps the cached rows of one collection for a fresh server read.
func (s *Store) ReplaceEntities(ctx context.Context, kind paprika.Collection, rows []paprika.Raw) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM entities WHERE kind = ?`, string(kind)); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO entities (kind, uid, raw) VALUES (?, ?, ?)`,
			string(kind), r.UID, string(r.JSON)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Entities returns the cached rows of one collection.
func (s *Store) Entities(ctx context.Context, kind paprika.Collection) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT raw FROM entities WHERE kind = ? ORDER BY uid`, string(kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(raw))
	}
	return out, rows.Err()
}

// Load returns one cached collection decoded into its read projection.
func Load[T any](ctx context.Context, s *Store, kind paprika.Collection) ([]T, error) {
	raws, err := s.Entities(ctx, kind)
	if err != nil {
		return nil, err
	}
	return paprika.DecodeAll[T](raws)
}

// RowCounts returns how many rows are cached per collection, recipes included.
func (s *Store) RowCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, count(*) FROM entities GROUP BY kind
		UNION ALL SELECT ?, count(*) FROM recipes`, paprika.Recipes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}

// Snapshot writes a consistent copy of the cache database to path.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseStamp(v sql.NullString) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, v.String)
	return t
}
