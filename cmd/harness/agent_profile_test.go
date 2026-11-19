package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/agents"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
	"github.com/arhammahajan/model-router/internal/harness/profiles"
)

type capturedLaunch struct {
	calls    int
	agent    string
	profile  string
	resolved agents.Resolved
}

func isolateHarnessEnv(t *testing.T) harnessenv.Paths {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("HARNESS_DOTFILES", filepath.Join(tmp, "dots-missing"))
	t.Setenv("HARNESS_AGENT", "")
	t.Setenv("HARNESS_ACCESS", "default")
	return harnessenv.DefaultPaths()
}

func stubLaunch(t *testing.T) *capturedLaunch {
	t.Helper()
	cap := &capturedLaunch{}
	orig := harnessLaunchAgent
	t.Cleanup(func() { harnessLaunchAgent = orig })
	harnessLaunchAgent = func(resolved agents.Resolved, agent, profile string) int {
		cap.calls++
		cap.agent = agent
		cap.profile = profile
		cap.resolved = resolved
		return 0
	}
	return cap
}

func mustCreateHarnessProfile(t *testing.T, paths harnessenv.Paths, name, agent string, enabled bool, prepend ...string) {
	t.Helper()
	s, err := profiles.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	p := profiles.Profile{Agent: agent, Enabled: enabled, Router: true, PrependArgs: prepend}
	if err := s.Create(name, p, profiles.CreateOptions{NoSeed: true}); err != nil {
		t.Fatalf("Create(%s) = %v", name, err)
	}
}

func writeHarnessT3(t *testing.T, paths harnessenv.Paths, instances string) {
	t.Helper()
	dir := filepath.Join(paths.Home, ".t3", "userdata")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"providerInstances": {` + instances + `}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	code := fn()
	_ = w.Close()
	os.Stderr = orig
	data, _ := io.ReadAll(r)
	return code, string(data)
}

func resolvedArgs(res agents.Resolved) []string { return res.Args }

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestBareAgentZeroProfiles(t *testing.T) {
	paths := isolateHarnessEnv(t)
	_ = paths
	cap := stubLaunch(t)
	code, _ := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "--hello"}) })
	if code != 0 {
		t.Fatalf("bare launch exit = %d, want 0", code)
	}
	if cap.calls != 1 || cap.agent != "pi" || cap.profile != "default" {
		t.Fatalf("launch = %+v, want one pi default launch", cap)
	}
	if !containsArg(resolvedArgs(cap.resolved), "--hello") {
		t.Fatalf("forwarded args missing --hello: %v", cap.resolved.Args)
	}
	if cap.resolved.Binary != "pi" {
		t.Fatalf("binary = %q, want pi", cap.resolved.Binary)
	}
}

func TestSecondProfileRequiresExplicitSelection(t *testing.T) {
	paths := isolateHarnessEnv(t)
	// Adding a single custom profile gives pi 2 profiles: default + solo.
	mustCreateHarnessProfile(t, paths, "solo", "pi", true, "--pre")
	cap := stubLaunch(t)
	code, msg := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "--foo", "bar"}) })
	if code != 2 {
		t.Fatalf("2-profile exit = %d, want 2 (stderr %q)", code, msg)
	}
	if cap.calls != 0 {
		t.Fatal("2-profile setup must not auto-launch")
	}
	for _, want := range []string{"default", "solo", "harness pi profile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr %q must mention %q", msg, want)
		}
	}
}

func TestExplicitDefaultProfileLaunchesUnmodified(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "solo", "pi", true, "--pre")
	cap := stubLaunch(t)
	code, msg := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "profile", "default", "--foo"}) })
	if code != 0 {
		t.Fatalf("explicit default exit = %d, want 0 (%q)", code, msg)
	}
	if cap.calls != 1 || cap.agent != "pi" {
		t.Fatalf("launch = %+v, want one pi default launch", cap)
	}
	if containsArg(cap.resolved.Args, "--pre") {
		t.Fatalf("default profile must be unmodified, got %v", cap.resolved.Args)
	}
	if !containsArg(cap.resolved.Args, "--foo") {
		t.Fatalf("default profile must forward caller args, got %v", cap.resolved.Args)
	}
}

