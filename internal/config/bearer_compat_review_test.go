package config

// Adversarial F1 review: none ignores env, required guards, compat needs
// zero effective keys, bool strictness, order/dedup. Hermetic: temp HOME/XDG,
// cleared ingress-auth env, no network.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBearerCompatReviewCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func bearerCompatBaseMinimal(extra string) string {
	return `[server]
auth_mode = "none"
allow_dynamic_routes = false
` + extra + `
[retry]
max_attempts = 1
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "up-secret"
[[models]]
name = "m"
provider = "up"
model = "x"
`
}

func TestBearerCompatReviewNoneIgnoresEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MODEL_ROUTER_API_KEYS", "env-a, env-b")
	t.Setenv("MODEL_ROUTER_API_KEY", "env-single")
	cfg, err := Load(writeBearerCompatReviewCfg(t, bearerCompatBaseMinimal("bearer_session_compat = true")))
	if err != nil {
		t.Fatalf("none + env + compat true must be admitted (env ignored): %v", err)
	}
	if len(cfg.Server.APIKeys) != 0 {
		t.Fatalf("none must ignore env keys: %q", cfg.Server.APIKeys)
	}
	if !cfg.Server.BearerSessionCompat {
		t.Fatalf("flag must be stored")
	}
}

func TestBearerCompatReviewEnvPluralFailsValueFree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MODEL_ROUTER_API_KEYS", "sekret-a, sekret-b")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	body := strings.Replace(bearerCompatBaseMinimal("bearer_session_compat = true"), "auth_mode = \"none\"\n", "", 1)
	_, err := Load(writeBearerCompatReviewCfg(t, body))
	if err == nil {
		t.Fatalf("compat true + plural env keys must fail")
	}
	if strings.Contains(err.Error(), "sekret-a") || strings.Contains(err.Error(), "sekret-b") {
		t.Fatalf("startup failure must be value-free: %v", err)
	}
}

func TestBearerCompatReviewRequiredGuards(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	body := "[server]\nauth_mode = \"required\"\nallow_dynamic_routes = false\n[retry]\nmax_attempts = 1\n[providers.up]\ndialect = \"openai-chat\"\nbase_url = \"http://127.0.0.1:9\"\napi_key = \"up-secret\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	if _, err := Load(writeBearerCompatReviewCfg(t, body)); err == nil {
		t.Fatalf("required with zero effective keys must fail")
	}
	// required + one env key passes (compat off).
	t.Setenv("MODEL_ROUTER_API_KEY", "env-ok")
	if _, err := Load(writeBearerCompatReviewCfg(t, body)); err != nil {
		t.Fatalf("required + env key must pass when compat off: %v", err)
	}
}

func TestBearerCompatReviewBoolStrict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	for _, bad := range []string{"bearer_session_compat = 1", "bearer_session_compat = \"true\"", "bearer_session_compat = \"yes\""} {
		if _, err := Load(writeBearerCompatReviewCfg(t, bearerCompatBaseMinimal(bad))); err == nil {
			t.Fatalf("%s must fail bool check", bad)
		}
	}
}

func TestBearerCompatReviewDedupOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MODEL_ROUTER_API_KEYS", "b, a, b")
	t.Setenv("MODEL_ROUTER_API_KEY", "a")
	body := "[server]\nallow_dynamic_routes = false\n[retry]\nmax_attempts = 1\n[providers.up]\ndialect = \"openai-chat\"\nbase_url = \"http://127.0.0.1:9\"\napi_key = \"up-secret\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	cfg, err := Load(writeBearerCompatReviewCfg(t, body))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b", "a"}
	if len(cfg.Server.APIKeys) != 2 || cfg.Server.APIKeys[0] != want[0] || cfg.Server.APIKeys[1] != want[1] {
		t.Fatalf("order-preserved dedup: got %q want %q", cfg.Server.APIKeys, want)
	}
	if cfg.Server.AuthMode != "required" {
		t.Fatalf("omitted auth_mode with keys must infer required, got %q", cfg.Server.AuthMode)
	}
}
