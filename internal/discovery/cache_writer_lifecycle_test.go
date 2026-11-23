package discovery

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/router"
)

func TestCacheCloseFlushesUnstartedOwner(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	reg := router.NewRegistry(cfg)
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Upstream: "m1", Provider: "up"}}, nil, float64(wallNow())); err != nil {
		t.Fatal(err)
	}
	s := NewCacheStore(cfg, reg)
	s.NotifyDirty()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Close(ctx)
	if _, err := os.Stat(cfg.DiscoveryCache.Path); err != nil {
		t.Fatalf("Close dropped the pending save: %v", err)
	}
}

func TestCacheRepeatedStartKeepsSingleWriter(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	s := NewCacheStore(cfg, router.NewRegistry(cfg))
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s.beforeEncode = func() { entered <- struct{}{}; <-release }
	s.Start()
	s.Start()
	s.NotifyDirty()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	s.NotifyDirty()
	select {
	case <-entered:
		t.Error("repeated Start created concurrent writers")
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Close(ctx)
}

func TestCacheRepeatedCloseDoesNotExtendDrainBudget(t *testing.T) {
	cfg := cacheTestConfig(t, t.TempDir())
	s := NewCacheStore(cfg, router.NewRegistry(cfg))
	entered, release := make(chan struct{}), make(chan struct{})
	s.beforeEncode = func() { close(entered); <-release }
	s.Start()
	s.NotifyDirty()
	<-entered
	defer func() { close(release); <-s.done }()
	first, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	s.Close(first)
	cancel()
	second, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	defer cancelSecond()
	returned := make(chan struct{})
	go func() { s.Close(second); close(returned) }()
	select {
	case <-returned:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("deferred Close added another wait after the drain deadline")
	}
}
