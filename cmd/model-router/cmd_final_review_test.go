// Final bounded acceptance review: CLI offline proof, HTTP drain with the
// cache on the same absolute deadline, listen-error path, readiness seed
// and no duplicate cache validators in executable paths.
//
// New file only; no production edits. Hermetic tempdirs, controlled HOME,
// fake httptest upstreams only.
package main

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/model-router/internal/discovery"
	"github.com/arhammahajan/model-router/internal/server"
)

func TestFinalReviewValidateConfigPerformsNoIO(t *testing.T) {
	dccEnv(t)
	var hits atomic.Int64
	var payload atomic.Value
	payload.Store(`[{"id": "cat-a"}]`)
	var healthy atomic.Bool
	healthy.Store(true)
	srv := dccCatalogSrv(t, &hits, &payload, &healthy)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "missing", "dc.json")
	cfg := dccWriteCfg(t, srv.URL, nil, cachePath, "")
	if code := run([]string{"--config", cfg, "--validate-config"}); code != 0 {
		t.Fatalf("validate must exit 0, got %d", code)
	}
	if hits.Load() != 0 {
		t.Fatal("validate-config must not probe the catalog")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatal("validate-config must not create the cache file")
	}
}

func TestFinalReviewReadinessSeedWritesCache(t *testing.T) {
	dccEnv(t)
	var hits atomic.Int64
	var payload atomic.Value
	payload.Store(`[{"id": "cat-a"}]`)
	var healthy atomic.Bool
	healthy.Store(true)
	srv := dccCatalogSrv(t, &hits, &payload, &healthy)
	defer srv.Close()
	cachePath := filepath.Join(t.TempDir(), "dc.json")
	cfg := dccWriteCfg(t, srv.URL, nil, cachePath, "")
	if code := run([]string{"--config", cfg, "--check-readiness"}); code != 0 {
		t.Fatalf("seed readiness must exit 0, got %d", code)
	}
	if hits.Load() == 0 {
		t.Fatal("readiness must run the bounded probe")
	}
	fi, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("readiness success must leave the cache file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode=%o want 600", fi.Mode().Perm())
	}
}

func TestFinalReviewServeDrainBoundedWithCache(t *testing.T) {
	dccEnv(t)
	cfgPath := offlineConfig(t)
	cfg := mustLoad(t, cfgPath)
	srv := server.New(cfg)
	store := discovery.NewCacheStore(cfg, srv.Registry())
	store.Start()
	store.NotifyDirty()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() { done <- serve(ln, srv, nil, nil, store, 500*time.Millisecond, stop) }()
	time.Sleep(100 * time.Millisecond)
	stop <- os.Interrupt
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve must drain 0 with cache, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not drain within the bound with cache attached")
	}
	if _, err := os.Stat(cfg.DiscoveryCache.Path); err != nil {
		t.Fatalf("drain must flush the pending save: %v", err)
	}
}

func TestFinalReviewListenErrorReturnsWithoutHanging(t *testing.T) {
	dccEnv(t)
	cfg := mustLoad(t, offlineConfig(t))
	srv := server.New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	stop := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() { done <- serve(ln, srv, nil, nil, nil, 500*time.Millisecond, stop) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("listen failure must exit 1, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listen-error path hung the drain")
	}
}

// Executable CLI paths must not grow a second cache validator, encoder
// policy or writer API: main.go stays free of cache validation identifiers
// and no non-test Go file may introduce StartupRestore, LoadWithDeadline
// or SyncSave.
func TestFinalReviewNoDuplicateCacheValidatorsInExecutable(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller unavailable")
	}
	dir := filepath.Dir(self)
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, ident := range []string{
			"decodeUnit", "validateEncoded", "safeID", "basicSafeID",
			"schema_version", "encoding/json",
			"StartupRestore", "LoadWithDeadline", "SyncSave",
		} {
			if strings.Contains(string(raw), ident) {
				t.Fatalf("executable %s must not carry cache validator %q", name, ident)
			}
		}
	}
	root := filepath.Join(dir, "..", "..")
	var violations []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, ident := range []string{"StartupRestore", "LoadWithDeadline", "SyncSave"} {
			if strings.Contains(string(raw), ident) {
				violations = append(violations, path+":"+ident)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(violations) != 0 {
		t.Fatalf("forbidden second-writer APIs present: %v", violations)
	}
}
