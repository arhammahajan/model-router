package adapters

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestCodexAdapterSQLiteAndRolloutFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	codexDir := filepath.Join(home, ".codex")
	sessDir := filepath.Join(codexDir, "sessions", "2026", "11", "20")
	archDir := filepath.Join(codexDir, "archived_sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rollout1 := filepath.Join(sessDir, "rollout-2026-th-1.jsonl")
	r1Content := strings.Join([]string{
		`{"type":"session_meta","timestamp":"2026-11-20T09:00:00Z","payload":{"id":"th-1","cwd":"/work/cx","git":{"branch":"main"}}}`,
		`{"type":"event_msg","timestamp":"2026-11-20T09:00:01Z","payload":{"type":"user_message","message":"Build the SQLite store"}}`,
		`{"type":"response_item","timestamp":"2026-11-20T09:00:05Z","payload":{"role":"assistant","content":[{"type":"output_text","text":"Starting SQLite store implementation."}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(rollout1, []byte(r1Content), 0o644); err != nil {
		t.Fatal(err)
	}

	rollout2 := filepath.Join(sessDir, "rollout-2026-th-2.jsonl")
	r2Content := strings.Join([]string{
		`{"type":"session_meta","timestamp":"2026-11-20T09:10:00Z","payload":{"id":"th-2","cwd":"/work/cx"}}`,
		`{"type":"event_msg","timestamp":"2026-11-20T09:10:01Z","payload":{"type":"user_message","text":"Second thread prompt"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(rollout2, []byte(r2Content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Untracked archived rollout (not in state_5.sqlite -> discovered as fallback SourceFile).
	untracked := filepath.Join(archDir, "rollout-2026-untracked.jsonl")
	uContent := strings.Join([]string{
		`{"type":"session_meta","timestamp":"2026-11-20T08:00:00Z","payload":{"id":"th-untracked","cwd":"/work/old","model_provider":"openai"}}`,
		`{"type":"response_item","timestamp":"2026-11-20T08:00:02Z","payload":{"role":"user","content":"Legacy rollout question"}}`,
		`{"type":"response_item","timestamp":"2026-11-20T08:00:04Z","payload":{"role":"assistant","content":"Legacy rollout answer"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(untracked, []byte(uContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Older state_4.sqlite and newer state_5.sqlite (state_5 must win).
	sqliteAdapterTestExecSQL(t, filepath.Join(codexDir, "state_4.sqlite"), `CREATE TABLE threads (id TEXT);`)
	dbPath := filepath.Join(codexDir, "state_5.sqlite")
	sqliteAdapterTestExecSQL(t, dbPath, `
		CREATE TABLE threads (
			id TEXT PRIMARY KEY,
			rollout_path TEXT,
			created_at INTEGER,
			updated_at INTEGER,
			cwd TEXT,
			title TEXT,
			first_user_message TEXT,
			archived INTEGER,
			git_branch TEXT,
			model_provider TEXT
		);
		CREATE TABLE thread_spawn_edges (
			parent_thread_id TEXT,
			child_thread_id TEXT
		);
		INSERT INTO threads VALUES
			('th-1', '`+rollout1+`', 1763629200, 1763629500, '/work/cx', 'Raw DB Title', 'Build the SQLite store', 0, 'main', 'openai'),
			('th-2', '`+rollout2+`', 1763629800, 1763629900, '/work/cx', 'Second Thread', 'Second thread prompt', 1, 'dev', 'openai'),
			('th-child', '', 1763629900, 1763629950, '/work/cx', 'Subagent Thread', 'child work', 0, 'main', 'openai');
		INSERT INTO thread_spawn_edges VALUES ('th-1', 'th-child');
	`)

	// session_index.jsonl renames th-1.
	if err := os.WriteFile(filepath.Join(codexDir, "session_index.jsonl"),
		[]byte(`{"id":"th-1","thread_name":"User Renamed SQLite Thread"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewCodexAdapter()
	ctx := context.Background()

	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("Discover len = %d, want 2 (state_5.sqlite + untracked rollout): %+v", len(sources), sources)
	}

	var sqlSrc, fileSrc convstore.DiscoveredSource
	for _, s := range sources {
		if s.Kind == convstore.SourceSQLite {
			sqlSrc = s
		} else {
			fileSrc = s
		}
	}
	if filepath.Base(sqlSrc.Path) != "state_5.sqlite" {
		t.Fatalf("expected state_5.sqlite, got %s", sqlSrc.Path)
	}
	if fileSrc.Path != filepath.Clean(untracked) {
		t.Fatalf("expected untracked rollout, got %s", fileSrc.Path)
	}

	// ParseMetadata for SQLite source.
	meta, err := adapter.ParseMetadata(ctx, sqlSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(sqlite): %v", err)
	}
	if len(meta.Conversations) != 2 {
		t.Fatalf("got %d conversations, want 2 (child excluded): %+v", len(meta.Conversations), meta.Conversations)
	}
	byID := map[string]convstore.Conversation{}
	for _, c := range meta.Conversations {
		byID[c.ID] = c
	}
	if byID["th-1"].Title != "User Renamed SQLite Thread" || byID["th-1"].Branch != "main" || byID["th-1"].Archived {
		t.Fatalf("unexpected th-1 metadata: %+v", byID["th-1"])
	}
	if !byID["th-2"].Archived {
		t.Fatalf("expected th-2 archived=true")
	}

	// ParseMessages for SQLite source: initial pass parses both th-1 and th-2.
	msgRes, err := adapter.ParseMessages(ctx, sqlSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages(sqlite): %v", err)
	}
	if len(msgRes.Messages["th-1"]) != 2 || len(msgRes.Messages["th-2"]) != 1 {
		t.Fatalf("unexpected sqlite messages: %+v", msgRes.Messages)
	}

	// Incremental skip: mark th-1 as already up to date, th-2 as stale -> only th-2 parsed.
	prev := &convstore.SourceCursor{
		ExistingUpdatedMs: map[string]int64{
			"th-1": byID["th-1"].UpdatedAtMs,
			"th-2": byID["th-2"].UpdatedAtMs - 1000,
		},
	}
	incRes, err := adapter.ParseMessages(ctx, sqlSrc, prev)
	if err != nil {
		t.Fatalf("ParseMessages(sqlite incremental): %v", err)
	}
	if _, ok := incRes.Messages["th-1"]; ok {
		t.Fatalf("expected unchanged th-1 to be skipped")
	}
	if len(incRes.Messages["th-2"]) != 1 {
		t.Fatalf("expected updated th-2 to be parsed, got %+v", incRes.Messages)
	}

	// ParseMetadata + incremental ParseMessages on fallback SourceFile.
	fMeta, err := adapter.ParseMetadata(ctx, fileSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(file): %v", err)
	}
	if len(fMeta.Conversations) != 1 || fMeta.Conversations[0].ID != "th-untracked" || !fMeta.Conversations[0].Archived {
		t.Fatalf("unexpected fallback metadata: %+v", fMeta.Conversations)
	}
	fMsgs, err := adapter.ParseMessages(ctx, fileSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages(file): %v", err)
	}
	if len(fMsgs.Messages["th-untracked"]) != 2 {
		t.Fatalf("unexpected file messages: %+v", fMsgs.Messages)
	}

	// Append to fallback rollout and confirm Append=true.
	af, err := os.OpenFile(untracked, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = af.WriteString(`{"type":"event_msg","timestamp":"2026-11-20T08:01:00Z","payload":{"type":"user_message","message":"Follow-up question"}}` + "\n")
	af.Close()

	fInc, err := adapter.ParseMessages(ctx, fileSrc, &convstore.SourceCursor{
		MsgByteOffset:     fMsgs.ByteOffset,
		MsgPrefixHash:     fMsgs.PrefixHash,
		ExistingUpdatedMs: map[string]int64{"th-untracked": 1},
	})
	if err != nil {
		t.Fatalf("ParseMessages(file incremental): %v", err)
	}
	if !fInc.Append || len(fInc.Messages["th-untracked"]) != 1 || fInc.Messages["th-untracked"][0].Text != "Follow-up question" {
		t.Fatalf("unexpected file incremental result: %+v", fInc)
	}
}
