package spaceresume

import (
	"strings"
	"testing"
)

func TestKdlUnescape(t *testing.T) {
	cases := map[string]string{
		`a\nb`: "a\nb", `a\tb`: "a\tb", `a\zb`: "azb",
		`\\`: "\\", `\"`: "\"", `\/`: "/",
		`a\b`: "a\bb"[0:1] + "\b"[0:1] + "b",
	}
	_ = cases
	if got := KdlUnescape(`a\nb`); got != "a\nb" {
		t.Errorf("got %q", got)
	}
	if got := KdlUnescape(`a\zb`); got != "azb" {
		t.Errorf("unknown escape = %q", got)
	}
	if got := KdlUnescape("trailing\\"); got != "trailing\\" {
		t.Errorf("trailing = %q", got)
	}
}

func TestParseKDLProperties(t *testing.T) {
	props := ParseKDLProperties(`pane command="claude" foo=bar {`)
	if props["command"] != "claude" || props["foo"] != "bar" {
		t.Errorf("props = %v", props)
	}
	props = ParseKDLProperties(`pane command="a \"q\" b"`)
	if props["command"] != `a "q" b` {
		t.Errorf("quoted = %v", props)
	}
}

func TestParseKDLStrings(t *testing.T) {
	got := ParseKDLStrings(`"__resume-wrap" "abc" plain "a b" "e\"s"`)
	want := []string{"__resume-wrap", "abc", "plain", "a b", `e"s`}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %q", got)
	}
	if got := ParseKDLStrings(`"a\nb"`); got[0] != "a\nb" {
		t.Errorf("escape = %q", got[0])
	}
}

func TestBraceDelta(t *testing.T) {
	if BraceDelta(`pane command="x" {`) != 1 {
		t.Error("open")
	}
	if BraceDelta(`command="{"`) != 0 {
		t.Error("brace in string ignored")
	}
	if BraceDelta(`}`) != -1 {
		t.Error("close")
	}
}

func TestRewriteLayout(t *testing.T) {
	inner := shlexJoin([]string{"claude", "--model", "opus"})
	encoded := b64Encode(inner)
	before := "layout {\n" +
		"  pane command=\"claude\" start_suspended=true {\n" +
		"      args \"--model\" \"opus\"\n" +
		"  }\n" +
		"  pane command=\"bash\" {\n" +
		"      args \"-c\" \"echo hi\"\n" +
		"  }\n" +
		"}\n"
	after := RewriteLayout(before)
	if strings.Contains(after, "start_suspended") {
		t.Errorf("start_suspended remains:\n%s", after)
	}
	if !strings.Contains(after, `command="zj"`) {
		t.Errorf("no zj rewrite:\n%s", after)
	}
	if !strings.Contains(after, `args "__resume-wrap" "`+encoded+`"`) {
		t.Errorf("no resume-wrap args:\n%s", after)
	}
	if !strings.Contains(after, `command="bash"`) {
		t.Errorf("non-agent pane changed:\n%s", after)
	}
	// Idempotent on non-pane text.
	plain := "tab name=\"x\" {\n}\n"
	if RewriteLayout(plain) != plain {
		t.Errorf("plain changed: %q", RewriteLayout(plain))
	}
}

func TestRewriteLayoutNonInteractiveSkipped(t *testing.T) {
	before := "pane command=\"claude\" {\n    args \"-p\" \"question\"\n}\n"
	if got := RewriteLayout(before); got != before {
		t.Errorf("headless pane rewritten:\n%s", got)
	}
}
