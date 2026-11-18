// Focused [llama_cpp] validation: URL/name/header/credential trust,
// generated inventory, and the deferred-enrichment rejections.
package config

import (
	"os"
	"strings"
	"testing"
)

func llamaEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LLAMA_SERVER_URL", "")
	t.Setenv("TEST_LLAMA_KEY", "secret-key")
	os.Unsetenv("LLAMA_SERVER_URL_EXTRA")
}

func loadLlama(t *testing.T, body string) *Config {
	t.Helper()
	llamaEnv(t)
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Llama == nil {
		t.Fatal("expected [llama_cpp] to parse")
	}
	return cfg
}

func rejectLlama(t *testing.T, body string) error {
	t.Helper()
	llamaEnv(t)
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatalf("expected rejection:\n%s", body)
	}
	return err
}

const llamaBase = `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
`

func TestLlamaURLTrustBoundary(t *testing.T) {
	for _, bad := range []string{
		"ftp://127.0.0.1:8080",
		"http://user:pass@127.0.0.1:8080",
		"http://127.0.0.1:8080/v1/models?key=x",
		"http://127.0.0.1:8080#frag",
		"http://127.0.0.1:8080/?x=1",
		"not-a-url",
		"",
	} {
		rejectLlama(t, llamaBase+`
[llama_cpp]
servers = ["`+bad+`"]
`)
	}
}

func TestLlamaPortsAndTrailingSlash(t *testing.T) {
	cfg := loadLlama(t, llamaBase+`
[llama_cpp]
servers = ["http://127.0.0.1:9931/"]
ports = [8080]
`)
	if len(cfg.Llama.Servers) != 2 {
		t.Fatalf("servers=%+v", cfg.Llama.Servers)
	}
	if cfg.Llama.Servers[0].URL != "http://127.0.0.1:9931" {
		t.Fatalf("trailing slash not stripped: %q", cfg.Llama.Servers[0].URL)
	}
	if cfg.Llama.Servers[1].URL != "http://127.0.0.1:8080" {
		t.Fatalf("port not mapped to loopback: %q", cfg.Llama.Servers[1].URL)
	}
	for _, bad := range []string{"ports = [0]", "ports = [70000]", "ports = [\"x\"]", "ports = [8.5]"} {
		rejectLlama(t, llamaBase+"[llama_cpp]\n"+bad+"\n")
	}
}

func TestLlamaDuplicateURLValidatedBeforeDedup(t *testing.T) {
	// Same URL twice collapses to one ordered entry.
	cfg := loadLlama(t, llamaBase+`
[llama_cpp]
servers = ["http://127.0.0.1:9931", "http://127.0.0.1:9931/"]
`)
	if len(cfg.Llama.Servers) != 1 {
		t.Fatalf("dup URLs must dedup: %+v", cfg.Llama.Servers)
	}
	// Every duplicate is still validated: a bad name on the collapsed
	// repeat must reject, not vanish silently.
	rejectLlama(t, llamaBase+`
[llama_cpp]
servers = ["http://127.0.0.1:9931", { url = "http://127.0.0.1:9931", name = "bad/name" }]
`)
}

