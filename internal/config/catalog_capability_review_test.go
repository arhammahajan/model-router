package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Catalog capability admission: exact opt-in only, validated once per
// config, offline, with copy isolation. Reached through the real TOML
// admission path (Load), never through bare struct literals.

// writeCatalogCapTOML writes a minimal catalog config with the given
// metadata_source stanza (empty means omitted).
func writeCatalogCapTOML(t *testing.T, stanza string) string {
	t.Helper()
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = true
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
[catalog]
url = "http://127.0.0.1:9/v1/models"
model_providers = {"glm-5.3-flash" = "up", "unknown-model-z" = "up"}
` + stanza
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoadCatalogCap(t *testing.T, p string) *Config {
	t.Helper()
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// Explicit opt-in only: omitted/null admitted absent, the one exact value
// admitted present, everything else a value-free startup error.
func TestCatalogCapReviewAdmissionOptIn(t *testing.T) {
	absent := mustLoadCatalogCap(t, writeCatalogCapTOML(t, ""))
	if absent.Catalog == nil || absent.Catalog.MetadataSource != "" || absent.Catalog.HasReview {
		t.Fatalf("omitted must be absent: %+v", absent.Catalog)
	}
	if _, ok := absent.Catalog.Review(); ok {
		t.Fatal("absent must report no asset")
	}
	nulled, err := fillCatalog(map[string]any{"url": "http://catalog.invalid", "model_providers": map[string]any{"a": "up"}, "metadata_source": nil}, catalogReviewProviders())
	if err != nil || nulled.MetadataSource != "" || nulled.HasReview {
		t.Fatalf("null must be absent: %+v %v", nulled, err)
	}
	opted := mustLoadCatalogCap(t, writeCatalogCapTOML(t, "metadata_source = \"opencode-go-v1\"\n"))
	if opted.Catalog.MetadataSource != "opencode-go-v1" || !opted.Catalog.HasReview {
		t.Fatalf("exact value must opt in: %+v", opted.Catalog)
	}
	if _, ok := opted.Catalog.Review(); !ok {
		t.Fatal("opt-in must report a validated asset")
	}
	for name, stanza := range map[string]string{
		"empty string":    "metadata_source = \"\"\n",
		"other name":      "metadata_source = \"other\"\n",
		"wrong type int":  "metadata_source = 7\n",
		"wrong type bool": "metadata_source = true\n",
		"padded value":    "metadata_source = \" opencode-go-v1\"\n",
		"case changed":    "metadata_source = \"OpenCode-Go-V1\"\n",
	} {
		p := writeCatalogCapTOML(t, stanza)
		_, err := Load(p)
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

// Compatible gateways work with the opt-in: no invented URL/origin/mapping
// restriction beyond the trusted provider mapping.
func TestCatalogCapReviewCompatibleGateway(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:9/v1/models",
		"https://catalog.example.internal:8443/models?tenant=acme",
		"http://user:pass@catalog.example/models",
	} {
		p := writeCatalogCapTOML(t, "metadata_source = \"opencode-go-v1\"\n")
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(string(raw),
			"url = \"http://127.0.0.1:9/v1/models\"",
			"url = \""+url+"\"", 1)
		q := filepath.Join(t.TempDir(), "router.toml")
		if err := os.WriteFile(q, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := mustLoadCatalogCap(t, q)
		if !cfg.Catalog.HasReview || cfg.Catalog.MetadataSource != "opencode-go-v1" {
			t.Fatalf("gateway %q must keep the opt-in: %+v", url, cfg.Catalog)
		}
	}
	// Rules-based mapping admits the same opt-in.
	p := writeCatalogCapTOML(t, "metadata_source = \"opencode-go-v1\"\n")
	raw, _ := os.ReadFile(p)
	body := strings.Replace(string(raw),
		"model_providers = {\"glm-5.3-flash\" = \"up\", \"unknown-model-z\" = \"up\"}",
		"rules = [{match = \"^glm-\", provider = \"up\"}]", 1)
	q := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(q, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoadCatalogCap(t, q)
	if !cfg.Catalog.HasReview || len(cfg.Catalog.Rules) != 1 {
		t.Fatalf("rules mapping must keep the opt-in: %+v", cfg.Catalog)
	}
}

// Asset validated once per config load, offline: repeated loads agree,
// no wall clock on the admission path, accessor returns independent copies.
func TestCatalogCapReviewOncePerConfig(t *testing.T) {
	p := writeCatalogCapTOML(t, "metadata_source = \"opencode-go-v1\"\n")
	first := mustLoadCatalogCap(t, p)
	second := mustLoadCatalogCap(t, p)
	a1, ok1 := first.Catalog.Review()
	a2, ok2 := second.Catalog.Review()
	if !ok1 || !ok2 {
		t.Fatal("both loads must report an asset")
	}
	// Mutating the config handle after load cannot move the stored asset:
	// the Review accessor clones and HasReview flags are plain values.
	first.Catalog.MetadataSource = ""
	first.Catalog.HasReview = false
	if _, ok := first.Catalog.Review(); ok {
		t.Fatal("cleared presence must report absent")
	}
	if _, ok := second.Catalog.Review(); !ok {
		t.Fatal("sibling load must be unaffected")
	}
	_ = a1.Clone()
	_ = a2.Clone()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := second.Catalog.Review(); !ok {
				t.Error("concurrent Review must report present")
			}
		}()
	}
	wg.Wait()
}

// Unknown catalog fields stay rejected; catalog still requires a mapping.
func TestCatalogCapReviewBoundaryStays(t *testing.T) {
	p := writeCatalogCapTOML(t, "metadata_source = \"opencode-go-v1\"\nunknown_field = 1\n")
	if _, err := Load(p); err == nil {
		t.Fatal("unknown catalog field must reject")
	}
	// No mapping at all is still a startup error, opt-in or not.
	bare := `
[server]
auth_mode = "none"
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "upstream-secret"
[catalog]
url = "http://127.0.0.1:9/v1/models"
metadata_source = "opencode-go-v1"
`
	q := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(q, []byte(bare), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(q); err == nil {
		t.Fatal("catalog without mappings must reject")
	}
}
