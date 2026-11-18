// Focused [discovery_cache] schema tests: absent=nil, strict finite
// positive (0 rejects), unknown keys and empty paths reject, ~/ expands
// via home only while $VAR gains no second expansion here.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const cacheBase = `
[server]
[retry]
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
`

func TestDiscoveryCacheAbsentIsNil(t *testing.T) {
	cfg, err := Load(writeTOML(t, cacheBase+`
[[models]]
name = "a"
provider = "up"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DiscoveryCache != nil {
		t.Fatal("absent table must be nil/disabled")
	}
}

func TestDiscoveryCacheDefaultsAndRejects(t *testing.T) {
	// Default max age applies once the table exists.
	cfg, err := Load(writeTOML(t, cacheBase+`
[[models]]
name = "a"
provider = "up"
[discovery_cache]
path = "/tmp/x.json"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DiscoveryCache.MaxAgeSeconds != DefaultDiscoveryCacheMaxAge {
		t.Fatalf("default max age wrong: %v", cfg.DiscoveryCache.MaxAgeSeconds)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"zero", "[discovery_cache]\npath = \"/tmp/x\"\nmax_age_seconds = 0"},
		{"negative", "[discovery_cache]\npath = \"/tmp/x\"\nmax_age_seconds = -1"},
		{"nan", "[discovery_cache]\npath = \"/tmp/x\"\nmax_age_seconds = nan"},
		{"inf", "[discovery_cache]\npath = \"/tmp/x\"\nmax_age_seconds = inf"},
		{"oversize", "[discovery_cache]\npath = \"/tmp/x\"\nmax_age_seconds = 99999999999"},
		{"empty-path", "[discovery_cache]\npath = \"  \"\n"},
		{"missing-path", "[discovery_cache]\nmax_age_seconds = 60\n"},
		{"unknown-key", "[discovery_cache]\npath = \"/tmp/x\"\nmode = \"fast\"\n"},
		{"tilde-other", "[discovery_cache]\npath = \"~other/x.json\"\n"},
		{"wrong-type", "[discovery_cache]\npath = 42\n"},
	} {
		_, err := Load(writeTOML(t, cacheBase+"[[models]]\nname = \"a\"\nprovider = \"up\"\n"+tc.body+"\n"))
		if err == nil {
			t.Fatalf("%s must reject", tc.name)
		}
	}
}

func TestDiscoveryCacheHomeExpandOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(writeTOML(t, cacheBase+"[[models]]\nname = \"a\"\nprovider = \"up\"\n[discovery_cache]\npath = \"~/cache.json\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.HasPrefix(cfg.DiscoveryCache.Path, home) {
		t.Fatalf("~ must expand via home: %q", cfg.DiscoveryCache.Path)
	}
	// No second env expansion in the cache module: a literal $VAR stays.
	t.Setenv("CACHE_DIR", "/tmp/exp")
	cfg2, err := Load(writeTOML(t, cacheBase+"[[models]]\nname = \"a\"\nprovider = \"up\"\n[discovery_cache]\npath = \"$CACHE_DIR/x.json\"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Contains(cfg2.DiscoveryCache.Path, "/tmp/exp") {
		t.Fatalf("cache module must not add env expansion: %q", cfg2.DiscoveryCache.Path)
	}
}
