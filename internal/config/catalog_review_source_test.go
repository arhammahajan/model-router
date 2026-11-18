package config

import (
	"strings"
	"testing"
)

// Admitted source validates once at config construction; absent stays
// absent; every other value fails closed with a value-free key-path error.
func TestFocusMetadataSourceAdmission(t *testing.T) {
	base := func(extra string) map[string]any {
		return map[string]any{
			"url": "http://127.0.0.1:9/v1/models", "model_providers": map[string]any{"a": "up"},
			"metadata_source": extra,
		}
	}
	_ = base
	// Omitted key is absent.
	cat, err := fillCatalog(map[string]any{"url": "http://catalog.invalid", "model_providers": map[string]any{"a": "up"}}, catalogReviewProviders())
	if err != nil || cat.MetadataSource != "" || cat.HasReview {
		t.Fatalf("omitted must be absent: %+v %v", cat, err)
	}
	// Null is absent.
	cat, err = fillCatalog(map[string]any{"url": "http://catalog.invalid", "model_providers": map[string]any{"a": "up"}, "metadata_source": nil}, catalogReviewProviders())
	if err != nil || cat.MetadataSource != "" || cat.HasReview {
		t.Fatalf("null must be absent: %+v %v", cat, err)
	}
	// The one admitted value validates the bundled asset once.
	cat, err = fillCatalog(map[string]any{"url": "http://catalog.invalid", "model_providers": map[string]any{"a": "up"}, "metadata_source": "opencode-go-v1"}, catalogReviewProviders())
	if err != nil || cat.MetadataSource != "opencode-go-v1" || !cat.HasReview {
		t.Fatalf("admitted must validate: %+v %v", cat, err)
	}
	// Review accessor returns an independent validated copy.
	a1, ok1 := cat.Review()
	a2, ok2 := cat.Review()
	if !ok1 || !ok2 {
		t.Fatal("review accessor must report present")
	}
	_ = a1.Clone()
	_ = a2.Clone()
	// Every other value fails closed, naming only the key path.
	for name, v := range map[string]any{
		"empty string":     "",
		"other name":       "other",
		"wrong type int":   int64(7),
		"wrong type bool":  true,
		"wrong type float": float64(1.5),
	} {
		_, err := fillCatalog(map[string]any{"url": "http://catalog.invalid", "model_providers": map[string]any{"a": "up"}, "metadata_source": v}, catalogReviewProviders())
		if err == nil {
			t.Fatalf("%s must reject", name)
		}
		if !strings.Contains(err.Error(), "catalog.metadata_source") {
			t.Fatalf("%s must name the key path: %v", name, err)
		}
		if strings.Contains(err.Error(), "other") || strings.Contains(err.Error(), "7") {
			t.Fatalf("%s must be value-free: %v", name, err)
		}
	}
}
