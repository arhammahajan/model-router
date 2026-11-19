package spaceresume

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncodedPath(t *testing.T) {
	if got := EncodedPath("/home/u/my proj"); got != "-home-u-my-proj" {
		t.Errorf("got %q", got)
	}
}

func TestDiscoverClaudeDirectAndFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	cwd := "/work/proj"
	proj := filepath.Join(home, ".claude", "projects", EncodedPath(cwd))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(proj, "old.jsonl")
	newf := filepath.Join(proj, "new.jsonl")
	os.WriteFile(old, []byte("{\"cwd\":"+jsonString(cwd)+",\"sessionId\":\"old-id\"}\n"), 0o644)
	os.WriteFile(newf, []byte("{\"cwd\":"+jsonString(cwd)+",\"sessionId\":\"new-id\"}\n"), 0o644)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(old, past, past)
	if got := DiscoverClaude(cwd); got != "new-id" {
		t.Errorf("direct = %q", got)
	}
	// Fallback: different encoding directory.
	other := filepath.Join(home, ".claude", "projects", "weird-enc")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "s.jsonl"), []byte("{\"cwd\":"+jsonString(cwd)+",\"session_id\":\"fb-id\"}\n"), 0o644)
	os.RemoveAll(proj)
	if got := DiscoverClaude(cwd); got != "fb-id" {
		t.Errorf("fallback = %q", got)
	}
	// No match.
	if got := DiscoverClaude("/nope"); got != "" {
		t.Errorf("no match = %q", got)
	}
}

func TestDiscoverCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	sess := filepath.Join(home, ".codex", "sessions", "2024")
	os.MkdirAll(sess, 0o755)
	cwd := "/work/cx"
	os.WriteFile(filepath.Join(sess, "rollout-a.jsonl"),
		[]byte("{\"type\":\"session_meta\",\"payload\":{\"cwd\":"+jsonString(cwd)+",\"session_id\":\"cx-1\"}}\n"), 0o644)
	os.WriteFile(filepath.Join(sess, "rollout-b.jsonl"),
		[]byte("{\"type\":\"other\"}\n"), 0o644)
	if got := DiscoverCodex(cwd); got != "cx-1" {
		t.Errorf("got %q", got)
	}
	if got := DiscoverCodex("/elsewhere"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDiscoverPiOmp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/work/p"
	piDir := filepath.Join(home, ".pi", "agent", "sessions")
	os.MkdirAll(piDir, 0o755)
	os.WriteFile(filepath.Join(piDir, "s.jsonl"),
		[]byte("{\"type\":\"session\",\"cwd\":"+jsonString(cwd)+",\"id\":\"pi-1\"}\n"), 0o644)
	if got := DiscoverPi(cwd); got != "pi-1" {
		t.Errorf("pi = %q", got)
	}
	ompDir := filepath.Join(home, ".omp", "agent", "sessions")
	os.MkdirAll(ompDir, 0o755)
	os.WriteFile(filepath.Join(ompDir, "s.jsonl"),
		[]byte("{\"type\":\"session\",\"cwd\":"+jsonString(cwd)+",\"id\":\"omp-1\"}\n"), 0o644)
	if got := DiscoverOmp(cwd); got != "omp-1" {
		t.Errorf("omp = %q", got)
	}
}

func TestDiscoverQwenRuntimeExcluded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/work/q"
	dir := filepath.Join(home, ".qwen", "projects", EncodedPath(cwd), "chats")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "a.jsonl.runtime.json"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.jsonl"),
		[]byte("{\"cwd\":"+jsonString(cwd)+",\"sessionId\":\"q-1\"}\n"), 0o644)
	if got := DiscoverQwen(cwd); got != "q-1" {
		t.Errorf("got %q", got)
	}
}

