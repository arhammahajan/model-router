package spaceresume

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResumeArgsTable(t *testing.T) {
	cases := []struct {
		agent, id string
		want      []string
	}{
		{"claude", "s1", []string{"--resume", "s1"}},
		{"claude", "", []string{"--continue"}},
		{"codex", "s1", []string{"resume", "s1"}},
		{"codex", "", []string{"resume", "--last"}},
		{"opencode", "s1", []string{"--session", "s1"}},
		{"opencode", "", []string{"--continue"}},
		{"muse", "", []string{"resume", "--last"}},
		{"cline", "s1", []string{"--id", "s1"}},
		{"cline", "", []string{}},
		{"omp", "s1", []string{"--resume=s1"}},
		{"omp", "", []string{"--continue"}},
		{"antigravity", "s1", []string{"--conversation", "s1"}},
		{"deepseek", "s1", []string{"--resume", "s1"}},
		{"dsh-tui", "", []string{"--continue"}},
		{"fx", "s1", []string{"--continue"}},
		{"fx-router", "", []string{"--continue"}},
		{"unknown-agent", "s1", []string{}},
		{"pi", "", []string{"--continue"}},
		{"kimi", "k", []string{"--session", "k"}},
	}
	for _, c := range cases {
		got := ResumeArgs(c.agent, c.id)
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("ResumeArgs(%q,%q) = %v, want %v", c.agent, c.id, got, c.want)
		}
	}
}

func TestStripResumeArgs(t *testing.T) {
	cases := []struct {
		agent string
		args  []string
		want  []string
	}{
		{"claude", []string{"--resume", "old", "foo"}, []string{"foo"}},
		{"claude", []string{"-r", "old", "--", "x"}, []string{"--", "x"}},
		{"claude", []string{"--continue", "x"}, []string{"x"}},
		{"codex", []string{"resume", "old", "exec", "run.py"}, []string{"exec", "run.py"}},
		{"codex", []string{"resume", "--last"}, []string{}},
		{"muse", []string{"resume", "old"}, []string{}},
		{"opencode", []string{"--session", "s", "-c"}, []string{}},
		{"gemini", []string{"--session-file", "/tmp/x", "prompt"}, []string{"prompt"}},
		{"omp", []string{"--resume=old", "x"}, []string{"x"}},
		{"fx", []string{"session", "resume", "old", "run"}, []string{"run"}},
		{"cline", []string{"--id", "old"}, []string{}},
		{"claude", []string{"plain", "args"}, []string{"plain", "args"}},
	}
	for _, c := range cases {
		got := StripResumeArgs(c.agent, c.args)
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("StripResumeArgs(%q,%v) = %v, want %v", c.agent, c.args, got, c.want)
		}
	}
}

func TestAlreadyResuming(t *testing.T) {
	if !AlreadyResuming("codex", []string{"resume", "--last"}) {
		t.Error("codex resume not detected")
	}
	if !AlreadyResuming("claude", []string{"-r", "x"}) {
		t.Error("claude -r not detected")
	}
	if !AlreadyResuming("claude", []string{"--continue"}) {
		t.Error("claude --continue not detected")
	}
	if !AlreadyResuming("omp", []string{"--resume=x"}) {
		t.Error("omp --resume= not detected")
	}
	if AlreadyResuming("claude", []string{"--model", "opus"}) {
		t.Error("false positive")
	}
	if AlreadyResuming("claude", nil) {
		t.Error("empty should be false")
	}
}

func TestIsNoninteractive(t *testing.T) {
	if !IsNoninteractive("claude", []string{"-p", "hi"}) {
		t.Error("claude -p")
	}
	if IsNoninteractive("claude", []string{"hi"}) {
		t.Error("claude interactive")
	}
	if !IsNoninteractive("codex", []string{"exec", "run.py"}) {
		t.Error("codex exec")
	}
	if IsNoninteractive("codex", []string{"--resume", "x"}) {
		t.Error("codex resume is interactive")
	}
	if !IsNoninteractive("cline", []string{}) {
		t.Error("cline without --tui is headless")
	}
	if IsNoninteractive("cline", []string{"--tui"}) {
		t.Error("cline --tui is interactive")
	}
	if !IsNoninteractive("fx", []string{"ask", "q"}) {
		t.Error("fx ask")
	}
	if IsNoninteractive("fx", []string{"other"}) {
		t.Error("fx other is interactive")
	}
	if !IsNoninteractive("grok", []string{"--single"}) {
		t.Error("grok --single")
	}
}

