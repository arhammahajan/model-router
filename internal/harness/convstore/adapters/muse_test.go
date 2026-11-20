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

func TestMuseAdapterDiscoverMetadataAndMessages(t *testing.T) {
	home := t.TempDir()
	dataHome := filepath.Join(home, "xdg-data")
	dbPath := filepath.Join(dataHome, "muse", "session-index.db")

	logPath := filepath.Join(t.TempDir(), "muse-1.jsonl")
	logContent := strings.Join([]string{
		`{"role":"user","timestamp":1763629001000,"content":"Analyze distributed lock contention"}`,
		`{"role":"assistant","timestamp":1763629002000,"content":[{"type":"text","text":"Found lock hot spot in lease.go."}]}`,
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}

	sqliteAdapterTestExecSQL(t, dbPath, `
		CREATE TABLE sessions (
			session_id TEXT PRIMARY KEY,
			workspace_root TEXT,
			git_branch TEXT,
			session_name TEXT,
			effective_long_title TEXT,
			title TEXT,
			msp_title TEXT,
			first_user_prompt TEXT,
			search_text TEXT,
			created_at_us INTEGER,
			updated_at_us INTEGER,
			session_log_path TEXT,
			message_count INTEGER,
			msp_parent_session_id TEXT
		);
		INSERT INTO sessions VALUES
			('ms-1', '/work/muse-repo', 'feat/locks', 'Named Lock Investigation', 'Long Title', 'Short', '', 'Analyze distributed lock contention', 'fallback search text', 1763629000000000, 1763629500000000, '`+logPath+`', 4, ''),
			('ms-2', '/work/muse-repo', 'main', '', '', 'No Log Session', '', 'Initial prompt for ms-2', 'Indexed fallback search text for ms-2', 1763628000000000, 1763628500000000, '/nonexistent/missing.jsonl', 2, NULL),
			('ms-child', '/work/muse-repo', 'main', 'Child Task', '', '', '', 'child prompt', 'child text', 1763629100000000, 1763629200000000, '', 1, 'ms-1');
	`)

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home, DataHome: dataHome})
	adapter := NewMuseAdapter()
	if adapter.Agent() != "muse" {
		t.Fatalf("Agent() = %q", adapter.Agent())
	}
	ctx := context.Background()

	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sources) != 1 || sources[0].Path != filepath.Clean(dbPath) {
		t.Fatalf("unexpected sources: %+v", sources)
	}

	meta, err := adapter.ParseMetadata(ctx, sources[0], nil)
	if err != nil {
		t.Fatalf("ParseMetadata: %v", err)
	}
	if len(meta.Conversations) != 2 {
		t.Fatalf("got %d convs, want 2 (child excluded): %+v", len(meta.Conversations), meta.Conversations)
	}
	byID := map[string]convstore.Conversation{}
	for _, c := range meta.Conversations {
		byID[c.ID] = c
	}
	m1 := byID["ms-1"]
	if m1.Title != "Named Lock Investigation" || m1.CWD != "/work/muse-repo" || m1.Branch != "feat/locks" || m1.SourcePath != filepath.Clean(logPath) || m1.MessageCount != 4 {
		t.Fatalf("unexpected ms-1 metadata: %+v", m1)
	}
	if m1.CreatedAtMs != 1763629000000 || m1.UpdatedAtMs != 1763629500000 {
		t.Fatalf("expected us->ms conversion, got created=%d updated=%d", m1.CreatedAtMs, m1.UpdatedAtMs)
	}

	// ParseMessages: ms-1 from log JSONL, ms-2 falls back to search_text.
	msgs, err := adapter.ParseMessages(ctx, sources[0], nil)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if _, ok := msgs.Messages["ms-child"]; ok {
		t.Fatalf("expected child session excluded")
	}
	if len(msgs.Messages["ms-1"]) != 2 || msgs.Messages["ms-1"][0].Text != "Analyze distributed lock contention" || msgs.Messages["ms-1"][1].Text != "Found lock hot spot in lease.go." {
		t.Fatalf("unexpected ms-1 messages: %+v", msgs.Messages["ms-1"])
	}
	if len(msgs.Messages["ms-2"]) != 1 || msgs.Messages["ms-2"][0].Text != "Indexed fallback search text for ms-2" {
		t.Fatalf("unexpected ms-2 fallback messages: %+v", msgs.Messages["ms-2"])
	}

	// Incremental skip: ms-1 unchanged, ms-2 stale.
	inc, err := adapter.ParseMessages(ctx, sources[0], &convstore.SourceCursor{
		ExistingUpdatedMs: map[string]int64{
			"ms-1": 1763629500000,
			"ms-2": 1763628000000,
		},
	})
	if err != nil {
		t.Fatalf("ParseMessages(inc): %v", err)
	}
	if _, ok := inc.Messages["ms-1"]; ok {
		t.Fatalf("expected unchanged ms-1 skipped")
	}
	if len(inc.Messages["ms-2"]) != 1 {
		t.Fatalf("expected updated ms-2 parsed, got %+v", inc.Messages)
	}
}
