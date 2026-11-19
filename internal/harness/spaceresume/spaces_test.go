package spaceresume

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func stubZellij(panes map[string][]map[string]any, sessions []string, screens map[string]string) func([]string, time.Duration) (string, int, error) {
	return func(args []string, _ time.Duration) (string, int, error) {
		if len(args) == 2 && args[0] == "list-sessions" {
			return strings.Join(sessions, "\n") + "\n", 0, nil
		}
		if len(args) >= 2 && args[0] == "--session" {
			session := args[1]
			rest := args[2:]
			if len(rest) >= 2 && rest[0] == "action" && rest[1] == "list-panes" {
				b, _ := json.Marshal(panes[session])
				return string(b), 0, nil
			}
			if len(rest) >= 3 && rest[0] == "action" && rest[1] == "dump-screen" {
				id := strings.TrimPrefix(rest[3], "terminal_")
				return screens[session+":"+id], 0, nil
			}
		}
		return "", 1, nil
	}
}

func TestSpaceAgents(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	ZellijFunc = stubZellij(map[string][]map[string]any{
		"s1": {
			{"id": float64(1), "is_plugin": false,
				"program_status": []any{map[string]any{"app": "claude-code"}}},
			{"id": float64(2), "is_plugin": false, "terminal_command": "bash"},
			{"id": float64(3), "is_plugin": true},
			{"id": "x", "is_plugin": false,
				"program_status": []any{map[string]any{"app": "codex"}}},
		},
	}, []string{"s1", "EXITED s-old extra"}, nil)
	outPath, _ := captureOutput(t)
	if code := SpaceAgents(); code != 0 {
		t.Fatalf("code = %d", code)
	}
	var payload map[string]map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFile(t, outPath))), &payload); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if payload["s1"]["1"] != "claude-code" {
		t.Errorf("payload = %v", payload)
	}
	if len(payload) != 1 {
		t.Errorf("EXITED session leaked: %v", payload)
	}
}

func TestSpaceSnapshot(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	ZellijFunc = stubZellij(map[string][]map[string]any{
		"s1": {
			{"id": float64(1), "is_plugin": false, "title": "t1",
				"program_status": []any{map[string]any{"app": "claude-code"}}},
			{"id": float64(2), "is_plugin": false, "title": "shell",
				"terminal_command": "bash"},
			{"id": float64(3), "is_plugin": false, "exited": true, "title": "gone",
				"program_status": []any{map[string]any{"app": "codex"}}},
		},
	}, []string{"s1"}, map[string]string{"s1:1": "line1\nline2\n"})
	outPath, _ := captureOutput(t)
	fixed := time.Now()
	NowFunc = func() time.Time { return fixed }
	if code := SpaceSnapshot(); code != 0 {
		t.Fatalf("code = %d", code)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFile(t, outPath))), &payload); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if payload["ok"] != true {
		t.Errorf("ok = %v", payload["ok"])
	}
	sessions := payload["sessions"].(map[string]any)
	entry := sessions["s1"].(map[string]any)
	panes := entry["panes"].(map[string]any)
	if len(panes) != 1 {
		t.Fatalf("panes = %v", panes)
	}
	pane := panes["1"].(map[string]any)
	if pane["app"] != "claude-code" || pane["dump_ok"] != true || pane["screen"] != "line1\nline2" {
		t.Errorf("pane = %v", pane)
	}
}

func TestSpaceSnapshotListFailure(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	ZellijFunc = func([]string, time.Duration) (string, int, error) { return "", 1, nil }
	outPath, _ := captureOutput(t)
	if code := SpaceSnapshot(); code != 0 {
		t.Fatalf("code = %d", code)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFile(t, outPath))), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["ok"] != false {
		t.Errorf("ok = %v", payload["ok"])
	}
}

func TestTrimScreenText(t *testing.T) {
	if got := TrimScreenText("a  \n\n"); got != "a" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x\n", 300)
	if got := TrimScreenText(long); len(strings.Split(got, "\n")) != 200 {
		t.Errorf("lines = %d", len(strings.Split(got, "\n")))
	}
	big := strings.Repeat("xxxxxxxxxxé\n", 20000)
	got := TrimScreenText(big)
	if len(got) > SnapshotMaxBytes {
		t.Errorf("bytes = %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Error("tail splits a UTF-8 sequence")
	}
	if !strings.HasSuffix(got, "é\n") && !strings.HasSuffix(got, "é") {
		t.Errorf("tail = %q", got[len(got)-8:])
	}
}

func TestPaneAppPrecedence(t *testing.T) {
	pane := map[string]any{
		"attention":        map[string]any{"detail": map[string]any{"app": "codex"}},
		"program_status":   []any{map[string]any{"app": "claude-code"}},
		"terminal_command": "bash",
	}
	if got := PaneApp(pane); got != "codex" {
		t.Errorf("attention first = %q", got)
	}
	pane2 := map[string]any{
		"program_status": []any{
			map[string]any{"app": "claude-code"},
			map[string]any{"app": "codex"},
		},
	}
	if got := PaneApp(pane2); got != "codex" {
		t.Errorf("latest record = %q", got)
	}
	pane3 := map[string]any{"terminal_command": "claude --continue"}
	if got := PaneApp(pane3); got != "claude-code" {
		t.Errorf("command detect = %q", got)
	}
	if got := PaneApp(map[string]any{}); got != "" {
		t.Errorf("empty = %q", got)
	}
}
