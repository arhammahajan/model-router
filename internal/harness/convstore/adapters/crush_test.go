package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestCrushAdapterDiscoverMetadataAndMessages(t *testing.T) {
	home := t.TempDir()
	dataHome := filepath.Join(home, "xdg-data")

	projCWD := filepath.Join(t.TempDir(), "my-crush-repo")
	projDataDir := filepath.Join(t.TempDir(), "crush-data-1")
	dbPath := filepath.Join(projDataDir, "crush.db")

	sqliteAdapterTestExecSQL(t, dbPath, `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT,
			message_count INTEGER,
			created_at INTEGER,
			updated_at INTEGER
		);
		CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			session_id TEXT,
			role TEXT,
			parts TEXT,
			created_at INTEGER
		);
		INSERT INTO sessions VALUES
			('cr-1', NULL, 'Crush SQLite Integration', 2, 1763629000, 1763629500),
			('cr-2', '', 'Second Crush Session', 1, 1763628000, 1763628500),
			('cr-child', 'cr-1', 'Subagent Session', 1, 1763629100, 1763629200);
		INSERT INTO messages VALUES
			('cm1', 'cr-1', 'user', '[{"type":"text","data":{"text":"Wire up crush adapter"}}]', 1763629001),
			('cm2', 'cr-1', 'assistant', '[{"type":"reasoning","data":{"text":"hidden"}},{"type":"text","data":{"text":"Implemented crush.go and tests."}}]', 1763629002),
			('cm3', 'cr-2', 'user', '[{"type":"text","text":"Plain text part format"}]', 1763628001),
			('cm4', 'cr-child', 'user', '[{"type":"text","data":{"text":"Child prompt"}}]', 1763629101);
	`)

	regPath := filepath.Join(dataHome, "crush", "projects.json")
	if err := os.MkdirAll(filepath.Dir(regPath), 0o755); err != nil {
		t.Fatal(err)
	}
	regPayload, _ := json.Marshal(map[string]any{
		"projects": []map[string]string{
			{"path": projCWD, "data_dir": projDataDir},
			{"path": "/nonexistent/proj", "data_dir": "/nonexistent/data"},
		},
	})
	if err := os.WriteFile(regPath, regPayload, 0o644); err != nil {
		t.Fatal(err)
	}

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home, DataHome: dataHome})
	adapter := NewCrushAdapter()
	if adapter.Agent() != "crush" {
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
	c1 := byID["cr-1"]
	if c1.Title != "Crush SQLite Integration" || c1.CWD != filepath.Clean(projCWD) || c1.MessageCount != 2 {
		t.Fatalf("unexpected cr-1 metadata: %+v", c1)
	}
	if c1.CreatedAtMs != 1763629000000 || c1.UpdatedAtMs != 1763629500000 {
		t.Fatalf("expected s->ms timestamps, got created=%d updated=%d", c1.CreatedAtMs, c1.UpdatedAtMs)
	}

	// ParseMessages initial pass.
	msgs, err := adapter.ParseMessages(ctx, sources[0], nil)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if _, ok := msgs.Messages["cr-child"]; ok {
		t.Fatalf("expected cr-child excluded from messages")
	}
	if len(msgs.Messages["cr-1"]) != 2 || msgs.Messages["cr-1"][0].Text != "Wire up crush adapter" || msgs.Messages["cr-1"][1].Text != "Implemented crush.go and tests." {
		t.Fatalf("unexpected cr-1 messages: %+v", msgs.Messages["cr-1"])
	}
	if len(msgs.Messages["cr-2"]) != 1 || msgs.Messages["cr-2"][0].Text != "Plain text part format" {
		t.Fatalf("unexpected cr-2 messages: %+v", msgs.Messages["cr-2"])
	}

	// Incremental skip: cr-1 unchanged, cr-2 stale.
	inc, err := adapter.ParseMessages(ctx, sources[0], &convstore.SourceCursor{
		ExistingUpdatedMs: map[string]int64{
			"cr-1": 1763629500000,
			"cr-2": 1763628000000,
		},
	})
	if err != nil {
		t.Fatalf("ParseMessages(inc): %v", err)
	}
	if _, ok := inc.Messages["cr-1"]; ok {
		t.Fatalf("expected unchanged cr-1 skipped")
	}
	if len(inc.Messages["cr-2"]) != 1 {
		t.Fatalf("expected updated cr-2 parsed, got %+v", inc.Messages)
	}
}
