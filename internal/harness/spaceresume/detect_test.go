package spaceresume

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var errTest = errors.New("test error")

func wrapRunBinForTest(fn func(name string, args []string, cwd string) (string, int, error)) func(string, []string, string, time.Duration) (string, int, error) {
	return func(name string, args []string, cwd string, _ time.Duration) (string, int, error) {
		return fn(name, args, cwd)
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"claude": "claude", "/usr/bin/claude": "claude",
		"CLAUDE": "claude", "claude.js": "claude",
		"agy": "antigravity", "dsh": "deepseek",
		"kimi-code": "kimi", "cursor": "cursor-agent",
		"codex": "codex", "opencode": "opencode",
		"bash": "", "vim": "", "": "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInferAgentHints(t *testing.T) {
	cases := map[string]string{
		"/opt/minimax/bin/cli":   "mcode",
		"/x/mistral-vibe/bin":    "vibe",
		"/usr/local/bin/dsh":     "deepseek",
		"/usr/local/bin/dsh-tui": "dsh-tui",
		"/x/fx":                  "fx", "/x/fx-router": "fx-router", // direct name wins over hints
		"/x/tool-fx-router-v2": "fx",
		"/x/pi":                "pi", "/usr/bin/pi": "pi",
		"something-omp-thing": "omp",
		"totally-unknown-xyz": "",
		"echo":                "",
	}
	for in, want := range cases {
		if got := InferAgent(in); got != want {
			t.Errorf("InferAgent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectAgent(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"claude", "--resume", "x"}, "claude"},
		{[]string{"bash", "claude"}, "claude"},
		{[]string{"bash", "-c", "claude"}, ""}, // -c scripts are opaque
		{[]string{"sh", "-lc", "codex"}, ""},
		{[]string{"bash", "-c", "echo hi"}, ""},
		{[]string{"harness", "claude"}, "claude"},
		{[]string{"harness", "profile"}, ""},
		{[]string{"zj", "__shim", "claude", "/usr/bin/claude"}, "claude"},
		{[]string{"zj", "__shim", "codex"}, "codex"},
		{[]string{"zj", "__shim", "--hold", "codex", "/usr/bin/codex"}, "codex"},
		{[]string{"env", "FOO=1", "claude"}, "claude"},
		{[]string{"node", "/opt/claude/cli.js"}, "claude"},
		{[]string{"python3", "/tmp/t/agent_claude_test"}, ""},
		{[]string{}, ""},
		{[]string{"ls", "-la"}, ""},
	}
	for _, c := range cases {
		if got := DetectAgent(c.argv); got != c.want {
			t.Errorf("DetectAgent(%v) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestDetectShellScriptShim(t *testing.T) {
	old := LookPathFunc
	defer func() { LookPathFunc = old }()
	if got := DetectShellScript([]string{"bash", "-c", "echo"}); got != "" {
		t.Errorf("sh -c echo = %q, want empty", got)
	}
	if got := DetectShellScript([]string{"bash", "claude"}); got != "claude" {
		t.Errorf("bash claude = %q", got)
	}
}

func TestCommandAgentResumeWrap(t *testing.T) {
	inner := shlexJoin([]string{"claude", "--resume", "abc"})
	encoded := b64Encode(inner)
	if got := CommandAgent("zj __resume-wrap " + encoded); got != "claude" {
		t.Errorf("resume-wrap decode = %q, want claude", got)
	}
	if got := CommandAgent("zj __resume-wrap !!!"); got != "" {
		t.Errorf("bad base64 = %q, want empty", got)
	}
	if got := CommandAgent("claude --continue"); got != "claude" {
		t.Errorf("plain = %q", got)
	}
}

func TestIsWrapperCommand(t *testing.T) {
	if !IsWrapperCommand([]string{"zj", "__shim", "claude"}) {
		t.Error("zj wrapper not detected")
	}
	if !IsWrapperCommand([]string{"sudo", "harness", "claude"}) {
		t.Error("harness wrapper not detected")
	}
	if IsWrapperCommand([]string{"claude"}) {
		t.Error("bare agent is a wrapper?")
	}
}

func TestProfileAgent(t *testing.T) {
	oldLook, oldRun := LookPathFunc, RunBinFunc
	defer func() { LookPathFunc, RunBinFunc = oldLook, oldRun }()
	LookPathFunc = func(string) (string, error) { return "/bin/harness", nil }
	// Wire via the real seam type using a shim:
	RunBinFunc = wrapRunBinForTest(func(name string, args []string, cwd string) (string, int, error) {
		if strings.Join(args, " ") != "profile --describe work" {
			t.Errorf("unexpected profile args %v", args)
		}
		return "claude --resume abc\n", 0, nil
	})
	if got := ProfileAgent("work"); got != "claude" {
		t.Errorf("ProfileAgent = %q", got)
	}
	LookPathFunc = func(string) (string, error) { return "", errTest }
	if got := ProfileAgent("work"); got != "" {
		t.Errorf("missing harness = %q", got)
	}
}

func TestDetectAgentSpacesShimProfile(t *testing.T) {
	if got := DetectAgent([]string{"spaces", "__shim-profile", "claude/work"}); got != "claude" {
		t.Errorf("DetectAgent(spaces __shim-profile claude/work) = %q, want claude", got)
	}
	if got := harnessProfileFromArgv([]string{"spaces", "__shim-profile", "claude/work"}); got != "work" {
		t.Errorf("harnessProfileFromArgv(spaces __shim-profile claude/work) = %q, want work", got)
	}
}
