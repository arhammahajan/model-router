package spaceresume

import (
	"os"
	"testing"
)

func writeFileForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteReaderBasic(t *testing.T) {
	path := t.TempDir() + "/t.db"
	writeTestDB(t, path, "sessions",
		"CREATE TABLE sessions (id TEXT, name TEXT)",
		[][]any{{"d1", "alpha"}, {"d2", "beta"}})
	rows, err := sqliteRows(path, "sessions", map[string]string{"name": "beta"}, "", 0)
	if err != nil || len(rows) != 1 || sqliteText(rows[0]["id"]) != "d2" {
		t.Fatalf("rows = %v err = %v", rows, err)
	}
	// Missing table errors.
	if _, err := sqliteRows(path, "nope", nil, "", 0); err == nil {
		t.Error("expected table error")
	}
	// Corrupt file degrades.
	bad := t.TempDir() + "/bad.db"
	writeFileForTest(t, bad, "not a database")
	if _, err := sqliteRows(bad, "sessions", nil, "", 0); err == nil {
		t.Error("expected corrupt error")
	}
}

func TestSQLiteLatestIDOrdering(t *testing.T) {
	path := t.TempDir() + "/o.db"
	writeTestDB(t, path, "session_v2",
		"CREATE TABLE session_v2 (id TEXT, directory TEXT, time_updated INTEGER)",
		[][]any{{"a", "/w", int64(1)}, {"b", "/w", int64(5)}, {"c", "/w", int64(3)}})
	rows, err := sqliteRows(path, "session_v2", map[string]string{"directory": "/w"}, "time_updated", 1)
	if err != nil || len(rows) != 1 || sqliteText(rows[0]["id"]) != "b" {
		t.Fatalf("rows = %v err = %v", rows, err)
	}
	if id, ok := sqliteLatestID(path, "session_v2", "directory", "/w", "id", "time_updated", false); !ok || id != "b" {
		t.Errorf("latest = %q %v", id, ok)
	}
	if _, ok := sqliteLatestID(path, "session_v2", "directory", "/missing", "id", "time_updated", false); ok {
		t.Error("missing should not match")
	}
}

func TestSQLiteTaskExclusion(t *testing.T) {
	path := t.TempDir() + "/m.db"
	writeTestDB(t, path, "local_runtime_sessions",
		"CREATE TABLE local_runtime_sessions (session_id TEXT, workspace_dir TEXT, session_kind TEXT, updated_at_ms INTEGER)",
		[][]any{
			{"task-9", "/w", "task", int64(9)},
			{"conv-1", "/w", "conversation", int64(2)},
			{"nullkind", "/w", nil, int64(8)},
		})
	// Newest non-task is nullkind (updated 8), task-9 skipped.
	if id, ok := sqliteLatestID(path, "local_runtime_sessions", "workspace_dir", "/w", "session_id", "updated_at_ms", true); !ok || id != "nullkind" {
		t.Errorf("got %q %v", id, ok)
	}
}

func TestSQLiteIntegerWidths(t *testing.T) {
	path := t.TempDir() + "/i.db"
	writeTestDB(t, path, "t",
		"CREATE TABLE t (a INTEGER, b TEXT)",
		[][]any{{int64(0), "zero"}, {int64(1), "one"}, {int64(-300), "neg"}, {int64(1 << 40), "big"}})
	rows, err := sqliteRows(path, "t", nil, "a", 0)
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows = %v err = %v", rows, err)
	}
	// Newest-first by a: big, one, zero, neg.
	if sqliteText(rows[0]["b"]) != "big" || sqliteText(rows[3]["b"]) != "neg" {
		t.Errorf("order = %v %v", rows[0]["b"], rows[3]["b"])
	}
}
