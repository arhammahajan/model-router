package router

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func testConfig() *config.Config {
	trueVal := true
	return &config.Config{
		Server: config.Server{AllowDynamicRoutes: true},
		Providers: map[string]*config.Provider{
			"up": {
				Name:                 "up",
				DefaultDialect:       config.DialectAnthropic,
				BaseURL:              "https://up.example",
				APIKey:               "k",
				Headers:              map[string]string{"X-Custom": "v"},
				Timeout:              600,
				CountTokens:          &trueVal,
				SessionSourceHeaders: []string{"x-session-id"},
			},
		},
		Models: []config.Model{
			{Name: "a", Provider: "up", Upstream: "x", Aliases: []string{"a-alias"},
				Metadata: json.RawMessage(`{"context_window":200000}`)},
			{Name: "b", Provider: "up", Upstream: "y", Disabled: true, Aliases: []string{"b-alias"}},
			// Provider/model-shaped alias: must stay reserved when disabled.
			{Name: "c", Provider: "up", Upstream: "z", Disabled: true, Aliases: []string{"up/c"}},
		},
	}
}

func TestExplicitAndAliasPrecedence(t *testing.T) {
	s := New(testConfig())
	m, p, fault := s.Resolve("a-alias")
	if fault != Found || m.Name != "a" || m.Upstream != "x" || p.Name != "up" {
		t.Fatalf("alias miss: %+v %+v %v", m, p, fault)
	}
	if _, _, fault := s.Resolve("b"); fault != DisabledReserved {
		t.Fatalf("disabled name must not resolve")
	}
	if _, _, fault := s.Resolve("b-alias"); fault != DisabledReserved {
		t.Fatalf("disabled alias must not resolve")
	}
}

func TestDisabledProviderShapedAliasNoDynamicBypass(t *testing.T) {
	s := New(testConfig())
	if _, _, fault := s.Resolve("up/c"); fault != DisabledReserved {
		t.Fatalf("disabled provider-shaped alias must not fall through to dynamic")
	}
	m, _, fault := s.Resolve("up/other")
	if fault != Found || m.Upstream != "other" {
		t.Fatalf("dynamic fallback broken: %+v %v", m, fault)
	}
}

func TestOmittedEnabledStaysUsable(t *testing.T) {
	s := New(testConfig())
	if got := s.UsableCount(); got != 2 { // enabled a plus its alias
		t.Fatalf("usable=%d want 2", got)
	}
	if !s.Ready() {
		t.Fatalf("one enabled named route must be ready")
	}
}

func TestSourceMutationCannotAlterSnapshot(t *testing.T) {
	cfg := testConfig()
	s := New(cfg)
	cfg.Models[0].Name = "mutated"
	cfg.Models[0].Aliases[0] = "mutated"
	cfg.Models[0].Metadata[0] = 'X'
	cfg.Providers["up"].Headers["X-Custom"] = "mutated"
	cfg.Providers["up"].BaseURL = "https://mutated.example"
	cfg.Server.AllowDynamicRoutes = false
	if _, _, fault := s.Resolve("mutated"); fault != NotFound {
		t.Fatalf("source mutation leaked into snapshot")
	}
	m, p, fault := s.Resolve("a-alias")
	if fault != Found || m.Name != "a" || p.Headers["X-Custom"] != "v" {
		t.Fatalf("snapshot corrupted by source mutation: %+v %+v", m, p)
	}
	if string(m.Metadata) != `{"context_window":200000}` || p.BaseURL != "https://up.example" {
		t.Fatal("source metadata or endpoint mutation leaked into snapshot")
	}
	if _, _, fault := s.Resolve("up/other"); fault != Found {
		t.Fatal("source dynamic-routing mutation leaked into snapshot")
	}
}

func TestReturnedValueMutationCannotAlterSnapshot(t *testing.T) {
	s := New(testConfig())
	m, p, fault := s.Resolve("a")
	if fault != Found {
		t.Fatal("explicit miss")
	}
	m.Name = "mutated"
	m.Aliases[0] = "mutated"
	m.Metadata[0] = 'X'
	p.Headers["X-Custom"] = "mutated"
	p.SessionSourceHeaders[0] = "mutated"
	if b := true; p.CountTokens != nil {
		*p.CountTokens = !b
	}
	m2, p2, fault := s.Resolve("a-alias")
	if fault != Found || m2.Name != "a" || m2.Aliases[0] != "a-alias" {
		t.Fatalf("returned model mutation leaked: %+v", m2)
	}
	if p2.Headers["X-Custom"] != "v" || p2.SessionSourceHeaders[0] != "x-session-id" {
		t.Fatalf("returned provider mutation leaked: %+v", p2)
	}
	if string(m2.Metadata) != `{"context_window":200000}` || p2.CountTokens == nil || !*p2.CountTokens {
		t.Fatal("returned metadata or count-token declaration mutation leaked")
	}
	listed := s.Models()
	listed[0].Name = "mutated"
	listed[0].Metadata[0] = 'X'
	again := s.Models()
	if again[0].Name != "a" {
		t.Fatalf("listing mutation leaked: %+v", again[0])
	}
	if string(again[0].Metadata) != `{"context_window":200000}` {
		t.Fatal("listing metadata mutation leaked")
	}
}

func TestModelsEnabledOnlyDeterministicOrder(t *testing.T) {
	s := New(testConfig())
	got := s.Models()
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("listing must be enabled-only in config order: %+v", got)
	}
	if string(got[0].Metadata) != `{"context_window":200000}` {
		t.Fatalf("metadata fidelity: %s", got[0].Metadata)
	}
}

func TestConcurrentResolveListReadiness(t *testing.T) {
	s := New(testConfig())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Resolve("a")
			s.Resolve("up/other")
			s.Models()
			s.UsableCount()
			s.Ready()
		}()
	}
	wg.Wait()
}