func TestIsolateAgent(t *testing.T) {
	got := IsolateAgent("opencode", []string{"opencode", "--session", "x"})
	if strings.Join(got, " ") != "opencode --standalone --session x" {
		t.Errorf("isolate = %v", got)
	}
	got = IsolateAgent("opencode", []string{"opencode", "--standalone"})
	if len(got) != 2 {
		t.Errorf("already isolated = %v", got)
	}
	got = IsolateAgent("opencode", []string{"zj", "__shim", "opencode"})
	if strings.Join(got, " ") != "zj __shim opencode" {
		t.Errorf("shim passthrough = %v", got)
	}
	got = IsolateAgent("claude", []string{"claude"})
	if len(got) != 1 {
		t.Errorf("non-opencode changed = %v", got)
	}
}

func TestWrapDecodeError(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	outPath, _ := captureOutput(t)
	ExecFn = func([]string, string) { t.Error("exec must not run on decode error") }
	if code := Wrap("!!!not-base64!!!", ""); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if got := readFile(t, outPath); got != "" {
		t.Errorf("stdout = %q, want empty", got)
	}
}

func TestWrapExplicitSession(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	TmuxFunc = nil
	tty, _ := os.CreateTemp("", "tty-*")
	tty.Close()
	t.Setenv("SPACES_TTY", tty.Name())
	var gotArgv []string
	var gotOriginal string
	ExecFn = func(argv []string, original string) { gotArgv, gotOriginal = argv, original }
	original := "claude --model opus"
	code := Wrap(b64Encode(original), "sess-9")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.Join(gotArgv, " ") != "claude --model opus --resume sess-9" {
		t.Errorf("argv = %v", gotArgv)
	}
	if gotOriginal != original {
		t.Errorf("original = %q", gotOriginal)
	}
}

func TestWrapDiscovery(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("SPACES_TTY", filepath.Join(home, "tty"))
	cwd := t.TempDir()
	t.Chdir(cwd)
	proj := filepath.Join(home, ".claude", "projects", EncodedPath(cwd))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"cwd":` + jsonString(cwd) + `,"sessionId":"discovered-1"}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "a.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotArgv []string
	ExecFn = func(argv []string, original string) { gotArgv = argv }
	if code := Wrap(b64Encode("claude"), ""); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.Join(gotArgv, " ") != "claude --resume discovered-1" {
		t.Errorf("argv = %v", gotArgv)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestWrapAlreadyResuming(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	t.Setenv("SPACES_TTY", filepath.Join(t.TempDir(), "tty"))
	var gotArgv []string
	ExecFn = func(argv []string, original string) { gotArgv = argv }
	if code := Wrap(b64Encode("claude --resume old"), ""); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.Join(gotArgv, " ") != "claude --resume old" {
		t.Errorf("argv = %v (must exec unchanged)", gotArgv)
	}
}

func TestWrapNonAgent(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	t.Setenv("SPACES_TTY", filepath.Join(t.TempDir(), "tty"))
	var gotArgv []string
	ExecFn = func(argv []string, original string) { gotArgv = argv }
	if code := Wrap(b64Encode("vim file.txt"), ""); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.Join(gotArgv, " ") != "vim file.txt" {
		t.Errorf("argv = %v", gotArgv)
	}
}

func TestHookResurrect(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	outPath, _ := captureOutput(t)
	t.Setenv("RESURRECT_COMMAND", "claude --model opus")
	if code := HookResurrect(); code != 0 {
		t.Fatalf("code = %d", code)
	}
	got := strings.TrimSpace(readFile(t, outPath))
	inner, err := b64Decode(strings.TrimPrefix(got, "zj __resume-wrap "))
	if err != nil || inner != "claude --model opus" {
		t.Errorf("output = %q", got)
	}
}

func TestHookResurrectPassthrough(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	outPath, _ := captureOutput(t)
	for _, cmd := range []string{"", "vim x", "claude -p q", "zj __resume-wrap abc"} {
		t.Setenv("RESURRECT_COMMAND", cmd)
		if code := HookResurrect(); code != 0 {
			t.Fatalf("cmd %q: code = %d", cmd, code)
		}
		if got := strings.TrimSpace(readFile(t, outPath)); got != strings.TrimSpace(cmd) {
			t.Errorf("cmd %q: output = %q", cmd, got)
		}
		resetFile(t, Stdout, outPath)
	}
}

func TestResumeCommand(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	outPath, _ := captureOutput(t)
	if code := ResumeCommand([]string{"claude", "/work/dir"}); code != 0 {
		t.Fatalf("code = %d", code)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFile(t, outPath))), &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload["agent"] != "claude" || payload["cwd"] != "/work/dir" {
		t.Errorf("payload = %v", payload)
	}
	if payload["session_id"] != nil {
		t.Errorf("session_id should be null, got %v", payload["session_id"])
	}
	args, _ := payload["args"].([]any)
	if len(args) != 1 || args[0] != "--continue" {
		t.Errorf("args = %v", payload["args"])
	}
	if code := ResumeCommand(nil); code != 2 {
		t.Errorf("no-args code = %d, want 2", code)
	}
}

