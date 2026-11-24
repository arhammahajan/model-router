package router

import (
	"sync"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func overrideTestConfig() *config.Config {
	return &config.Config{
		Server: config.Server{AllowDynamicRoutes: false},
		Providers: map[string]*config.Provider{
			"chatup": {Name: "chatup", DefaultDialect: config.DialectOpenAIChat, BaseURL: "https://up.example", APIKey: "k"},
			"anthup": {Name: "anthup", DefaultDialect: config.DialectAnthropic, BaseURL: "https://up.example", APIKey: "k"},
		},
		Models: []config.Model{
			{Name: "chat-native", Provider: "chatup", Upstream: "gpt"},
			{Name: "chat-to-anthropic", Provider: "chatup", Upstream: "claude", Dialect: config.DialectAnthropic, Aliases: []string{"cta-alias"}},
			{Name: "disabled-over", Provider: "chatup", Upstream: "x", Dialect: config.DialectGemini, Disabled: true},
		},
	}
}

func TestOverrideSnapshotConcurrentImmutable(t *testing.T) {
	cfg := overrideTestConfig()
	reg := NewRegistry(cfg)
	snap := reg.Current()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "chat-to-anthropic"
			if i%2 == 0 {
				name = "cta-alias"
			}
			m, p, fault := snap.Resolve(name)
			if fault != Found {
				t.Errorf("resolve fault=%v", fault)
				return
			}
			if got := m.EffectiveDialect(p); got != config.DialectAnthropic {
				t.Errorf("dest=%q", got)
				return
			}
			if p.DefaultDialect != config.DialectOpenAIChat {
				t.Errorf("provider mutated: %q", p.DefaultDialect)
				return
			}
			m.Dialect = "mutated"
			p.DefaultDialect = "mutated"
		}(i)
	}
	wg.Wait()
	m, p, _ := snap.Resolve("chat-to-anthropic")
	if m.EffectiveDialect(p) != config.DialectAnthropic || p.DefaultDialect != config.DialectOpenAIChat {
		t.Fatalf("shared snapshot mutated: %+v %+v", m, p)
	}
	if _, _, fault := snap.Resolve("disabled-over"); fault != DisabledReserved {
		t.Fatalf("disabled must stay reserved, got %v", fault)
	}
}
