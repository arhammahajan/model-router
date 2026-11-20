package adapters

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestAntigravityAdapterProfilesSubagentsAndChunkedFTS(t *testing.T) {
	home := t.TempDir()

	// Default DB using c0/c13/c14 FTS content column naming.
	defDB := filepath.Join(home, ".gemini", "antigravity-cli", "conversation_summaries.db")
	longBody := strings.Repeat("searchable_token_alpha ", 900) + "\n\n" + strings.Repeat("searchable_token_omega ", 900)
	sqliteAdapterTestExecSQL(t, defDB, `
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT,
			preview TEXT,
			workspace_uris TEXT,
			created_time TEXT,
			last_modified_time TEXT,
			step_count INTEGER,
			parent_conversation_id TEXT,
			is_subagent INTEGER
		);
		CREATE TABLE conversations_fts_content (
			id INTEGER PRIMARY KEY,
			c0 TEXT,
			c13 TEXT,
			c14 TEXT
		);
		INSERT INTO conversation_summaries VALUES
			('ag-main', 'Build Convstore', 'Build unified store', '["file:///work/model-router"]', '2026-11-20T09:00:00Z', '2026-11-20T10:00:00Z', 12, '', 0),
			('ag-sub', 'Nested Helper', 'subagent', '["file:///work/model-router"]', '2026-11-20T09:05:00Z', '2026-11-20T09:10:00Z', 3, 'ag-main', 1);
		INSERT INTO conversations_fts_content (c0, c13, c14) VALUES
			('ag-main', '`+longBody+`', 'Implemented SQLite adapters cleanly.'),
			('ag-sub', 'subagent secret user text', 'subagent agent text');
	`)

	// Profile shadow DB using named conversation_id/user_body/agent_body FTS columns.
	profDB := filepath.Join(home, ".model-router", "profiles", "antigravity", "pratham", "home", ".gemini", "antigravity-cli", "conversation_summaries.db")
	sqliteAdapterTestExecSQL(t, profDB, `
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT,
			preview TEXT,
			workspace_uris TEXT,
			created_time INTEGER,
			last_modified_time INTEGER,
			step_count INTEGER
		);
		CREATE TABLE conversations_fts_content (
			conversation_id TEXT,
			user_body TEXT,
			agent_body TEXT
		);
		INSERT INTO conversation_summaries VALUES
			('ag-prof-1', 'Profile Conversation', 'Preview text', '["file:///work/profile-repo"]', 1763629000000, 1763629500000, 5);
		INSERT INTO conversations_fts_content VALUES
			('ag-prof-1', 'Profile user prompt', 'Profile agent response');
	`)

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewAntigravityAdapter()
	if adapter.Agent() != "antigravity" {
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

	var defSrc, profSrc convstore.DiscoveredSource
	for _, s := range sources {
		if s.Profile == "" {
			defSrc = s
		} else {
			profSrc = s
		}
	}
	if profSrc.Profile != "pratham" {
		t.Fatalf("expected profile 'pratham', got %q", profSrc.Profile)
	}

	// Default DB ParseMetadata: skips subagent, strips file://, keeps Profile="".
	defMeta, err := adapter.ParseMetadata(ctx, defSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(default): %v", err)
	}
	if len(defMeta.Conversations) != 1 {
		t.Fatalf("got %d convs, want 1 (subagent skipped): %+v", len(defMeta.Conversations), defMeta.Conversations)
	}
	c := defMeta.Conversations[0]
	if c.ID != "ag-main" || c.CWD != "/work/model-router" || c.Title != "Build Convstore" || c.MessageCount != 12 || c.Profile != "" {
		t.Fatalf("unexpected default conv: %+v", c)
	}

	// Default DB ParseMessages: splits >16KB user_body across multiple user chunks without losing tail token.
	defMsgs, err := adapter.ParseMessages(ctx, defSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages(default): %v", err)
	}
	if _, ok := defMsgs.Messages["ag-sub"]; ok {
		t.Fatalf("expected subagent messages to be skipped")
	}
	mainMsgs := defMsgs.Messages["ag-main"]
	if len(mainMsgs) < 3 {
		t.Fatalf("expected >=3 messages (chunked user + assistant), got %d", len(mainMsgs))
	}
	var sawAlpha, sawOmega, sawAssistant bool
	for _, m := range mainMsgs {
		if len(m.Text) > convstore.MaxMessageTextBytes {
			t.Fatalf("chunk exceeded MaxMessageTextBytes: %d", len(m.Text))
		}
		if m.Role == "user" && strings.Contains(m.Text, "searchable_token_alpha") {
			sawAlpha = true
		}
		if m.Role == "user" && strings.Contains(m.Text, "searchable_token_omega") {
			sawOmega = true
		}
		if m.Role == "assistant" && m.Text == "Implemented SQLite adapters cleanly." {
			sawAssistant = true
		}
	}
	if !sawAlpha || !sawOmega || !sawAssistant {
		t.Fatalf("missing expected chunk content: alpha=%v omega=%v assistant=%v", sawAlpha, sawOmega, sawAssistant)
	}

	// Unchanged skip.
	skipped, err := adapter.ParseMessages(ctx, defSrc, &convstore.SourceCursor{
		ExistingUpdatedMs: map[string]int64{"ag-main": c.UpdatedAtMs},
	})
	if err != nil {
		t.Fatalf("ParseMessages(skip): %v", err)
	}
	if len(skipped.Messages) != 0 {
		t.Fatalf("expected unchanged ag-main skipped, got %+v", skipped.Messages)
	}

	// Profile DB metadata + messages with named FTS columns.
	profMeta, err := adapter.ParseMetadata(ctx, profSrc, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(profile): %v", err)
	}
	if len(profMeta.Conversations) != 1 || profMeta.Conversations[0].Profile != "pratham" || profMeta.Conversations[0].CWD != "/work/profile-repo" {
		t.Fatalf("unexpected profile metadata: %+v", profMeta.Conversations)
	}
	profMsgs, err := adapter.ParseMessages(ctx, profSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages(profile): %v", err)
	}
	if len(profMsgs.Messages["ag-prof-1"]) != 2 || profMsgs.Messages["ag-prof-1"][0].Text != "Profile user prompt" || profMsgs.Messages["ag-prof-1"][1].Text != "Profile agent response" {
		t.Fatalf("unexpected profile messages: %+v", profMsgs.Messages)
	}
}
