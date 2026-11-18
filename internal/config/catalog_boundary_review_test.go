package config

import (
	"strings"
	"testing"
)

func catalogReviewProviders() map[string]*Provider {
	return map[string]*Provider{"up": {Name: "up", Dialect: DialectOpenAIChat}}
}

func TestCatalogReviewNullMetadataSourceIsAbsent(t *testing.T) {
	_, err := fillCatalog(map[string]any{"url": "http://catalog.invalid", "model_providers": map[string]any{"a": "up"}, "metadata_source": nil}, catalogReviewProviders())
	if err != nil {
		t.Fatalf("null metadata source must be absent: %v", err)
	}
}

func TestCatalogReviewPythonWhitespaceAtConfigBoundary(t *testing.T) {
	_, err := fillCatalog(map[string]any{"url": "http://catalog.invalid", "model_providers": map[string]any{"\x1ca": "up"}}, catalogReviewProviders())
	if err == nil {
		t.Fatal("padded exact ID accepted")
	}
	cat, err := fillCatalog(map[string]any{"url": "\x1chttp://catalog.invalid\x1f", "rules": []any{map[string]any{"match": "\x1c^a$\x1f", "provider": " up "}}}, catalogReviewProviders())
	if err != nil || cat.Rules[0].Provider != "up" || !cat.Rules[0].Pattern.MatchString("a") {
		t.Fatalf("Python-stripped config strings changed meaning: cat=%+v err=%v", cat, err)
	}
}

func TestCatalogReviewProviderErrorsAreValueFree(t *testing.T) {
	const private = "private-provider-value"
	for _, tab := range []map[string]any{
		{"url": "http://catalog.invalid", "model_providers": map[string]any{"private-model-value": private}},
		{"url": "http://catalog.invalid", "rules": []any{map[string]any{"match": "a", "provider": private}}},
	} {
		_, err := fillCatalog(tab, catalogReviewProviders())
		if err == nil || strings.Contains(err.Error(), private) || strings.Contains(err.Error(), "private-model-value") {
			t.Fatalf("invalid provider error must name fields, not values: %v", err)
		}
	}
}
