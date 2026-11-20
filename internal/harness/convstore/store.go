package convstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultListLimit   = 2000
	defaultSearchLimit = 50
)

// Store wraps the unified SQLite conversation database.
type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

// Open opens (or initializes/recovers) the SQLite conversation store at path.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("convstore: db path is required")
	}
	if path != ":memory:" {
		path = filepath.Clean(path)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, fmt.Errorf("convstore: create db dir: %w", err)
		}
	}

	ctx := context.Background()
	db, err := openRawDB(path)
	if err != nil {
		return nil, fmt.Errorf("convstore: open %s: %w", path, err)
	}

	healthy, isEmptyNew := checkDBHealth(ctx, db)
	if healthy {
		if err := applyPragmas(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("convstore: apply pragmas: %w", err)
		}
		if err := ensureDBFilePermissions(path); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("convstore: chmod db: %w", err)
		}
		return &Store{db: db, path: path}, nil
	}

	if isEmptyNew {
		if err := initFreshSchema(ctx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("convstore: init schema: %w", err)
		}
		if err := ensureDBFilePermissions(path); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("convstore: chmod db: %w", err)
		}
		return &Store{db: db, path: path}, nil
	}

	// Schema mismatch or corruption: salvage overlay rows, recreate DB, restore overlay.
	salvaged := salvageOverlay(ctx, db)
	_ = db.Close()
	removeDBFiles(path)

	freshDB, err := openRawDB(path)
	if err != nil {
		return nil, fmt.Errorf("convstore: reopen after rebuild %s: %w", path, err)
	}
	if err := initFreshSchema(ctx, freshDB); err != nil {
		_ = freshDB.Close()
		return nil, fmt.Errorf("convstore: init schema after rebuild: %w", err)
	}
	if err := restoreOverlay(ctx, freshDB, salvaged); err != nil {
		_ = freshDB.Close()
		return nil, fmt.Errorf("convstore: restore overlay: %w", err)
	}
	if err := ensureDBFilePermissions(path); err != nil {
		_ = freshDB.Close()
		return nil, fmt.Errorf("convstore: chmod db: %w", err)
	}
	return &Store{db: freshDB, path: path}, nil
}

// Close closes the underlying SQLite database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the filesystem path of the SQLite database.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// withImmediateTx serializes intra-process writes and runs fn inside a SQLite
// BEGIN IMMEDIATE transaction on a dedicated connection.
func (s *Store) withImmediateTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}

	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// List returns conversations merged with overlay fields, filtered and ordered by recency.
func (s *Store) List(ctx context.Context, opts ListOptions) ([]Conversation, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}

	var conds []string
	var args []any

	if agent := CanonicalAgent(opts.Agent); agent != "" {
		conds = append(conds, "c.agent = ?")
		args = append(args, agent)
	}
	if !opts.IncludeArchived {
		conds = append(conds, "c.archived = 0")
		// Hide empty unstarted sessions (e.g. health-check probe stubs with no title, preview, or messages) by default.
		conds = append(conds, "(COALESCE(NULLIF(o.title, ''), c.title) != '' OR c.preview != '' OR c.message_count > 0)")
	}

	effCWDExpr := "COALESCE(NULLIF(o.cwd, ''), c.cwd)"
	if cwd := strings.TrimSpace(opts.CWD); cwd != "" {
		cleanCWD := filepath.Clean(cwd)
		if cleanCWD == "/" {
			conds = append(conds, "substr("+effCWDExpr+", 1, 1) = '/'")
		} else {
			normExpr := "CASE WHEN " + effCWDExpr + " = '/' THEN '/' ELSE RTRIM(" + effCWDExpr + ", '/') END"
			conds = append(conds, "("+normExpr+" = ? OR substr("+normExpr+", 1, ?) = ?)")
			prefix := cleanCWD + "/"
			args = append(args, cleanCWD, len(prefix), prefix)
		}
	}
	if !opts.IncludeTmp {
		conds = append(conds, ephemeralCWDExclusionSQL(effCWDExpr))
	}

	query := `
		SELECT
			c.row_id,
			c.agent,
			c.id,
			c.app,
			COALESCE(NULLIF(o.profile, ''), c.profile) AS eff_profile,
			COALESCE(NULLIF(o.title, ''), c.title) AS eff_title,
			c.preview,
			COALESCE(NULLIF(o.cwd, ''), c.cwd) AS eff_cwd,
			c.branch,
			COALESCE(NULLIF(o.model, ''), c.model) AS eff_model,
			c.created_at_ms,
			c.updated_at_ms,
			c.message_count,
			c.archived,
			c.source_key,
			c.source_path,
			c.resume_target,
			COALESCE(o.space, '') AS eff_space,
			COALESCE(o.last_outcome, '') AS eff_outcome,
			COALESCE(o.last_opened_ms, 0) AS eff_last_opened_ms
		FROM conversations c
		LEFT JOIN overlay o ON o.agent = c.agent AND o.session_id = c.id
	`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY MAX(c.updated_at_ms, COALESCE(o.last_opened_ms, 0)) DESC, c.row_id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("convstore: list: %w", err)
	}
	defer rows.Close()

	out := make([]Conversation, 0, 32)
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("convstore: list scan: %w", err)
		}
		if !opts.IncludeTmp && IsEphemeralCWD(c.CWD) {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("convstore: list rows: %w", err)
	}
	return out, nil
}

