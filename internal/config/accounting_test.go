// Focused [accounting] schema tests: absent=nil, defaults, unknown-key
// and bad max rejections, ${ENV} expansion via the loader, nil-gating on
// the Config pointer.
package config

import (
	"path/filepath"
	"strings"
	"testing"
)

const accountingBase = `
[server]
[retry]
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
`

func TestAccountingAbsentIsNil(t *testing.T) {
	cfg, err := Load(writeTOML(t, accountingBase+`
[[models]]
name = "a"
provider = "up"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Accounting != nil {
		t.Fatal("absent table must be nil/disabled")
	}
}

func TestAccountingPresentDefaults(t *testing.T) {
	cfg, err := Load(writeTOML(t, accountingBase+`
[[models]]
name = "a"
provider = "up"
[accounting]
path = "/tmp/usage.jsonl"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Accounting == nil {
		t.Fatal("present table must not be nil")
	}
	if cfg.Accounting.Path != "/tmp/usage.jsonl" {
		t.Fatalf("path: %q", cfg.Accounting.Path)
	}
	if cfg.Accounting.MaxFileBytes != DefaultAccountingMaxFileBytes {
		t.Fatalf("default max_file_bytes wrong: %d", cfg.Accounting.MaxFileBytes)
	}
}

func TestAccountingExplicitMaxAccepted(t *testing.T) {
	cfg, err := Load(writeTOML(t, accountingBase+`
[[models]]
name = "a"
provider = "up"
[accounting]
path = "/tmp/usage.jsonl"
max_file_bytes = 1048576
`))
	if err != nil {
		t.Fatalf("1 MiB minimum must be accepted: %v", err)
	}
	if cfg.Accounting.MaxFileBytes != 1048576 {
		t.Fatalf("explicit max_file_bytes wrong: %d", cfg.Accounting.MaxFileBytes)
	}
}

func TestAccountingRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing-path", "[accounting]\nmax_file_bytes = 1048576\n"},
		{"empty-path", "[accounting]\npath = \"   \"\n"},
		{"wrong-path-type", "[accounting]\npath = 42\n"},
		{"unknown-key", "[accounting]\npath = \"/tmp/x\"\nmode = \"fast\"\n"},
		{"nested-table", "[accounting.path]\nx = 1\n"},
		{"max-below-mib", "[accounting]\npath = \"/tmp/x\"\nmax_file_bytes = 1048575\n"},
		{"max-zero", "[accounting]\npath = \"/tmp/x\"\nmax_file_bytes = 0\n"},
		{"max-negative", "[accounting]\npath = \"/tmp/x\"\nmax_file_bytes = -1048576\n"},
		{"max-float", "[accounting]\npath = \"/tmp/x\"\nmax_file_bytes = 2097152.5\n"},
		{"max-string", "[accounting]\npath = \"/tmp/x\"\nmax_file_bytes = \"2097152\"\n"},
		{"missing-env", "[accounting]\npath = \"${ACCT_MISSING_DIR}/u.jsonl\"\n"},
	} {
		_, err := Load(writeTOML(t, accountingBase+"[[models]]\nname = \"a\"\nprovider = \"up\"\n"+tc.body+"\n"))
		if err == nil {
			t.Fatalf("%s must reject", tc.name)
		}
		if !strings.Contains(err.Error(), "accounting") && tc.name != "missing-env" {
			t.Fatalf("%s: error must name the accounting table: %v", tc.name, err)
		}
	}
	// A non-table [accounting] value is also rejected at the top level.
	_, err := Load(writeTOML(t, accountingBase+"[[models]]\nname = \"a\"\nprovider = \"up\"\naccounting = 3\n"))
	if err == nil {
		t.Fatal("non-table accounting must reject")
	}
}

func TestAccountingEnvExpansion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ACCT_DIR", dir)
	cfg, err := Load(writeTOML(t, accountingBase+`
[[models]]
name = "a"
provider = "up"
[accounting]
path = "${ACCT_DIR}/usage.jsonl"
max_file_bytes = 8388608
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Accounting.Path != filepath.Join(dir, "usage.jsonl") {
		t.Fatalf("env expansion wrong: %q", cfg.Accounting.Path)
	}
	if cfg.Accounting.MaxFileBytes != 8388608 {
		t.Fatalf("max_file_bytes wrong: %d", cfg.Accounting.MaxFileBytes)
	}
}
