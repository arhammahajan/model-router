package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

func captureMainStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = oldOut
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return code, buf.String()
}

func TestSpaceConversationsAndCLIContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	oldPaths := harnessDefaultPaths
	harnessDefaultPaths = func() harnessenv.Paths {
		return harnessenv.Paths{
			Home:       home,
			ConfigHome: filepath.Join(home, ".config"),
			DataHome:   filepath.Join(home, ".local", "share"),
			StateHome:  filepath.Join(home, ".local", "state"),
			CacheHome:  filepath.Join(home, ".cache"),
		}
	}
	defer func() { harnessDefaultPaths = oldPaths }()

	// Seed a realistic Claude JSONL conversation.
	projDir := filepath.Join(home, ".claude", "projects", "-home-user-dev-router")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"user","sessionId":"claude-s1","cwd":"/home/user/dev/router","gitBranch":"feat/retry","timestamp":"2026-11-20T10:00:00Z","message":{"role":"user","content":"Please fix the 429 rate limit backoff jitter"}}` + "\n" +
		`{"type":"assistant","sessionId":"claude-s1","cwd":"/home/user/dev/router","timestamp":"2026-11-20T10:00:05Z","message":{"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"I updated exponential backoff jitter to cap at 30 seconds."}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "claude-s1.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Sync via CLI (`harness conversations sync --json`).
	code, out := captureMainStdout(t, func() int {
		return run([]string{"conversations", "sync", "--json"})
	})
	if code != 0 {
		t.Fatalf("conversations sync code = %d, out = %s", code, out)
	}

	// 2. Status via CLI (`harness conversations status --json`).
	code, out = captureMainStdout(t, func() int {
		return run([]string{"conversations", "status", "--json"})
	})
	if code != 0 {
		t.Fatalf("conversations status code = %d, out = %s", code, out)
	}
	var status convstore.IndexingStatus
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("decode status: %v (%s)", err, out)
	}
	if status.Conversations != 1 || status.IndexedMessages < 2 {
		t.Fatalf("unexpected status: %+v", status)
	}

	// 3. Record overlay via `harness spaces __space-conversations --record`.
	recPayload, _ := json.Marshal(convstore.OverlayRecord{
		Agent:        "claude",
		SessionID:    "claude-s1",
		Profile:      "work",
		Space:        "router-space",
		LastOpenedMs: 1700000999000,
	})
	code, out = captureMainStdout(t, func() int {
		return run([]string{"spaces", "__space-conversations", "--record", string(recPayload)})
	})
	if code != 0 {
		t.Fatalf("__space-conversations --record code = %d, out = %s", code, out)
	}

	// 4. Cached list via `harness spaces __space-conversations --cached-only`.
	code, out = captureMainStdout(t, func() int {
		return run([]string{"spaces", "__space-conversations", "--cached-only"})
	})
	if code != 0 {
		t.Fatalf("__space-conversations --cached-only code = %d, out = %s", code, out)
	}
	var listResp convstore.ListResponse
	if err := json.Unmarshal([]byte(out), &listResp); err != nil {
		t.Fatalf("decode listResp: %v (%s)", err, out)
	}
	if !listResp.OK || len(listResp.Conversations) != 1 {
		t.Fatalf("unexpected listResp: %+v", listResp)
	}
	c := listResp.Conversations[0]
	if c.ID != "claude-s1" || c.Profile != "work" || c.Space != "router-space" || c.App != "claude-code" {
		t.Fatalf("unexpected conversation overlay merge: %+v", c)
	}

	// 5. Full-message search via `harness spaces __space-conversations --search`.
	code, out = captureMainStdout(t, func() int {
		return run([]string{"spaces", "__space-conversations", "--search", "exponential backoff"})
	})
	if code != 0 {
		t.Fatalf("__space-conversations --search code = %d, out = %s", code, out)
	}
	var searchResp convstore.SearchResponse
	if err := json.Unmarshal([]byte(out), &searchResp); err != nil {
		t.Fatalf("decode searchResp: %v (%s)", err, out)
	}
	if !searchResp.OK || len(searchResp.Hits) != 1 {
		t.Fatalf("unexpected searchResp: %+v", searchResp)
	}
	if searchResp.Hits[0].Conversation.ID != "claude-s1" || searchResp.Hits[0].MatchKind != "message" || searchResp.Hits[0].Snippet == "" {
		t.Fatalf("unexpected search hit: %+v", searchResp.Hits[0])
	}
}
