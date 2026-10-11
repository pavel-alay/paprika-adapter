package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
)

// CachedRecipe is a recipe as Paprika returned it plus when it was fetched.
type CachedRecipe struct {
	Raw       json.RawMessage
	FetchedAt time.Time
}

// RecipeHashes returns uid -> content hash for every cached recipe.
func (s *Store) RecipeHashes(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uid, hash FROM recipes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, hash string
		if err := rows.Scan(&uid, &hash); err != nil {
			return nil, err
		}
		out[uid] = hash
	}
	return out, rows.Err()
}

// PutRecipe stores one recipe and indexes it for search. hash is the value from
// the recipe list, which is what later syncs compare against.
func (s *Store) PutRecipe(ctx context.Context, r paprika.Raw, hash string, fetchedAt time.Time) error {
	rec, err := paprika.Decode[paprika.Recipe](r.JSON)
	if err != nil {
		return err
	}
	names, err := s.categoryNames(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO recipes (uid, hash, name, in_trash, raw, fetched_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(uid) DO UPDATE SET hash = excluded.hash, name = excluded.name, in_trash = excluded.in_trash,
			raw = excluded.raw, fetched_at = excluded.fetched_at`,
		r.UID, hash, rec.Name, rec.InTrash, string(r.JSON), stamp(fetchedAt)); err != nil {
		return err
	}
	if err := indexRecipe(ctx, tx, rec, names); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteRecipes drops recipes that Paprika no longer lists.
func (s *Store) DeleteRecipes(ctx context.Context, uids []string) error {
	for _, uid := range uids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM recipes WHERE uid = ?`, uid); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM recipes_fts WHERE uid = ?`, uid); err != nil {
			return err
		}
	}
	return nil
}

func indexRecipe(ctx context.Context, tx *sql.Tx, r paprika.Recipe, categoryNames map[string]string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recipes_fts WHERE uid = ?`, r.UID); err != nil {
		return err
	}
	var cats []string
	for _, uid := range r.Categories {
		cats = append(cats, categoryNames[uid])
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO recipes_fts (uid, name, ingredients, categories, description, notes, source) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.UID, r.Name, r.Ingredients, strings.Join(cats, "\n"), r.Description, r.Notes, r.Source)
	return err
}

func (s *Store) categoryNames(ctx context.Context) (map[string]string, error) {
	cats, err := Load[paprika.Category](ctx, s, paprika.Categories)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(cats))
	for _, c := range cats {
		names[c.UID] = c.Name
	}
	return names, nil
}

// Recipe returns one cached recipe, or ok=false.
func (s *Store) Recipe(ctx context.Context, uid string) (CachedRecipe, bool, error) {
	var raw string
	var fetched sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT raw, fetched_at FROM recipes WHERE uid = ?`, uid).Scan(&raw, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return CachedRecipe{}, false, nil
	}
	if err != nil {
		return CachedRecipe{}, false, err
	}
	return CachedRecipe{Raw: json.RawMessage(raw), FetchedAt: parseStamp(fetched)}, true, nil
}

// AllRecipes returns every cached recipe ordered by name, for exports.
func (s *Store) AllRecipes(ctx context.Context) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT raw FROM recipes ORDER BY name COLLATE NOCASE, uid`)
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

// RebuildSearchIndex re-derives the whole full-text index. It is needed when
// categories change, because recipes are indexed under their category names.
func (s *Store) RebuildSearchIndex(ctx context.Context) error {
	names, err := s.categoryNames(ctx)
	if err != nil {
		return err
	}
	raws, err := s.AllRecipes(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, raw := range raws {
		r, err := paprika.Decode[paprika.Recipe](raw)
		if err != nil {
			return err
		}
		if err := indexRecipe(ctx, tx, r, names); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SearchQuery selects recipes. With neither Text nor Category it lists recipes by name.
type SearchQuery struct {
	Text           string
	Category       string // matched against category names
	IncludeTrashed bool
	Limit          int
}

// SearchRecipes returns matching recipes, best match first, and the total match count.
func (s *Store) SearchRecipes(ctx context.Context, q SearchQuery) ([]json.RawMessage, int, error) {
	from := `FROM recipes r WHERE (r.in_trash = 0 OR @trash)`
	order := `ORDER BY r.name COLLATE NOCASE, r.uid`
	args := []any{sql.Named("trash", q.IncludeTrashed)}
	if match := ftsQuery(q.Text, q.Category); match != "" {
		from = `FROM recipes_fts JOIN recipes r ON r.uid = recipes_fts.uid
			WHERE recipes_fts MATCH @match AND (r.in_trash = 0 OR @trash)`
		// Weights follow the fts column order: uid, name, ingredients, categories, description, notes, source.
		order = `ORDER BY bm25(recipes_fts, 0, 10, 4, 6, 2, 1, 1), r.name COLLATE NOCASE, r.uid`
		args = append(args, sql.Named("match", match))
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) `+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.raw `+from+` `+order+` LIMIT @limit`, append(args, sql.Named("limit", q.Limit))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, 0, err
		}
		out = append(out, json.RawMessage(raw))
	}
	return out, total, rows.Err()
}

// ftsQuery turns free text into an FTS5 expression: every word must match, each
// as a prefix. Words are quoted so user input can never inject FTS syntax.
func ftsQuery(text, category string) string {
	var parts []string
	if t := ftsTerms(text); t != "" {
		parts = append(parts, t)
	}
	if c := ftsTerms(category); c != "" {
		parts = append(parts, "categories : ("+c+")")
	}
	return strings.Join(parts, " AND ")
}

func ftsTerms(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	terms := make([]string, 0, len(words))
	for _, w := range words {
		terms = append(terms, `"`+w+`"*`)
	}
	return strings.Join(terms, " ")
}
