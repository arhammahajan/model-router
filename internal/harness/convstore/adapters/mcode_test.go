package adapters

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestMcodeAdapterDiscoverMetadataAndMessages(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, ".minimax", "v2", "sqlite", "runtime-state.sqlite")

	sqliteAdapterTestExecSQL(t, dbPath, `
		CREATE TABLE local_runtime_sessions (
			session_id TEXT PRIMARY KEY,
			workspace_dir TEXT,
			session_kind TEXT,
			title TEXT,
			purpose TEXT,
			record_json TEXT,
			created_at_ms INTEGER,
			updated_at_ms INTEGER,
			parent_session_id TEXT,
			hidden INTEGER,
			archived INTEGER
		);
		CREATE TABLE local_runtime_message_rows (
			session_id TEXT,
			role TEXT,
			data_json TEXT,
			created_at_ms INTEGER
		);
		INSERT INTO local_runtime_sessions VALUES
			('mc-1', '/work/mcode', 'conversation', 'Fix Memory Leak', 'Investigate leak', '{}', 1763629000000, 1763629500000, '', 0, 0),
			('mc-2', '/work/mcode', 'conversation', '', '', '{"title":"Fallback Record Title","model":"minimax-m2"}', 1763628000000, 1763628500000, NULL, 0, 1),
			('mc-task', '/work/mcode', 'task', 'Background Task', '', '{}', 1763629100000, 1763629200000, '', 0, 0),
			('mc-child', '/work/mcode', 'conversation', 'Child Session', '', '{}', 1763629100000, 1763629200000, 'mc-1', 0, 0),
			('mc-hidden', '/work/mcode', 'conversation', 'Hidden Session', '', '{}', 1763629100000, 1763629200000, '', 1, 0);
		INSERT INTO local_runtime_message_rows VALUES
			('mc-1', 'user', '{"content":"Find the goroutine leak in worker.go"}', 1763629001000),
			('mc-1', 'assistant', '{"content":[{"type":"thinking","text":"hmm"},{"type":"text","text":"Channel was never closed on cancel."}]}', 1763629002000),
			('mc-1', 'system', '{"content":"system noise"}', 1763629000500),
			('mc-2', 'user', '{"text":"Check archived session"}', 1763628001000),
			('mc-task', 'user', '{"text":"Should be skipped"}', 1763629101000);
	`)

	// Isolated profile shadow home DB.
	shadowDB := filepath.Join(home, ".model-router", "profiles", "mcode", "staging", "home", ".minimax", "v2", "sqlite", "runtime-state.sqlite")
	sqliteAdapterTestExecSQL(t, shadowDB, `
		CREATE TABLE local_runtime_sessions (
			session_id TEXT PRIMARY KEY,
			workspace_dir TEXT,
			session_kind TEXT,
			title TEXT,
			created_at_ms INTEGER,
			updated_at_ms INTEGER
		);
		INSERT INTO local_runtime_sessions VALUES
			('mc-stage', '/work/stage', 'conversation', 'Staging Run', 1763630000000, 1763630100000);
	`)

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewMcodeAdapter()
	if adapter.Agent() != "mcode" {
		t.Fatalf("Agent() = %q", adapter.Agent())
	}
	ctx := context.Background()

	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("Discover len = %d, want 2: %+v", len(sources), sources)
	}

	var defSrc, stageSrc convstore.DiscoveredSource
	for _, s := range sources {
		if s.Profile == "" {
			defSrc = s
		} else {
			stageSrc = s
		}
	}
	if stageSrc.Profile != "staging" {
		t.Fatalf("expected staging profile, got %+v", stageSrc)
	}

	meta, err := adapter.ParseMetadata(ctx, defSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}
	if len(meta.Conversations) != 2 {
		t.Fatalf("got %d convs, want 2 (task/child/hidden excluded): %+v", len(meta.Conversations), meta.Conversations)
	}
	byID := map[string]convstore.Conversation{}
	for _, c := range meta.Conversations {
		byID[c.ID] = c
	}
	if byID["mc-1"].Title != "Fix Memory Leak" || byID["mc-1"].CWD != "/work/mcode" || byID["mc-1"].Archived {
		t.Fatalf("unexpected mc-1: %+v", byID["mc-1"])
	}
	if byID["mc-2"].Title != "Fallback Record Title" || byID["mc-2"].Model != "minimax-m2" || !byID["mc-2"].Archived {
		t.Fatalf("unexpected mc-2: %+v", byID["mc-2"])
	}

	// ParseMessages initial pass.
	msgs, err := adapter.ParseMessages(ctx, defSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if _, ok := msgs.Messages["mc-task"]; ok {
		t.Fatalf("expected task session excluded")
	}
	if len(msgs.Messages["mc-1"]) != 2 || msgs.Messages["mc-1"][0].Text != "Find the goroutine leak in worker.go" || msgs.Messages["mc-1"][1].Text != "Channel was never closed on cancel." {
		t.Fatalf("unexpected mc-1 messages: %+v", msgs.Messages["mc-1"])
	}

	// Incremental skip.
	inc, err := adapter.ParseMessages(ctx, defSrc, &convstore.SourceCursor{
		ExistingUpdatedMs: map[string]int64{
			"mc-1": 1763629500000,
			"mc-2": 1763628000000,
		},
	})
	if err != nil {
		t.Fatalf("ParseMessages(inc): %v", err)
	}
	if _, ok := inc.Messages["mc-1"]; ok {
		t.Fatalf("expected unchanged mc-1 skipped")
	}
	if len(inc.Messages["mc-2"]) != 1 || inc.Messages["mc-2"][0].Text != "Check archived session" {
		t.Fatalf("expected updated mc-2 parsed, got %+v", inc.Messages)
	}
}