func TestDiscoverCopilot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/work/cp"
	ws := filepath.Join(home, ".copilot", "session-state", "abc", "workspace.yaml")
	os.MkdirAll(filepath.Dir(ws), 0o755)
	os.WriteFile(ws, []byte("id: cp-1\ncwd: "+cwd+"\nupdated_at: 2024-01-02\n"), 0o644)
	if got := DiscoverCopilot(cwd); got != "cp-1" {
		t.Errorf("got %q", got)
	}
	if got := DiscoverCopilot("/other"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDiscoverVibeClineGrok(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/work/v"
	vdir := filepath.Join(home, ".vibe", "logs", "session", "unified", "s1")
	os.MkdirAll(vdir, 0o755)
	os.WriteFile(filepath.Join(vdir, "meta.json"),
		[]byte(`{"session_id":"v-1","environment":{"working_directory":`+jsonString(cwd)+`},"start_time":"t1"}`), 0o644)
	if got := DiscoverVibe(cwd); got != "v-1" {
		t.Errorf("vibe = %q", got)
	}
	cdir := filepath.Join(home, ".cline", "data", "sessions", "w")
	os.MkdirAll(cdir, 0o755)
	os.WriteFile(filepath.Join(cdir, "s.json"),
		[]byte(`{"cwd":`+jsonString(cwd)+`,"session_id":"cl-1","started_at":"t2"}`), 0o644)
	os.WriteFile(filepath.Join(cdir, "s.messages.json"), []byte(`{}`), 0o644)
	if got := DiscoverCline(cwd); got != "cl-1" {
		t.Errorf("cline = %q", got)
	}
	gdir := filepath.Join(home, ".grok", "sessions", "a", "b")
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	os.MkdirAll(gdir, 0o755)
	os.WriteFile(filepath.Join(gdir, "summary.json"),
		[]byte(`{"info":{"cwd":`+jsonString(cwd)+`,"id":"g-1"},"last_active_at":"t3"}`), 0o644)
	if got := DiscoverGrok(cwd); got != "g-1" {
		t.Errorf("grok = %q", got)
	}
}

func TestDiscoverDeepseek(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DSH_HOME", filepath.Join(home, ".dsh"))
	cwd := "/work/ds"
	enc := "--" + strings.Trim(strings.ReplaceAll(cwd, "/", "-"), "-") + "--"
	dir := filepath.Join(home, ".dsh", "sessions", enc, "sess-abc")
	os.MkdirAll(dir, 0o755)
	if got := DiscoverDeepseek(cwd); got != "sess-abc" {
		t.Errorf("got %q", got)
	}
	if got := DiscoverDeepseek("/missing"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDiscoverGemini(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GEMINI_HOME", filepath.Join(home, ".gemini"))
	cwd := "/work/gm"
	sum := sha256.Sum256([]byte(cwd))
	want := hex.EncodeToString(sum[:])
	dir := filepath.Join(home, ".gemini", "tmp", "h", "chats")
	os.MkdirAll(dir, 0o755)
	header := `{"projectHash":"` + want + `","sessionId":"gm-1"}` + "\n"
	os.WriteFile(filepath.Join(dir, "session-1.jsonl"), []byte(header), 0o644)
	os.WriteFile(filepath.Join(dir, "session-2.jsonl"), []byte("{}\n"), 0o644)
	if got := DiscoverGemini(cwd); got != "gm-1" {
		t.Errorf("got %q", got)
	}
	if got := GeminiSessionFile("gm-1"); !strings.HasSuffix(got, "session-1.jsonl") {
		t.Errorf("session file = %q", got)
	}
	if got := DiscoverGemini("/other"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestDiscoverKimiFx(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	LookPathFunc = func(string) (string, error) { return "/bin/kimi", nil }
	RunBinFunc = wrapRunBinForTest(func(name string, args []string, cwd string) (string, int, error) {
		return `[{"id":"kimi-1"}]`, 0, nil
	})
	if got := DiscoverKimi("/w"); got != "kimi-1" {
		t.Errorf("kimi = %q", got)
	}
	LookPathFunc = func(string) (string, error) { return "", errTest }
	RunBinFunc = wrapRunBinForTest(func(name string, args []string, cwd string) (string, int, error) {
		if name == filepath.Join("x") {
			return "", 0, errTest
		}
		return `{"sessions":[{"id":"fx-1"}]}`, 0, nil
	})
	home := t.TempDir()
	t.Setenv("HOME", home) // no fx on PATH, no libexec fallback
	if got := DiscoverFx("/w"); got != "" {
		t.Errorf("fx without binary = %q", got)
	}
}

func TestDiscoverDispatch(t *testing.T) {
	if got := Discover("nope-agent", "/w"); got != "" {
		t.Errorf("unknown = %q", got)
	}
	if got := Discover("fx", "/nonexistent-cwd-xyz"); got != "" {
		t.Errorf("fx recover = %q", got)
	}
}

func TestDiscoverOpencodeKiloMuseMcode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	data := filepath.Join(home, "data")
	t.Setenv("XDG_DATA_HOME", data)
	cwd := "/work/sql"
	mk := func(path, create string, rows [][]any) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		writeTestDB(t, path, strings.Fields(create)[2], create, rows)
	}
	opdb := filepath.Join(data, "opencode", "opencode.db")
	mk(opdb, "CREATE TABLE session_v2 (id TEXT, directory TEXT, time_updated INTEGER)",
		[][]any{{"old", cwd, int64(1)}, {"new", cwd, int64(2)}, {"other", "/x", int64(9)}})
	if got := DiscoverOpencode(cwd); got != "new" {
		t.Errorf("opencode = %q", got)
	}
	kdb := filepath.Join(data, "kilo", "kilo.db")
	mk(kdb, "CREATE TABLE session (id TEXT, directory TEXT, time_updated INTEGER)",
		[][]any{{"k-1", cwd, int64(5)}})
	if got := DiscoverKilo(cwd); got != "k-1" {
		t.Errorf("kilo = %q", got)
	}
	mdb := filepath.Join(data, "muse", "session-index.db")
	mk(mdb, "CREATE TABLE sessions (session_id TEXT, workspace_root TEXT, updated_at_us INTEGER)",
		[][]any{{"m-1", cwd, int64(7)}})
	if got := DiscoverMuse(cwd); got != "m-1" {
		t.Errorf("muse = %q", got)
	}
	mcdb := filepath.Join(home, ".minimax", "v2", "sqlite", "runtime-state.sqlite")
	mk(mcdb, "CREATE TABLE local_runtime_sessions (session_id TEXT, workspace_dir TEXT, session_kind TEXT, updated_at_ms INTEGER)",
		[][]any{{"task-1", cwd, "task", int64(9)}, {"conv-1", cwd, "conversation", int64(3)}})
	if got := DiscoverMcode(cwd); got != "conv-1" {
		t.Errorf("mcode = %q", got)
	}
}

func TestDiscoverAntigravityCrush(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	data := filepath.Join(home, "data")
	t.Setenv("XDG_DATA_HOME", data)
	cwd := filepath.Join(t.TempDir(), "ag")
	agdb := filepath.Join(home, ".gemini", "antigravity-cli", "conversation_summaries.db")
	os.MkdirAll(filepath.Dir(agdb), 0o755)
	writeTestDB(t, agdb, "conversation_summaries",
		"CREATE TABLE conversation_summaries (conversation_id TEXT, workspace_uris TEXT, last_modified_time INTEGER)",
		[][]any{{"ag-1", `["file://` + cwd + `"]`, int64(4)}, {"ag-2", `["file:///else"]`, int64(8)}})
	if got := DiscoverAntigravity(cwd); got != "ag-1" {
		t.Errorf("antigravity = %q", got)
	}
	// Backtick-quoted CREATE TABLE columns and profile shadow homes.
	profDB := filepath.Join(home, ".model-router", "profiles", "antigravity", "saucerf166", "home", ".gemini", "antigravity-cli", "conversation_summaries.db")
	os.MkdirAll(filepath.Dir(profDB), 0o755)
	writeTestDB(t, profDB, "conversation_summaries",
		"CREATE TABLE `conversation_summaries` (`conversation_id` text,`workspace_uris` text NOT NULL,`last_modified_time` datetime NOT NULL)",
		[][]any{{"ag-prof", `["file://` + cwd + `"]`, int64(10)}})
	if got := DiscoverAntigravityForProfile(cwd, "saucerf166"); got != "ag-prof" {
		t.Errorf("antigravity profile = %q, want ag-prof", got)
	}
	if got := DiscoverAntigravity(cwd); got != "ag-prof" {
		t.Errorf("antigravity fallback across profiles = %q, want ag-prof", got)
	}
	crushCwd := filepath.Join(t.TempDir(), "crushwork")
	cdb := filepath.Join(crushCwd, ".crush", "crush.db")
	os.MkdirAll(filepath.Dir(cdb), 0o755)
	writeTestDB(t, cdb, "sessions",
		"CREATE TABLE sessions (id TEXT, updated_at INTEGER)",
		[][]any{{"c-1", int64(1)}, {"c-2", int64(2)}})
	if got := DiscoverCrush(crushCwd); got != "c-2" {
		t.Errorf("crush local = %q", got)
	}
	// Registry fallback.
	os.Remove(cdb)
	reg := filepath.Join(data, "crush", "projects.json")
	os.MkdirAll(filepath.Dir(reg), 0o755)
	altDir := t.TempDir()
	altDB := filepath.Join(altDir, "crush.db")
	writeTestDB(t, altDB, "sessions",
		"CREATE TABLE sessions (id TEXT, updated_at INTEGER)",
		[][]any{{"c-9", int64(1)}})
	os.WriteFile(reg, []byte(`{"projects":[{"path":"`+crushCwd+`","data_dir":"`+altDir+`"}]}`), 0o644)
	if got := DiscoverCrush(crushCwd); got != "c-9" {
		t.Errorf("crush registry = %q", got)
	}
}

func TestSessionIDFromJSONFiles(t *testing.T) {
	dir := t.TempDir()
	cwd := "/w"
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	os.WriteFile(a, []byte("{\"cwd\":"+jsonString(cwd)+",\"id\":\"via-id\"}\n"), 0o644)
	os.WriteFile(b, []byte("{\"cwd\":"+jsonString(cwd)+"}\n"), 0o644)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(b, past, past)
	if got := SessionIDFromJSONFiles([]string{a, b}, cwd); got != "via-id" {
		t.Errorf("got %q", got)
	}
	// Stem fallback.
	os.Remove(a)
	if got := SessionIDFromJSONFiles([]string{b}, cwd); got != "b" {
		t.Errorf("stem = %q", got)
	}
	if got := SessionIDFromJSONFiles(nil, cwd); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestDiscoverAntigravityWithOptionsExcludeAndTitle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(t.TempDir(), "repo")

	// Default DB has a conversation that must NOT be leaked when profile DB exists.
	defDB := filepath.Join(home, ".gemini", "antigravity-cli", "conversation_summaries.db")
	os.MkdirAll(filepath.Dir(defDB), 0o755)
	writeTestDB(t, defDB, "conversation_summaries",
		"CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, preview TEXT, workspace_uris TEXT, last_modified_time INTEGER)",
		[][]any{
			{"ag-default", "Default Conv", "Default Conv", `["file://` + cwd + `"]`, int64(50)},
		})

	profDB := filepath.Join(home, ".model-router", "profiles", "antigravity", "saucerf166", "home", ".gemini", "antigravity-cli", "conversation_summaries.db")
	os.MkdirAll(filepath.Dir(profDB), 0o755)
	writeTestDB(t, profDB, "conversation_summaries",
		"CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, preview TEXT, workspace_uris TEXT, last_modified_time INTEGER)",
		[][]any{
			{"ag-1", "Fix tmux bug", "Fix tmux bug", `["file://` + cwd + `"]`, int64(10)},
			{"ag-2", "Add feature X", "Add feature X", `["file://` + cwd + `"]`, int64(20)},
		})

	// Title match prefers older ag-1 over newer ag-2.
	if got := DiscoverAntigravityWithOptions(cwd, "saucerf166", "Fix tmux bug", nil); got != "ag-1" {
		t.Fatalf("title match = %q, want ag-1", got)
	}
	// Excluding ag-2 returns ag-1.
	if got := DiscoverAntigravityWithOptions(cwd, "saucerf166", "", map[string]bool{"ag-2": true}); got != "ag-1" {
		t.Fatalf("exclude ag-2 = %q, want ag-1", got)
	}
	// Excluding both returns "" instead of leaking ag-default from default DB.
	if got := DiscoverAntigravityWithOptions(cwd, "saucerf166", "", map[string]bool{"ag-1": true, "ag-2": true}); got != "" {
		t.Fatalf("all excluded = %q, want empty", got)
	}
}
