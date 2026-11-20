package convstore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestDefaultPaths(t *testing.T) {
	paths := harnessenv.Paths{Home: "/home/alice"}
	if got, want := DefaultDBPath(paths), "/home/alice/.model-router/conversations.db"; got != want {
		t.Fatalf("DefaultDBPath = %q, want %q", got, want)
	}
	if got, want := DefaultIndexLockPath(paths), "/home/alice/.model-router/conversations.index.lock"; got != want {
		t.Fatalf("DefaultIndexLockPath = %q, want %q", got, want)
	}
}

func TestOpenSchemaPragmasAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "convstore")
	dbPath := filepath.Join(dir, "conversations.db")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if st.Path() != dbPath {
		t.Fatalf("Path = %q, want %q", st.Path(), dbPath)
	}

	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("db mode = %04o, want 0600", perm)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0700 {
		t.Fatalf("dir mode = %04o, want 0700", perm)
	}

	var journalMode string
	if err := st.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var fk int
	if err := st.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}

	var userVer int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&userVer); err != nil {
		t.Fatalf("pragma user_version: %v", err)
	}
	if userVer != schemaVersion {
		t.Fatalf("user_version = %d, want %d", userVer, schemaVersion)
	}
}

func TestOpenRecoveryOnVersionMismatchPreservesOverlay(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "conversations.db")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open initial: %v", err)
	}

	err = st.UpsertOverlay(ctx, OverlayRecord{
		Agent:        "claude-code",
		SessionID:    "sess-keep",
		Profile:      "work",
		Space:        "api",
		CWD:          "/repo/api",
		Title:        "Pinned Title",
		Model:        "claude-opus",
		LastOutcome:  "done",
		LastOpenedMs: 1700000005000,
		UpdatedAtMs:  1700000005000,
	})
	if err != nil {
		t.Fatalf("UpsertOverlay: %v", err)
	}

	// Simulate an incompatible schema version.
	if _, err := st.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen should detect mismatch, salvage overlay, rebuild DB, and restore overlay.
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open after version mismatch: %v", err)
	}
	defer st2.Close()

	var ver int
	if err := st2.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if ver != schemaVersion {
		t.Fatalf("rebuilt user_version = %d, want %d", ver, schemaVersion)
	}

	var prof, space, cwd, title, model, outcome string
	var lastOpened int64
	err = st2.db.QueryRow(`
		SELECT profile, space, cwd, title, model, last_outcome, last_opened_ms
		FROM overlay WHERE agent = 'claude' AND session_id = 'sess-keep'
	`).Scan(&prof, &space, &cwd, &title, &model, &outcome, &lastOpened)
	if err != nil {
		t.Fatalf("query restored overlay: %v", err)
	}
	if prof != "work" || space != "api" || cwd != "/repo/api" || title != "Pinned Title" || model != "claude-opus" || outcome != "done" || lastOpened != 1700000005000 {
		t.Fatalf("restored overlay mismatch: prof=%q space=%q cwd=%q title=%q model=%q outcome=%q lastOpened=%d",
			prof, space, cwd, title, model, outcome, lastOpened)
	}
}

func TestOpenRecoveryOnCorruptFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "conversations.db")
	if err := os.WriteFile(dbPath, []byte("this is definitely not a valid sqlite database file"), 0600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("corrupt wal"), 0600); err != nil {
		t.Fatalf("write corrupt wal: %v", err)
	}

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open corrupt file: %v", err)
	}
	defer st.Close()

	status, err := st.Status(context.Background())
	if err != nil {
		t.Fatalf("Status after recovery: %v", err)
	}
	if status.Conversations != 0 || status.Sources != 0 {
		t.Fatalf("unexpected status after corrupt recovery: %+v", status)
	}
}

func TestUpsertOverlayNonDestructiveMerge(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "conversations.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Seed with alias "agy" -> canonical "antigravity".
	if err := st.UpsertOverlay(ctx, OverlayRecord{
		Agent:        "agy",
		SessionID:    "s1",
		Profile:      "personal",
		Space:        "main",
		CWD:          "/home/dev/proj",
		Title:        "Initial Title",
		Model:        "gemini-pro",
		LastOutcome:  "running",
		LastOpenedMs: 2000,
		UpdatedAtMs:  2000,
	}); err != nil {
		t.Fatalf("first UpsertOverlay: %v", err)
	}

	// Second update only updates LastOutcome and LastOpenedMs (and attempts lower UpdatedAtMs);
	// empty fields must preserve prior values.
	if err := st.UpsertOverlay(ctx, OverlayRecord{
		Agent:        "antigravity",
		SessionID:    "s1",
		LastOutcome:  "done",
		LastOpenedMs: 1500, // lower than 2000 -> MAX keeps 2000
		UpdatedAtMs:  3000,
	}); err != nil {
		t.Fatalf("second UpsertOverlay: %v", err)
	}

	// Third update bumps LastOpenedMs and changes Title.
	if err := st.UpsertOverlay(ctx, OverlayRecord{
		Agent:        "antigravity",
		SessionID:    "s1",
		Title:        "Renamed Title",
		LastOpenedMs: 5000,
		UpdatedAtMs:  5000,
	}); err != nil {
		t.Fatalf("third UpsertOverlay: %v", err)
	}

	var rec OverlayRecord
	err = st.db.QueryRow(`
		SELECT agent, session_id, profile, space, cwd, title, model, last_outcome, last_opened_ms, updated_at_ms
		FROM overlay WHERE agent = 'antigravity' AND session_id = 's1'
	`).Scan(&rec.Agent, &rec.SessionID, &rec.Profile, &rec.Space, &rec.CWD, &rec.Title, &rec.Model, &rec.LastOutcome, &rec.LastOpenedMs, &rec.UpdatedAtMs)
	if err != nil {
		t.Fatalf("scan overlay: %v", err)
	}

	if rec.Profile != "personal" || rec.Space != "main" || rec.CWD != "/home/dev/proj" ||
		rec.Title != "Renamed Title" || rec.Model != "gemini-pro" || rec.LastOutcome != "done" ||
		rec.LastOpenedMs != 5000 || rec.UpdatedAtMs != 5000 {
		t.Fatalf("unexpected merged overlay: %+v", rec)
	}
}

