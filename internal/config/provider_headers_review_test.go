package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReviewCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Losing source (kwargs.headers) carrying a custom session name must fail:
// validation covers every configured source, not just the winner.
func TestReviewLegacyLosingCustomSessionRejected(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
session_header = "x-review-sess"
[providers.up.litellm_kwargs]
headers = { "X-Review-Sess" = "static-losing" }
extra_headers = { "X-Ok" = "v" }
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeReviewCfg(t, body))
	if err == nil {
		t.Fatal("losing kwargs.headers with custom session name must be rejected")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Fatalf("error must mention session: %v", err)
	}
	if strings.Contains(err.Error(), "static-losing") {
		t.Fatalf("error must stay value-free: %v", err)
	}
}

// Winning source (extra_headers) with a hardcoded native identity must fail
// even when the custom session header is different.
func TestReviewLegacyWinningHardcodedSessionRejected(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
session_header = "x-other"
[providers.up.litellm_kwargs]
headers = { "X-Ok" = "v" }
extra_headers = { "Session-Id" = "static" }
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeReviewCfg(t, body))
	if err == nil {
		t.Fatal("extra_headers with hardcoded session identity must be rejected")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Fatalf("error must mention session: %v", err)
	}
}

// litellm_provider with a non-string type must be rejected value-free with
// the native-route hint.
func TestReviewLitellmProviderWrongTypeRejected(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
litellm_provider = 123
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeReviewCfg(t, body))
	if err == nil {
		t.Fatal("non-string litellm_provider must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.up.litellm_provider") {
		t.Fatalf("error must name keypath: %v", err)
	}
	if !strings.Contains(err.Error(), "native route") {
		t.Fatalf("error must hint native route: %v", err)
	}
}

// Mismatched spelling must be rejected without echoing the operator value.
func TestReviewLitellmProviderMismatchValueFree(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "openai-chat"
base_url = "https://up.example"
litellm_provider = "anthropic"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeReviewCfg(t, body))
	if err == nil {
		t.Fatal("mismatched litellm_provider must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.up.litellm_provider") {
		t.Fatalf("error must name keypath: %v", err)
	}
	// The rejected spelling must not leak; the hint wording contains no
	// dialect spelling.
	if strings.Contains(err.Error(), `"anthropic"`) {
		t.Fatalf("error must stay value-free: %v", err)
	}
}

// Non-table litellm_kwargs must be rejected without echo.
func TestReviewLitellmKwargsNonTableRejected(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
litellm_kwargs = "nope"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeReviewCfg(t, body))
	if err == nil {
		t.Fatal("non-table litellm_kwargs must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.up.litellm_kwargs") {
		t.Fatalf("error must name keypath: %v", err)
	}
	if strings.Contains(err.Error(), "nope") {
		t.Fatalf("error must stay value-free: %v", err)
	}
}

// Empty litellm_kwargs table loads with nil TranslatedHeaders (unconfigured
// behavior preserved, no empty-map allocation leak).
func TestReviewLitellmKwargsEmptyTableNil(t *testing.T) {
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
[providers.up.litellm_kwargs]
[[models]]
name = "m"
provider = "up"
model = "x"
`
	cfg, err := Load(writeReviewCfg(t, body))
	if err != nil {
		t.Fatalf("empty litellm_kwargs table must load: %v", err)
	}
	if cfg.Providers["up"].TranslatedHeaders != nil {
		t.Fatalf("empty table must keep nil TranslatedHeaders: %v", cfg.Providers["up"].TranslatedHeaders)
	}
}

// Credential keys inside litellm_kwargs are rejected with the api_key hint
// and without echoing the secret.
func TestReviewLegacyCredentialValueFree(t *testing.T) {
	secret := "sk-review-secret-xyz"
	body := `
[server]
auth_mode = "none"
[providers.up]
dialect = "anthropic"
base_url = "https://up.example"
[providers.up.litellm_kwargs]
api_key = "` + secret + `"
[[models]]
name = "m"
provider = "up"
model = "x"
`
	_, err := Load(writeReviewCfg(t, body))
	if err == nil {
		t.Fatal("api_key inside litellm_kwargs must be rejected")
	}
	if !strings.Contains(err.Error(), "providers.up.litellm_kwargs.api_key") {
		t.Fatalf("error must name keypath: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error must stay value-free")
	}
}

// Single-map case variants collapse deterministically (sorted-last wins).
// This locks the baseline so a future change cannot silently alter which
// spelling survives; cross-source order (headers < extra_headers) is covered
// by the implementation tests.
func TestReviewSingleMapCaseVariantBaseline(t *testing.T) {
	got := mergeLegacyHeaders(
		map[string]string{"X-Dup": "kh-upper", "x-dup": "kh-lower"},
		nil,
	)
	if len(got) != 1 {
		t.Fatalf("single-map case variants must collapse to one entry: %v", got)
	}
	for k, v := range got {
		if !strings.EqualFold(k, "x-dup") {
			t.Fatalf("unexpected key: %v", got)
		}
		// ASCII sort: "X-Dup" < "x-dup", so the lowercase spelling wins.
		if v != "kh-lower" || k != "x-dup" {
			t.Fatalf("sorted-last must win within one map: %v", got)
		}
	}
}
