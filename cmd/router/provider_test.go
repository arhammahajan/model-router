package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/routercli/routerctl"
)

func TestProvidersCommandTableAndJSON(t *testing.T) {
	orig := routerFetchProviders
	t.Cleanup(func() { routerFetchProviders = orig })

	sample := []routerctl.ProviderInfo{
		{
			Name:              "cliproxyapi",
			Enabled:           true,
			Status:            "reachable",
			Mode:              "embedded",
			BaseURL:           "http://127.0.0.1:8317/v1",
			Scheme:            "http",
			Host:              "127.0.0.1",
			Port:              8317,
			DefaultDialect:    "openai-chat",
			SupportedDialects: []string{"openai-chat", "openai-responses", "anthropic"},
			Compatibility:     "lossy",
			TimeoutSeconds:    600,
			AuthHeader:        "auto",
			APIKeyConfigured:  true,
		},
		{
			Name:              "llama_cpp",
			Enabled:           true,
			Status:            "unreachable",
			BaseURL:           "http://127.0.0.1:9931/v1",
			Scheme:            "http",
			Host:              "127.0.0.1",
			Port:              9931,
			DefaultDialect:    "openai-chat",
			SupportedDialects: []string{"openai-chat", "anthropic"},
			Compatibility:     "lossy",
			TimeoutSeconds:    600,
			AuthHeader:        "auto",
		},
		{
			Name:              "strata",
			Enabled:           false,
			Status:            "disabled",
			DefaultDialect:    "openai-chat",
			SupportedDialects: []string{"openai-chat", "anthropic"},
		},
	}

	routerFetchProviders = func(_ context.Context, _ routerctl.Options) ([]routerctl.ProviderInfo, error) {
		return sample, nil
	}

	code, stdout, _ := captureBoth(t, func() int { return run([]string{"providers"}) })
	if code != 0 ||
		!strings.Contains(stdout, "PROVIDER") ||
		!strings.Contains(stdout, "STATUS") ||
		!strings.Contains(stdout, "HOST:PORT") ||
		!strings.Contains(stdout, "DIALECTS") ||
		!strings.Contains(stdout, "COMPAT") ||
		!strings.Contains(stdout, "127.0.0.1:8317") ||
		!strings.Contains(stdout, "127.0.0.1:9931") ||
		!strings.Contains(stdout, "disabled") {
		t.Fatalf("providers = (%d, %q)", code, stdout)
	}

	code, stdout, _ = captureBoth(t, func() int { return run([]string{"providers", "--json"}) })
	if code != 0 {
		t.Fatalf("providers --json = %d", code)
	}
	var decoded []routerctl.ProviderInfo
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || len(decoded) != 3 {
		t.Fatalf("providers --json decode err=%v len=%d", err, len(decoded))
	}

	code, _, stderr := captureBoth(t, func() int { return run([]string{"providers", "extra"}) })
	if code != 2 || !strings.Contains(stderr, "no positional arguments") {
		t.Fatalf("providers extra = (%d, %q)", code, stderr)
	}
}
