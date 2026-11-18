package config

// Independent bounded adversarial review: JSON/CLI config surface.
//
// Scope: LoadWithOptions env-file ordering and isolation, keyless compat
// admission, strict JSON numbers/bools, opaque metadata overflow, null
// sparse-override withdraw, single-object/EOF/UTF8, suffix dispatch and
// value-free errors. Stable source: git show HEAD:model_router/config.py.
// No production edits; hermetic tempdirs and controlled HOME/XDG only.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const jrBaseTOML = "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"

const jrBaseJSON = `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`

func TestJSONReviewExplicitBeatsDefaultFirstWins(t *testing.T) {
	isolateEnv(t)
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "ai"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "ai", "credentials.env"), []byte("TEST_JR_ORDER=from-default\nTEST_JR_ONLY_DEFAULT=dv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := t.TempDir()
	f1 := writeLoadFile(t, dir, "a.env", "TEST_JR_ORDER=first\nTEST_JR_ONLY_EXPLICIT=ev\n")
	f2 := writeLoadFile(t, dir, "b.env", "TEST_JR_ORDER=second\nTEST_JR_SECOND=sv\n")
	body := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\napi_key_env = \"TEST_JR_ORDER\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	p := writeLoadFile(t, dir, "router.toml", body)
	os.Unsetenv("TEST_JR_ORDER")
	cfg, err := LoadWithOptions(p, LoadOptions{EnvFiles: []string{f1, f2}})
	if err != nil {
		t.Fatalf("explicit+default load: %v", err)
	}
	if cfg.Providers["up"].APIKey != "first" {
		t.Fatalf("explicit must beat default and first file must win, got %q", cfg.Providers["up"].APIKey)
	}
	if os.Getenv("TEST_JR_ONLY_DEFAULT") != "dv" || os.Getenv("TEST_JR_SECOND") != "sv" {
		t.Fatalf("both layers must contribute disjoint keys")
	}
	// Present-empty in the process still wins over explicit files.
	os.Unsetenv("TEST_JR_ORDER")
	t.Setenv("TEST_JR_ORDER", "")
	if _, err := LoadWithOptions(p, LoadOptions{EnvFiles: []string{f1}}); err == nil {
		t.Fatalf("present-empty must win, expected missing-key error")
	} else if !strings.Contains(err.Error(), "TEST_JR_ORDER") {
		t.Fatalf("error must name the variable: %v", err)
	}
}

func TestJSONReviewNoDefaultEnvKeepsExplicit(t *testing.T) {
	isolateEnv(t)
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "ai"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "ai", "credentials.env"), []byte("TEST_JR_ISO=from-default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := t.TempDir()
	ef := writeLoadFile(t, dir, "e.env", "TEST_JR_EXPLICIT=yes-explicit\n")
	body := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\napi_key_env = \"TEST_JR_EXPLICIT\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	p := writeLoadFile(t, dir, "router.toml", body)
	os.Unsetenv("TEST_JR_ISO")
	os.Unsetenv("TEST_JR_EXPLICIT")
	cfg, err := LoadWithOptions(p, LoadOptions{NoDefaultEnv: true, EnvFiles: []string{ef}})
	if err != nil {
		t.Fatalf("explicit must still load under NoDefaultEnv: %v", err)
	}
	if cfg.Providers["up"].APIKey != "yes-explicit" {
		t.Fatalf("explicit value lost: %q", cfg.Providers["up"].APIKey)
	}
	if _, present := os.LookupEnv("TEST_JR_ISO"); present {
		t.Fatalf("NoDefaultEnv must not load the default file")
	}
}

