package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/harness/profiles"
)

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	code := fn()
	_ = w.Close()
	os.Stdout = orig
	data, _ := io.ReadAll(r)
	return code, string(data)
}

func captureBoth(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()
	outOrig, errOrig := os.Stdout, os.Stderr
	or, ow, _ := os.Pipe()
	er, ew, _ := os.Pipe()
	os.Stdout, os.Stderr = ow, ew
	code := fn()
	_ = ow.Close()
	_ = ew.Close()
	os.Stdout, os.Stderr = outOrig, errOrig
	out, _ := io.ReadAll(or)
	errs, _ := io.ReadAll(er)
	return code, string(out), string(errs)
}

func mustCreateCLIProfile(t *testing.T, name, agent string) {
	t.Helper()
	paths := isolateHarnessEnv(t)
	_ = paths
	// isolateHarnessEnv already set env; DefaultPaths reads it. Create via runProfileCreate.
	if code, _ := captureStderr(t, func() int {
		// capture stdout too to silence "created profile"
		orig := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		c := runProfile([]string{"create", name, "--agent", agent})
		_ = w.Close()
		os.Stdout = orig
		_, _ = io.ReadAll(r)
		return c
	}); code != 0 {
		t.Fatalf("create %s = %d", name, code)
	}
}

func TestProfileCreateExtendedFlags(t *testing.T) {
	isolateHarnessEnv(t)
	// Default create: router=false, overlay=true (shares MCPs, skills, and chats while isolating auth).
	code, out := captureStdout(t, func() int {
		return runProfile([]string{"create", "clean", "--agent", "claude"})
	})
	if code != 0 {
		t.Fatalf("create default exit = %d (%s)", code, out)
	}
	_, showOut := captureStdout(t, func() int { return runProfile([]string{"show", "clean"}) })
	for _, want := range []string{"router:       false", "overlay:      true"} {
		if !strings.Contains(showOut, want) {
			t.Errorf("default profile show must contain %q:\n%s", want, showOut)
		}
	}
	// Explicit opt-in with --router and --overlay.
	code, out = captureStdout(t, func() int {
		return runProfile([]string{"create", "cx", "--agent", "codex", "--binary", "/bin/cx", "--home-path", "/shared", "--no-overlay", "--router"})
	})
	if code != 0 {
		t.Fatalf("create exit = %d (%s)", code, out)
	}
	_, showOut = captureStdout(t, func() int { return runProfile([]string{"show", "cx"}) })
	for _, want := range []string{"/bin/cx", "/shared", "overlay:      false", "router:       true"} {
		if !strings.Contains(showOut, want) {
			t.Errorf("show %q must contain %q:\n%s", "cx", want, showOut)
		}
	}
	// Conflicting flags rejected.
	if code, _, _ := captureBoth(t, func() int {
		return runProfile([]string{"create", "bad1", "--agent", "pi", "--router", "--no-router"})
	}); code != 2 {
		t.Fatalf("conflicting --router/--no-router exit = %d, want 2", code)
	}
	if code, _, _ := captureBoth(t, func() int {
		return runProfile([]string{"create", "bad2", "--agent", "pi", "--overlay", "--no-overlay"})
	}); code != 2 {
		t.Fatalf("conflicting --overlay/--no-overlay exit = %d, want 2", code)
	}
	// --binary/--home-path before name
	code, _ = captureStdout(t, func() int {
		return runProfile([]string{"create", "--agent", "pi", "--binary", "/bin/pi", "pi1"})
	})
	if code != 0 {
		t.Fatalf("create flag-first exit = %d", code)
	}
}

func TestProfileCreateRejectsUnknownFlagWithoutWriting(t *testing.T) {
	paths := isolateHarnessEnv(t)
	code, out, stderr := captureBoth(t, func() int {
		return run([]string{"profile", "create", "bad", "--agent", "codex", "--no-seed", "--unknown"})
	})
	if code != 2 {
		t.Errorf("create exit = %d, want 2 (stdout: %q, stderr: %q)", code, out, stderr)
	}
	if _, err := os.Stat(profiles.ProfilesDir(paths)); !os.IsNotExist(err) {
		t.Errorf("profile directory stat = %v, want it to be absent", err)
	}
}

