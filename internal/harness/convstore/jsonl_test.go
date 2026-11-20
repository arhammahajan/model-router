package convstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadHeadTailLinesSmallAndLarge(t *testing.T) {
	tmp := t.TempDir()

	// Small file fits inside headBytes+tailBytes.
	smallPath := filepath.Join(tmp, "small.jsonl")
	if err := os.WriteFile(smallPath, []byte("{\"n\":1}\n\n{\"n\":2}\n{\"n\":3}\n"), 0600); err != nil {
		t.Fatalf("write small: %v", err)
	}
	head, tail, err := ReadHeadTailLines(smallPath, 1024, 1024, 0, 0)
	if err != nil {
		t.Fatalf("ReadHeadTailLines small: %v", err)
	}
	if len(head) != 3 || tail != nil {
		t.Fatalf("small head=%v tail=%v", head, tail)
	}

	// Large file larger than headBytes+tailBytes.
	largePath := filepath.Join(tmp, "large.jsonl")
	var sb strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, "{\"idx\":%03d,\"pad\":\"%s\"}\n", i, strings.Repeat("x", 40))
	}
	if err := os.WriteFile(largePath, []byte(sb.String()), 0600); err != nil {
		t.Fatalf("write large: %v", err)
	}
	head, tail, err = ReadHeadTailLines(largePath, 512, 512, 3, 2)
	if err != nil {
		t.Fatalf("ReadHeadTailLines large: %v", err)
	}
	if len(head) != 3 || !strings.Contains(head[0], `"idx":000`) {
		t.Fatalf("unexpected large head: %v", head)
	}
	if len(tail) != 2 || !strings.Contains(tail[1], `"idx":399`) {
		t.Fatalf("unexpected large tail: %v", tail)
	}
}

func TestReadIncrementalJSONLAppendTruncateAndOversized(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "stream.jsonl")

	// Include an oversized line (> 256 KB) between two normal lines.
	hugeLine := `{"big":"` + strings.Repeat("A", DefaultMaxJSONLLineBytes+1024) + `"}`
	initial := "{\"id\":1}\n" + hugeLine + "\n{\"id\":2}\n"
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	var got1 []string
	off1, hash1, app1, err := ReadIncrementalJSONL(path, 0, "", 0, func(line []byte) error {
		got1 = append(got1, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("ReadIncrementalJSONL 1: %v", err)
	}
	if app1 {
		t.Fatalf("first read should have appended=false")
	}
	if off1 != int64(len(initial)) || hash1 == "" {
		t.Fatalf("off1=%d want %d, hash1=%q", off1, len(initial), hash1)
	}
	if len(got1) != 2 || got1[0] != `{"id":1}` || got1[1] != `{"id":2}` {
		t.Fatalf("expected oversized line skipped, got %v", got1)
	}

	// Append a third line -> should seek directly and return appended=true with only line 3.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("{\"id\":3}\n"); err != nil {
		f.Close()
		t.Fatalf("append write: %v", err)
	}
	f.Close()

	var got2 []string
	off2, hash2, app2, err := ReadIncrementalJSONL(path, off1, hash1, 0, func(line []byte) error {
		got2 = append(got2, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("ReadIncrementalJSONL 2: %v", err)
	}
	if !app2 {
		t.Fatalf("append read expected appended=true")
	}
	if len(got2) != 1 || got2[0] != `{"id":3}` {
		t.Fatalf("append read got %v, want only id=3", got2)
	}
	if off2 <= off1 || hash2 == hash1 {
		t.Fatalf("expected offset and hash to advance")
	}

	// Truncate/rewrite file -> prefix hash fails, falls back to full stream with appended=false.
	rewritten := "{\"id\":99}\n"
	if err := os.WriteFile(path, []byte(rewritten), 0600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var got3 []string
	off3, _, app3, err := ReadIncrementalJSONL(path, off2, hash2, 0, func(line []byte) error {
		got3 = append(got3, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("ReadIncrementalJSONL 3: %v", err)
	}
	if app3 {
		t.Fatalf("truncated file expected appended=false")
	}
	if off3 != int64(len(rewritten)) || len(got3) != 1 || got3[0] != `{"id":99}` {
		t.Fatalf("truncated read got off=%d lines=%v", off3, got3)
	}
}
