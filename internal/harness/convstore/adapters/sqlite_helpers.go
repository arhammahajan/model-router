package adapters

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// OpenReadOnly opens a native agent SQLite database in read-only mode with a
// short busy timeout so indexing never blocks or mutates live coding agents.
func OpenReadOnly(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(1500)")
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("adapters: open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// StatSQLite stats both the primary SQLite file and its sibling -wal file so
// WAL-mode writes invalidate the watermark immediately.
func StatSQLite(path string) (sizeBytes, mtimeNs, walSize, walMtimeNs int64, ok bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0, 0, 0, 0, false
	}
	sizeBytes = fi.Size()
	mtimeNs = fi.ModTime().UnixNano()
	if wfi, werr := os.Stat(path + "-wal"); werr == nil && !wfi.IsDir() {
		walSize = wfi.Size()
		walMtimeNs = wfi.ModTime().UnixNano()
	}
	return sizeBytes, mtimeNs, walSize, walMtimeNs, true
}

// TableExists reports whether a table or virtual table exists in db.
func TableExists(ctx context.Context, db *sql.DB, table string) bool {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name = ? LIMIT 1`,
		table,
	).Scan(&name)
	return err == nil && name != ""
}

// ColumnExists reports whether table has column col.
func ColumnExists(ctx context.Context, db *sql.DB, table, col string) bool {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info("`+strings.ReplaceAll(table, `"`, `""`)+`")`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
			if strings.EqualFold(name, col) {
				return true
			}
		}
	}
	return false
}

// NullString unwraps a sql.NullString safely.
func NullString(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

// NullInt64 unwraps a sql.NullInt64 safely.
func NullInt64(ni sql.NullInt64) int64 {
	if !ni.Valid {
		return 0
	}
	return ni.Int64
}
