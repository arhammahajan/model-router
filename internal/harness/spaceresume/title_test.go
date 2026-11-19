package spaceresume

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverTitleClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	cwd := "/work/claude-proj"
	proj := filepath.Join(home, ".claude", "projects", EncodedPath(cwd))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. First user prompt + AI title + custom title priority.
	s1 := filepath.Join(proj, "sess-1.jsonl")
	lines := strings.Join([]string{
		`{"type":"user","cwd":` + jsonString(cwd) + `,"sessionId":"sess-1","message":{"content":"First user prompt for claude"}}`,
		`{"type":"ai-title","aiTitle":"AI Generated Claude Title"}`,
		`{"type":"custom-title","customTitle":"Custom Renamed Title"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(s1, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverTitle("claude", cwd, "sess-1"); got != "Custom Renamed Title" {
		t.Errorf("custom title = %q, want Custom Renamed Title", got)
	}

	// 2. Without custom-title, ai-title wins over first user prompt.
	s2 := filepath.Join(proj, "sess-2.jsonl")
	lines2 := strings.Join([]string{
		`{"type":"user","cwd":` + jsonString(cwd) + `,"sessionId":"sess-2","message":{"content":"Initial user prompt"}}`,
		`{"type":"ai-title","aiTitle":"AI Title Wins"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(s2, []byte(lines2), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverTitle("claude-code", cwd, "sess-2"); got != "AI Title Wins" {
		t.Errorf("ai title = %q, want AI Title Wins", got)
	}

	// 3. Only user message ( skipping IDE system tags ).
	s3 := filepath.Join(proj, "sess-3.jsonl")
	lines3 := strings.Join([]string{
		`{"type":"user","cwd":` + jsonString(cwd) + `,"sessionId":"sess-3","message":{"content":[{"type":"text","text":"<ide_opened_file>foo.go</ide_opened_file>"},{"type":"text","text":"Fix the conversation title fallback"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(s3, []byte(lines3), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverTitle("claude", cwd, "sess-3"); got != "Fix the conversation title fallback" {
		t.Errorf("user prompt title = %q", got)
	}
}

func TestDiscoverTitleCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	sess := filepath.Join(home, ".codex", "sessions", "2026", "11", "20")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := "/work/codex-proj"
	rollout := filepath.Join(sess, "rollout-2026-cx-99.jsonl")
	content := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"cx-99","cwd":` + jsonString(cwd) + `}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"# AGENTS.md instructions for /work/codex-proj"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Refactor the model router title resolution"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(rollout, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverTitle("codex", cwd, "cx-99"); got != "Refactor the model router title resolution" {
		t.Errorf("codex title by id = %q", got)
	}
	if got := DiscoverTitle("codex", cwd, ""); got != "Refactor the model router title resolution" {
		t.Errorf("codex title by cwd = %q", got)
	}
}

func TestDiscoverTitleSQLiteHarnesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	data := filepath.Join(home, "data")
	t.Setenv("XDG_DATA_HOME", data)
	cwd := filepath.Join(t.TempDir(), "sql-work")

	mk := func(path, table, create string, rows [][]any) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestDB(t, path, table, create, rows)
	}

	// OpenCode
	opdb := filepath.Join(data, "opencode", "opencode.db")
	mk(opdb, "session",
		"CREATE TABLE session (id TEXT, directory TEXT, title TEXT, time_updated INTEGER)",
		[][]any{{"oc-1", cwd, "OpenCode Auth Refactor", int64(10)}})
	if got := DiscoverTitle("opencode", cwd, ""); got != "OpenCode Auth Refactor" {
		t.Errorf("opencode = %q", got)
	}

	// Kilo
	kdb := filepath.Join(data, "kilo", "kilo.db")
	mk(kdb, "session",
		"CREATE TABLE session (id TEXT, directory TEXT, title TEXT, time_updated INTEGER)",
		[][]any{{"kl-1", cwd, "Kilo TUI Polish", int64(20)}})
	if got := DiscoverTitle("kilo", cwd, "kl-1"); got != "Kilo TUI Polish" {
		t.Errorf("kilo = %q", got)
	}

	// Muse
	mdb := filepath.Join(data, "muse", "session-index.db")
	mk(mdb, "sessions",
		"CREATE TABLE sessions (session_id TEXT, workspace_root TEXT, session_name TEXT, effective_long_title TEXT, title TEXT, msp_title TEXT, first_user_prompt TEXT, updated_at_us INTEGER)",
		[][]any{{"m-1", cwd, "", "Muse Deep Research Title", "Short", "", "raw prompt", int64(30)}})
	if got := DiscoverTitle("muse", cwd, ""); got != "Muse Deep Research Title" {
		t.Errorf("muse = %q", got)
	}

	// Mcode
	mcdb := filepath.Join(home, ".minimax", "v2", "sqlite", "runtime-state.sqlite")
	mk(mcdb, "local_runtime_sessions",
		"CREATE TABLE local_runtime_sessions (session_id TEXT, workspace_dir TEXT, session_kind TEXT, title TEXT, purpose TEXT, record_json TEXT, updated_at_ms INTEGER)",
		[][]any{{"mc-1", cwd, "conversation", "Mcode Runtime Title", "purpose", "{}", int64(40)}})
	if got := DiscoverTitle("mcode", cwd, ""); got != "Mcode Runtime Title" {
		t.Errorf("mcode = %q", got)
	}

	// Antigravity (including profile shadow home discovery)
	profDB := filepath.Join(home, ".model-router", "profiles", "antigravity", "saucerf166", "home", ".gemini", "antigravity-cli", "conversation_summaries.db")
	mk(profDB, "conversation_summaries",
		"CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, preview TEXT, workspace_uris TEXT, last_modified_time INTEGER)",
		[][]any{{"ag-1", "Antigravity Profile Session", "preview", `["file://` + cwd + `"]`, int64(50)}})
	if got := DiscoverTitle("antigravity", cwd, ""); got != "Antigravity Profile Session" {
		t.Errorf("antigravity = %q", got)
	}

	// Crush
	cdb := filepath.Join(cwd, ".crush", "crush.db")
	mk(cdb, "sessions",
		"CREATE TABLE sessions (id TEXT, title TEXT, updated_at INTEGER)",
		[][]any{{"cr-1", "Crush Local DB Session", int64(60)}})
	if got := DiscoverTitle("crush", cwd, ""); got != "Crush Local DB Session" {
		t.Errorf("crush = %q", got)
	}
}

func TestDiscoverTitleFileHarnessesAndBatchCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	t.Setenv("GEMINI_HOME", filepath.Join(home, ".gemini"))
	cwd := "/work/files-proj"

	// Grok
	gdir := filepath.Join(home, ".grok", "sessions", "p1", "s1")
	os.MkdirAll(gdir, 0o755)
	os.WriteFile(filepath.Join(gdir, "summary.json"),
		[]byte(`{"session_summary":"Grok Session Summary","info":{"cwd":`+jsonString(cwd)+`,"id":"g-1"},"last_active_at":"2026-11-20T01:00:00Z"}`), 0o644)
	if got := DiscoverTitle("grok", cwd, "g-1"); got != "Grok Session Summary" {
		t.Errorf("grok = %q", got)
	}

	// Copilot
	ws := filepath.Join(home, ".copilot", "session-state", "cp-1", "workspace.yaml")
	os.MkdirAll(filepath.Dir(ws), 0o755)
	os.WriteFile(ws, []byte("id: cp-1\ncwd: "+cwd+"\nsummary: Copilot Workspace Summary\nupdated_at: 2026-11-20\n"), 0o644)
	if got := DiscoverTitle("copilot", cwd, "cp-1"); got != "Copilot Workspace Summary" {
		t.Errorf("copilot = %q", got)
	}

	// Gemini
	sum := sha256.Sum256([]byte(cwd))
	hash := hex.EncodeToString(sum[:])
	gmdir := filepath.Join(home, ".gemini", "tmp", "h", "chats")
	os.MkdirAll(gmdir, 0o755)
	gmLines := strings.Join([]string{
		`{"projectHash":"` + hash + `","sessionId":"gm-1"}`,
		`{"role":"user","content":"Gemini first prompt title"}`,
	}, "\n") + "\n"
	os.WriteFile(filepath.Join(gmdir, "session-1.jsonl"), []byte(gmLines), 0o644)
	if got := DiscoverTitle("gemini", cwd, "gm-1"); got != "Gemini first prompt title" {
		t.Errorf("gemini = %q", got)
	}

	// Batch SpaceHookTitles JSON CLI
	reqBytes, _ := json.Marshal([]map[string]string{
		{"key": "%1", "agent": "grok", "cwd": cwd, "session_id": "g-1"},
		{"key": "%2", "agent": "copilot", "cwd": cwd, "session_id": "cp-1"},
	})
	outPath, _ := captureOutput(t)
	if code := SpaceHookTitles([]string{string(reqBytes)}); code != 0 {
		t.Fatalf("SpaceHookTitles code = %d", code)
	}
	var resp struct {
		OK     bool              `json:"ok"`
		Titles map[string]string `json:"titles"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFile(t, outPath))), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if !resp.OK || resp.Titles["%1"] != "Grok Session Summary" || resp.Titles["%2"] != "Copilot Workspace Summary" {
		t.Errorf("batch titles = %+v", resp)
	}
}
