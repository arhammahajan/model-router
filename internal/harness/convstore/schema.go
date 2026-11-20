package convstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
	_ "modernc.org/sqlite"
)

const schemaVersion = 1

// DefaultDBPath returns the default SQLite path for the unified conversation store.
func DefaultDBPath(paths harnessenv.Paths) string {
	return filepath.Join(harnessenv.ModelRouterDir(paths), "conversations.db")
}

// DefaultIndexLockPath returns the flock path coordinating background message indexing.
func DefaultIndexLockPath(paths harnessenv.Paths) string {
	return filepath.Join(harnessenv.ModelRouterDir(paths), "conversations.index.lock")
}

const schemaDDL = `
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sources (
	source_key TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	profile TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL,
	kind TEXT NOT NULL,
	meta_size_bytes INTEGER NOT NULL DEFAULT 0,
	meta_mtime_ns INTEGER NOT NULL DEFAULT 0,
	meta_wal_size INTEGER NOT NULL DEFAULT 0,
	meta_wal_mtime_ns INTEGER NOT NULL DEFAULT 0,
	meta_checked_ms INTEGER NOT NULL DEFAULT 0,
	msg_size_bytes INTEGER NOT NULL DEFAULT 0,
	msg_mtime_ns INTEGER NOT NULL DEFAULT 0,
	msg_wal_size INTEGER NOT NULL DEFAULT 0,
	msg_wal_mtime_ns INTEGER NOT NULL DEFAULT 0,
	msg_byte_offset INTEGER NOT NULL DEFAULT 0,
	msg_prefix_hash TEXT NOT NULL DEFAULT '',
	msg_indexed_ms INTEGER NOT NULL DEFAULT 0,
	fail_count INTEGER NOT NULL DEFAULT 0,
	failed_mtime_ns INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sources_agent ON sources(agent);

CREATE TABLE IF NOT EXISTS conversations (
	row_id INTEGER PRIMARY KEY AUTOINCREMENT,
	agent TEXT NOT NULL,
	id TEXT NOT NULL,
	app TEXT NOT NULL DEFAULT '',
	profile TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	preview TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	created_at_ms INTEGER NOT NULL DEFAULT 0,
	updated_at_ms INTEGER NOT NULL DEFAULT 0,
	msg_updated_at_ms INTEGER NOT NULL DEFAULT 0,
	message_count INTEGER NOT NULL DEFAULT 0,
	archived INTEGER NOT NULL DEFAULT 0,
	source_key TEXT NOT NULL REFERENCES sources(source_key) ON DELETE CASCADE,
	source_path TEXT NOT NULL DEFAULT '',
	resume_target TEXT NOT NULL DEFAULT '',
	UNIQUE(agent, id)
);
CREATE INDEX IF NOT EXISTS idx_conversations_source_key ON conversations(source_key);
CREATE INDEX IF NOT EXISTS idx_conversations_agent_updated ON conversations(agent, updated_at_ms DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_updated ON conversations(updated_at_ms DESC, row_id DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_cwd ON conversations(cwd);

CREATE TABLE IF NOT EXISTS overlay (
	agent TEXT NOT NULL,
	session_id TEXT NOT NULL,
	profile TEXT NOT NULL DEFAULT '',
	space TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	last_outcome TEXT NOT NULL DEFAULT '',
	last_opened_ms INTEGER NOT NULL DEFAULT 0,
	updated_at_ms INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (agent, session_id)
);

CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	conv_row_id INTEGER NOT NULL REFERENCES conversations(row_id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	role TEXT NOT NULL,
	text TEXT NOT NULL,
	ts_ms INTEGER NOT NULL DEFAULT 0,
	truncated INTEGER NOT NULL DEFAULT 0,
	UNIQUE(conv_row_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conv_row_id, seq);

CREATE VIRTUAL TABLE IF NOT EXISTS conversations_fts USING fts5(
	title,
	preview,
	cwd,
	branch,
	model,
	profile,
	content='conversations',
	content_rowid='row_id',
	tokenize="unicode61 tokenchars '_.-/+'",
	prefix='2 3'
);

CREATE TRIGGER IF NOT EXISTS conversations_ai AFTER INSERT ON conversations BEGIN
	INSERT INTO conversations_fts(rowid, title, preview, cwd, branch, model, profile)
	VALUES (new.row_id, new.title, new.preview, new.cwd, new.branch, new.model, new.profile);
END;

CREATE TRIGGER IF NOT EXISTS conversations_ad AFTER DELETE ON conversations BEGIN
	INSERT INTO conversations_fts(conversations_fts, rowid, title, preview, cwd, branch, model, profile)
	VALUES ('delete', old.row_id, old.title, old.preview, old.cwd, old.branch, old.model, old.profile);
END;

CREATE TRIGGER IF NOT EXISTS conversations_au AFTER UPDATE ON conversations BEGIN
	INSERT INTO conversations_fts(conversations_fts, rowid, title, preview, cwd, branch, model, profile)
	VALUES ('delete', old.row_id, old.title, old.preview, old.cwd, old.branch, old.model, old.profile);
	INSERT INTO conversations_fts(rowid, title, preview, cwd, branch, model, profile)
	VALUES (new.row_id, new.title, new.preview, new.cwd, new.branch, new.model, new.profile);
END;

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	user_text,
	assistant_text,
	identifiers,
	tokenize="unicode61 tokenchars '_.-/+'",
	prefix='2 3'
);

INSERT INTO messages_fts(messages_fts, rank) VALUES ('automerge', 8);

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
	DELETE FROM messages_fts WHERE rowid = old.id;
END;
`