func TestProfileNamesAreScopedToAgent(t *testing.T) {
	isolateHarnessEnv(t)
	launch := stubLaunch(t)
	command := func(want int, args ...string) (string, string) {
		t.Helper()
		code, out, stderr := captureBoth(t, func() int { return run(args) })
		if code != want {
			t.Fatalf("%v exited %d, want %d (stdout: %q, stderr: %q)", args, code, want, out, stderr)
		}
		return out, stderr
	}
	out, _ := command(0, "profile", "list")
	for _, agent := range profiles.KnownAgents() {
		if !strings.Contains(out, "default\t"+agent+"\t") {
			t.Errorf("fresh profile list missing default for %s", agent)
		}
	}

	command(0, "profile", "create", "work", "--agent", "codex", "--binary", "/fake/codex", "--no-seed")
	command(0, "profile", "create", "work", "--agent", "claude", "--binary", "/fake/claude", "--no-seed")
	command(1, "profile", "create", "work", "--agent", "codex", "--no-seed")
	out, _ = command(0, "profile", "list")
	for _, row := range []string{"work\tclaude\n", "work\tcodex\n"} {
		if !strings.Contains(out, row) {
			t.Errorf("profile list missing %q", row)
		}
	}
	_, stderr := command(2, "profile", "work")
	for _, ref := range []string{"claude/work", "codex/work"} {
		if !strings.Contains(stderr, ref) {
			t.Errorf("ambiguous launch missing %q in %q", ref, stderr)
		}
	}
	command(1, "profile", "remove", "work")
	command(0, "codex", "profile", "work")
	if launch.calls != 1 || launch.agent != "codex" || launch.resolved.Binary != "/fake/codex" {
		t.Fatalf("agent-scoped launch = %+v", launch)
	}
	command(0, "profile", "claude/work")
	if launch.calls != 2 || launch.agent != "claude" || launch.resolved.Binary != "/fake/claude" {
		t.Fatalf("qualified launch = %+v", launch)
	}

	command(0, "profile", "edit", "work", "--agent", "claude", "--display-name", "Claude Work")
	command(0, "profile", "disable", "claude/work")
	command(2, "claude", "profile", "work")
	command(0, "profile", "enable", "work", "--agent", "claude")
	command(0, "profile", "clone", "claude/work", "copy", "--no-seed")
	command(0, "profile", "clone", "codex/work", "copy", "--no-seed")
	command(0, "profile", "rename", "copy", "renamed", "--agent", "claude")
	command(0, "profile", "remove", "codex/copy", "--purge")
	command(2, "profile", "show", "codex/copy")
	command(0, "profile", "show", "claude/renamed")
	out, _ = command(0, "profile", "get", "work", "display-name", "--agent", "claude")
	if strings.TrimSpace(out) != "Claude Work" {
		t.Fatalf("Claude profile display name = %q", out)
	}
	out, _ = command(0, "profile", "path", "codex/work")
	if !strings.HasSuffix(strings.TrimSpace(out), "/codex/work/profile.json") {
		t.Fatalf("Codex profile path = %q", out)
	}
	command(0, "profile", "edit", "codex/default", "--display-name", "Codex Default")
	out, _ = command(0, "profile", "list", "--agent", "codex")
	if !strings.Contains(out, "default\tcodex\tCodex Default\n") {
		t.Fatalf("customized default missing from list: %q", out)
	}
}