func TestJSONReviewCompatKeylessAdmission(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	compat := func(extra string) string {
		return "[server]\nauth_mode = \"none\"\nbearer_session_compat = true\n" + extra + "[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	}
	// Keyless compat is admitted by the file layer (CLI --api-key rejection
	// is proven in the cmd review).
	p := writeLoadFile(t, dir, "ok.toml", compat(""))
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("keyless compat must load: %v", err)
	}
	if !cfg.Server.BearerSessionCompat || len(cfg.Server.APIKeys) != 0 {
		t.Fatalf("compat state wrong: %+v", cfg.Server)
	}
	// Configured keys with compat is a startup error, value-free.
	bad := "[server]\nauth_mode = \"required\"\napi_keys = [\"sekrit-1\"]\nbearer_session_compat = true\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	p2 := writeLoadFile(t, dir, "bad.toml", bad)
	if _, err := Load(p2); err == nil {
		t.Fatalf("compat with keys must be rejected")
	} else if !strings.Contains(err.Error(), "bearer_session_compat") || strings.Contains(err.Error(), "sekrit-1") {
		t.Fatalf("compat error must name the boundary without the secret: %v", err)
	}
	// Ambient client keys trip the same boundary when auth is inferred.
	t.Setenv("MODEL_ROUTER_API_KEY", "ambient-key-canary")
	p3 := writeLoadFile(t, dir, "amb.toml", "[server]\nbearer_session_compat = true\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n")
	if _, err := Load(p3); err == nil {
		t.Fatalf("ambient keys must trip compat under inferred auth")
	} else if strings.Contains(err.Error(), "ambient-key-canary") {
		t.Fatalf("compat error leaked ambient key: %v", err)
	}
	// Ambient keys are ignored under explicit auth_mode = "none".
	t.Setenv("MODEL_ROUTER_API_KEY", "ambient-ignored")
	p4 := writeLoadFile(t, dir, "none.toml", compat(""))
	cfg4, err := Load(p4)
	if err != nil {
		t.Fatalf("auth none must ignore ambient keys: %v", err)
	}
	if len(cfg4.Server.APIKeys) != 0 {
		t.Fatalf("auth none must not absorb ambient keys: %v", cfg4.Server.APIKeys)
	}
}

func TestJSONReviewExponentAndBoolStrict(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	// Exponent form is a float: rejected for integer fields, admitted for
	// finite-positive float fields.
	expPort := strings.Replace(jrBaseJSON, `"auth_mode": "none"`, `"auth_mode": "none", "port": 1e3`, 1)
	if _, err := Load(writeLoadFile(t, dir, "exp.json", expPort)); err == nil {
		t.Fatalf("exponent port must be rejected as non-integer")
	}
	expAttempts := `{"server": {"auth_mode": "none"}, "retry": {"max_attempts": 2e0}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`
	if _, err := Load(writeLoadFile(t, dir, "expa.json", expAttempts)); err == nil {
		t.Fatalf("exponent max_attempts must be rejected as non-integer")
	}
	expTimeout := `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com", "timeout": 1e1}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`
	cfg, err := Load(writeLoadFile(t, dir, "expt.json", expTimeout))
	if err != nil {
		t.Fatalf("exponent timeout is a finite float and must load: %v", err)
	}
	if cfg.Providers["up"].Timeout != 10 {
		t.Fatalf("exponent timeout value wrong: %v", cfg.Providers["up"].Timeout)
	}
	// Bool/int cross-satisfaction is rejected both ways.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"intAsBool", `{"server": {"auth_mode": "none", "allow_dynamic_routes": 1}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`},
		{"boolAsInt", `{"server": {"auth_mode": "none", "port": true}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`},
		{"stringAsBool", `{"server": {"auth_mode": "none", "allow_dynamic_routes": "true"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`},
	} {
		if _, err := Load(writeLoadFile(t, dir, tc.name+".json", tc.body)); err == nil {
			t.Fatalf("%s must be rejected", tc.name)
		}
	}
}

func TestJSONReviewMetadataOverflowOpaqueExact(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	const huge = "99999999999999999999999999"
	body := `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x", "metadata": {"big": ` + huge + `, "note": "n"}}]}`
	cfg, err := Load(writeLoadFile(t, dir, "meta.json", body))
	if err != nil {
		t.Fatalf("metadata overflow must stay opaque and load: %v", err)
	}
	m, _, ok := cfg.LookupModel("m")
	if !ok {
		t.Fatalf("lookup m")
	}
	if !strings.Contains(string(m.Metadata), huge) {
		t.Fatalf("overflow digits must be exact, got %s", m.Metadata)
	}
	// Same digits outside metadata are a strict business-rule error,
	// value-free (no float round-trip into a port).
	bad := `{"server": {"auth_mode": "none", "port": ` + huge + `}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`
	if _, err := Load(writeLoadFile(t, dir, "bad.json", bad)); err == nil {
		t.Fatalf("non-metadata overflow must be rejected")
	} else if strings.Contains(err.Error(), huge) {
		t.Fatalf("overflow error must be value-free: %v", err)
	}
	// Oversized metadata is bounded without echoing its bytes.
	bigVal := strings.Repeat("q", 70*1024)
	p := writeLoadFile(t, dir, "big.toml", jrBaseTOML[:strings.Index(jrBaseTOML, "[[models]]")]+"[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n[models.metadata]\nk = \""+bigVal+"\"\n")
	if _, err := Load(p); err == nil {
		t.Fatalf("oversized metadata must be rejected")
	} else if strings.Contains(err.Error(), bigVal) {
		t.Fatalf("metadata bound error must not echo bytes")
	}
}

