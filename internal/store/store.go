package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type Meme struct {
	ID                   string   `json:"id"`
	Filename             string   `json:"-"`
	OriginalName         string   `json:"original_name"`
	MimeType             string   `json:"mime_type"`
	Size                 int64    `json:"size"`
	Description          string   `json:"description"`
	DescriptionStatus    string   `json:"description_status"`
	DescriptionGenerated bool     `json:"description_generated"`
	Tags                 []string `json:"tags"`
	CreatedAt            string   `json:"created_at"`
	SortOrder            int64    `json:"-"`
}

type ListResult struct {
	Memes      []Meme `json:"memes"`
	NextCursor string `json:"next_cursor,omitempty"`
	Total      int    `json:"total"`
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS memes (
    id TEXT PRIMARY KEY,
    filename TEXT NOT NULL UNIQUE,
    original_name TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    size INTEGER NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    description_status TEXT NOT NULL DEFAULT 'none',
    description_generated INTEGER NOT NULL DEFAULT 0,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS memes_created_at_idx ON memes(created_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS tags (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS meme_tags (
    meme_id TEXT NOT NULL REFERENCES memes(id) ON DELETE CASCADE,
    tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (meme_id, tag_id)
);
CREATE INDEX IF NOT EXISTS meme_tags_tag_idx ON meme_tags(tag_id, meme_id);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if err := s.ensureSortOrder(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureSortOrder(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(memes)`)
	if err != nil {
		return fmt.Errorf("inspect meme schema: %w", err)
	}
	hasSortOrder := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan meme schema: %w", err)
		}
		if name == "sort_order" {
			hasSortOrder = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasSortOrder {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE memes ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add meme sort order: %w", err)
		}
	}
	var zeroCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memes WHERE sort_order = 0`).Scan(&zeroCount); err != nil {
		return err
	}
	if zeroCount == 0 {
		return nil
	}
	ordered, err := s.db.QueryContext(ctx, `SELECT id FROM memes ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return err
	}
	ids := []string{}
	for ordered.Next() {
		var id string
		if err := ordered.Scan(&id); err != nil {
			ordered.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := ordered.Close(); err != nil {
		return err
	}
	for index, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE memes SET sort_order = ? WHERE id = ?`, int64(len(ids)-index), id); err != nil {
			return fmt.Errorf("backfill meme sort order: %w", err)
		}
	}
	return nil
}

func (s *Store) Create(ctx context.Context, meme Meme, tags []string) error {
	if meme.ID == "" {
		meme.ID = uuid.NewString()
	}
	if meme.CreatedAt == "" {
		meme.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if meme.DescriptionStatus == "" {
		meme.DescriptionStatus = "none"
	}
	if meme.SortOrder <= 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order), 0) + 1 FROM memes`).Scan(&meme.SortOrder); err != nil {
			return fmt.Errorf("allocate meme sort order: %w", err)
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO memes
		(id, filename, original_name, mime_type, size, description, description_status, description_generated, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		meme.ID, meme.Filename, meme.OriginalName, meme.MimeType, meme.Size, meme.Description,
		meme.DescriptionStatus, boolInt(meme.DescriptionGenerated), meme.SortOrder, meme.CreatedAt, meme.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert meme: %w", err)
	}
	if err := s.setTags(ctx, meme.ID, tags); err != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM memes WHERE id = ?`, meme.ID)
		return err
	}
	return nil
}

