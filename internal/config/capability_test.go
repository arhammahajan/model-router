package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func writeCapCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const capBase = `
[server]
auth_mode = "none"
[retry]
max_attempts = 1
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[[models]]
name = "a"
provider = "up"
`

func TestCapabilityOverridesValid(t *testing.T) {
	p := writeCapCfg(t, capBase+`
[capabilities.overrides.a.tools]
supported = true
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CapabilityOverrides["a"]["tools.supported"].Equal(capability.BoolValue(true)) {
		t.Fatalf("override not stored: %v", cfg.CapabilityOverrides)
	}
	// Unknown names validate without warnings.
	p2 := writeCapCfg(t, capBase+`
[capabilities.overrides.absent-model.tools]
supported = true
`)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg2.CapabilityOverrides["absent-model"]["tools.supported"].Equal(capability.BoolValue(true)) {
		t.Fatalf("unknown names must validate")
	}
	// Absent table means empty.
	p3 := writeCapCfg(t, capBase)
	cfg3, err := Load(p3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg3.CapabilityOverrides) != 0 {
		t.Fatalf("absent must be empty")
	}
}

func TestCapabilityOverridesReject(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unknown-field", "[capabilities.overrides.a.nope]\nsupported = true\n"},
		{"dotted-key", "[capabilities.overrides.a]\n\"tools.supported\" = true\n"},
		{"empty-object", "[capabilities.overrides.a]\n"},
		{"bad-value", "[capabilities.overrides.a.tools]\nsupported = \"yes\"\n"},
		{"bad-relation", "[capabilities.overrides.a.reasoning.budget]\nmin_tokens = 10\nmax_tokens = 5\n"},
		{"extra-table-key", "[capabilities]\nextra = 1\n"},
		{"untrimmed", "[capabilities.overrides.\" bad \".tools]\nsupported = true\n"},
	}
	for _, tc := range cases {
		p := writeCapCfg(t, capBase+tc.body)
		_, err := Load(p)
		if err == nil {
			t.Fatalf("%s must reject", tc.name)
		}
	}
}

func TestCapabilityRejectsStayRejected(t *testing.T) {
	// The one admitted review source validates; every other
	// metadata_source value stays rejected (value-free).
	p := writeCapCfg(t, `
[server]
auth_mode = "none"
[retry]
max_attempts = 1
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[catalog]
url = "http://127.0.0.1:9/v1/models"
metadata_source = "opencode-go-v1"
[[catalog.rules]]
match = ".*"
provider = "up"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("admitted metadata_source must validate: %v", err)
	}
	if cfg.Catalog == nil || cfg.Catalog.MetadataSource != "opencode-go-v1" || !cfg.Catalog.HasReview {
		t.Fatalf("admitted source must be stored with validated asset: %+v", cfg.Catalog)
	}
	for _, tc := range []struct {
		name string
		bad  string
		can  string
	}{
		{"other name", "[catalog]\nurl = \"http://127.0.0.1:9/v1/models\"\nmetadata_source = \"other\"\n[[catalog.rules]]\nmatch = \".*\"\nprovider = \"up\"\n", "other"},
		{"empty string", "[catalog]\nurl = \"http://127.0.0.1:9/v1/models\"\nmetadata_source = \"\"\n[[catalog.rules]]\nmatch = \".*\"\nprovider = \"up\"\n", ""},
	} {
		pb := writeCapCfg(t, `
[server]
auth_mode = "none"
[retry]
max_attempts = 1
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
`+tc.bad)
		if _, err := Load(pb); err == nil {
			t.Fatalf("%s: metadata_source must stay rejected", tc.name)
		} else if got := err.Error(); !strings.Contains(got, "catalog.metadata_source") {
			t.Fatalf("%s: rejection must name the key path: %v", tc.name, err)
		}
	}
	// Wrong TOML type (non-string) also rejects without echoing a value.
	pb := writeCapCfg(t, `
[server]
auth_mode = "none"
[retry]
max_attempts = 1
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[catalog]
url = "http://127.0.0.1:9/v1/models"
metadata_source = 7
[[catalog.rules]]
match = ".*"
provider = "up"
`)
	if _, err := Load(pb); err == nil {
		t.Fatal("non-string metadata_source must be rejected")
	} else if !strings.Contains(err.Error(), "catalog.metadata_source") {
		t.Fatalf("wrong-type rejection must name the key path: %v", err)
	}
	// bearer true with zero effective keys is admitted (supersedes the
	// provisional blanket rejection); true with keys still fails.
	p2 := writeCapCfg(t, `
[server]
auth_mode = "none"
bearer_session_compat = true
[retry]
max_attempts = 1
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
`)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatalf("keyless bearer true must be admitted: %v", err)
	}
	if !cfg2.Server.BearerSessionCompat {
		t.Fatalf("flag must be stored true")
	}
}