func TestMultiProfileFailsWithHint(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "alpha", "pi", true)
	mustCreateHarnessProfile(t, paths, "beta", "pi", true)
	cap := stubLaunch(t)
	code, msg := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "--foo"}) })
	if code != 2 {
		t.Fatalf("multi-profile exit = %d, want 2 (stderr %q)", code, msg)
	}
	if cap.calls != 0 {
		t.Fatal("multi-profile must not launch anything")
	}
	for _, want := range []string{"alpha", "beta", "harness pi profile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr %q must mention %q", msg, want)
		}
	}
}

func TestExplicitProfileLaunch(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "alpha", "pi", true)
	mustCreateHarnessProfile(t, paths, "beta", "pi", true)
	cap := stubLaunch(t)
	code, _ := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "profile", "beta", "--x", "1"}) })
	if code != 0 {
		t.Fatalf("explicit launch exit = %d, want 0", code)
	}
	if cap.calls != 1 || cap.agent != "pi" || cap.profile != "beta" {
		t.Fatalf("launch = %+v, want beta profile launch", cap)
	}
	if !containsArg(cap.resolved.Args, "--x") || !containsArg(cap.resolved.Args, "1") {
		t.Fatalf("explicit caller args must forward, got %v", cap.resolved.Args)
	}
}

func TestExplicitProfileSingleStillWorks(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "solo", "pi", true)
	cap := stubLaunch(t)
	code, _ := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "profile", "solo"}) })
	if code != 0 {
		t.Fatalf("explicit single exit = %d, want 0", code)
	}
	if cap.calls != 1 {
		t.Fatal("explicit single profile must launch")
	}
}

func TestExplicitProfileMissingName(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "alpha", "pi", true)
	cap := stubLaunch(t)
	code, msg := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "profile"}) })
	if code != 2 {
		t.Fatalf("missing name exit = %d, want 2 (%q)", code, msg)
	}
	if cap.calls != 0 {
		t.Fatal("missing name must not launch")
	}
	if !strings.Contains(msg, "alpha") {
		t.Errorf("stderr %q must list available profiles", msg)
	}
}

func TestExplicitProfileUnknown(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "alpha", "pi", true)
	cap := stubLaunch(t)
	code, msg := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "profile", "nope"}) })
	if code != 2 {
		t.Fatalf("unknown exit = %d, want 2 (%q)", code, msg)
	}
	if cap.calls != 0 {
		t.Fatal("unknown profile must not launch")
	}
	for _, want := range []string{"nope", "alpha"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr %q must mention %q", msg, want)
		}
	}
}

func TestExplicitProfileWrongAgent(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "mine", "pi", true)
	mustCreateHarnessProfile(t, paths, "c-one", "codex", true)
	cap := stubLaunch(t)
	code, msg := captureStderr(t, func() int { return runAgentLaunch([]string{"codex", "profile", "mine"}) })
	if code != 2 {
		t.Fatalf("wrong-agent exit = %d, want 2 (%q)", code, msg)
	}
	if cap.calls != 0 {
		t.Fatal("wrong-agent profile must not launch")
	}
	for _, want := range []string{"mine", "pi", "codex"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr %q must mention %q", msg, want)
		}
	}
}

func TestAgentAliasRouting(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "a-one", "antigravity", true)
	cap := stubLaunch(t)
	code, _ := captureStderr(t, func() int { return runAgentLaunch([]string{"agy", "profile", "a-one", "--z"}) })
	if code != 0 {
		t.Fatalf("alias explicit exit = %d, want 0", code)
	}
	if cap.calls != 1 || cap.agent != "antigravity" {
		t.Fatalf("alias launch = %+v, want antigravity", cap)
	}
	if !containsArg(cap.resolved.Args, "--z") {
		t.Fatalf("alias forwarded args missing: %v", cap.resolved.Args)
	}
}