func TestProfileEditEnableDisable(t *testing.T) {
	isolateHarnessEnv(t)
	mustCreateCLIProfile(t, "e1", "pi")

	if code, _, _ := captureBoth(t, func() int {
		return runProfile([]string{"edit", "e1", "--display-name", "Pretty", "--env", "FOO=bar", "--arg", "--x"})
	}); code != 0 {
		t.Fatalf("edit exit = %d", code)
	}
	_, out, _ := captureBoth(t, func() int { return runProfile([]string{"show", "e1"}) })
	for _, want := range []string{"Pretty", "FOO=bar", "--x"} {
		if !strings.Contains(out, want) {
			t.Errorf("show after edit must contain %q:\n%s", want, out)
		}
	}
	// flag-first ordering
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"edit", "--env", "B=2", "e1"}) }); code != 0 {
		t.Fatalf("edit flag-first exit = %d", code)
	}
	// unset + clear-args
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"edit", "e1", "--unset-env", "FOO", "--clear-args"}) }); code != 0 {
		t.Fatalf("edit unset/clear exit = %d", code)
	}
	_, out, _ = captureBoth(t, func() int { return runProfile([]string{"show", "e1"}) })
	if strings.Contains(out, "FOO=") {
		t.Errorf("unset-env must remove FOO:\n%s", out)
	}
	// disable blocks resolve; enable restores
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"disable", "e1"}) }); code != 0 {
		t.Fatalf("disable exit = %d", code)
	}
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"show", "e1"}) }); code != 2 {
		t.Fatalf("disabled show exit = %d, want 2", code)
	}
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"enable", "e1"}) }); code != 0 {
		t.Fatalf("enable exit = %d", code)
	}
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"show", "e1"}) }); code != 0 {
		t.Fatalf("re-enabled show exit = %d", code)
	}
	// router toggles
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"edit", "e1", "--no-router"}) }); code != 0 {
		t.Fatalf("edit --no-router exit = %d", code)
	}
	_, out, _ = captureBoth(t, func() int { return runProfile([]string{"show", "e1"}) })
	if !strings.Contains(out, "router:       false") {
		t.Errorf("show must contain router:false:\n%s", out)
	}
	// conflicting flags
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"edit", "e1", "--router", "--no-router"}) }); code != 2 {
		t.Fatalf("conflicting router flags exit = %d, want 2", code)
	}
	// unknown profile
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"edit", "nope", "--display-name", "X"}) }); code != 1 {
		t.Fatalf("edit missing exit = %d, want 1", code)
	}
}

func TestProfileCloneRenameCLI(t *testing.T) {
	isolateHarnessEnv(t)
	mustCreateCLIProfile(t, "src", "pi")
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"clone", "src", "dst"}) }); code != 0 {
		t.Fatalf("clone exit = %d", code)
	}
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"show", "dst"}) }); code != 0 {
		t.Fatalf("show clone exit = %d", code)
	}
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"clone", "src", "dst"}) }); code != 1 {
		t.Fatalf("dup clone exit = %d, want 1", code)
	}
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"rename", "dst", "dst2"}) }); code != 0 {
		t.Fatalf("rename exit = %d", code)
	}
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"show", "dst"}) }); code != 2 {
		t.Fatalf("old name after rename exit = %d, want 2", code)
	}
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"show", "dst2"}) }); code != 0 {
		t.Fatalf("new name after rename exit = %d", code)
	}
}

func TestProfileListAgentFilter(t *testing.T) {
	paths := isolateHarnessEnv(t)
	mustCreateHarnessProfile(t, paths, "c-one", "codex", true)
	mustCreateHarnessProfile(t, paths, "p-one", "pi", true)

	code, out := captureStdout(t, func() int { return runProfile([]string{"list", "--agent", "codex"}) })
	if code != 0 {
		t.Fatalf("list --agent exit = %d", code)
	}
	if !strings.Contains(out, "c-one") || strings.Contains(out, "p-one") {
		t.Errorf("agent filter wrong:\n%s", out)
	}
	// = form + json
	code, out = captureStdout(t, func() int { return runProfile([]string{"list", "--agent=pi", "--json"}) })
	if code != 0 {
		t.Fatalf("list --agent= --json exit = %d", code)
	}
	var entries []profiles.ListEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("json decode: %v\n%s", err, out)
	}
	if len(entries) != 2 || entries[0].Name != "default" || entries[0].Source != profiles.SourceDefault || entries[1].Name != "p-one" || entries[1].Agent != "pi" {
		t.Fatalf("filtered json wrong: %+v", entries)
	}
	// alias + antigravity-native mapping
	code, out = captureStdout(t, func() int { return runProfile([]string{"list", "--agent", "agy", "--json"}) })
	if code != 0 {
		t.Fatalf("list alias exit = %d", code)
	}
	var agyEntries []profiles.ListEntry
	if err := json.Unmarshal([]byte(out), &agyEntries); err != nil {
		t.Fatal(err)
	}
	if len(agyEntries) != 1 || agyEntries[0].Name != profiles.DefaultProfileName || agyEntries[0].Agent != "antigravity" {
		t.Fatalf("agy filter should contain only default antigravity profile, got %+v", agyEntries)
	}
	// unknown agent
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"list", "--agent", "bogus"}) }); code != 2 {
		t.Fatalf("list bogus agent exit = %d, want 2", code)
	}
	// unfiltered json still works and carries agent
	code, out = captureStdout(t, func() int { return runProfile([]string{"list", "--json"}) })
	if code != 0 {
		t.Fatalf("list --json exit = %d", code)
	}
	var all []map[string]any
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		t.Fatal(err)
	}
	if want := len(profiles.KnownAgents()) + 2; len(all) != want {
		t.Fatalf("unfiltered list = %d, want %d", len(all), want)
	}
}