func scanConversation(scanner interface{ Scan(dest ...any) error }) (Conversation, error) {
	var c Conversation
	var archivedInt int
	err := scanner.Scan(
		&c.RowID,
		&c.Agent,
		&c.ID,
		&c.App,
		&c.Profile,
		&c.Title,
		&c.Preview,
		&c.CWD,
		&c.Branch,
		&c.Model,
		&c.CreatedAtMs,
		&c.UpdatedAtMs,
		&c.MessageCount,
		&archivedInt,
		&c.SourceKey,
		&c.SourcePath,
		&c.ResumeTarget,
		&c.Space,
		&c.LastOutcome,
		&c.LastOpenedMs,
	)
	if err != nil {
		return Conversation{}, err
	}
	c.Archived = archivedInt != 0
	if c.App == "" {
		c.App = DisplayApp(c.Agent)
	}
	return c, nil
}

func ephemeralCWDExclusionSQL(expr string) string {
	return "NOT (" +
		expr + " = '/tmp' OR substr(" + expr + ", 1, 5) = '/tmp/' OR " +
		expr + " = '/var/tmp' OR substr(" + expr + ", 1, 9) = '/var/tmp/' OR " +
		expr + " = '/private/tmp' OR substr(" + expr + ", 1, 13) = '/private/tmp/'" +
		")"
}

// UpsertOverlay merges non-empty tmux/hook enrichment fields into the overlay table
// without clobbering existing non-empty fields with empty values.
func (s *Store) UpsertOverlay(ctx context.Context, rec OverlayRecord) error {
	rec.Agent = CanonicalAgent(rec.Agent)
	rec.SessionID = strings.TrimSpace(rec.SessionID)
	if rec.Agent == "" || rec.SessionID == "" {
		return errors.New("convstore: overlay requires non-empty agent and session_id")
	}
	if rec.UpdatedAtMs <= 0 {
		rec.UpdatedAtMs = time.Now().UnixMilli()
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
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
		`,
			rec.Agent, rec.SessionID, rec.Profile, rec.Space, rec.CWD,
			rec.Title, rec.Model, rec.LastOutcome, rec.LastOpenedMs, rec.UpdatedAtMs,
		)
		if err != nil {
			return fmt.Errorf("convstore: upsert overlay: %w", err)
		}
		return nil
	})
}

const pendingSourcesWhereSQL = `
	(
		msg_mtime_ns != meta_mtime_ns
		OR msg_size_bytes != meta_size_bytes
		OR msg_wal_mtime_ns != meta_wal_mtime_ns
		OR msg_wal_size != meta_wal_size
		OR msg_indexed_ms = 0
	)
	AND NOT (fail_count >= 3 AND failed_mtime_ns = meta_mtime_ns)
`

// Status returns a snapshot of the SQLite index state for UI banners and CLI status.
func (s *Store) Status(ctx context.Context) (IndexingStatus, error) {
	st := IndexingStatus{DBPath: s.path}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&st.Conversations); err != nil {
		return st, fmt.Errorf("convstore: status conversations: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&st.Sources); err != nil {
		return st, fmt.Errorf("convstore: status sources: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources WHERE `+pendingSourcesWhereSQL).Scan(&st.PendingSources); err != nil {
		return st, fmt.Errorf("convstore: status pending: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&st.IndexedMessages); err != nil {
		return st, fmt.Errorf("convstore: status messages: %w", err)
	}

	var syncedStr string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'last_synced_ms'`).Scan(&syncedStr)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("convstore: status meta: %w", err)
	}
	if syncedStr != "" {
		if v, perr := strconv.ParseInt(strings.TrimSpace(syncedStr), 10, 64); perr == nil {
			st.LastSyncedMs = v
		}
	}
	return st, nil
}