func buildSQLiteDSN(path string) string {
	if path == ":memory:" {
		return ":memory:"
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "temp_store(MEMORY)")
	u.RawQuery = q.Encode()
	return u.String()
}

func openRawDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", buildSQLiteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	return db, nil
}

func applyPragmas(ctx context.Context, db *sql.DB) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA temp_store = MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func initFreshSchema(ctx context.Context, db *sql.DB) error {
	if err := applyPragmas(ctx, db); err != nil {
		return fmt.Errorf("apply pragmas: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// checkDBHealth verifies quick_check and user_version.
// Returns (healthy, isEmptyNewDB).
func checkDBHealth(ctx context.Context, db *sql.DB) (healthy bool, isEmptyNewDB bool) {
	var qc string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&qc); err != nil {
		return false, false
	}
	if strings.ToLower(strings.TrimSpace(qc)) != "ok" {
		return false, false
	}

	var ver int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		return false, false
	}
	if ver == schemaVersion {
		// Verify core tables exist so a manually bumped empty DB still initializes cleanly.
		var cnt int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('meta','sources','conversations','overlay','messages','conversations_fts','messages_fts')`,
		).Scan(&cnt)
		if err == nil && cnt == 7 {
			return true, false
		}
		return false, false
	}
	if ver == 0 {
		var objCount int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master`).Scan(&objCount); err == nil && objCount == 0 {
			return false, true
		}
	}
	return false, false
}

// salvageOverlay attempts a best-effort read of any readable rows from the overlay table
// before rebuilding a corrupt or mismatched-version database.
func salvageOverlay(ctx context.Context, db *sql.DB) []OverlayRecord {
	if db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT agent, session_id, profile, space, cwd, title, model, last_outcome, last_opened_ms, updated_at_ms
		FROM overlay
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []OverlayRecord
	for rows.Next() {
		var r OverlayRecord
		if err := rows.Scan(
			&r.Agent, &r.SessionID, &r.Profile, &r.Space, &r.CWD,
			&r.Title, &r.Model, &r.LastOutcome, &r.LastOpenedMs, &r.UpdatedAtMs,
		); err != nil {
			continue
		}
		r.Agent = CanonicalAgent(r.Agent)
		if r.Agent != "" && r.SessionID != "" {
			out = append(out, r)
		}
	}
	return out
}

func restoreOverlay(ctx context.Context, db *sql.DB, records []OverlayRecord) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO overlay (
			agent, session_id, profile, space, cwd, title, model, last_outcome, last_opened_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent, session_id) DO UPDATE SET
			profile = CASE WHEN excluded.profile != '' THEN excluded.profile ELSE overlay.profile END,
			space = CASE WHEN excluded.space != '' THEN excluded.space ELSE overlay.space END,
			cwd = CASE WHEN excluded.cwd != '' THEN excluded.cwd ELSE overlay.cwd END,
			title = CASE WHEN excluded.title != '' THEN excluded.title ELSE overlay.title END,
			model = CASE WHEN excluded.model != '' THEN excluded.model ELSE overlay.model END,
			last_outcome = CASE WHEN excluded.last_outcome != '' THEN excluded.last_outcome ELSE overlay.last_outcome END,
			last_opened_ms = MAX(excluded.last_opened_ms, overlay.last_opened_ms),
			updated_at_ms = MAX(excluded.updated_at_ms, overlay.updated_at_ms)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range records {
		if _, err := stmt.ExecContext(ctx,
			CanonicalAgent(r.Agent), r.SessionID, r.Profile, r.Space, r.CWD,
			r.Title, r.Model, r.LastOutcome, r.LastOpenedMs, r.UpdatedAtMs,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func removeDBFiles(path string) {
	if path == "" || path == ":memory:" {
		return
	}
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

func ensureDBFilePermissions(path string) error {
	if path == "" || path == ":memory:" {
		return nil
	}
	if err := os.Chmod(path, 0600); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
