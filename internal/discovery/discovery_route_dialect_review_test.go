package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

// rdReviewLoadConfig loads TOML through the real admission path so the
// digest tests observe canonicalization (explicit-equal -> "").
func rdReviewLoadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	// Hermetic credential-file isolation: Load consults default files.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

const rdReviewBase = `
[server]
auth_mode = "none"
[providers.up]
dialect = "openai-chat"
base_url = "https://up.example"
api_key = "k"
[[models]]
name = "m"
provider = "up"
model = "gpt"
`

func TestRdReviewDigestOmittedEqualsEqual(t *testing.T) {
	omitted := rdReviewLoadConfig(t, rdReviewBase)
	equalBody := rdReviewBase + "dialect = \"openai-chat\"\n"
	_ = equalBody
	// Append the explicit-equal dialect to the single model block.
	equal := rdReviewLoadConfig(t, `
[server]
auth_mode = "none"
[providers.up]
dialect = "openai-chat"
base_url = "https://up.example"
api_key = "k"
[[models]]
name = "m"
provider = "up"
model = "gpt"
dialect = "openai-chat"
`)
	if got, want := cacheIdentity(equal), cacheIdentity(omitted); got != want {
		t.Fatal("omitted==explicit-equal must preserve digest")
	}
}

func TestRdReviewDigestRotationInvalidates(t *testing.T) {
	base := rdReviewLoadConfig(t, rdReviewBase)
	rotated := rdReviewLoadConfig(t, `
[server]
auth_mode = "none"
[providers.up]
dialect = "openai-chat"
base_url = "https://up.example"
api_key = "k"
[[models]]
name = "m"
provider = "up"
model = "gpt"
dialect = "anthropic"
`)
	if cacheIdentity(rotated) == cacheIdentity(base) {
		t.Fatal("override rotation must change digest")
	}
	// A second rotation to yet another destination also changes it.
	rotated2 := rdReviewLoadConfig(t, `
[server]
auth_mode = "none"
[providers.up]
dialect = "openai-chat"
base_url = "https://up.example"
api_key = "k"
[[models]]
name = "m"
provider = "up"
model = "gpt"
dialect = "gemini"
`)
	if cacheIdentity(rotated2) == cacheIdentity(base) || cacheIdentity(rotated2) == cacheIdentity(rotated) {
		t.Fatal("each distinct override must have a distinct digest")
	}
}

func TestRdReviewDigestDisabledOverrideCounts(t *testing.T) {
	base := rdReviewLoadConfig(t, rdReviewBase)
	disabled := rdReviewLoadConfig(t, rdReviewBase+`
[[models]]
name = "d"
provider = "up"
model = "gpt2"
dialect = "anthropic"
enabled = false
`)
	if cacheIdentity(disabled) == cacheIdentity(base) {
		t.Fatal("disabled override must still feed the digest")
	}
}
