// Package accounting appends metadata-only usage records for completed
// generations to a rotating JSONL file, opt-in via the router's
// [accounting] config section.
//
// Records never carry prompt text, bodies, headers, session ids or any
// identity: only the fixed field set below. Complete is fail-open —
// every filesystem error is dropped so metering can never fail a request.
package accounting

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// DefaultMaxFileBytes mirrors the [accounting] config default applied when
// max_file_bytes is omitted: 100 MiB. New falls back to it for a
// non-positive bound so a programmatically built recorder stays sane.
const DefaultMaxFileBytes = 104857600

// Record is one completed generation. Field order is the contract: the
// JSONL object keys appear exactly in this order, and no field beyond
// this set may ever be added without changing the documented schema.
type Record struct {
	TS           float64 `json:"ts"`    // unix seconds
	Model        string  `json:"model"` // public route name
	Provider     string  `json:"provider"`
	Ingress      string  `json:"ingress"`  // dialect of the client request
	Upstream     string  `json:"upstream"` // dialect sent upstream
	Status       int     `json:"status"`   // http status
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	DurationMS   int64   `json:"duration_ms"`
	Stream       bool    `json:"stream"`
}

// Recorder is a mutex-guarded append-only JSONL writer. A nil Recorder is
// usable: Complete is a no-op, so call sites need no extra guard.
type Recorder struct {
	mu      sync.Mutex
	path    string
	rotated string
	max     int64
}

// New builds a Recorder appending to path, rotating to path.1 (one kept
// generation) once the file exceeds maxFileBytes. It performs no failing
// I/O: a best-effort parent-directory create runs here and every later
// error is dropped at append time.
func New(path string, maxFileBytes int64) *Recorder {
	if maxFileBytes <= 0 {
		maxFileBytes = DefaultMaxFileBytes
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	return &Recorder{path: path, rotated: path + ".1", max: maxFileBytes}
}

// Complete appends one record as a single JSONL line. When the current
// file exceeds the bound, it first rotates path to path.1, dropping the
// previous generation. Errors are dropped: metering never fails a
// request, and the rotation/append pair is one critical section so
// concurrent completes never interleave.
func (r *Recorder) Complete(rec Record) {
	if r == nil {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if size(r.path) > r.max {
		// One generation kept: the old path.1 is discarded, the current
		// file becomes the new path.1, and the append starts fresh.
		_ = os.Remove(r.rotated)
		_ = os.Rename(r.path, r.rotated)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func size(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
