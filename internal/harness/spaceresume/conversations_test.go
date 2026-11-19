package spaceresume

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResumeArgsAuditFixes(t *testing.T) {
	cases := []struct {
		agent, id string
		want      []string
	}{
		{"copilot", "cop-1", []string{"--resume=cop-1"}},
		{"copilot", "", []string{"--continue"}},
		{"grok", "grk-1", []string{"--resume", "grk-1"}},
		{"grok", "", []string{"--continue"}},
		{"vibe", "vib-1", []string{"--resume", "vib-1"}},
		{"vibe", "", []string{"--continue"}},
		{"gemini", "/tmp/chats/session-1.jsonl", []string{"--session-file", "/tmp/chats/session-1.jsonl"}},
		{"gemini", "gem-id-99", []string{"--resume", "gem-id-99"}},
		{"gemini", "", []string{"--resume", "latest"}},
		{"pi", "/home/u/.pi/agent/sessions/s1.jsonl", []string{"--session", "/home/u/.pi/agent/sessions/s1.jsonl"}},
	}
	for _, c := range cases {
		got := ResumeArgs(c.agent, c.id)
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("ResumeArgs(%q, %q) = %v, want %v", c.agent, c.id, got, c.want)
		}
	}
}

func TestSpaceConversationsDegradedAndDelegated(t *testing.T) {
	old := SpaceConversationsFunc
	defer func() { SpaceConversationsFunc = old }()

	outPath, _ := captureOutput(t)
	SpaceConversationsFunc = nil
	if code := SpaceConversations([]string{"--cached-only"}); code != 0 {
		t.Fatalf("degraded code = %d, want 0", code)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFile(t, outPath))), &payload); err != nil {
		t.Fatalf("invalid degraded json: %v", err)
	}
	if payload["ok"] != false {
		t.Fatalf("expected ok=false, got %v", payload["ok"])
	}

	var gotArgs []string
	SpaceConversationsFunc = func(args []string) int {
		gotArgs = append([]string(nil), args...)
		return 7
	}
	if code := SpaceConversations([]string{"--search", "backoff"}); code != 7 {
		t.Fatalf("delegated code = %d, want 7", code)
	}
	if strings.Join(gotArgs, " ") != "--search backoff" {
		t.Fatalf("delegated args = %v", gotArgs)
	}
}

func TestRecordConversationOverlayCallSites(t *testing.T) {
	snap := saveSeams()
	defer snap.restore()
	oldRec := RecordConversationOverlay
	oldRecOutcome := RecordOutcome
	defer func() {
		RecordConversationOverlay = oldRec
		RecordOutcome = oldRecOutcome
	}()

	fixedNow := time.UnixMilli(1700000123456)
	NowFunc = func() time.Time { return fixedNow }

	// 1. Wrap with explicitSessionID records last_opened_ms + profile.
	var recorded []map[string]any
	RecordConversationOverlay = func(entry map[string]any) {
		recorded = append(recorded, entry)
	}
	TmuxFunc = nil
	ExecFn = func([]string, string) {}
	t.Setenv("SPACES_TTY", filepath.Join(t.TempDir(), "tty"))
	if code := Wrap(b64Encode("harness claude profile work"), "sess-open-1"); code != 0 {
		t.Fatalf("Wrap code = %d", code)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected 1 overlay record from Wrap, got %d", len(recorded))
	}
	if recorded[0]["agent"] != "claude" || recorded[0]["session_id"] != "sess-open-1" ||
		recorded[0]["profile"] != "work" || recorded[0]["last_opened_ms"] != int64(1700000123456) {
		t.Fatalf("unexpected Wrap overlay record: %#v", recorded[0])
	}

	// 2. SpaceHookRecordOutcome records overlay with space/outcome/profile.
	recorded = nil
	outPath, _ := captureOutput(t)
	_ = outPath
	RecordOutcome = func(map[string]any) error { return nil }
	raw, _ := json.Marshal(map[string]any{
		"session":    "devspace",
		"agent":      "codex",
		"session_id": "codex-42",
		"profile":    "personal",
		"cwd":        "/tmp/repo",
		"title":      "Fix retries",
		"outcome":    "done",
	})
	if code := SpaceHookRecordOutcome([]string{string(raw)}); code != 0 {
		t.Fatalf("SpaceHookRecordOutcome code = %d", code)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected 1 overlay record from SpaceHookRecordOutcome, got %d", len(recorded))
	}
	if recorded[0]["agent"] != "codex" || recorded[0]["session_id"] != "codex-42" ||
		recorded[0]["profile"] != "personal" || recorded[0]["space"] != "devspace" ||
		recorded[0]["last_outcome"] != "done" {
		t.Fatalf("unexpected outcome overlay record: %#v", recorded[0])
	}

	// 3. HookClient records overlay when session_id is present and not a child.
	recorded = nil
	t.Setenv("ZELLIJ_SESSION_NAME", "zspace")
	t.Setenv("ZELLIJ_PANE_ID", "terminal_3")
	t.Setenv("HARNESS_PROFILE", "team")
	HookDisabled = func() bool { return false }
	NormalizeEvent = func(agent, event string, payload map[string]any, env map[string]string) map[string]any {
		return map[string]any{
			"event": "stop",
			"state": "done",
			"cwd":   "/work/app",
			"title": "Hook title",
			"model": "claude-opus-4-6",
		}
	}
	Publish = func(map[string]any) bool { return true }
	AncestorsFunc = func() [][]string { return nil }
	TtyPathsFunc = func() []string { return nil }

	tmpOut, err := os.CreateTemp(t.TempDir(), "hook-*")
	if err != nil {
		t.Fatal(err)
	}
	tmpOut.Close()
	if code := hookClientWithPayload("claude", "Stop", map[string]any{"session_id": "hook-sess-9"}, false); code != 0 {
		t.Fatalf("hookClientWithPayload code = %d", code)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected 1 overlay record from hookClientWithPayload, got %d", len(recorded))
	}
	if recorded[0]["session_id"] != "hook-sess-9" || recorded[0]["profile"] != "team" ||
		recorded[0]["space"] != "zspace" || recorded[0]["last_outcome"] != "done" ||
		recorded[0]["model"] != "claude-opus-4-6" {
		t.Fatalf("unexpected hook overlay record: %#v", recorded[0])
	}
}
