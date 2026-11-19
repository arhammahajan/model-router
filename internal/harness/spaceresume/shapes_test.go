package spaceresume

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests pin the byte-exact JSON shapes verified against zj-resume.py.

func TestSpaceAgentsExactShape(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	ZellijFunc = stubZellij(map[string][]map[string]any{
		"b": {
			{"id": float64(2), "terminal_command": "opencode --continue"},
		},
		"a": {
			{"id": float64(1), "program_status": []any{map[string]any{"app": "claude-code"}}},
		},
	}, []string{"b", "a"}, nil)
	outPath, _ := captureOutput(t)
	if code := SpaceAgents(); code != 0 {
		t.Fatalf("code = %d", code)
	}
	want := `{"a": {"1": "claude-code"}, "b": {"2": "opencode"}}` + "\n"
	if got := readFile(t, outPath); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestSpaceSnapshotExactShape(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	ZellijFunc = stubZellij(map[string][]map[string]any{
		"s1": {
			{"id": float64(1), "title": "t1",
				"program_status": []any{map[string]any{"app": "claude-code"}}},
		},
	}, []string{"s1"}, map[string]string{"s1:1": "hi"})
	outPath, _ := captureOutput(t)
	fixed := time.Now()
	NowFunc = func() time.Time { return fixed }
	if code := SpaceSnapshot(); code != 0 {
		t.Fatalf("code = %d", code)
	}
	// Verified byte-identical against `zj-resume.py space-snapshot`.
	want := `{"ok":true,"sessions":{"s1":{"ok":true,"panes":{"1":{"app":"claude-code","dump_ok":true,"screen":"hi","title":"t1"}}}}}` + "\n"
	if got := readFile(t, outPath); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestSpaceSnapshotPaneCap(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	var panes []map[string]any
	for i := 1; i <= 70; i++ {
		panes = append(panes, map[string]any{
			"id":             float64(i),
			"program_status": []any{map[string]any{"app": "claude-code"}},
		})
	}
	ZellijFunc = stubZellij(map[string][]map[string]any{"s": panes},
		[]string{"s"}, map[string]string{})
	// Screen lookup returns empty success for every pane.
	outPath, _ := captureOutput(t)
	fixed := time.Now()
	NowFunc = func() time.Time { return fixed }
	if code := SpaceSnapshot(); code != 0 {
		t.Fatalf("code = %d", code)
	}
	got := readFile(t, outPath)
	if n := strings.Count(got, `"dump_ok":true`); n != 64 {
		t.Errorf("dumped panes = %d, want 64", n)
	}
	if n := strings.Count(got, `"dump_ok":false`); n != 6 {
		t.Errorf("skipped panes = %d, want 6", n)
	}
}

func TestResumeCommandExactShape(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", home)
	outPath, _ := captureOutput(t)
	ResumeCommand([]string{"opencode", "/w"})
	want := `{"agent": "opencode", "cwd": "/w", "session_id": null, "args": ["--continue"]}` + "\n"
	if got := readFile(t, outPath); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestSpaceHooksDetailPaneInt(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	SpaceHooksSummary = func() map[string]any { return map[string]any{} }
	var gotPane any
	SpaceHooksDetail = func(s string, p any) map[string]any {
		gotPane = p
		return map[string]any{}
	}
	outPath, _ := captureOutput(t)
	for in, want := range map[string]any{"3": 3, "03": 3, "+4": 4, "x": "x", "3x": "3x"} {
		resetFile(t, Stdout, outPath)
		if code := SpaceHooks([]string{"--detail", "s", in}); code != 0 {
			t.Fatalf("in %q: code = %d", in, code)
		}
		if fmt.Sprintf("%v|%T", gotPane, gotPane) != fmt.Sprintf("%v|%T", want, want) {
			t.Errorf("in %q: pane = %v (%T), want %v (%T)", in, gotPane, gotPane, want, want)
		}
	}
}