func (s *Store) setTags(ctx context.Context, memeID string, tags []string) error {
	for _, tag := range tags {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO tags(name) VALUES (?) ON CONFLICT(name) DO NOTHING`, tag); err != nil {
			return fmt.Errorf("insert tag: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO meme_tags(meme_id, tag_id) SELECT ?, id FROM tags WHERE name = ? ON CONFLICT DO NOTHING`, memeID, tag); err != nil {
			return fmt.Errorf("link tag: %w", err)
		}
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (Meme, error) {
	var meme Meme
	var generated int
	err := s.db.QueryRowContext(ctx, `SELECT id, filename, original_name, mime_type, size, description,
		description_status, description_generated, sort_order, created_at FROM memes WHERE id = ?`, id).
		Scan(&meme.ID, &meme.Filename, &meme.OriginalName, &meme.MimeType, &meme.Size, &meme.Description,
			&meme.DescriptionStatus, &generated, &meme.SortOrder, &meme.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Meme{}, ErrNotFound
	}
	if err != nil {
		return Meme{}, fmt.Errorf("get meme: %w", err)
	}
	meme.DescriptionGenerated = generated != 0
	meme.Tags, err = s.tags(ctx, meme.ID)
	if err != nil {
		return Meme{}, err
	}
	return meme, nil
}

func (s *Store) Pending(ctx context.Context) ([]Meme, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, filename, mime_type, description, description_status, description_generated, sort_order, created_at FROM memes WHERE description_status = 'pending' ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list pending descriptions: %w", err)
	}
	defer rows.Close()
	pending := []Meme{}
	for rows.Next() {
		var meme Meme
		var generated int
		if err := rows.Scan(&meme.ID, &meme.Filename, &meme.MimeType, &meme.Description, &meme.DescriptionStatus, &generated, &meme.SortOrder, &meme.CreatedAt); err != nil {
			return nil, err
		}
		meme.DescriptionGenerated = generated != 0
		pending = append(pending, meme)
	}
	return pending, rows.Err()
}

