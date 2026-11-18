// Accounting table: optional [accounting] with path and max_file_bytes.
// Absent table means disabled (nil): no recorder, no overhead. The
// section mirrors [search]/[catalog]/[discovery_cache] nil-gating.
package config

import (
	"fmt"
	"strings"
)

// AccountingConfig is the validated [accounting] table.
type AccountingConfig struct {
	// Path is the required non-empty JSONL target. ${ENV} expansion runs
	// in the shared config loader before validation, per its rules.
	Path string
	// MaxFileBytes bounds the active file; rotation keeps one generation.
	MaxFileBytes int64
}

const (
	// DefaultAccountingMaxFileBytes is 100 MiB, applied once the table
	// exists.
	DefaultAccountingMaxFileBytes = 104857600
	// MinAccountingMaxFileBytes is the 1 MiB floor: smaller bounds are a
	// startup error so the active file cannot rotate per record.
	MinAccountingMaxFileBytes = 1048576
)

var accountingKeys = map[string]bool{"path": true, "max_file_bytes": true}

func fillAccounting(tab map[string]any) (*AccountingConfig, error) {
	if err := rejectUnknown(tab, accountingKeys, "accounting",
		" (supported: path, max_file_bytes)"); err != nil {
		return nil, err
	}
	rawPath, ok := tab["path"]
	if !ok {
		return nil, fmt.Errorf("accounting.path is required")
	}
	path, ok := asString(rawPath)
	if !ok || strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("accounting.path must be a nonempty path")
	}
	// ${ENV} expansion already ran in the loader; a leftover ${...} here
	// means the value reached validation unexpanded, which never happens
	// through Load. Keep the literal as-is for programmatic builds.
	out := &AccountingConfig{
		Path:         strings.TrimSpace(path),
		MaxFileBytes: DefaultAccountingMaxFileBytes,
	}
	if v, ok := tab["max_file_bytes"]; ok {
		n, ok := asInt(v)
		if !ok {
			return nil, fmt.Errorf("accounting: 'max_file_bytes' must be a positive integer")
		}
		if n < MinAccountingMaxFileBytes {
			return nil, fmt.Errorf("accounting: 'max_file_bytes' must be at least %d (1 MiB)", MinAccountingMaxFileBytes)
		}
		out.MaxFileBytes = n
	}
	return out, nil
}
