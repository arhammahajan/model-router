package spaceresume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLayoutTree(t *testing.T, root, session, metaName string, layoutText string) string {
	t.Helper()
	dir := filepath.Join(root, "abc123")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-metadata.kdl"),
		[]byte("name \""+metaName+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-layout.kdl"),
		[]byte(layoutText), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "session-layout.kdl")
}

func TestPrepareRestoreMetadataScan(t *testing.T) {
	cache := t.TempDir()
	info := filepath.Join(cache, "session_info")
	t.Setenv("ZJ_ZELLIJ_CACHE_DIR", info)
	before := "pane command=\"claude\" start_suspended=true {\n    args \"x\"\n}\n"
	layout := writeLayoutTree(t, info, "work", "work", before)
	if code := PrepareRestore("work"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	after, _ := os.ReadFile(layout)
	if strings.Contains(string(after), "start_suspended") {
		t.Errorf("not rewritten:\n%s", after)
	}
	if !strings.Contains(string(after), `command="zj"`) {
		t.Errorf("agent not wrapped:\n%s", after)
	}
	// No temp file left behind.
	if entries, _ := os.ReadDir(filepath.Dir(layout)); len(entries) != 2 {
		t.Errorf("stray files in layout dir")
	}
	// Unknown session is a silent 0.
	if code := PrepareRestore("missing"); code != 0 {
		t.Errorf("missing code = %d", code)
	}
}

func TestPrepareRestoreUnchanged(t *testing.T) {
	cache := t.TempDir()
	info := filepath.Join(cache, "session_info")
	t.Setenv("ZJ_ZELLIJ_CACHE_DIR", info)
	before := "pane command=\"bash\" {\n}\n"
	layout := writeLayoutTree(t, info, "plain", "plain", before)
	fiBefore, _ := os.Stat(layout)
	if code := PrepareRestore("plain"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	fiAfter, _ := os.Stat(layout)
	if !fiAfter.ModTime().Equal(fiBefore.ModTime()) {
		t.Error("unchanged layout was rewritten")
	}
}

func TestIndexedLayout(t *testing.T) {
	cache := t.TempDir()
	info := filepath.Join(cache, "session_info")
	t.Setenv("ZJ_ZELLIJ_CACHE_DIR", info)
	dir := filepath.Join(info, "dir-9")
	os.MkdirAll(dir, 0o755)
	layout := filepath.Join(dir, "session-layout.kdl")
	os.WriteFile(layout, []byte("pane command=\"bash\" {\n}\n"), 0o644)
	db := filepath.Join(cache, "session_index.sqlite")
	writeTestDB(t, db, "sessions",
		"CREATE TABLE sessions (id TEXT, name TEXT)",
		[][]any{{"dir-9", "indexed-space"}})
	if got := IndexedLayout("indexed-space"); got != layout {
		t.Errorf("got %q want %q", got, layout)
	}
	if got := IndexedLayout("nope"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestSessionInfoDirVariants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("ZJ_ZELLIJ_CACHE_DIR", "")
	dirs := SessionInfoDirectories()
	if len(dirs) == 0 {
		t.Fatal("no dirs")
	}
	base := filepath.Join(home, "cache", "zellij")
	os.MkdirAll(filepath.Join(base, "contract_version_9", "session_info"), 0o755)
	dirs = SessionInfoDirectories()
	found := false
	for _, d := range dirs {
		if strings.Contains(d, "contract_version_9") {
			found = true
		}
	}
	if !found {
		t.Errorf("contract glob not picked up: %v", dirs)
	}
	dbs := SessionIndexDatabases()
	if len(dbs) != 0 {
		t.Errorf("expected no databases, got %v", dbs)
	}
}

func TestPrepareRestoreReadError(t *testing.T) {
	cache := t.TempDir()
	info := filepath.Join(cache, "session_info")
	t.Setenv("ZJ_ZELLIJ_CACHE_DIR", info)
	dir := filepath.Join(info, "d1")
	os.MkdirAll(dir, 0o755)
	// Metadata present but layout is a directory: FindSavedLayout requires a
	// regular file so this session is simply not found -> 0.
	os.MkdirAll(filepath.Join(dir, "session-layout.kdl"), 0o755)
	os.WriteFile(filepath.Join(dir, "session-metadata.kdl"), []byte("name \"odd\"\n"), 0o644)
	outPath, _ := captureOutput(t)
	if code := PrepareRestore("odd"); code != 0 {
		t.Errorf("code = %d", code)
	}
	if got := readFile(t, outPath); got != "" {
		t.Errorf("stdout = %q", got)
	}
}
