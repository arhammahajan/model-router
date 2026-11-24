package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Focused limit-policy admission: TOML valid, JSON null, all invalid
// spellings, losing/unknown names still validate startup value-free.

func writeJSONCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLlamaLimitsFallbackAdmits(t *testing.T) {
	llamaEnv(t)
	cfg, err := Load(writeCfg(t, llamaBase+"[providers.llama_cpp]\ncontext_fallback = 64000\n"))
	if err != nil {
		t.Fatalf("fallback must admit: %v", err)
	}
	if cfg.Llama.ContextFallback == nil || *cfg.Llama.ContextFallback != 64000 {
		t.Fatalf("fallback not stored: %+v", cfg.Llama)
	}
	// Absent stays nil (unknown, never the Python implicit default).
	cfg = loadLlama(t, llamaBase+"[providers.llama_cpp]\n")
	if cfg.Llama.ContextFallback != nil {
		t.Fatalf("absent fallback must stay nil, got %v", *cfg.Llama.ContextFallback)
	}
}

func TestLlamaLimitsFallbackRejects(t *testing.T) {
	for _, body := range []string{
		llamaBase + "[providers.llama_cpp]\ncontext_fallback = 0\n",
		llamaBase + "[providers.llama_cpp]\ncontext_fallback = -5\n",
		llamaBase + "[providers.llama_cpp]\ncontext_fallback = 1.5\n",
		llamaBase + "[providers.llama_cpp]\ncontext_fallback = true\n",
		llamaBase + "[providers.llama_cpp]\ncontext_fallback = \"big\"\n",
	} {
		if err := rejectLlama(t, body); err == nil {
			t.Fatalf("fallback must reject:\n%s", body)
		}
	}
}

func TestLlamaLimitsOverridesAdmit(t *testing.T) {
	llamaEnv(t)
	cfg, err := Load(writeCfg(t, llamaBase+`[providers.llama_cpp]
[providers.llama_cpp.limit_overrides.m1]
context_window = 4096
default_output_tokens = 0
`))
	if err != nil {
		t.Fatalf("overrides must admit: %v", err)
	}
	if got := *cfg.Llama.LimitOverrides["m1"]["context_window"]; got != 4096 {
		t.Fatalf("override not stored: %+v", cfg.Llama.LimitOverrides)
	}
	if got := *cfg.Llama.LimitOverrides["m1"]["default_output_tokens"]; got != 0 {
		t.Fatalf("zero default must admit: %+v", cfg.Llama.LimitOverrides)
	}
}

func TestLlamaLimitsOverridesReject(t *testing.T) {
	bads := []string{
		// unknown limit key
		llamaBase + "[providers.llama_cpp.limit_overrides.m]\nnope = 1\n",
		// zero non-default
		llamaBase + "[providers.llama_cpp.limit_overrides.m]\ncontext_window = 0\n",
		// negative default
		llamaBase + "[providers.llama_cpp.limit_overrides.m]\ndefault_output_tokens = -1\n",
		// bool/float/string coercion
		llamaBase + "[providers.llama_cpp.limit_overrides.m]\ncontext_window = true\n",
		llamaBase + "[providers.llama_cpp.limit_overrides.m]\ncontext_window = 1.5\n",
		llamaBase + "[providers.llama_cpp.limit_overrides.m]\ncontext_window = \"10\"\n",
		// non-table
		llamaBase + "[providers.llama_cpp]\nlimit_overrides = { m = 1 }\n",
		// bad model id
		llamaBase + "[providers.llama_cpp.limit_overrides.\" padded \"]\ncontext_window = 1\n",
	}
	for _, body := range bads {
		if err := rejectLlama(t, body); err == nil {
			t.Fatalf("override must reject:\n%s", body)
		}
	}
}

func TestLlamaLimitsJSONNullWithdraw(t *testing.T) {
	llamaEnv(t)
	body := `{"server":{"auth_mode":"none","allow_dynamic_routes":false},"retry":{"max_attempts":1},"providers":{"up":{"dialect":"anthropic","base_url":"http://127.0.0.1:9"},"llama_cpp":{"limit_overrides":{"m":{"context_window":null,"default_output_tokens":null}}}}}`
	cfg, err := Load(writeJSONCfg(t, body))
	if err != nil {
		t.Fatalf("JSON null must admit as explicit withdraw: %v", err)
	}
	inner, ok := cfg.Llama.LimitOverrides["m"]
	if !ok {
		t.Fatalf("missing model: %+v", cfg.Llama.LimitOverrides)
	}
	if v, has := inner["context_window"]; !has || v != nil {
		t.Fatalf("null must be explicit nil withdraw: %+v", inner)
	}
}

func TestLlamaLimitsLosersValidate(t *testing.T) {
	// Unknown/losing model names still validate value-free before any I/O.
	if err := rejectLlama(t, llamaBase+"[providers.llama_cpp.limit_overrides.unknown-loser]\ncontext_window = 0\n"); err == nil {
		t.Fatal("losing override with bad value must still reject")
	}
	// Valid losing override admits (publication filters later).
	cfg := loadLlama(t, llamaBase+"[providers.llama_cpp.limit_overrides.unknown-loser]\ncontext_window = 77\n")
	if *cfg.Llama.LimitOverrides["unknown-loser"]["context_window"] != 77 {
		t.Fatalf("losing override must be stored for publication filtering: %+v", cfg.Llama.LimitOverrides)
	}
}
