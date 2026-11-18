// Review probes for the llama slice: duplicate-field validation,
// credential/header source coverage and numeric bounds. All cases are
// hermetic (no I/O, no env creds beyond t.Setenv).
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Duplicate URLs collapse keeping the first entry, but every duplicate is
// still validated: bad headers/credentials/names on a collapsed repeat
// must reject rather than vanish silently.
func TestLlamaReviewDuplicateFieldsValidated(t *testing.T) {
	base := llamaBase
	// Bad headers on the collapsed repeat reject.
	if err := rejectLlama(t, base+`
[llama_cpp]
servers = ["http://127.0.0.1:9931", { url = "http://127.0.0.1:9931", headers = { "Bad Name!" = "v" } }]
`); err == nil {
		t.Fatal("duplicate with bad headers must reject")
	}
	// Bad credential reference on the repeat rejects without echoing values.
	if err := rejectLlama(t, base+`
[llama_cpp]
servers = ["http://127.0.0.1:9931", { url = "http://127.0.0.1:9931", api_key_env = "LLAMA_MISSING_KEY_XYZ" }]
`); err == nil {
		t.Fatal("duplicate with bad credential must reject")
	}
	// Unknown server keys on the repeat reject.
	if err := rejectLlama(t, base+`
[llama_cpp]
servers = ["http://127.0.0.1:9931", { url = "http://127.0.0.1:9931", bogus = 1 }]
`); err == nil {
		t.Fatal("duplicate with unknown key must reject")
	}
	// First entry wins for key/headers when the URL repeats.
	// (loadLlama pins TEST_LLAMA_KEY=secret-key; the point is the first
	// duplicate's env-selected key wins over the repeat's literal key.)
	cfg := loadLlama(t, base+`
[llama_cpp]
api_key = "global-key"
headers = { X-G = "g" }
servers = [
  { url = "http://127.0.0.1:9931", name = "first", api_key_env = "TEST_LLAMA_KEY", headers = { X-S = "first" } },
  { url = "http://127.0.0.1:9931/", name = "second", api_key = "other", headers = { X-S = "second" } },
]
`)
	if len(cfg.Llama.Servers) != 1 {
		t.Fatalf("dup URLs must dedup: %+v", cfg.Llama.Servers)
	}
	got := cfg.Llama.Servers[0]
	if got.Name != "first" || got.APIKey != "secret-key" || got.Headers["X-S"] != "first" {
		t.Fatalf("first duplicate must win: %+v", got)
	}
}

// All credential sources resolve; per-server headers override global on
// conflict; static Authorization in headers is admitted at config (the
// probe scrubs it, generation keeps a single selected credential).
func TestLlamaReviewCredentialFileAndHeaderOverride(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("  file-key \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadLlama(t, llamaBase+`
[llama_cpp]
headers = { X-Dup = "global", Authorization = "static-bogus" }
servers = [{ url = "http://127.0.0.1:9931", name = "s", api_key_file = "`+keyFile+`", headers = { X-Dup = "server" } }]
`)
	srv := cfg.Llama.Servers[0]
	if srv.APIKey != "file-key" {
		t.Fatalf("api_key_file must resolve stripped: %q", srv.APIKey)
	}
	if srv.Headers["X-Dup"] != "server" {
		t.Fatalf("server header must win: %+v", srv.Headers)
	}
	provs := cfg.LlamaProviders()
	if provs["s"].Headers["X-Dup"] != "server" {
		t.Fatalf("generated provider must carry merged headers: %+v", provs["s"].Headers)
	}
	if provs["s"].APIKey != "file-key" {
		t.Fatalf("generated provider must carry selected key: %q", provs["s"].APIKey)
	}
}

// Numeric bounds: NaN/Inf timeouts reject, scalar ports admit, float ports
// reject, unknown server keys reject.
func TestLlamaReviewNumericBounds(t *testing.T) {
	for _, body := range []string{
		llamaBase + "[llama_cpp]\ntimeout = 1e400\n",
		llamaBase + "[llama_cpp]\nrefresh_seconds = 1e400\n",
	} {
		if err := rejectLlama(t, body); err == nil {
			t.Fatalf("non-finite bound must reject:\n%s", body)
		}
	}
	cfg := loadLlama(t, llamaBase+"[llama_cpp]\nports = 8080\n")
	if len(cfg.Llama.Servers) != 1 || cfg.Llama.Servers[0].URL != "http://127.0.0.1:8080" {
		t.Fatalf("scalar port must map to loopback: %+v", cfg.Llama.Servers)
	}
	if err := rejectLlama(t, llamaBase+"[llama_cpp]\nports = 8.5\n"); err == nil {
		t.Fatal("float scalar port must reject")
	}
	if err := rejectLlama(t, llamaBase+"[llama_cpp]\nservers = [{ url = \"http://127.0.0.1:9931\", limit_overrides = {} }]\n"); err == nil {
		t.Fatal("unknown per-server key must reject")
	}
	if !strings.HasPrefix(cfg.Llama.Servers[0].Name, "llama-") {
		t.Fatalf("generated name missing: %q", cfg.Llama.Servers[0].Name)
	}
}