func TestProfileShowSecrets(t *testing.T) {
	paths := isolateHarnessEnv(t)
	s, err := profiles.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	hp := filepath.Join(t.TempDir(), "hp")
	p := profiles.Profile{
		Agent: "pi", Enabled: true, Router: true,
		DisplayName: "Show", BinaryPath: "/bin/pi", HomePath: hp,
		PrependArgs: []string{"--a", "--b"},
		Env:         map[string]string{"PLAIN": "hello", "API_KEY": "sekret", "myToken": "tok", "OTHER": "x"},
	}
	if err := s.Create("showme", p, profiles.CreateOptions{NoSeed: true}); err != nil {
		t.Fatal(err)
	}
	_, out, _ := captureBoth(t, func() int { return runProfile([]string{"show", "showme"}) })
	for _, want := range []string{
		"name:         showme", "source:       store", "agent:        pi",
		"enabled:      true", "display name: Show", "binary:       /bin/pi",
		"home path:    " + hp, "router:       true", "args:         --a --b",
		"PLAIN=hello", "OTHER=x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show must contain %q:\n%s", want, out)
		}
	}
	// secrets masked
	if strings.Contains(out, "sekret") || strings.Contains(out, "tok") {
		t.Errorf("show must mask secrets:\n%s", out)
	}
	if strings.Count(out, "***") < 2 {
		t.Errorf("show must mask at least 2 secrets:\n%s", out)
	}
	// sorted env: OTHER before PLAIN (alphabetical)
	ioo, ipo := strings.Index(out, "OTHER=x"), strings.Index(out, "PLAIN=hello")
	if ioo < 0 || ipo < 0 || ioo > ipo {
		t.Errorf("env must be sorted:\n%s", out)
	}
	// codex overlay line
	mustCreateHarnessProfile(t, paths, "cx1", "codex", true)
	_, out2, _ := captureBoth(t, func() int { return runProfile([]string{"show", "cx1"}) })
	if !strings.Contains(out2, "overlay:") {
		t.Errorf("codex show must contain overlay:\n%s", out2)
	}
}

func TestLaunchMaterializesCodexOverlay(t *testing.T) {
	paths := isolateHarnessEnv(t)
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "overlay-file"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := profiles.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	if err := s.Create("cx", profiles.Profile{Agent: "codex", Enabled: true, Router: true, HomePath: shared, Overlay: &on}, profiles.CreateOptions{NoSeed: true}); err != nil {
		t.Fatal(err)
	}
	np, _ := s.Get("cx")
	shadow := np.Profile.ShadowHome
	// remove any overlay link planted at create time to prove launch materializes it
	_ = os.Remove(filepath.Join(shadow, "overlay-file"))
	// add a new shared file after create; launch must pick it up
	if err := os.WriteFile(filepath.Join(shared, "late-file"), []byte("late"), 0o644); err != nil {
		t.Fatal(err)
	}
	cap := stubLaunch(t)
	_ = cap
	// describe must NOT materialize
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"describe", "cx"}) }); code != 0 {
		t.Fatalf("describe exit = %d", code)
	}
	if _, err := os.Lstat(filepath.Join(shadow, "late-file")); !os.IsNotExist(err) {
		t.Fatal("describe must not materialize the overlay")
	}
	// real launch must materialize
	var buf bytes.Buffer
	_ = buf
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"cx"}) }); code != 0 {
		t.Fatalf("launch exit = %d", code)
	}
	if _, err := os.Lstat(filepath.Join(shadow, "late-file")); err != nil {
		t.Fatalf("launch must materialize overlay: %v", err)
	}
	// second launch idempotent
	if code, _, _ := captureBoth(t, func() int { return runProfile([]string{"cx"}) }); code != 0 {
		t.Fatalf("second launch exit = %d", code)
	}
}

