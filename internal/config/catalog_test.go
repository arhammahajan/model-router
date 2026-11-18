// Catalog config validation: the only accepted discovery table.
package config

import (
	"strings"
	"testing"
)

func catalogBase(url string) string {
	return minimal + "\n[catalog]\nurl = \"" + url + "\"\n" +
		"model_providers = {\"openai/gpt-4\" = \"up\"}\n"
}

func TestCatalogValidExact(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	cfg, err := Load(writeCfg(t, catalogBase("https://catalog.example/models")))
	if err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	if cfg.Catalog == nil {
		t.Fatal("catalog missing")
	}
	if cfg.Catalog.Timeout != DefaultCatalogTimeout || cfg.Catalog.RefreshSeconds != DefaultCatalogRefresh {
		t.Fatalf("defaults wrong: %+v", cfg.Catalog)
	}
	if cfg.Catalog.ModelProviders["openai/gpt-4"] != "up" {
		t.Fatalf("mapping lost: %+v", cfg.Catalog.ModelProviders)
	}
}

func TestCatalogValidRules(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := minimal + "\n[catalog]\nurl = \"http://127.0.0.1:9/v1/models\"\n" +
		"timeout = 5\nrefresh_seconds = 60\n" +
		"[[catalog.rules]]\nmatch = \"^openai/.*$\"\nprovider = \"up\"\n"
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("valid rules catalog rejected: %v", err)
	}
	if len(cfg.Catalog.Rules) != 1 {
		t.Fatalf("rules lost: %+v", cfg.Catalog)
	}
}

func TestCatalogInvalidTable(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad scheme", minimal + "\n[catalog]\nurl = \"ftp://x/y\"\nmodel_providers = {\"a\" = \"up\"}\n", "http"},
		{"missing url", minimal + "\n[catalog]\nmodel_providers = {\"a\" = \"up\"}\n", "url"},
		{"bad timeout zero", minimal + "\n[catalog]\nurl = \"http://x/y\"\ntimeout = 0\nmodel_providers = {\"a\" = \"up\"}\n", "timeout"},
		{"bad timeout negative", minimal + "\n[catalog]\nurl = \"http://x/y\"\ntimeout = -1\nmodel_providers = {\"a\" = \"up\"}\n", "timeout"},
		{"negative refresh", minimal + "\n[catalog]\nurl = \"http://x/y\"\nrefresh_seconds = -1\nmodel_providers = {\"a\" = \"up\"}\n", "refresh_seconds"},
		{"empty mapping and rules", minimal + "\n[catalog]\nurl = \"http://x/y\"\n", "at least one"},
		{"unknown rule provider", minimal + "\n[catalog]\nurl = \"http://x/y\"\n[[catalog.rules]]\nmatch = \"a\"\nprovider = \"nope\"\n", "unknown provider"},
		{"unknown exact provider", minimal + "\n[catalog]\nurl = \"http://x/y\"\nmodel_providers = {\"a\" = \"nope\"}\n", "unknown provider"},
		{"bad regex", minimal + "\n[catalog]\nurl = \"http://x/y\"\n[[catalog.rules]]\nmatch = \"(unclosed\"\nprovider = \"up\"\n", "regex"},
		{"lookaround rejected", minimal + "\n[catalog]\nurl = \"http://x/y\"\n[[catalog.rules]]\nmatch = \"(?=a)a\"\nprovider = \"up\"\n", "regex"},
		{"backref rejected", minimal + "\n[catalog]\nurl = \"http://x/y\"\n[[catalog.rules]]\nmatch = \"(a)\\\\1\"\nprovider = \"up\"\n", "regex"},
		{"shorthand rejected", minimal + "\n[catalog]\nurl = \"http://x/y\"\n[[catalog.rules]]\nmatch = \"\\\\d+\"\nprovider = \"up\"\n", "regex"},
		{"unknown key rejected", minimal + "\n[catalog]\nurl = \"http://x/y\"\nmodel_providers = {\"a\" = \"up\"}\nendpoint = \"x\"\n", "unsupported"},
		{"padded model id rejected", minimal + "\n[catalog]\nurl = \"http://x/y\"\nmodel_providers = {\" a\" = \"up\"}\n", "model IDs"},
	}
	for _, tc := range cases {
		if _, err := Load(writeCfg(t, tc.body)); err == nil {
			t.Fatalf("%s: must fail", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q must mention %q", tc.name, err, tc.want)
		}
	}
}

func TestCatalogRejectsValueFree(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	canary := "evil-canary-9f27"
	body := minimal + "\n[catalog]\nurl = \"http://x/y\"\n[[catalog.rules]]\nmatch = \"(?=" +
		canary + ")" + canary + "\"\nprovider = \"up\"\n"
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatal("non-portable rule must fail")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("rejection echoes rule text: %v", err)
	}
}

func TestCatalogAbsentByDefault(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	cfg, err := Load(writeCfg(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Catalog != nil {
		t.Fatalf("catalog must be nil when absent: %+v", cfg.Catalog)
	}
}

func TestCatalogRefreshZeroAccepted(t *testing.T) {
	t.Setenv("TEST_UP_KEY_M2", "dummy")
	body := minimal + "\n[catalog]\nurl = \"http://x/y\"\nrefresh_seconds = 0\nmodel_providers = {\"a\" = \"up\"}\n"
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("refresh 0 must be accepted: %v", err)
	}
	if cfg.Catalog.RefreshSeconds != 0 {
		t.Fatalf("refresh not kept: %+v", cfg.Catalog)
	}
}
