// Llama limit config: typed context_fallback + limit_overrides.
//
// Owns the closed validation for explicit llama limit policy. Absent
// fallback stays unknown (no Python implicit default is invented).
// Explicit positive int64 fallback applies only when observed context is
// unknown. Overrides match raw upstream IDs exactly (before prefix and
// local rename), keys are closed to LLAMA_LIMIT_FIELDS, values are exact
// int64 (no bool/float/string coercion) or explicit null withdraw.
package config

import (
	"fmt"
	"sort"
	"strings"
)

// LlamaLimitFields is the closed limit vocabulary (mirrors Python
// LLAMA_LIMIT_FIELDS).
var LlamaLimitFields = []string{
	"context_window",
	"trained_context_window",
	"max_input_tokens",
	"max_output_tokens",
	"default_output_tokens",
}

func isLlamaLimitField(k string) bool {
	for _, f := range LlamaLimitFields {
		if k == f {
			return true
		}
	}
	return false
}

func llamaLimitMinimum(key string) int64 {
	if key == "default_output_tokens" {
		return 0
	}
	return 1
}

// CloneLlamaLimitOverrides deep-copies the override table.
func CloneLlamaLimitOverrides(in map[string]map[string]*int64) map[string]map[string]*int64 {
	if in == nil {
		return nil
	}
	out := make(map[string]map[string]*int64, len(in))
	for model, fields := range in {
		cp := make(map[string]*int64, len(fields))
		for k, v := range fields {
			if v == nil {
				cp[k] = nil
			} else {
				nv := *v
				cp[k] = &nv
			}
		}
		out[model] = cp
	}
	return out
}

func copyInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// parseLlamaLimits validates context_fallback + limit_overrides startup
// value-free before any I/O. TOML integers arrive as int64, JSON integers
// normalize to int64, floats to float64, null to nil; only exact int64
// satisfies a limit, nil satisfies an explicit withdraw.
func parseLlamaLimits(tab map[string]any) (*int64, map[string]map[string]*int64, error) {
	const where = "llama_cpp"
	var fallback *int64
	if raw, ok := tab["context_fallback"]; ok {
		n, ok := asInt(raw)
		if !ok || n <= 0 {
			return nil, nil, fmt.Errorf("%s: 'context_fallback' must be a positive integer", where)
		}
		fallback = copyInt64Ptr(&n)
	}
	var overrides map[string]map[string]*int64
	if raw, ok := tab["limit_overrides"]; ok {
		outer, ok := asTable(raw)
		if !ok {
			return nil, nil, fmt.Errorf("%s.limit_overrides: must be a table", where)
		}
		overrides = make(map[string]map[string]*int64, len(outer))
		names := make([]string, 0, len(outer))
		for k := range outer {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, modelID := range names {
			label := fmt.Sprintf("%s.limit_overrides[%q]", where, modelID)
			if modelID == "" || modelID != strings.TrimSpace(modelID) {
				return nil, nil, fmt.Errorf("%s: model ID must be a nonempty string without surrounding whitespace", label)
			}
			inner, ok := asTable(outer[modelID])
			if !ok {
				return nil, nil, fmt.Errorf("%s: must be a table/object", label)
			}
			fields := make(map[string]*int64, len(inner))
			keys := make([]string, 0, len(inner))
			for k := range inner {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if !isLlamaLimitField(key) {
					return nil, nil, fmt.Errorf("%s: unknown limit %q", label, key)
				}
				minimum := llamaLimitMinimum(key)
				val := inner[key]
				if val == nil {
					fields[key] = nil
					continue
				}
				n, ok := asInt(val)
				if !ok || n < minimum {
					return nil, nil, fmt.Errorf("%s.%s: must be an integer >= %d or null", label, key, minimum)
				}
				nv := n
				fields[key] = &nv
			}
			if len(fields) != 0 {
				overrides[modelID] = fields
			}
		}
		if len(overrides) == 0 {
			overrides = nil
		}
	}
	return fallback, overrides, nil
}
