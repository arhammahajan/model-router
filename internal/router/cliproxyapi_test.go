package router

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

func proxyTestConfig() *config.Config {
	return &config.Config{
		Server: config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{
			"up": {Name: "up", Dialect: config.DialectOpenAIChat, BaseURL: "http://127.0.0.1:9", Timeout: 600},
		},
		CLIProxyAPI: &config.CLIProxyAPIConfig{BaseURL: "http://127.0.0.1:8317/v1"},
	}
}

func TestSnapshotIncludesGeneratedProxyProvider(t *testing.T) {
	s := New(proxyTestConfig())
	if names := s.ProviderNames(); len(names) != 2 {
		t.Fatalf("providers = %v, want up + cliproxyapi", names)
	}
	m, p, fault := s.Resolve("cliproxyapi/anything-here")
	if fault != Found || p.Name != "cliproxyapi" || m.Upstream != "anything-here" {
		t.Fatalf("dynamic proxy route broken: %+v %+v %v", m, p, fault)
	}
	if p.Dialect != config.DialectOpenAIChat {
		t.Fatalf("proxy dialect must be openai-chat, got %q", p.Dialect)
	}
}

func TestDynamicSlashSuffixSurvives(t *testing.T) {
	s := New(proxyTestConfig())
	m, p, fault := s.Resolve("cliproxyapi/opencode-go/glm-5.3")
	if fault != Found || p.Name != "cliproxyapi" {
		t.Fatalf("slash dynamic miss: %+v %+v %v", m, p, fault)
	}
	// Only the first segment is the provider; the opaque remainder,
	// including its own slash, is the upstream ID.
	if m.Upstream != "opencode-go/glm-5.3" {
		t.Fatalf("slash suffix lost: %+v", m)
	}
	if m.Name != "cliproxyapi/opencode-go/glm-5.3" {
		t.Fatalf("public name not preserved: %+v", m)
	}
}

func TestOpaqueCatalogIDsResolveBeforeDynamic(t *testing.T) {
	cfg := proxyTestConfig()
	s := New(cfg)
	// Publish proxy rows through the reused catalog layer: opaque IDs,
	// including slash-prefixed ones, with no dialect guessing.
	attempt := CatalogAttempt{
		Success: &CatalogSuccess{
			Entries: []CatalogEntry{
				{Name: "opencode-go/glm-5.3", Provider: "cliproxyapi", Upstream: "opencode-go/glm-5.3"},
				{Name: "plain-model", Provider: "cliproxyapi", Upstream: "plain-model"},
			},
		},
	}
	r := NewRegistry(cfg)
	if err := r.CommitCatalog(attempt); err != nil {
		t.Fatalf("commit: %v", err)
	}
	pinned := r.Current()
	for _, id := range []string{"opencode-go/glm-5.3", "plain-model"} {
		m, p, fault := pinned.Resolve(id)
		if fault != Found || p.Name != "cliproxyapi" || m.Upstream != id {
			t.Fatalf("%q: %+v %+v %v", id, m, p, fault)
		}
		if p.Dialect != config.DialectOpenAIChat {
			t.Fatalf("%q dialect guessed: %q", id, p.Dialect)
		}
	}
	// No hardcoded OpenCode provider exists in the new mode.
	for _, name := range pinned.ProviderNames() {
		if name == "opencode-go" {
			t.Fatal("new mode must not hardcode an opencode-go provider")
		}
	}
	_ = s
}