func TestJSONReviewNullWithdrawNoSilentRoute(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	// Null metadata withdraws to absent.
	body := `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x", "metadata": null}]}`
	cfg, err := Load(writeLoadFile(t, dir, "null.json", body))
	if err != nil {
		t.Fatalf("null metadata must withdraw: %v", err)
	}
	m, _, ok := cfg.LookupModel("m")
	if !ok || len(m.Metadata) != 0 {
		t.Fatalf("null metadata must leave absent metadata: %+v", m)
	}
	// Unknown override names validate but never create routes.
	body2 := `{"server": {"auth_mode": "none"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}], "capabilities": {"overrides": {"ghost-route": {"reasoning": {"supported": true}}}}}`
	cfg2, err := Load(writeLoadFile(t, dir, "ghost.json", body2))
	if err != nil {
		t.Fatalf("unknown override names must validate: %v", err)
	}
	if _, _, ok := cfg2.LookupModel("ghost-route"); ok {
		t.Fatalf("override must not create a silent route")
	}
	if len(cfg2.Models) != 1 {
		t.Fatalf("override must not append models: %d", len(cfg2.Models))
	}
}

func TestJSONReviewSingleObjectEOFUTF8(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"array", `[]`},
		{"scalar", `"just-a-string"`},
		{"number", `42`},
		{"empty", ``},
		{"blank", "  \n\t "},
		{"twoObjects", jrBaseJSON + "\n" + jrBaseJSON},
		{"trailingArray", jrBaseJSON + " [1]"},
	} {
		if _, err := Load(writeLoadFile(t, dir, tc.name+".json", tc.body)); err == nil {
			t.Fatalf("%s must be rejected as non-single-object", tc.name)
		}
	}
	// Invalid UTF-8 is malformed JSON, value-free.
	raw := append([]byte(`{"server": {"auth_mode": "`), 0xff, 0xfe)
	raw = append(raw, []byte(`"}}`)...)
	p := filepath.Join(dir, "utf8.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatalf("invalid UTF-8 must be rejected")
	}
	// Trailing whitespace/newlines after one object are fine.
	cfg, err := Load(writeLoadFile(t, dir, "ws.json", jrBaseJSON+"\n  \n"))
	if err != nil {
		t.Fatalf("trailing whitespace must be accepted: %v", err)
	}
	if len(cfg.Models) != 1 {
		t.Fatalf("ws doc models wrong")
	}
}

func TestJSONReviewSuffixDispatchNoYAMLHalfParse(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	tomlBody := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	// Unknown suffix tries JSON then TOML.
	if _, err := Load(writeLoadFile(t, dir, "router.cfg", jrBaseJSON)); err != nil {
		t.Fatalf("unknown suffix JSON must load: %v", err)
	}
	if _, err := Load(writeLoadFile(t, dir, "router2.nonsense", tomlBody)); err != nil {
		t.Fatalf("unknown suffix TOML fallback must load: %v", err)
	}
	// Suffix fold is case-insensitive for the known families.
	if _, err := Load(writeLoadFile(t, dir, "R.JSON", jrBaseJSON)); err != nil {
		t.Fatalf("casefold .JSON: %v", err)
	}
	if _, err := Load(writeLoadFile(t, dir, "R.Toml", tomlBody)); err != nil {
		t.Fatalf("casefold .Toml: %v", err)
	}
	// YAML suffix is always an explicit migration error, even when the
	// payload is valid TOML/JSON (no half parse).
	for _, n := range []string{"r.yaml", "R.YML"} {
		_, err := Load(writeLoadFile(t, dir, n, tomlBody))
		if err == nil || !strings.Contains(err.Error(), "use TOML/JSON") {
			t.Fatalf("%s must be a YAML migration error, got %v", n, err)
		}
	}
	if _, err := Load(writeLoadFile(t, dir, "j.yaml", jrBaseJSON)); err == nil {
		t.Fatalf("YAML suffix must not half-parse JSON")
	}
}