func TestWrapAntigravityHarnessProfile(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	profDB := filepath.Join(home, ".model-router", "profiles", "antigravity", "saucerf166", "home", ".gemini", "antigravity-cli", "conversation_summaries.db")
	os.MkdirAll(filepath.Dir(profDB), 0o755)
	writeTestDB(t, profDB, "conversation_summaries",
		"CREATE TABLE `conversation_summaries` (`conversation_id` text,`workspace_uris` text NOT NULL,`last_modified_time` datetime NOT NULL)",
		[][]any{{"conv-123", `["file://` + cwd + `"]`, "2026-11-20 08:16:10+00:00"}})

	var execd []string
	ExecFn = func(argv []string, _ string) {
		execd = append([]string(nil), argv...)
	}

	encoded := b64Encode("harness agy profile saucerf166")
	if code := Wrap(encoded, ""); code != 0 {
		t.Fatalf("Wrap(harness agy profile) code = %d", code)
	}
	want := "harness agy profile saucerf166 --conversation conv-123"
	if strings.Join(execd, " ") != want {
		t.Fatalf("execd = %q, want %q", strings.Join(execd, " "), want)
	}

	execd = nil
	encodedShim := b64Encode("harness spaces __shim-profile antigravity/saucerf166")
	if code := Wrap(encodedShim, ""); code != 0 {
		t.Fatalf("Wrap(__shim-profile) code = %d", code)
	}
	wantShim := "harness spaces __shim-profile antigravity/saucerf166 --conversation conv-123"
	if strings.Join(execd, " ") != wantShim {
		t.Fatalf("execd shim = %q, want %q", strings.Join(execd, " "), wantShim)
	}
}

func TestShlexRoundTrip(t *testing.T) {
	argv := []string{"claude", "--model", "op'us", "a b", ""}
	joined := shlexJoin(argv)
	back, err := shlexSplit(joined)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(back, "\x00") != strings.Join(argv, "\x00") {
		t.Errorf("round trip %q -> %v", joined, back)
	}
	if _, err := shlexSplit("unclosed 'quote"); err == nil {
		t.Error("expected quote error")
	}
}

func TestChooseBaseCommand(t *testing.T) {
	old := AncestorsFunc
	defer func() { AncestorsFunc = old }()
	cands := [][]string{
		{"bash"},
		{"claude", "--resume", "old"},
		{"zj", "__resume-wrap", "eA=="},
	}
	base := ChooseBaseCommand("claude", cands)
	if strings.Join(base, " ") != "claude" {
		t.Errorf("base = %v", base)
	}
	// Wrapper preferred (realistic shim shape: APP then BIN).
	cands = [][]string{
		{"claude", "--model", "x"},
		{"zj", "__shim", "claude", "/usr/bin/claude"},
	}
	base = ChooseBaseCommand("claude", cands)
	if strings.Join(base, " ") != "zj __shim claude /usr/bin/claude" {
		t.Errorf("wrapper base = %v", base)
	}
}

func TestAgentIsNoninteractiveNearest(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	AncestorsFunc = func() [][]string {
		return [][]string{{"claude"}, {"claude", "-p", "q"}}
	}
	// Nearest agent command is interactive.
	if AgentIsNoninteractive("claude") {
		t.Error("nearest is interactive, want false")
	}
	AncestorsFunc = func() [][]string {
		return [][]string{{"claude", "-p", "q"}, {"claude"}}
	}
	if !AgentIsNoninteractive("claude") {
		t.Error("nearest is -p, want true")
	}
	AncestorsFunc = func() [][]string {
		return [][]string{{"bash"}}
	}
	if AgentIsNoninteractive("claude") {
		t.Error("no agent ancestor, want false")
	}
	_ = time.Second
}
