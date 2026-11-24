package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/routercli/routerctl"
)

func TestProviderCommandListStatusAndJSON(t *testing.T) {
	origList := routerFetchProviders
	origOne := routerFetchProvider
	t.Cleanup(func() {
		routerFetchProviders = origList
		routerFetchProvider = origOne
	})

	sample := []routerctl.ProviderInfo{
		{
			Name:                "cliproxyapi",
			Enabled:             true,
			Status:              "reachable",
			Mode:                "embedded",
			BaseURL:             "http://127.0.0.1:8317/v1",
			Scheme:              "http",
			Host:                "127.0.0.1",
			Port:                8317,
			DefaultDialect:      "openai-chat",
			SupportedDialects:   []string{"openai-chat", "openai-responses", "anthropic"},
			Compatibility:       "lossy",
			TimeoutSeconds:      600,
			ProbeTimeoutSeconds: 30,
			AuthHeader:          "auto",
			APIKeyConfigured:    true,
		},
		{
			Name:                "llama_cpp",
			Enabled:             true,
			Status:              "unreachable",
			BaseURL:             "http://127.0.0.1:9931/v1",
			Scheme:              "http",
			Host:                "127.0.0.1",
			Port:                9931,
			DefaultDialect:      "openai-chat",
			SupportedDialects:   []string{"openai-chat", "anthropic"},
			Compatibility:       "lossy",
			TimeoutSeconds:      600,
			ProbeTimeoutSeconds: 1,
			AuthHeader:          "auto",
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
	routerFetchProvider = func(_ context.Context, _ routerctl.Options, name string) (routerctl.ProviderInfo, error) {
		canon := routerctl.NormalizeProviderName(name)
		for _, it := range sample {
			if it.Name == canon {
				return it, nil
			}
		}
		return routerctl.ProviderInfo{}, fmt.Errorf("unknown provider %q", name)
	}

	// Default list shows available built-in providers.
	code, stdout, _ := captureBoth(t, func() int { return run([]string{"provider"}) })
	if code != 0 || !strings.Contains(stdout, "PROVIDER") || !strings.Contains(stdout, "cliproxyapi") || !strings.Contains(stdout, "llama_cpp") || !strings.Contains(stdout, "strata") {
		t.Fatalf("provider list = (%d, %q)", code, stdout)
	}

	// Status table shows per-provider status and HTTP server host:port.
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"provider", "status"}) })
	if code != 0 || !strings.Contains(stdout, "HOST:PORT") || !strings.Contains(stdout, "127.0.0.1:8317") || !strings.Contains(stdout, "127.0.0.1:9931") || !strings.Contains(stdout, "disabled") {
		t.Fatalf("provider status = (%d, %q)", code, stdout)
	}

	// JSON list.
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"provider", "status", "--json"}) })
	if code != 0 {
		t.Fatalf("provider status --json = %d", code)
	}
	var decoded []routerctl.ProviderInfo
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || len(decoded) != 3 {
		t.Fatalf("provider status --json decode err=%v len=%d", err, len(decoded))
	}

	// Single provider detail via alias.
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"provider", "status", "cpa"}) })
	if code != 0 || !strings.Contains(stdout, "port:          8317") || !strings.Contains(stdout, "mode:          embedded") {
		t.Fatalf("provider status cpa = (%d, %q)", code, stdout)
	}

	// Shorthand single provider detail with trailing --json flag.
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"provider", "llama.cpp", "--json"}) })
	if code != 0 {
		t.Fatalf("provider llama.cpp --json = (%d, %q)", code, stdout)
	}
	var one routerctl.ProviderInfo
	if err := json.Unmarshal([]byte(stdout), &one); err != nil || one.Port != 9931 || one.Name != "llama_cpp" {
		t.Fatalf("provider llama.cpp --json decode err=%v info=%+v", err, one)
	}

	// Unknown provider exits 1.
	code, _, stderr := captureBoth(t, func() int { return run([]string{"provider", "opencode"}) })
	if code != 1 || !strings.Contains(stderr, "unknown provider") {
		t.Fatalf("unknown provider = (%d, %q)", code, stderr)
	}
}
