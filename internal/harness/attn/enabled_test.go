package attn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnabledGating(t *testing.T) {
	newFixture(t) // buffer tty: a writable record target

	t.Setenv("ZELLIJ_SESSION_NAME", "")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	if Enabled() {
		t.Error("Enabled() outside Zellij and tmux = true, want false")
	}

	// Inside tmux without TMUX_PANE is not a targetable pane.
	t.Setenv("TMUX", "/tmp/tmux-1000/default,42,0")
	if Enabled() {
		t.Error("Enabled() with TMUX but no TMUX_PANE = true, want false")
	}

	// Inside tmux with TMUX + TMUX_PANE enables OSC 7501 attention reporting.
	t.Setenv("TMUX_PANE", "%12")
	if !Enabled() {
		t.Error("Enabled() inside tmux = false, want true")
	}
	for _, v := range []string{"0", "false"} {
		t.Setenv("HARNESS_ATTN", v)
		if Enabled() {
			t.Errorf("Enabled() inside tmux with HARNESS_ATTN=%q = true, want false", v)
		}
	}
	t.Setenv("HARNESS_ATTN", "")
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")

	t.Setenv("ZELLIJ_SESSION_NAME", "main")
	if !Enabled() {
		t.Error("Enabled() inside Zellij = false, want true")
	}

	// Present-but-empty HARNESS_ATTN means enabled.
	t.Setenv("HARNESS_ATTN", "")
	if !Enabled() {
		t.Error("Enabled() with empty HARNESS_ATTN = false, want true")
	}

	for _, v := range []string{"0", "false"} {
		t.Setenv("HARNESS_ATTN", v)
		if Enabled() {
			t.Errorf("Enabled() with HARNESS_ATTN=%q = true, want false", v)
		}
	}

	t.Setenv("HARNESS_ATTN", "1")
	if !Enabled() {
		t.Error("Enabled() with HARNESS_ATTN=1 = false, want true")
	}

	// T3 invocations still report: their panes are real.
	t.Setenv("HARNESS_AGENT", "t3-run")
	if !Enabled() {
		t.Error("Enabled() with HARNESS_AGENT set = false, want true")
	}

	// An unusable record target disables reporting.
	t.Setenv("HARNESS_ATTN", "")
	TtyWriter = failingWriter{}
	if Enabled() {
		t.Error("Enabled() with unwritable target = true, want false")
	}

	// A nil target is not writable either.
	TtyWriter = nil
	if Enabled() {
		t.Error("Enabled() with nil TtyWriter = true, want false")
	}
}

// Enabled probes the default device through the seam: the TtyPath
// redirect keeps /dev/tty out of the test.
func TestEnabledProbesDefaultDevice(t *testing.T) {
	newFixture(t)
	t.Setenv("ZELLIJ_SESSION_NAME", "main")
	t.Setenv("HARNESS_ATTN", "")
	oldPath, oldTty := TtyPath, TtyWriter
	t.Cleanup(func() { TtyPath, TtyWriter = oldPath, oldTty })

	dev := filepath.Join(t.TempDir(), "tty")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	TtyPath, TtyWriter = dev, ttyDevice{}
	if !Enabled() {
		t.Error("Enabled() with writable device = false, want true")
	}

	TtyPath = filepath.Join(t.TempDir(), "absent-tty")
	if Enabled() {
		t.Error("Enabled() with unopenable device = true, want false")
	}
}