func TestListFilteringOverlayMergeAndOrdering(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "conversations.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Seed sources and conversations directly via a mock adapter sync.
	ad := &mockAdapter{
		agent: "claude",
		sources: []DiscoveredSource{
			{SourceKey: "claude::/s1", Agent: "claude", Path: "/s1", Kind: SourceFile, SizeBytes: 100, MtimeNs: 1000},
		},
		metaByKey: map[string][]Conversation{
			"claude::/s1": {
				{ID: "c-old-opened", Title: "Old Updated But Recently Opened", CWD: "/work/repo", UpdatedAtMs: 1000},
				{ID: "c-new", Title: "Newest Native", CWD: "/work/repo/sub", UpdatedAtMs: 3000, Model: "sonnet"},
				{ID: "c-archived", Title: "Archived Chat", CWD: "/work/repo", UpdatedAtMs: 4000, Archived: true},
				{ID: "c-tmp", Title: "Tmp Test Run", CWD: "/tmp/scratch-123", UpdatedAtMs: 5000},
				{ID: "c-other-cwd", Title: "Other Project", CWD: "/work/repo2", UpdatedAtMs: 2500},
			},
		},
	}
	adCodex := &mockAdapter{
		agent: "codex",
		sources: []DiscoveredSource{
			{SourceKey: "codex::/s2", Agent: "codex", Path: "/s2", Kind: SourceFile, SizeBytes: 50, MtimeNs: 2000},
		},
		metaByKey: map[string][]Conversation{
			"codex::/s2": {
				{ID: "x1", Title: "Codex Chat", CWD: "/work/repo", UpdatedAtMs: 2000},
			},
		},
	}

	if _, err := st.SyncMetadata(ctx, nil, []Adapter{ad, adCodex}); err != nil {
		t.Fatalf("SyncMetadata: %v", err)
	}

	// Add overlay bumping c-old-opened via last_opened_ms and overriding title/space/model.
	if err := st.UpsertOverlay(ctx, OverlayRecord{
		Agent:        "claude-code",
		SessionID:    "c-old-opened",
		Profile:      "pro",
		Space:        "backend",
		Title:        "Overlay Title",
		Model:        "opus",
		LastOutcome:  "done",
		LastOpenedMs: 9000,
	}); err != nil {
		t.Fatalf("UpsertOverlay: %v", err)
	}

	// Default list: excludes archived and /tmp, ordered by max(updated_at_ms, last_opened_ms) DESC.
	got, err := st.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("List default: %v", err)
	}
	wantIDs := []string{"c-old-opened", "c-new", "c-other-cwd", "x1"}
	if len(got) != len(wantIDs) {
		t.Fatalf("default list len = %d, want %d (%+v)", len(got), len(wantIDs), got)
	}
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Fatalf("default list[%d].ID = %q, want %q", i, got[i].ID, w)
		}
	}
	// Check overlay merge on c-old-opened.
	if got[0].Title != "Overlay Title" || got[0].Profile != "pro" || got[0].Space != "backend" ||
		got[0].Model != "opus" || got[0].LastOutcome != "done" || got[0].LastOpenedMs != 9000 ||
		got[0].App != "claude-code" {
		t.Fatalf("overlay merge mismatch: %+v", got[0])
	}

	// Agent filter with alias "claude-code".
	gotClaude, err := st.List(ctx, ListOptions{Agent: "claude-code"})
	if err != nil {
		t.Fatalf("List agent: %v", err)
	}
	if len(gotClaude) != 3 {
		t.Fatalf("claude list len = %d, want 3", len(gotClaude))
	}

	// CWD filter: "/work/repo" matches "/work/repo" and "/work/repo/sub", NOT "/work/repo2".
	gotCWD, err := st.List(ctx, ListOptions{CWD: "/work/repo/"})
	if err != nil {
		t.Fatalf("List CWD: %v", err)
	}
	if len(gotCWD) != 3 {
		t.Fatalf("cwd list len = %d, want 3 (%+v)", len(gotCWD), gotCWD)
	}
	for _, c := range gotCWD {
		if c.ID == "c-other-cwd" {
			t.Fatalf("CWD prefix filter erroneously matched sibling directory /work/repo2")
		}
	}

	// IncludeArchived + IncludeTmp.
	gotAll, err := st.List(ctx, ListOptions{IncludeArchived: true, IncludeTmp: true})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(gotAll) != 6 {
		t.Fatalf("all list len = %d, want 6", len(gotAll))
	}

	// Limit.
	gotLim, err := st.List(ctx, ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List limit: %v", err)
	}
	if len(gotLim) != 2 {
		t.Fatalf("limit list len = %d, want 2", len(gotLim))
	}

	// Ensure unused sql import not needed; sanity check Status.
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Conversations != 6 || status.Sources != 2 || status.PendingSources != 2 || status.LastSyncedMs <= 0 {
		t.Fatalf("unexpected status: %+v", status)
	}
	_ = sql.ErrNoRows
}