func (s *Store) UpdateDescription(ctx context.Context, id, description, status string, generated bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE memes SET description = ?, description_status = ?, description_generated = ?, updated_at = ? WHERE id = ?`,
		description, status, boolInt(generated), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("update description: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateGeneratedContent stores the result of one vision request. Generated
// hashtags are only attached when the meme still has no tags; this prevents a
// retry or a slow worker from overwriting tags supplied by the uploader.
func (s *Store) UpdateGeneratedContent(ctx context.Context, id, description, status string, generated bool, tags []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin generated content update: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE memes SET description = ?, description_status = ?, description_generated = ?, updated_at = ? WHERE id = ?`,
		description, status, boolInt(generated), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("update generated content: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	if len(tags) > 0 {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meme_tags WHERE meme_id = ?`, id).Scan(&existing); err != nil {
			return fmt.Errorf("check existing tags: %w", err)
		}
		if existing == 0 {
			for _, tag := range tags {
				if _, err := tx.ExecContext(ctx, `INSERT INTO tags(name) VALUES (?) ON CONFLICT(name) DO NOTHING`, tag); err != nil {
					return fmt.Errorf("insert generated tag: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO meme_tags(meme_id, tag_id) SELECT ?, id FROM tags WHERE name = ? ON CONFLICT DO NOTHING`, id, tag); err != nil {
					return fmt.Errorf("link generated tag: %w", err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit generated content: %w", err)
	}
	return nil
}

func (s *Store) List(ctx context.Context, limit int, cursor, tag string) (ListResult, error) {
	if limit < 1 || limit > 100 {
		limit = 40
	}
	cursorOrder, cursorTime, cursorID, err := decodeCursor(cursor)
	if err != nil {
		return ListResult{}, err
	}

	where := []string{"1 = 1"}
	args := make([]any, 0, 6)
	if cursor != "" {
		where = append(where, `(m.sort_order < ? OR (m.sort_order = ? AND (m.created_at < ? OR (m.created_at = ? AND m.id < ?))))`)
		args = append(args, cursorOrder, cursorOrder, cursorTime, cursorTime, cursorID)
	}
	if tag != "" {
		where = append(where, `EXISTS (SELECT 1 FROM meme_tags filter_mt JOIN tags filter_t ON filter_t.id = filter_mt.tag_id WHERE filter_mt.meme_id = m.id AND filter_t.name = ?)`)
		args = append(args, strings.ToLower(strings.TrimPrefix(tag, "#")))
	}
	args = append(args, limit+1)
	query := `SELECT m.id, m.filename, m.original_name, m.mime_type, m.size, m.description,
		m.description_status, m.description_generated, m.sort_order, m.created_at,
		COALESCE(GROUP_CONCAT(t.name, ','), '')
		FROM memes m
		LEFT JOIN meme_tags mt ON mt.meme_id = m.id
		LEFT JOIN tags t ON t.id = mt.tag_id
		WHERE ` + strings.Join(where, " AND ") + `
		GROUP BY m.id ORDER BY m.sort_order DESC, m.created_at DESC, m.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ListResult{}, fmt.Errorf("list memes: %w", err)
	}
	defer rows.Close()
	memes := make([]Meme, 0, limit)
	for rows.Next() {
		var meme Meme
		var generated int
		var tagList string
		if err := rows.Scan(&meme.ID, &meme.Filename, &meme.OriginalName, &meme.MimeType, &meme.Size,
			&meme.Description, &meme.DescriptionStatus, &generated, &meme.SortOrder, &meme.CreatedAt, &tagList); err != nil {
			return ListResult{}, fmt.Errorf("scan meme: %w", err)
		}
		meme.DescriptionGenerated = generated != 0
		if tagList != "" {
			meme.Tags = strings.Split(tagList, ",")
			sort.Strings(meme.Tags)
		} else {
			meme.Tags = []string{}
		}
		memes = append(memes, meme)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, err
	}
	next := ""
	if len(memes) > limit {
		last := memes[limit-1]
		memes = memes[:limit]
		next = encodeCursor(last.SortOrder, last.CreatedAt, last.ID)
	}
	total, err := s.count(ctx, tag)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Memes: memes, NextCursor: next, Total: total}, nil
}

func (s *Store) count(ctx context.Context, tag string) (int, error) {
	var count int
	var err error
	if tag == "" {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memes`).Scan(&count)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memes m WHERE EXISTS (SELECT 1 FROM meme_tags mt JOIN tags t ON t.id = mt.tag_id WHERE mt.meme_id = m.id AND t.name = ?)`, strings.ToLower(strings.TrimPrefix(tag, "#"))).Scan(&count)
	}
	return count, err
}

func (s *Store) tags(ctx context.Context, memeID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.name FROM tags t JOIN meme_tags mt ON mt.tag_id = t.id WHERE mt.meme_id = ? ORDER BY t.name`, memeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		result = append(result, tag)
	}
	return result, rows.Err()
}

func (s *Store) Filename(ctx context.Context, id string) (string, error) {
	var filename string
	err := s.db.QueryRowContext(ctx, `SELECT filename FROM memes WHERE id = ?`, id).Scan(&filename)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return filename, err
}

func (s *Store) Delete(ctx context.Context, id string) (Meme, error) {
	meme, err := s.Get(ctx, id)
	if err != nil {
		return Meme{}, err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM memes WHERE id = ?`, id)
	if err != nil {
		return Meme{}, fmt.Errorf("delete meme: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Meme{}, err
	}
	if count == 0 {
		return Meme{}, ErrNotFound
	}
	return meme, nil
}

// Move places id immediately before beforeID in the current display order.
// An empty beforeID moves the meme to the end. Re-numbering keeps future
// inserts and cursor pagination deterministic after repeated rearrangements.
func (s *Store) Move(ctx context.Context, id, beforeID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin meme move: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM memes ORDER BY sort_order DESC, created_at DESC, id DESC`)
	if err != nil {
		return fmt.Errorf("list memes for move: %w", err)
	}
	ids := []string{}
	for rows.Next() {
		var currentID string
		if err := rows.Scan(&currentID); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, currentID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	from := -1
	for index, currentID := range ids {
		if currentID == id {
			from = index
			break
		}
	}
	if from < 0 {
		return ErrNotFound
	}
	ids = append(ids[:from], ids[from+1:]...)
	to := len(ids)
	if beforeID != "" {
		to = -1
		for index, currentID := range ids {
			if currentID == beforeID {
				to = index
				break
			}
		}
		if to < 0 {
			return ErrNotFound
		}
	}
	ids = append(ids, "")
	copy(ids[to+1:], ids[to:])
	ids[to] = id
	for index, currentID := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE memes SET sort_order = ? WHERE id = ?`, int64(len(ids)-index), currentID); err != nil {
			return fmt.Errorf("update meme move order: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit meme move: %w", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

var ErrNotFound = errors.New("meme not found")

func encodeCursor(sortOrder int64, createdAt, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d\x00%s\x00%s", sortOrder, createdAt, id)))
}

func decodeCursor(value string) (int64, string, string, error) {
	if value == "" {
		return 0, "", "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, "", "", fmt.Errorf("invalid cursor")
	}
	parts := strings.SplitN(string(decoded), "\x00", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return 0, "", "", fmt.Errorf("invalid cursor")
	}
	var sortOrder int64
	if _, err := fmt.Sscan(parts[0], &sortOrder); err != nil {
		return 0, "", "", fmt.Errorf("invalid cursor")
	}
	return sortOrder, parts[1], parts[2], nil
}
