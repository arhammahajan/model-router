package config

// Focused session-policy normalization contracts: case handling, default
// and explicit source order, dedicated-header gating and the static ban.

import (
	"strings"
	"testing"
)

func loadBody(t *testing.T, providerLines string) (*Provider, error) {
	t.Helper()
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://api.anthropic.com"
` + providerLines + `
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		return nil, err
	}
	return cfg.Providers["up"], nil
}

func TestSessionHeaderCaseNormalized(t *testing.T) {
	p, err := loadBody(t, "session_header = \"X-Sess-Out\"\nsession_source_headers = [\"Session-Id\"]")
	if err != nil {
		t.Fatalf("accept mixed-case policy: %v", err)
	}
	if p.SessionHeader != "x-sess-out" || len(p.SessionSourceHeaders) != 1 || p.SessionSourceHeaders[0] != "session-id" {
		t.Fatalf("policy must be case-normalized: %+v", p)
	}
}

func TestSessionExplicitEmptyDisablesNative(t *testing.T) {
	p, err := loadBody(t, "session_header = \"x-sess\"\nsession_source_headers = []")
	if err != nil {
		t.Fatalf("explicit [] must be accepted: %v", err)
	}
	if p.SessionSourceHeaders == nil || len(p.SessionSourceHeaders) != 0 {
		t.Fatalf("explicit [] must stay empty (not defaulted): %#v", p.SessionSourceHeaders)
	}
}

func TestSessionSourceDuplicatesRejected(t *testing.T) {
	_, err := loadBody(t, "session_header = \"x-sess\"\nsession_source_headers = [\"session-id\", \"Session-Id\"]")
	if err == nil {
		t.Fatalf("duplicate sources (case variants) must fail")
	}
}

func TestSessionHeaderDedicatedOnly(t *testing.T) {
	for _, h := range []string{"authorization", "x-api-key", "connection", "content-length",
		"accept", "accept-encoding", "anthropic-beta", "anthropic-version", "openai-beta", "user-agent"} {
		if _, err := loadBody(t, "session_header = \""+h+"\""); err == nil {
			t.Fatalf("credential/transport/protocol header %q must not be a session_header", h)
		}
	}
}

func TestSessionStaticCustomDestinationRejected(t *testing.T) {
	_, err := loadBody(t, "session_header = \"x-sess\"\nheaders = { \"X-Sess\" = \"static\" }")
	if err == nil || !strings.Contains(err.Error(), "static session headers") {
		t.Fatalf("static custom destination must fail: %v", err)
	}
	_, err = loadBody(t, "session_header = \"x-sess\"\nheaders = { \"x-opencode-session\" = \"static\" }")
	if err == nil {
		t.Fatalf("static x-opencode-session must fail")
	}
}

func TestSessionPolicyKeysRequireHeader(t *testing.T) {
	for _, line := range []string{
		"session_prompt_cache_key = true",
		"session_missing = \"reject\"",
	} {
		if _, err := loadBody(t, line); err == nil {
			t.Fatalf("policy key without session_header must fail: %s", line)
		}
	}
}
