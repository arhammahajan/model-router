// Recorder tests: append, rotation keeping one generation, fail-open on
// unwritable paths, nil-safety, and the exact record field set/order.
package accounting

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func sampleRecord(status int) Record {
	return Record{
		TS:           1732023177.5,
		Model:        "public-model",
		Provider:     "up",
		Ingress:      "openai-chat",
		Upstream:     "anthropic",
		Status:       status,
		InputTokens:  7,
		OutputTokens: 3,
		CachedTokens: 2,
		DurationMS:   42,
		Stream:       true,
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestCompleteAppendsJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	r := New(path, DefaultMaxFileBytes)

	r.Complete(sampleRecord(200))
	r.Complete(sampleRecord(500))

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	var got Record
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line 1 not valid JSON: %v", err)
	}
	if got != sampleRecord(200) {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if !strings.HasSuffix(string(mustRead(t, path)), "\n") {
		t.Fatal("each record must end with a newline")
	}
}

func TestRecordFieldSetAndOrder(t *testing.T) {
	b, err := json.Marshal(sampleRecord(200))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ts":1732023177.5,"model":"public-model","provider":"up",` +
		`"ingress":"openai-chat","upstream":"anthropic","status":200,` +
		`"input_tokens":7,"output_tokens":3,"cached_tokens":2,` +
		`"duration_ms":42,"stream":true}`
	if string(b) != want {
		t.Fatalf("field order/set mismatch:\n got %s\nwant %s", b, want)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 11 {
		t.Fatalf("record must carry exactly the 11 documented fields, got %d", len(keys))
	}
	for _, banned := range []string{"prompt", "body", "header", "session", "user", "key"} {
		if strings.Contains(strings.ToLower(string(b)), banned) {
			t.Fatalf("record leaked forbidden field content: %s", b)
		}
	}
}

func TestCompleteRotatesKeepingOneGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	r := New(path, 8) // any record exceeds this bound

	r.Complete(sampleRecord(200))
	first := mustRead(t, path)

	r.Complete(sampleRecord(201))
	// First rotate: generation 1 moved to path.1, path fresh.
	if got := mustRead(t, path+".1"); got != first {
		t.Fatalf("rotated generation mismatch:\n got %q\nwant %q", got, first)
	}
	second := mustRead(t, path)

	r.Complete(sampleRecord(202))
	// Second rotate: only one generation kept, the older one dropped.
	if got := mustRead(t, path+".1"); got != second {
		t.Fatalf("older generation must be dropped:\n got %q\nwant %q", got, second)
	}
	third := mustRead(t, path)
	if third == second || !strings.HasSuffix(third, "\n") {
		t.Fatalf("active file must hold the newest record: %q", third)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage.jsonl.2")); !os.IsNotExist(err) {
		t.Fatal("rotation must keep exactly one generation (no .2)")
	}
}

func TestCompleteFailOpen(t *testing.T) {
	// Unwritable directory: every Complete drops its error and returns.
	r := New(filepath.Join(t.TempDir(), "missing-dir", "usage.jsonl"), DefaultMaxFileBytes)
	for i := 0; i < 3; i++ {
		r.Complete(sampleRecord(200))
	}
	// Path occupied by a directory: rotation/append failures are dropped.
	dir := t.TempDir()
	r2 := New(filepath.Join(dir, "usage.jsonl"), 8)
	if err := os.Mkdir(filepath.Join(dir, "usage.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	r2.Complete(sampleRecord(200))
	r2.Complete(sampleRecord(201))
	// Nil recorder: no panic, no-op.
	var nilRec *Recorder
	nilRec.Complete(sampleRecord(200))
}

func TestCompleteConcurrentAppendsAreLineSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	r := New(path, DefaultMaxFileBytes)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := sampleRecord(200)
			rec.DurationMS = int64(i)
			r.Complete(rec)
		}(i)
	}
	wg.Wait()
	lines := readLines(t, path)
	if len(lines) != 16 {
		t.Fatalf("want 16 complete lines, got %d", len(lines))
	}
	for _, line := range lines {
		var got Record
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("interleaved or torn line: %q (%v)", line, err)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
