package attn

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeChild writes an executable shell script under dir and returns
// its path.
func fakeChild(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// oscRecords splits a captured record stream into full records.
func oscRecords(s string) []string {
	var recs []string
	for _, part := range strings.Split(s, "\x1b\\") {
		if part != "" {
			recs = append(recs, part+"\x1b\\")
		}
	}
	return recs
}

func assertRecords(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("records = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWrapExitZero(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	bin := fakeChild(t, dir, "agent", `printf '%s\n' "$*" >> "`+log+`"; exit 0`)
	code := Wrap("claude-code", bin, []string{"--flag", "x"}, nil)
	if code != 0 {
		t.Fatalf("Wrap exit = %d, want 0", code)
	}
	assertRecords(t, oscRecords(f.tty.String()), []string{
		"\x1b]7501;state=working:app=claude-code\x1b\\",
		"\x1b]7501;state=idle:app=claude-code\x1b\\",
		"\x1b]7501;state=done:app=claude-code:msg=ZmluaXNoZWQ=\x1b\\",
	})
	if f.out.Len() != 0 {
		t.Errorf("stdout fallback = %q, want empty", f.out.String())
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "--flag x\n" {
		t.Errorf("child argv = %q, want %q", got, "--flag x\n")
	}
}

func TestWrapExitThree(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	bin := fakeChild(t, dir, "agent", "exit 3")
	code := Wrap("kimi", bin, nil, nil)
	if code != 3 {
		t.Fatalf("Wrap exit = %d, want 3", code)
	}
	assertRecords(t, oscRecords(f.tty.String()), []string{
		"\x1b]7501;state=working:app=kimi\x1b\\",
		"\x1b]7501;state=idle:app=kimi\x1b\\",
		"\x1b]7501;state=error:app=kimi:msg=" +
			base64.StdEncoding.EncodeToString([]byte("exited with status 3")) + "\x1b\\",
	})
}

func TestWrapSpawnFailure(t *testing.T) {
	f := newFixture(t)
	bin := filepath.Join(t.TempDir(), "missing-agent")
	code := Wrap("deepseek", bin, nil, nil)
	if code != 127 {
		t.Fatalf("Wrap exit = %d, want 127", code)
	}
	assertRecords(t, oscRecords(f.tty.String()), []string{
		"\x1b]7501;state=working:app=deepseek\x1b\\",
		"\x1b]7501;state=idle:app=deepseek\x1b\\",
		"\x1b]7501;state=error:app=deepseek:msg=" +
			base64.StdEncoding.EncodeToString([]byte("exited with status 127")) + "\x1b\\",
	})
}

func TestWrapSignalDeath(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	bin := fakeChild(t, dir, "agent", "kill -9 $$")
	code := Wrap("zj", bin, nil, nil)
	if code != 137 { // 128 + SIGKILL, shell-style
		t.Fatalf("Wrap exit = %d, want 137", code)
	}
	assertRecords(t, oscRecords(f.tty.String()), []string{
		"\x1b]7501;state=working:app=zj\x1b\\",
		"\x1b]7501;state=idle:app=zj\x1b\\",
		"\x1b]7501;state=error:app=zj:msg=" +
			base64.StdEncoding.EncodeToString([]byte("exited with status 137")) + "\x1b\\",
	})
}

func TestWrapEnvAppendWins(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "env.log")
	bin := fakeChild(t, dir, "envchild",
		`printf '%s\n' "$ATTN_ADDED" >> "`+log+`"; printf '%s\n' "$ATTN_EXISTING" >> "`+log+`"`)
	t.Setenv("ATTN_EXISTING", "base")
	envAppend := []string{"ATTN_ADDED=added", "ATTN_EXISTING=overridden"}
	code := Wrap("opencode", bin, nil, envAppend)
	if code != 0 {
		t.Fatalf("Wrap exit = %d, want 0", code)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "added\noverridden\n"; got != want {
		t.Errorf("child env = %q, want %q", got, want)
	}
	// The attention lifecycle is unaffected by the environment edits.
	assertRecords(t, oscRecords(f.tty.String()), []string{
		"\x1b]7501;state=working:app=opencode\x1b\\",
		"\x1b]7501;state=idle:app=opencode\x1b\\",
		"\x1b]7501;state=done:app=opencode:msg=ZmluaXNoZWQ=\x1b\\",
	})
}

func TestChildEnv(t *testing.T) {
	t.Setenv("ATTN_CHILD_BASE", "keep")
	t.Setenv("ATTN_CHILD_BOTH", "old")
	envAppend := []string{"ATTN_CHILD_NEW=new", "ATTN_CHILD_BOTH=new", "malformed"}
	env := childEnv(envAppend)
	seen := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			if kv != "malformed" {
				t.Errorf("unexpected malformed entry %q", kv)
			}
			continue
		}
		if prev, dup := seen[k]; dup {
			t.Errorf("duplicate key %q (%q and %q)", k, prev, v)
			continue
		}
		seen[k] = v
	}
	if seen["ATTN_CHILD_BASE"] != "keep" {
		t.Errorf("base entry lost: %q", seen["ATTN_CHILD_BASE"])
	}
	if seen["ATTN_CHILD_NEW"] != "new" {
		t.Errorf("appended entry = %q, want new", seen["ATTN_CHILD_NEW"])
	}
	if seen["ATTN_CHILD_BOTH"] != "new" {
		t.Errorf("shadowed entry = %q, want new (envAppend must win)", seen["ATTN_CHILD_BOTH"])
	}
	// One inherited entry is dropped in favour of its envAppend
	// replacement; the rest pass through untouched.
	if want := len(os.Environ()) - 1 + len(envAppend); len(env) != want {
		t.Errorf("env length = %d, want %d", len(env), want)
	}
}

func TestWrapWithProfileSetsAndClearsTmuxProfile(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "tty")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldTty, oldOut, oldPath := TtyWriter, Out, TtyPath
	oldEmit, oldProfile := TmuxEmitFunc, TmuxProfileFunc
	t.Cleanup(func() {
		TtyWriter, Out, TtyPath = oldTty, oldOut, oldPath
		TmuxEmitFunc, TmuxProfileFunc = oldEmit, oldProfile
	})
	TtyPath = dev
	TtyWriter = ttyDevice{}

	var events []string
	TmuxEmitFunc = func(app, state, msg, kind string) {
		events = append(events, "emit:"+state+":"+app)
	}
	TmuxProfileFunc = func(profile string) {
		events = append(events, "profile:"+profile)
	}

	dir := t.TempDir()
	bin := fakeChild(t, dir, "agent", "exit 0")
	if code := WrapWithProfile("antigravity", "pratham", bin, nil, nil); code != 0 {
		t.Fatalf("WrapWithProfile exit = %d, want 0", code)
	}
	want := []string{
		"profile:pratham",
		"emit:working:antigravity",
		"emit:idle:antigravity",
		"emit:done:antigravity",
		"profile:",
	}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
