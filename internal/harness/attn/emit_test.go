package attn

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// fixture captures both record targets and restores them on cleanup,
// so tests never touch the real /dev/tty.
type fixture struct {
	tty bytes.Buffer
	out bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	oldTty, oldOut := TtyWriter, Out
	TtyWriter, Out = &f.tty, &f.out
	t.Cleanup(func() { TtyWriter, Out = oldTty, oldOut })
	return f
}

// failingWriter rejects every write, standing in for an unusable tty.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func TestEmitStateAndAppOnly(t *testing.T) {
	f := newFixture(t)
	Emit("zj", "working", "", "")
	want := "\x1b]7501;state=working:app=zj\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
	if f.out.Len() != 0 {
		t.Errorf("stdout fallback = %q, want empty", f.out.String())
	}
}

func TestEmitKindBeforeMsg(t *testing.T) {
	f := newFixture(t)
	Emit("opencode", "blocked", "need input", "permission")
	want := "\x1b]7501;state=blocked:app=opencode:kind=permission:msg=" +
		base64.StdEncoding.EncodeToString([]byte("need input")) + "\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
}

func TestEmitBase64Msg(t *testing.T) {
	f := newFixture(t)
	Emit("zj", "done", "finished", "")
	// Hardcoded StdEncoding of "finished" to pin the bytes exactly.
	want := "\x1b]7501;state=done:app=zj:msg=ZmluaXNoZWQ=\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
	f.tty.Reset()
	Emit("zj", "error", "exited with status 3", "")
	want = "\x1b]7501;state=error:app=zj:msg=" +
		base64.StdEncoding.EncodeToString([]byte("exited with status 3")) + "\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
}

func TestEmitKindWithoutMsg(t *testing.T) {
	f := newFixture(t)
	Emit("pi", "blocked", "", "auth")
	want := "\x1b]7501;state=blocked:app=pi:kind=auth\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
}

func TestEmitFallbackOnWriteFailure(t *testing.T) {
	f := newFixture(t)
	TtyWriter = failingWriter{}
	Emit("zj", "idle", "", "")
	want := "\x1b]7501;state=idle:app=zj\x1b\\"
	if got := f.out.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if f.tty.Len() != 0 {
		t.Errorf("tty = %q, want empty", f.tty.String())
	}
}

func TestEmitNilTtyWriter(t *testing.T) {
	f := newFixture(t)
	TtyWriter = nil
	Emit("zj", "working", "", "")
	want := "\x1b]7501;state=working:app=zj\x1b\\"
	if got := f.out.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// The default TtyWriter writes the device at TtyPath and closes it
// again. The seam redirects TtyPath to a temp file; /dev/tty is never
// opened.
func TestDefaultTtyDeviceWritesDevice(t *testing.T) {
	f := newFixture(t)
	dev := filepath.Join(t.TempDir(), "tty")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath := TtyPath
	TtyPath = dev
	t.Cleanup(func() { TtyPath = oldPath })
	TtyWriter = ttyDevice{}
	Emit("zj", "working", "", "")
	data, err := os.ReadFile(dev)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "\x1b]7501;state=working:app=zj\x1b\\" {
		t.Errorf("device = %q", got)
	}
	if f.out.Len() != 0 {
		t.Errorf("stdout fallback = %q, want empty", f.out.String())
	}
}

// An unopenable device (missing path, O_WRONLY) makes the default
// TtyWriter fail and Emit fall back to Out.
func TestDefaultTtyDeviceFallback(t *testing.T) {
	f := newFixture(t)
	oldPath := TtyPath
	TtyPath = filepath.Join(t.TempDir(), "missing-tty")
	t.Cleanup(func() { TtyPath = oldPath })
	TtyWriter = ttyDevice{}
	Emit("zj", "idle", "", "")
	want := "\x1b]7501;state=idle:app=zj\x1b\\"
	if got := f.out.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}
