package router

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

func TestSnapshotPreservesDisabledNativeSessionSources(t *testing.T) {
	p := testProvider()
	p.SessionSourceHeaders = []string{}
	p.SessionMissing = "reject"
	s := New(&config.Config{
		Providers: map[string]*config.Provider{"up": p},
		Models:    []config.Model{{Name: "m", Provider: "up", Upstream: "upstream"}},
	})
	_, resolved, fault := s.Resolve("m")
	if fault != Found {
		t.Fatal("route not resolved")
	}
	_, _, rerr := PrepareSession(resolved, hdr("Thread-Id", "disabled-source"), []byte(`{}`), false, NoCompatFallback())
	if rerr == nil || rerr.Code != "missing_session_id" {
		t.Fatalf("snapshot must preserve disabled native extraction, got %v", rerr)
	}
}
