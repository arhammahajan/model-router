package attn

import (
	"encoding/base64"
	"testing"
)

func TestPrime(t *testing.T) {
	f := newFixture(t)
	Prime("claude-code")
	want := "\x1b]7501;state=working:app=claude-code\x1b\\" +
		"\x1b]7501;state=idle:app=claude-code\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
	if f.out.Len() != 0 {
		t.Errorf("stdout fallback = %q, want empty", f.out.String())
	}
}

func TestFinishExitZero(t *testing.T) {
	f := newFixture(t)
	Finish("claude-code", 0)
	want := "\x1b]7501;state=done:app=claude-code:msg=ZmluaXNoZWQ=\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
}

func TestFinishNonZero(t *testing.T) {
	f := newFixture(t)
	Finish("kimi", 3)
	want := "\x1b]7501;state=error:app=kimi:msg=" +
		base64.StdEncoding.EncodeToString([]byte("exited with status 3")) + "\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
}

func TestFinishFormatsCodeAsGiven(t *testing.T) {
	f := newFixture(t)
	// Signal-derived codes reach Finish already mapped (Wrap does
	// 128+signal); Finish formats the number it is given.
	Finish("zj", 137)
	want := "\x1b]7501;state=error:app=zj:msg=" +
		base64.StdEncoding.EncodeToString([]byte("exited with status 137")) + "\x1b\\"
	if got := f.tty.String(); got != want {
		t.Errorf("tty = %q, want %q", got, want)
	}
}
