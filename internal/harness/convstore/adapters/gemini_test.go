package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func TestGeminiAdapterDiscoverMetadataMessages(t *testing.T) {
	home := t.TempDir()
	env := convstore.NewScanEnv(harnessenv.Paths{Home: home})
	adapter := NewGeminiAdapter()
	if adapter.Agent() != "gemini" {
		t.Fatalf("Agent() = %q, want gemini", adapter.Agent())
	}

	// Project 1: CWD resolved via .project_root, JSONL session file.
	cwd1 := "/work/gemini-one"
	sum1 := sha256.Sum256([]byte(cwd1))
	hash1 := hex.EncodeToString(sum1[:])
	proj1Dir := filepath.Join(home, ".gemini", "tmp", hash1)
	if err := os.MkdirAll(filepath.Join(proj1Dir, "chats"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj1Dir, ".project_root"), []byte(cwd1+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess1 := filepath.Join(proj1Dir, "chats", "session-2026-11-20-gm1.jsonl")
	lines1 := strings.Join([]string{
		`{"sessionId":"gm-1","projectHash":"` + hash1 + `","startTime":"2026-11-20T10:00:00Z"}`,
		`{"type":"user","content":"Design the indexer pipeline","timestamp":"2026-11-20T10:01:00Z"}`,
		`{"type":"model","parts":[{"text":"Here is a two-stage pipeline."}],"timestamp":"2026-11-20T10:02:00Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sess1, []byte(lines1), 0o644); err != nil {
		t.Fatal(err)
	}

	// Project 2: CWD resolved via ~/.gemini/projects.json, single JSON session file.
	cwd2 := "/work/gemini-two"
	sum2 := sha256.Sum256([]byte(cwd2))
	hash2 := hex.EncodeToString(sum2[:])
	if err := os.WriteFile(filepath.Join(home, ".gemini", "projects.json"),
		[]byte(`{"projects":{"`+cwd2+`":"my-proj-two"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	proj2Chats := filepath.Join(home, ".gemini", "tmp", "my-proj-two", "chats")
	if err := os.MkdirAll(proj2Chats, 0o755); err != nil {
		t.Fatal(err)
	}
	sess2 := filepath.Join(proj2Chats, "session-2026-11-20-gm2.json")
	json2 := `{"sessionId":"gm-2","projectHash":"` + hash2 + `","startTime":"2026-11-20T11:00:00Z","lastUpdated":"2026-11-20T11:10:00Z","title":"JSON Session Title","messages":[{"role":"user","content":"Hello JSON"},{"role":"assistant","content":"Hi from JSON"}]}`
	if err := os.WriteFile(sess2, []byte(json2), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sources, err := adapter.Discover(ctx, env)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("Discover returned %d sources, want 2: %+v", len(sources), sources)
	}

	var src1, src2 convstore.DiscoveredSource
	for _, s := range sources {
		if s.Path == sess1 {
			src1 = s
		} else {
			src2 = s
		}
	}

	meta1, err := adapter.ParseMetadata(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(1) error: %v", err)
	}
	c1 := meta1.Conversations[0]
	if c1.ID != "gm-1" || c1.CWD != cwd1 || c1.Title != "Design the indexer pipeline" || c1.ResumeTarget != sess1 {
		t.Fatalf("c1 = %+v", c1)
	}

	meta2, err := adapter.ParseMetadata(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(2) error: %v", err)
	}
	c2 := meta2.Conversations[0]
	if c2.ID != "gm-2" || c2.CWD != cwd2 || c2.Title != "JSON Session Title" || c2.ResumeTarget != sess2 {
		t.Fatalf("c2 = %+v", c2)
	}

	// Messages from JSONL + incremental append.
	msgRes1, err := adapter.ParseMessages(ctx, src1, nil)
	if err != nil {
		t.Fatalf("ParseMessages(1) error: %v", err)
	}
	msgs1 := msgRes1.Messages["gm-1"]
	if len(msgs1) != 2 || msgs1[0].Role != "user" || msgs1[1].Role != "assistant" || msgs1[1].Text != "Here is a two-stage pipeline." {
		t.Fatalf("msgs1 = %+v", msgs1)
	}

	f, err := os.OpenFile(sess1, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"role":"user","content":"Add FTS5 snippets","timestamp":"2026-11-20T10:05:00Z"}` + "\n")
	_ = f.Close()

	incRes, err := adapter.ParseMessages(ctx, src1, &convstore.SourceCursor{
		MsgByteOffset: msgRes1.ByteOffset,
		MsgPrefixHash: msgRes1.PrefixHash,
	})
	if err != nil {
		t.Fatalf("incremental ParseMessages error: %v", err)
	}
	if !incRes.Append || len(incRes.Messages["gm-1"]) != 1 || incRes.Messages["gm-1"][0].Text != "Add FTS5 snippets" {
		t.Fatalf("incRes = %+v", incRes)
	}

	// Messages from JSON.
	msgRes2, err := adapter.ParseMessages(ctx, src2, nil)
	if err != nil {
		t.Fatalf("ParseMessages(2) error: %v", err)
	}
	if len(msgRes2.Messages["gm-2"]) != 2 {
		t.Fatalf("msgs2 = %+v", msgRes2.Messages)
	}

	// Session 3: real Gemini CLI shape with nested <session_context> + $set.summary.
	chatsDir := filepath.Dir(sess1)
	sess3 := filepath.Join(chatsDir, "session-2026-11-20-gm3.jsonl")
	s3Lines := strings.Join([]string{
		`{"sessionId":"gm-3","projectHash":"h3","startTime":"2026-11-20T14:00:00Z"}`,
		`{"type":"user","content":[{"text":"<session_context>setup <loaded_context><hook_context>hook</hook_context></loaded_context></session_context>"}]}`,
		`{"type":"user","content":[{"text":"list out all the files in this directory"}]}`,
		`{"type":"gemini","content":"Listing files now."}`,
		`{"$set":{"lastUpdated":"2026-11-20T14:02:00Z","summary":"List files in the current directory"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sess3, []byte(s3Lines), 0o644); err != nil {
		t.Fatal(err)
	}
	meta3, err := adapter.ParseMetadata(ctx, convstore.DiscoveredSource{
		SourceKey: convstore.MakeSourceKey("gemini", "", sess3),
		Agent:     "gemini",
		Path:      sess3,
		Kind:      convstore.SourceFile,
	}, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(3) error: %v", err)
	}
	if len(meta3.Conversations) != 1 || meta3.Conversations[0].Title != "List files in the current directory" ||
		meta3.Conversations[0].Preview != "list out all the files in this directory" ||
		meta3.Conversations[0].MessageCount != 2 {
		t.Fatalf("meta3 = %+v", meta3.Conversations)
	}

	// Session 4: empty session containing only <session_context> must be skipped.
	sess4 := filepath.Join(chatsDir, "session-2026-11-20-gm4.jsonl")
	s4Lines := strings.Join([]string{
		`{"sessionId":"gm-4","projectHash":"h4","startTime":"2026-11-20T15:00:00Z"}`,
		`{"type":"user","content":[{"text":"<session_context>setup <loaded_context>ctx</loaded_context></session_context>"}]}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sess4, []byte(s4Lines), 0o644); err != nil {
		t.Fatal(err)
	}
	meta4, err := adapter.ParseMetadata(ctx, convstore.DiscoveredSource{
		SourceKey: convstore.MakeSourceKey("gemini", "", sess4),
		Agent:     "gemini",
		Path:      sess4,
		Kind:      convstore.SourceFile,
	}, nil)
	if err != nil {
		t.Fatalf("ParseMetadata(4) error: %v", err)
	}
	if len(meta4.Conversations) != 0 {
		t.Fatalf("expected context-only empty Gemini session to be skipped, got %+v", meta4.Conversations)
	}
}