func TestLlamaEnvFallbackChain(t *testing.T) {
	t.Setenv("LLAMA_SERVER_URL", "http://127.0.0.1:1111;,http://127.0.0.1:2222,,http://127.0.0.1:1111")
	cfg, err := Load(writeCfg(t, llamaBase+"[llama_cpp]\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Llama.Servers) != 2 ||
		cfg.Llama.Servers[0].URL != "http://127.0.0.1:1111" ||
		cfg.Llama.Servers[1].URL != "http://127.0.0.1:2222" {
		t.Fatalf("env fallback order/dedup wrong: %+v", cfg.Llama.Servers)
	}
	// Unset env falls back to the conventional local port.
	os.Unsetenv("LLAMA_SERVER_URL")
	cfg, err = Load(writeCfg(t, llamaBase+"[llama_cpp]\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Llama.Servers) != 1 || cfg.Llama.Servers[0].URL != DefaultLlamaURL {
		t.Fatalf("default fallback wrong: %+v", cfg.Llama.Servers)
	}
}

func TestLlamaExplicitNamesReserved(t *testing.T) {
	// Collision with a static provider name rejects.
	err := rejectLlama(t, llamaBase+`
[llama_cpp]
servers = [{ url = "http://127.0.0.1:9931", name = "up" }]
`)
	if !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("wrong error: %v", err)
	}
	// Duplicate explicit names reject.
	rejectLlama(t, llamaBase+`
[llama_cpp]
servers = [
  { url = "http://127.0.0.1:9931", name = "a" },
  { url = "http://127.0.0.1:9932", name = "a" },
]
`)
	// Slash in an explicit name rejects.
	rejectLlama(t, llamaBase+`
[llama_cpp]
servers = [{ url = "http://127.0.0.1:9931", name = "a/b" }]
`)
}

func TestLlamaGeneratedNamesUnique(t *testing.T) {
	// Distinct URLs sanitizing to one base uniquify with -2.
	cfg := loadLlama(t, llamaBase+`
[llama_cpp]
servers = ["http://h:1/a-b", "http://h:1/a+b"]
`)
	names := []string{cfg.Llama.Servers[0].Name, cfg.Llama.Servers[1].Name}
	if names[0] != "llama-h-1-a-b" || names[1] != "llama-h-1-a-b-2" {
		t.Fatalf("generated names wrong: %q", names)
	}
	// Stripped names behave as absent.
	cfg = loadLlama(t, llamaBase+`
[llama_cpp]
servers = [{ url = "http://127.0.0.1:9931", name = "   " }]
`)
	if !strings.HasPrefix(cfg.Llama.Servers[0].Name, "llama-") {
		t.Fatalf("blank name must generate: %q", cfg.Llama.Servers[0].Name)
	}
}

func TestLlamaCompatibilityAndProviders(t *testing.T) {
	for _, tc := range []struct {
		compat string
		lossy  bool
	}{
		{"strict", false},
		{"lossy", true},
	} {
		cfg := loadLlama(t, llamaBase+`
[llama_cpp]
compatibility = "`+tc.compat+`"
`)
		provs := cfg.LlamaProviders()
		if len(provs) != 1 {
			t.Fatalf("providers=%d", len(provs))
		}
		for name, p := range provs {
			if p.Dialect != DialectOpenAIChat {
				t.Fatalf("dialect=%q", p.Dialect)
			}
			if p.Lossy != tc.lossy {
				t.Fatalf("lossy=%v want %v", p.Lossy, tc.lossy)
			}
			if p.Timeout != DefaultProviderTimeout {
				t.Fatalf("generation timeout must be the default, got %v", p.Timeout)
			}
			if p.LitellmProvider != "openai" {
				t.Fatalf("litellm_provider=%q", p.LitellmProvider)
			}
			if p.BaseURL != DefaultLlamaURL+"/v1" {
				t.Fatalf("base=%q", p.BaseURL)
			}
			if name != cfg.Llama.Servers[0].Name {
				t.Fatalf("provider key %q != server name", name)
			}
		}
	}
	rejectLlama(t, llamaBase+"[llama_cpp]\ncompatibility = \"nope\"\n")
	rejectLlama(t, llamaBase+"[llama_cpp]\ndrop_params = true\n")
	rejectLlama(t, llamaBase+"[llama_cpp]\nunknown_key = 1\n")
}

func TestLlamaV1SuffixAndHeaders(t *testing.T) {
	cfg := loadLlama(t, llamaBase+`
[llama_cpp]
headers = { X-Global = "g" }
servers = [{ url = "http://127.0.0.1:9931/v1", name = "s", headers = { X-Srv = "s" } }]
`)
	if cfg.Llama.Servers[0].Headers["X-Global"] != "g" || cfg.Llama.Servers[0].Headers["X-Srv"] != "s" {
		t.Fatalf("header merge wrong: %+v", cfg.Llama.Servers[0].Headers)
	}
	provs := cfg.LlamaProviders()
	if provs["s"].BaseURL != "http://127.0.0.1:9931/v1" {
		t.Fatalf("v1 suffix doubled: %q", provs["s"].BaseURL)
	}
	if provs["s"].Headers["X-Global"] != "g" {
		t.Fatal("global headers must also be generation headers")
	}
}

func TestLlamaCredentialSelection(t *testing.T) {
	t.Setenv("TEST_LLAMA_KEY", "secret-key")
	// Server key wins over the global one.
	cfg := loadLlama(t, llamaBase+`
[llama_cpp]
api_key = "global-key"
servers = [{ url = "http://127.0.0.1:9931", name = "s", api_key_env = "TEST_LLAMA_KEY" }]
`)
	if cfg.Llama.Servers[0].APIKey != "secret-key" {
		t.Fatalf("server key must win: %q", cfg.Llama.Servers[0].APIKey)
	}
	// Without a server key the global key applies.
	cfg = loadLlama(t, llamaBase+`
[llama_cpp]
api_key = "global-key"
servers = ["http://127.0.0.1:9931"]
`)
	if cfg.Llama.Servers[0].APIKey != "global-key" {
		t.Fatalf("global key must inherit: %q", cfg.Llama.Servers[0].APIKey)
	}
	// Missing env names reject without echoing values.
	if err := rejectLlama(t, llamaBase+"[llama_cpp]\napi_key_env = \"LLAMA_MISSING_KEY_XYZ\"\n"); err == nil {
		t.Fatal("expected env error")
	}
}

func TestLlamaDeferredEnrichmentRejects(t *testing.T) {
	// Root migration (M4 Next): explicit fallback/overrides are now typed
	// config, no longer startup-rejected. Valid values admit; empty stays
	// an exact no-op.
	cfg := loadLlama(t, llamaBase+"[llama_cpp]\ncontext_fallback = 128000\n")
	if cfg.Llama.ContextFallback == nil || *cfg.Llama.ContextFallback != 128000 {
		t.Fatalf("fallback must admit: %+v", cfg.Llama)
	}
	cfg = loadLlama(t, llamaBase+"[llama_cpp]\nlimit_overrides = { m = { context_window = 10 } }\n")
	if cfg.Llama.LimitOverrides["m"]["context_window"] == nil {
		t.Fatalf("overrides must admit: %+v", cfg.Llama.LimitOverrides)
	}
	// An empty overrides table is an exact no-op and is admitted.
	cfg = loadLlama(t, llamaBase+"[llama_cpp]\nlimit_overrides = {}\n")
	if cfg.Llama == nil {
		t.Fatal("empty overrides must be admitted")
	}
}

func TestLlamaRefreshZeroAndBounds(t *testing.T) {
	cfg := loadLlama(t, llamaBase+"[llama_cpp]\nrefresh_seconds = 0\n")
	if cfg.Llama.RefreshSeconds != 0 {
		t.Fatalf("refresh=%v", cfg.Llama.RefreshSeconds)
	}
	rejectLlama(t, llamaBase+"[llama_cpp]\ntimeout = -1\n")
	rejectLlama(t, llamaBase+"[llama_cpp]\ntimeout = 0\n")
}

func TestLlamaOnlyZeroProviders(t *testing.T) {
	llamaEnv(t)
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false
[retry]
max_attempts = 1
[llama_cpp]
servers = ["http://127.0.0.1:9931"]
`
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("llama-only must load with zero providers: %v", err)
	}
	if len(cfg.Providers) != 0 || len(cfg.LlamaProviders()) != 1 {
		t.Fatalf("providers=%d llama=%d", len(cfg.Providers), len(cfg.LlamaProviders()))
	}
}