func TestHarnessAgentBypassesProfileRouting(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "alpha", "pi", true)
	mustCreateHarnessProfile(t, paths, "beta", "pi", true)
	t.Setenv("HARNESS_AGENT", "pi")
	cap := stubLaunch(t)
	code, _ := captureStderr(t, func() int { return runAgentLaunch([]string{"probe-arg"}) })
	if code != 0 {
		t.Fatalf("HARNESS_AGENT bare exit = %d, want 0", code)
	}
	if cap.calls != 1 || cap.agent != "pi" || cap.profile != "" {
		t.Fatalf("HARNESS_AGENT must bare-launch without profile, got %+v", cap)
	}
	if !containsArg(cap.resolved.Args, "probe-arg") {
		t.Fatalf("HARNESS_AGENT args must pass through, got %v", cap.resolved.Args)
	}
}

func TestHarnessAgentProfileKeywordNotParsed(t *testing.T) {
	paths := isolateHarnessEnv(t)
	_ = paths
	t.Setenv("HARNESS_AGENT", "pi")
	cap := stubLaunch(t)
	code, _ := captureStderr(t, func() int { return runAgentLaunch([]string{"profile"}) })
	if code != 0 {
		t.Fatalf("HARNESS_AGENT profile-arg exit = %d, want 0 (bare launch)", code)
	}
	if cap.calls != 1 {
		t.Fatal("HARNESS_AGENT must treat 'profile' as a plain agent arg")
	}
	if !containsArg(cap.resolved.Args, "profile") {
		t.Fatalf("bare args must contain 'profile', got %v", cap.resolved.Args)
	}
}

func TestDisabledCustomProfileRestoresDefaultAutoLaunch(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "dead", "pi", false, "--pre")
	cap := stubLaunch(t)
	code, msg := captureStderr(t, func() int { return runAgentLaunch([]string{"pi", "--arg"}) })
	if code != 0 {
		t.Fatalf("disabled-excluded exit = %d, want 0 (%q)", code, msg)
	}
	if cap.calls != 1 || cap.agent != "pi" {
		t.Fatalf("only default enabled must auto-launch default, got %+v", cap)
	}
	if containsArg(cap.resolved.Args, "--pre") || !containsArg(cap.resolved.Args, "--arg") {
		t.Fatalf("expected unmodified default args, got %v", cap.resolved.Args)
	}
}

func TestT3SettingsIgnoredByHarness(t *testing.T) {
	paths := isolateHarnessEnv(t)
	writeHarnessT3(t, paths, `"codex": {"displayName": "Codex"}, "codex_router": {"displayName": "Codex Router"}`)
	cap := stubLaunch(t)
	code, msg := captureStderr(t, func() int { return runAgentLaunch([]string{"codex", "--arg"}) })
	if code != 0 {
		t.Fatalf("T3 settings must be ignored (default-only launch), exit = %d (%q)", code, msg)
	}
	if cap.calls != 1 || cap.agent != "codex" {
		t.Fatalf("launch = %+v, want codex default", cap)
	}
	if !containsArg(cap.resolved.Args, "--arg") {
		t.Fatalf("forwarded args missing: %v", cap.resolved.Args)
	}
}

func TestLegacyProfileSubcommandStillLaunches(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "mine", "pi", true)
	cap := stubLaunch(t)
	code, _ := captureStderr(t, func() int { return runProfile([]string{"mine", "--keep"}) })
	if code != 0 {
		t.Fatalf("legacy profile launch exit = %d, want 0", code)
	}
	if cap.calls != 1 || cap.agent != "pi" || cap.profile != "mine" {
		t.Fatalf("legacy launch = %+v, want pi/mine", cap)
	}
	if !containsArg(cap.resolved.Args, "--keep") {
		t.Fatalf("legacy forwarded args missing: %v", cap.resolved.Args)
	}
}

func TestUnknownAgentStillUsageError(t *testing.T) {
	isolateHarnessEnv(t)
	stubLaunch(t)
	code, _ := captureStderr(t, func() int { return runAgentLaunch([]string{"bogus"}) })
	if code != 2 {
		t.Fatalf("unknown agent exit = %d, want 2", code)
	}
}
