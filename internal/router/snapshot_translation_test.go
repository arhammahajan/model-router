package router

import (
	"testing"
)

func TestTranslatedHeadersSnapshotIsolation(t *testing.T) {
	cfg := testConfig()
	cfg.Providers["up"].TranslatedHeaders = map[string]string{"X-Legacy": "v"}
	cfg.Providers["up"].LitellmProvider = "anthropic"
	s := New(cfg)
	// Source mutation must not leak.
	cfg.Providers["up"].TranslatedHeaders["X-Legacy"] = "mutated"
	cfg.Providers["up"].TranslatedHeaders["X-New"] = "mutated"
	m, p, fault := s.Resolve("a")
	if fault != Found {
		t.Fatal("resolve miss")
	}
	_ = m
	if p.TranslatedHeaders["X-Legacy"] != "v" {
		t.Fatalf("source mutation leaked: %v", p.TranslatedHeaders)
	}
	if _, ok := p.TranslatedHeaders["X-New"]; ok {
		t.Fatalf("source addition leaked: %v", p.TranslatedHeaders)
	}
	if p.LitellmProvider != "anthropic" {
		t.Fatalf("litellm_provider not snapshotted: %q", p.LitellmProvider)
	}
	// Returned mutation must not leak.
	p.TranslatedHeaders["X-Legacy"] = "mutated2"
	_, p2, fault := s.Resolve("a")
	if fault != Found {
		t.Fatal("resolve miss")
	}
	if p2.TranslatedHeaders["X-Legacy"] != "v" {
		t.Fatalf("returned mutation leaked: %v", p2.TranslatedHeaders)
	}
}

func TestTranslatedHeadersNilStaysNil(t *testing.T) {
	cfg := testConfig()
	s := New(cfg)
	_, p, fault := s.Resolve("a")
	if fault != Found {
		t.Fatal("resolve miss")
	}
	if p.TranslatedHeaders != nil && len(p.TranslatedHeaders) != 0 {
		t.Fatalf("zero-value programmatic literal must keep prior behavior: %v", p.TranslatedHeaders)
	}
}
