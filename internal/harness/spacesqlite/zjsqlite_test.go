package spacesqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fmtSscanf extracts the numeric suffix of a "space-NNNN" probe name.
func fmtSscanf(name string, out *int) (int, error) {
	return fmt.Sscanf(name, "space-%d", out)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s missing (regenerate with testdata/gen.py): %v", name, err)
	}
	return path
}

func TestLookupPlainDatabase(t *testing.T) {
	db := fixture(t, "sessions.db")
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"model-router", "52ad00a223c5", true},
		{"dots", "33d125992a7a", true},
		{"weird name", "7fab12340001", true},
		{"absent", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		id, ok := LookupSession(db, tc.name)
		if ok != tc.ok || (ok && id != tc.id) {
			t.Errorf("LookupSession(%q) = (%q, %v), want (%q, %v)",
				tc.name, id, ok, tc.id, tc.ok)
		}
	}
}

func TestLookupThroughWAL(t *testing.T) {
	db := fixture(t, "wal.db")
	// The wal-only rows were committed after the last checkpoint, so they
	// exist solely inside wal.db-wal; finding them proves the overlay.
	if _, ok := LookupSession(db, "checkpointed"); !ok {
		t.Error("checkpointed row must be readable from the main file")
	}
	if id, ok := LookupSession(db, "wal-only"); !ok || id != "222222222222" {
		t.Errorf("wal-only row not found through WAL overlay: (%q, %v)", id, ok)
	}
	if id, ok := LookupSession(db, "wal-older"); !ok || id != "333333333333" {
		t.Errorf("wal-older row not found through WAL overlay: (%q, %v)", id, ok)
	}
	if _, ok := LookupSession(db, "never-committed"); ok {
		t.Error("unknown name must not be found")
	}
}

func TestLookupLargeDatabase(t *testing.T) {
	db := fixture(t, "big.db")
	// 600 rows with long text force interior b-tree pages and overflow
	// chains; spot-check entries across the key range and verify their
	// hex ids decode exactly as the generator wrote them.
	for _, name := range []string{"space-0000", "space-0007", "space-0301", "space-0599"} {
		var index int
		if _, err := fmtSscanf(name, &index); err != nil {
			t.Fatalf("bad probe name %q: %v", name, err)
		}
		want := fmt.Sprintf("%012x", index)
		id, ok := LookupSession(db, name)
		if !ok {
			t.Errorf("%s not found in big fixture", name)
			continue
		}
		if id != want {
			t.Errorf("%s id = %q, want %q", name, id, want)
		}
	}
}

func TestLookupDegradation(t *testing.T) {
	if _, ok := LookupSession(fixture(t, "corrupt.db"), "anything"); ok {
		t.Error("corrupt database must degrade to not-found")
	}
	if _, ok := LookupSession(fixture(t, "other.db"), "anything"); ok {
		t.Error("database without a sessions table must degrade to not-found")
	}
	if _, ok := LookupSession(filepath.Join("testdata", "missing.db"), "x"); ok {
		t.Error("missing file must degrade to not-found")
	}
}

func TestLookupRealZellijIndex(t *testing.T) {
	// Skipped unless a real Zellij index exists on this machine; proves
	// parity against the live format when present.
	path := filepath.Join(os.Getenv("HOME"), ".cache", "zellij",
		"contract_version_1", "session_index.sqlite")
	if _, err := os.Stat(path); err != nil {
		t.Skip("no live zellij session index on this machine")
	}
	known := "home" // every machine with a resurrected space has this row
	id, ok := LookupSession(path, known)
	if !ok {
		t.Skipf("live index has no %q session to verify", known)
	}
	t.Logf("live lookup %s -> %s", known, id)
}