func TestProfileInitShowJSONGetAndDefaultLaunch(t *testing.T) {
	paths := isolateHarnessEnv(t)
	// `harness profile init` seeds per-agent default profile.json files under ~/.model-router/profiles.
	code, out := captureStdout(t, func() int { return runProfile([]string{"init"}) })
	if code != 0 {
		t.Fatalf("profile init exit = %d (%s)", code, out)
	}
	if strings.TrimSpace(out) != profiles.ProfilesDir(paths) {
		t.Fatalf("init path = %q, want %q", strings.TrimSpace(out), profiles.ProfilesDir(paths))
	}
	// Edit ~/.model-router/profiles/claude/default/profile.json directly, just like any other profile.
	claudeDefFile := profiles.ProfileFilePath(paths, "claude", profiles.DefaultProfileName)
	raw, err := os.ReadFile(claudeDefFile)
	if err != nil {
		t.Fatalf("read default profile.json: %v", err)
	}
	var claudeDef map[string]any
	if err := json.Unmarshal(raw, &claudeDef); err != nil {
		t.Fatal(err)
	}
	customHome := filepath.Join(paths.Home, "my-claude-home")
	claudeDef["binaryPath"] = "/custom/bin/claude"
	claudeDef["homePath"] = customHome
	claudeDef["env"] = map[string]any{"CUSTOM_ENV": "hello"}
	claudeDef["prependArgs"] = []any{"--fast"}
	claudeDef["router"] = false
	edited, err := json.MarshalIndent(claudeDef, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeDefFile, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	// Query via `harness profile show default --agent claude --json`.
	code, jsonOut := captureStdout(t, func() int {
		return runProfile([]string{"show", "default", "--agent", "claude", "--json"})
	})
	if code != 0 {
		t.Fatalf("show default --json exit = %d", code)
	}
	var snap profileShowSnapshot
	if err := json.Unmarshal([]byte(jsonOut), &snap); err != nil {
		t.Fatalf("decode show --json: %v\n%s", err, jsonOut)
	}
	if snap.EffectiveBinary != "/custom/bin/claude" || snap.EffectiveHomePath != customHome || snap.Router || snap.Env["CUSTOM_ENV"] != "hello" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	// Query individual fields via `harness profile get`.
	for field, want := range map[string]string{
		"binary":         "/custom/bin/claude",
		"home-path":      customHome,
		"env.CUSTOM_ENV": "hello",
		"args":           "--fast",
		"router":         "false",
	} {
		code, got := captureStdout(t, func() int {
			return runProfile([]string{"get", "default", field, "--agent", "claude"})
		})
		if code != 0 || strings.TrimSpace(got) != want {
			t.Errorf("get default %s = (code=%d, %q), want %q", field, code, strings.TrimSpace(got), want)
		}
	}
	// Launching `harness claude` picks up the edited default profile from profiles.json.
	cap := stubLaunch(t)
	code, _, stderr := captureBoth(t, func() int { return runAgentLaunch([]string{"claude", "--extra"}) })
	if code != 0 {
		t.Fatalf("runAgentLaunch(claude) exit = %d (%s)", code, stderr)
	}
	if cap.resolved.Binary != "/custom/bin/claude" {
		t.Fatalf("launched binary = %q, want /custom/bin/claude", cap.resolved.Binary)
	}
	if strings.Join(cap.resolved.Args, " ") != "--fast --extra" {
		t.Fatalf("launched args = %v, want [--fast --extra]", cap.resolved.Args)
	}
	foundEnv := false
	foundHome := false
	for _, kv := range cap.resolved.Env {
		if kv == "CUSTOM_ENV=hello" {
			foundEnv = true
		}
		if kv == "CLAUDE_CONFIG_DIR="+customHome {
			foundHome = true
		}
	}
	if !foundEnv || !foundHome {
		t.Fatalf("launched env missing overrides: %v", cap.resolved.Env)
	}
}
