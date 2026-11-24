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
			Name:              "opencode-chat",
			Kind:              "generic",
			Status:            "configured",
			BaseURL:           "https://opencode.ai/inference/go/openai/v1",
			Scheme:            "https",
			Host:              "opencode.ai",
			Port:              443,
			DefaultDialect:    "openai-chat",
			SupportedDialects: []string{"openai-chat"},
			Compatibility:     "lossy",
			TimeoutSeconds:    600,
			AuthHeader:        "auto",
			APIKeyConfigured:  true,
		},
		{
			Name:                "llama-127.0.0.1-9931",
			Kind:                "llama_cpp",
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
			APIKeyConfigured:    false,
		},
	}

	routerFetchProviders = func(_ context.Context, _ routerctl.Options) ([]routerctl.ProviderInfo, error) {
		return sample, nil
	}
	routerFetchProvider = func(_ context.Context, _ routerctl.Options, name string) (routerctl.ProviderInfo, error) {
		for _, it := range sample {
			if it.Name == name {
				return it, nil
			}
		}
		return routerctl.ProviderInfo{}, fmt.Errorf("provider %q not found", name)
	}

	// Default table list.
	code, stdout, _ := captureBoth(t, func() int { return run([]string{"provider"}) })
	if code != 0 || !strings.Contains(stdout, "HOST:PORT") || !strings.Contains(stdout, "127.0.0.1:9931") || !strings.Contains(stdout, "opencode.ai:443") {
		t.Fatalf("provider list = (%d, %q)", code, stdout)
	}

	// JSON list.
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"provider", "list", "--json"}) })
	if code != 0 {
		t.Fatalf("provider list --json = %d", code)
	}
	var decoded []routerctl.ProviderInfo
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || len(decoded) != 2 {
		t.Fatalf("provider list --json decode err=%v len=%d", err, len(decoded))
	}

	// Single provider detail.
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"provider", "status", "llama-127.0.0.1-9931"}) })
	if code != 0 || !strings.Contains(stdout, "port:          9931") || !strings.Contains(stdout, "probe_timeout: 1s") {
		t.Fatalf("provider status <name> = (%d, %q)", code, stdout)
	}

	// Shorthand single provider detail with trailing --json flag.
	code, stdout, _ = captureBoth(t, func() int { return run([]string{"provider", "opencode-chat", "--json"}) })
	if code != 0 {
		t.Fatalf("provider <name> --json = (%d, %q)", code, stdout)
	}
	var one routerctl.ProviderInfo
	if err := json.Unmarshal([]byte(stdout), &one); err != nil || one.Port != 443 || !one.APIKeyConfigured {
		t.Fatalf("provider <name> --json decode err=%v info=%+v", err, one)
	}

	// Unknown provider exits 1.
	code, _, stderr := captureBoth(t, func() int { return run([]string{"provider", "nope"}) })
	if code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("unknown provider = (%d, %q)", code, stderr)
	}
}
