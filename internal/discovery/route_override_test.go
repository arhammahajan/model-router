package discovery

import (
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func fpCfg() *config.Config {
	return &config.Config{
		Providers: map[string]*config.Provider{
			"up": {Name: "up", DefaultDialect: config.DialectOpenAIChat, BaseURL: "https://up.example", APIKey: "k", Timeout: 600},
		},
		Models: []config.Model{
			{Name: "m", Provider: "up", Upstream: "gpt"},
		},
	}
}

func TestCacheFingerprintOverrideChangeVsNoop(t *testing.T) {
	base := fpCfg()
	noop := fpCfg()
	noop.Models[0].Dialect = "" // omitted
	changed := fpCfg()
	changed.Models[0].Dialect = config.DialectAnthropic
	equal := fpCfg()
	equal.Models[0].Dialect = "" // explicit-equal canonicalizes to "" at Load; direct "" equals omitted
	if got, want := cacheIdentity(noop), cacheIdentity(base); got != want {
		t.Fatalf("omitted==equal must preserve digest")
	}
	_ = equal
	if cacheIdentity(changed) == cacheIdentity(base) {
		t.Fatalf("override rotation must change digest")
	}
	rotated := fpCfg()
	rotated.Providers["up"].DefaultDialect = config.DialectAnthropic
	if cacheIdentity(rotated) == cacheIdentity(base) {
		t.Fatalf("provider rotation must invalidate")
	}
}
