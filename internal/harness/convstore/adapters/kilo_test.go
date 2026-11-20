package adapters

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestKiloAdapterDiscoverMetadataAndMessages(t *testing.T) {
	home := t.TempDir()
	dataHome := filepath.Join(home, "xdg-data")
	dbPath := filepath.Join(dataHome, "kilo", "kilo.db")

	sqliteAdapterTestExecSQL(t, dbPath, `
		CREATE TABLE session (
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
		INSERT INTO session VALUES
			('kl-1', '', 'Kilo Dashboard Polish', '/work/kilo', 1763629000000, 1763629500000, 0),
			('kl-child', 'kl-1', 'Child Task', '/work/kilo', 1763629100000, 1763629200000, 0);
		INSERT INTO message VALUES
			('km1', 'kl-1', 1763629001000, '{"role":"user"}'),
			('km2', 'kl-1', 1763629002000, '{"role":"assistant"}');
		INSERT INTO part VALUES
			('kp1', 'km1', 'kl-1', 1763629001000, '{"type":"text","text":"Polish kilo sidebar colors"}'),
			('kp2', 'km2', 'kl-1', 1763629002000, '{"type":"text","text":"Updated theme palette."}');
	`)

	env := convstore.NewScanEnv(harnessenv.Paths{Home: home, DataHome: dataHome})
	adapter := NewKiloAdapter()
	if adapter.Agent() != "kilo" {
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
	if len(meta.Conversations) != 1 || meta.Conversations[0].ID != "kl-1" || meta.Conversations[0].Title != "Kilo Dashboard Polish" {
		t.Fatalf("unexpected metadata: %+v", meta.Conversations)
	}

	msgs, err := adapter.ParseMessages(ctx, sources[0], nil)
	if err != nil {
		t.Fatalf("ParseMessages: %v", err)
	}
	if len(msgs.Messages["kl-1"]) != 2 || msgs.Messages["kl-1"][0].Text != "Polish kilo sidebar colors" {
		t.Fatalf("unexpected messages: %+v", msgs.Messages)
	}

	// Unchanged skip.
	skipped, err := adapter.ParseMessages(ctx, sources[0], &convstore.SourceCursor{
		ExistingUpdatedMs: map[string]int64{"kl-1": 1763629500000},
	})
	if err != nil {
		t.Fatalf("ParseMessages(skip): %v", err)
	}
	if len(skipped.Messages) != 0 {
		t.Fatalf("expected unchanged session to be skipped, got %+v", skipped.Messages)
	}
}
