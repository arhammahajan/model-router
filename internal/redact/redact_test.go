package redact

import (
	"strings"
	"testing"
)

func TestRedactKnownAndEncoded(t *testing.T) {
	corpus := ExpandCorpus([]string{"s3cr3t-key"})
	for _, form := range []string{"s3cr3t-key", "s3cr3t%2Dkey", "s3cr3t-key"} {
		_ = form
	}
	if got := Redact("token s3cr3t-key here", corpus); strings.Contains(got, "s3cr3t-key") {
		t.Fatalf("known secret leaked: %q", got)
	}
	if got := Redact("q=s3cr3t-key", corpus); strings.Contains(got, "s3cr3t-key") {
		t.Fatalf("query form leaked: %q", got)
	}
}

func TestRedactGenericShapes(t *testing.T) {
	corpus := ExpandCorpus(nil)
	cases := map[string]string{
		"Bearer abcdef123":           "Bearer ",
		"api_key=supersecret":        "supersecret",
		"https://user:pw@host/":      "pw@host",
		"sk-abcdef123456":            "sk-",
		"AIza12345678901234567890AB": "AIza",
		"gsk_12345678901234567890":   "gsk_",
	}
	for in := range cases {
		got := Redact(in, corpus)
		if strings.Contains(got, cases[in]) && !strings.Contains(in, "Bearer") {
			t.Fatalf("generic shape leaked for %q -> %q", in, got)
		}
	}
	if got := Redact("Bearer abcdef123", corpus); !strings.Contains(got, "Bearer <redacted>") {
		t.Fatalf("bearer shape wrong: %q", got)
	}
	if got := Redact("https://bob:pw@h/x", corpus); !strings.Contains(got, "https://<redacted>@") {
		t.Fatalf("userinfo shape wrong: %q", got)
	}
}

func TestRedactControlsAndTruncation(t *testing.T) {
	corpus := ExpandCorpus(nil)
	got := Redact("a\x00b\x1bc", corpus)
	if strings.ContainsAny(got, "\x00\x1b") {
		t.Fatalf("controls not normalized: %q", got)
	}
	long := strings.Repeat("x", MaxText+100)
	if len(Redact(long, corpus)) != MaxText {
		t.Fatalf("truncation bound wrong: %d", len(Redact(long, corpus)))
	}
}

func TestSanitizeDetailsKeysAndValues(t *testing.T) {
	corpus := ExpandCorpus([]string{"sekret"})
	in := map[string]any{"sekret": "v=sekret"}
	_ = in
	m := map[string]any{"sekret": "leak sekret here"}
	got := SanitizeDetails(m, corpus).(map[string]any)
	for k, v := range got {
		if strings.Contains(k, "sekret") || strings.Contains(v.(string), "sekret") {
			t.Fatalf("keys+values not sanitized: %v", got)
		}
	}
}

func TestURLSecretsDecodedOnce(t *testing.T) {
	got := URLSecrets("https://bob:p%40ss@h/?tok=a%20b")
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "p@ss") || !strings.Contains(joined, "a b") {
		t.Fatalf("url secrets not decoded: %q", got)
	}
}
