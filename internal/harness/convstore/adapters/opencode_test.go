package adapters

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
	_ "modernc.org/sqlite"
)

func sqliteAdapterTestExecSQL(t *testing.T, dbPath, ddl string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open test db %s: %v", dbPath, err)
	}
	defer db.Close()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("exec ddl on %s: %v", dbPath, err)
	}
}

func TestOpencodeAdapterSessionV2AndMessagePartSchemas(t *testing.T) {
	home := t.TempDir()
	dataHome := filepath.Join(home, "xdg-data")
	dbPath := filepath.Join(dataHome, "opencode", "opencode.db")

	sqliteAdapterTestExecSQL(t, dbPath, `
		CREATE TABLE session_v2 (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			title TEXT,
			directory TEXT,
			time_created INTEGER,
			time_updated INTEGER,
			time_archived INTEGER
		);
		CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT,
			time_created INTEGER,
			data TEXT
		);
		CREATE TABLE part (
			id TEXT PRIMARY KEY,
			message_id TEXT,
			session_id TEXT,
			time_created INTEGER,
			data TEXT
		);
		INSERT INTO session_v2 VALUES
			('oc-1', NULL, 'Refactor Router', '/work/router', 1763629000000, 1763629500000, 0),
			('oc-2', '', 'Archived Session', '/work/old', 1763620000000, 1763620500000, 1763621000000),
			('oc-child', 'oc-1', 'Nested Subagent', '/work/router', 1763629100000, 1763629200000, 0);
		INSERT INTO message VALUES
			('m1', 'oc-1', 1763629001000, '{"role":"user"}'),
			('m2', 'oc-1', 1763629002000, '{"role":"assistant"}'),
			('m3', 'oc-2', 1763620001000, '{"role":"user"}'),
			('m4', 'oc-child', 1763629101000, '{"role":"user"}');
		INSERT INTO part VALUES
			('p1', 'm1', 'oc-1', 1763629001000, '{"type":"text","text":"Split proxy handler"}'),
			('p2', 'm2', 'oc-1', 1763629002000, '{"type":"reasoning","text":"hidden thought"}'),
			('p3', 'm2', 'oc-1', 1763629002100, '{"type":"tool","text":"tool dump"}'),
			('p4', 'm2', 'oc-1', 1763629002200, '{"type":"text","text":"Extracted proxy.go cleanly."}'),
			('p5', 'm3', 'oc-2', 1763620001000, '{"type":"text","text":"Old archived prompt"}'),
			('p6', 'm4', 'oc-child', 1763629101000, '{"type":"text","text":"Subagent prompt"}');
	`)

	// Also add an isolated profile shadow DB with session_message table.
	shadowDB := filepath.Join(home, ".model-router", "profiles", "opencode", "alt", "home", "opencode", "opencode.db")
	sqliteAdapterTestExecSQL(t, shadowDB, `
		CREATE TABLE session (
			id TEXT PRIMARY KEY,
			title TEXT,
			directory TEXT,
			time_created INTEGER,
			time_updated INTEGER
		);
		CREATE TABLE session_message (
			session_id TEXT,
			role TEXT,
			data TEXT,
			time_created INTEGER
		);
		INSERT INTO session VALUES ('alt-1', 'Alt Profile Work', '/work/alt', 1763630000000, 1763630100000);
		INSERT INTO session_message VALUES ('alt-1', 'user', '{"text":"Hello from alt profile"}', 1763630001000);
	`)

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home, DataHome: dataHome})
	adapter := NewOpencodeAdapter()
	ctx := context.Background()

	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("Discover len = %d, want 2: %+v", len(sources), sources)
	}

	var defaultSrc, altSrc convstore.DiscoveredSource
	for _, s := range sources {
		if s.Profile == "" {
			defaultSrc = s
		} else {
			altSrc = s
		}
	}
	if altSrc.Profile != "alt" {
		t.Fatalf("expected alt profile, got %+v", altSrc)
	}

	meta, err := adapter.ParseMetadata(ctx, defaultSrc, nil)
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
	if byID["oc-1"].Title != "Refactor Router" || byID["oc-1"].CWD != "/work/router" || byID["oc-1"].Archived {
		t.Fatalf("unexpected oc-1: %+v", byID["oc-1"])
	}
	if !byID["oc-2"].Archived {
		t.Fatalf("expected oc-2 archived=true")
	}

	// ParseMessages initial pass.
	msgRes, err := adapter.ParseMessages(ctx, defaultSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if _, ok := msgRes.Messages["oc-child"]; ok {
		t.Fatalf("expected oc-child to be excluded from messages")
	}
	if len(msgRes.Messages["oc-1"]) != 2 || msgRes.Messages["oc-1"][0].Text != "Split proxy handler" || msgRes.Messages["oc-1"][1].Text != "Extracted proxy.go cleanly." {
		t.Fatalf("unexpected oc-1 messages: %+v", msgRes.Messages["oc-1"])
	}

	// Incremental skip: oc-1 unchanged, oc-2 updated.
	incRes, err := adapter.ParseMessages(ctx, defaultSrc, &convstore.SourceCursor{
		ExistingUpdatedMs: map[string]int64{
			"oc-1": 1763629500000,
			"oc-2": 1763620000000,
		},
	})
	if err != nil {
		t.Fatalf("ParseMessages(inc): %v", err)
	}
	if _, ok := incRes.Messages["oc-1"]; ok {
		t.Fatalf("expected unchanged oc-1 to be skipped")
	}
	if len(incRes.Messages["oc-2"]) != 1 || incRes.Messages["oc-2"][0].Text != "Old archived prompt" {
		t.Fatalf("expected updated oc-2 messages, got %+v", incRes.Messages)
	}

	// Verify alt profile session_message schema.
	altMsgs, err := adapter.ParseMessages(ctx, altSrc, nil)
	if err != nil {
		t.Fatalf("ParseMessages(alt): %v", err)
	}
	if len(altMsgs.Messages["alt-1"]) != 1 || altMsgs.Messages["alt-1"][0].Text != "Hello from alt profile" {
		t.Fatalf("unexpected alt messages: %+v", altMsgs.Messages)
	}
}
