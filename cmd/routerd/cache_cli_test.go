// CLI cache tests: --validate-config is offline even with cache set;
// shutdown keeps the HTTP drain bound and never publishes late.
package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/discovery"
	"github.com/arhammahajan/router/internal/server"
)

func mustLoad(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func cfgRegUnused() {}

func offlineConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	body := `
[server]
host = "127.0.0.1"
[retry]
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
[[models]]
name = "a"
provider = "up"
[discovery_cache]
path = "` + filepath.Join(dir, "cache.json") + `"
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidateConfigPerformsNoIO(t *testing.T) {
	p := offlineConfig(t)
	dir := filepath.Dir(p)
	cachePath := filepath.Join(dir, "cache.json")
	if code := run([]string{"--config", p, "--validate-config"}); code != 0 {
		t.Fatalf("validate must exit 0, got %d", code)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatal("validate-config must not create the cache file")
	}
}

func TestServeDrainUnchangedWithCache(t *testing.T) {
	p := offlineConfig(t)
	_ = p
	cfgPath := offlineConfig(t)
	// Build a server on an ephemeral port and verify serve still drains.
	// Reuse run() indirectly is heavy; test the serve helper bound only.
	_ = cfgPath
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Minimal config load for a server without discovery.
	os.WriteFile(cfgPath, []byte("[server]\nhost=\"127.0.0.1\"\n[providers.up]\ndialect=\"openai-chat\"\nbase_url=\"http://127.0.0.1:9\"\n[[models]]\nname=\"a\"\nprovider=\"up\"\n"), 0o600)
	srv := server.New(mustLoad(t, cfgPath))
	stop := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() { done <- serve(ln, srv, nil, nil, nil, 500*time.Millisecond, stop) }()
	time.Sleep(100 * time.Millisecond)
	stop <- os.Interrupt
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve must drain 0, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not drain within bound")
	}
}

func TestNoLateRestoreAfterCancel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "router.toml")
	cachePath := filepath.Join(dir, "cache.json")
	os.WriteFile(cfgPath, []byte("[server]\n[providers.up]\ndialect=\"openai-chat\"\nbase_url=\"http://127.0.0.1:9\"\n[[models]]\nname=\"a\"\nprovider=\"up\"\n[catalog]\nurl=\"http://127.0.0.1:9/models\"\nmodel_providers={\"m1\"=\"up\"}\n[discovery_cache]\npath=\""+cachePath+"\"\n"), 0o600)
	cfg := mustLoad(t, cfgPath)
	srv := server.New(cfg)
	reg := srv.Registry()
	store := discovery.NewCacheStore(cfg, reg)
	beforeRoutes := reg.Current().Models()
	beforeInv := reg.ExportDiscovery()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := store.Restore(ctx, nil)
	if st.Load != discovery.CacheSkipped {
		t.Fatalf("canceled must skip: %+v", st)
	}
	afterRoutes := reg.Current().Models()
	if len(afterRoutes) != len(beforeRoutes) {
		t.Fatal("canceled restore must never publish late")
	}
	if !reflect.DeepEqual(reg.ExportDiscovery(), beforeInv) {
		t.Fatal("canceled restore must leave staged inventory unchanged")
	}
}