func TestJSONReviewMalformedCanaryLeakFree(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	const canary = "canary-9f27-secret-value"
	// Malformed JSON carrying a secret must not echo it.
	bad := `{"server": {"auth_mode": "` + canary + `"`
	if _, err := Load(writeLoadFile(t, dir, "bad.json", bad)); err == nil {
		t.Fatalf("malformed JSON must fail")
	} else if strings.Contains(err.Error(), canary) {
		t.Fatalf("malformed error leaked canary: %v", err)
	}
	// Unknown schema keys name the path, never the value.
	bad2 := `{"server": {"auth_mode": "none", "zzz_unknown": "` + canary + `"}, "providers": {"up": {"dialect": "anthropic", "base_url": "https://api.anthropic.com"}}, "models": [{"name": "m", "provider": "up", "model": "x"}]}`
	if _, err := Load(writeLoadFile(t, dir, "unk.json", bad2)); err == nil {
		t.Fatalf("unknown key must fail")
	} else if strings.Contains(err.Error(), canary) {
		t.Fatalf("unknown-key error leaked value: %v", err)
	}
	// Missing credential env names the variable, and a present file value
	// never appears in the follow-on error.
	body := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\napi_key_env = \"TEST_JR_MISSING_VAR\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	os.Unsetenv("TEST_JR_MISSING_VAR")
	if _, err := Load(writeLoadFile(t, dir, "miss.toml", body)); err == nil {
		t.Fatalf("missing env must fail")
	} else if !strings.Contains(err.Error(), "TEST_JR_MISSING_VAR") {
		t.Fatalf("missing env must name the variable: %v", err)
	}
}

func TestJSONReviewLookupAndPrivateMetadata(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	body := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\naliases = [\"alias-a\"]\n[models.metadata]\nowner = \"team\"\n"
	cfg, err := Load(writeLoadFile(t, dir, "r.toml", body))
	if err != nil {
		t.Fatal(err)
	}
	m, p, ok := cfg.LookupModel("alias-a")
	if !ok || m.Name != "m" || p.Name != "up" {
		t.Fatalf("alias lookup must resolve to the canonical route")
	}
	if _, _, ok := cfg.LookupModel("no-such-route"); ok {
		t.Fatalf("unknown lookup must be false")
	}
	if !strings.Contains(string(m.Metadata), "team") {
		t.Fatalf("private metadata must be retained verbatim: %s", m.Metadata)
	}
	// Disabled routes stay in the index (reserved) but carry the flag.
	body2 := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\nenabled = false\n"
	cfg2, err := Load(writeLoadFile(t, dir, "d.toml", body2))
	if err != nil {
		t.Fatal(err)
	}
	m2, _, ok := cfg2.LookupModel("m")
	if !ok || !m2.Disabled {
		t.Fatalf("disabled route must stay indexed with the flag: %+v", m2)
	}
}

func TestJSONReviewEnvExpansionStaysInValue(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	// Quotes/newlines smuggled via ${} expand inside one string value and
	// can never alter the document shape (business rules run after parse).
	t.Setenv("TEST_JR_INJECT", "x\", evil = 1")
	body := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\nheaders = { x-note = \"${TEST_JR_INJECT}\" }\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	cfg, err := Load(writeLoadFile(t, dir, "inj.toml", body))
	if err != nil {
		t.Fatalf("injection string must stay a value: %v", err)
	}
	if got := cfg.Providers["up"].Headers["x-note"]; got != "x\", evil = 1" {
		t.Fatalf("expanded value wrong: %q", got)
	}
	// Missing ${} is a named, value-free error.
	body2 := "[server]\nauth_mode = \"none\"\n[providers.up]\ndialect = \"anthropic\"\nbase_url = \"https://api.anthropic.com\"\nheaders = { x-note = \"${TEST_JR_DEFINITELY_MISSING}\" }\n[[models]]\nname = \"m\"\nprovider = \"up\"\nmodel = \"x\"\n"
	os.Unsetenv("TEST_JR_DEFINITELY_MISSING")
	if _, err := Load(writeLoadFile(t, dir, "miss2.toml", body2)); err == nil {
		t.Fatalf("missing expansion must fail")
	} else if !strings.Contains(err.Error(), "TEST_JR_DEFINITELY_MISSING") {
		t.Fatalf("expansion error must name the variable: %v", err)
	}
}
