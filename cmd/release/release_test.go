package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveTargets(t *testing.T) {
	got, err := resolveTargets(nil, "darwin/arm64", true, "")
	if err != nil || len(got) != 1 || got[0] != "darwin/arm64" {
		t.Fatalf("default CGO targets = (%v, %v), want [darwin/arm64]", got, err)
	}

	got, err = resolveTargets(nil, "darwin/arm64", false, "")
	if err != nil || len(got) != len(supportedTargets) {
		t.Fatalf("default no-cgo targets = (%v, %v), want all %d", got, err, len(supportedTargets))
	}

	if _, err := resolveTargets([]string{"linux/amd64"}, "darwin/arm64", true, ""); err == nil {
		t.Fatal("expected error for cross-compiling with CGO enabled and no CC")
	}

	got, err = resolveTargets([]string{"linux/amd64"}, "darwin/arm64", true, "x86_64-linux-gnu-gcc")
	if err != nil || len(got) != 1 || got[0] != "linux/amd64" {
		t.Fatalf("cross-compile with CC = (%v, %v)", got, err)
	}

	if _, err := resolveTargets([]string{"windows/amd64"}, "darwin/arm64", false, ""); err == nil {
		t.Fatal("expected error for unsupported target")
	}
}

func TestWriteDeterministicTarGzAndVerify(t *testing.T) {
	dir := t.TempDir()
	files := map[string]archiveEntry{
		"routerd":                  {Data: []byte("bin"), Mode: 0o755},
		"config.example.toml":      {Data: []byte("cfg"), Mode: 0o600},
		"cliproxyapi.example.yaml": {Data: []byte("yaml"), Mode: 0o600},
		"README.md":                {Data: []byte("readme"), Mode: 0o644},
		"THIRD_PARTY_NOTICES":      {Data: []byte("notices"), Mode: 0o644},
	}
	p1 := filepath.Join(dir, "a.tar.gz")
	p2 := filepath.Join(dir, "b.tar.gz")
	if err := writeDeterministicTarGz(p1, "routerd-0.1.0-darwin-arm64", files); err != nil {
		t.Fatal(err)
	}
	if err := writeDeterministicTarGz(p2, "routerd-0.1.0-darwin-arm64", files); err != nil {
		t.Fatal(err)
	}
	b1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("deterministic tar.gz produced different bytes across runs")
	}

	extractDir := t.TempDir()
	if err := extractAndVerifyArchive(p1, extractDir); err != nil {
		t.Fatalf("extractAndVerifyArchive: %v", err)
	}
	for name, entry := range files {
		got, err := os.ReadFile(filepath.Join(extractDir, name))
		if err != nil {
			t.Fatalf("read extracted %s: %v", name, err)
		}
		if !bytes.Equal(got, entry.Data) {
			t.Fatalf("extracted %s = %q, want %q", name, got, entry.Data)
		}
	}
}

func TestBuildNotices(t *testing.T) {
	goroot := t.TempDir()
	if err := os.WriteFile(filepath.Join(goroot, "LICENSE"), []byte("GO BSD LICENSE"), 0o644); err != nil {
		t.Fatal(err)
	}
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modDir, "LICENSE.md"), []byte("MIT LICENSE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "NOTICE"), []byte("EXTRA NOTICE"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := buildNotices(goroot, []linkedModule{{Path: "example.com/pkg", Version: "v1.2.3", Dir: modDir}})
	if err != nil {
		t.Fatalf("buildNotices: %v", err)
	}
	text := string(got)
	for _, want := range []string{"GO BSD LICENSE", "example.com/pkg v1.2.3", "MIT LICENSE", "EXTRA NOTICE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notices missing %q:\n%s", want, text)
		}
	}

	emptyDir := t.TempDir()
	if _, err := buildNotices(goroot, []linkedModule{{Path: "example.com/nolicense", Version: "v0.1.0", Dir: emptyDir}}); err == nil {
		t.Fatal("expected error when linked module has no license file")
	}
}
