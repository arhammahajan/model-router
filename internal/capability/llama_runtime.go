// Local server runtime: the closed engine kind carried on each
// LlamaServerSnapshot, never on per-model observations or cache rows.
//
// The discovery boundary derives it from raw SERVER props on each
// successful probe: the first whitespace-separated token of build_info,
// lowercased, identifies strata/polystrata as Strata; everything else,
// including absent/blank/malformed props, defaults to LlamaCpp on that
// live probe. Router/swap still ignore global modality/template/limit
// evidence and perform no fan-out; they may use global build_info ONLY as
// server engine identity, matching source. A cache-only server is
// Unobserved: no live probe has established it. Restore resets runtime to
// Unobserved. Membership (HasLlamaProvider) is separate from runtime.
//
// This file also owns the exact llama thinking budget map exactly once:
// minimal=1024, low=2048, medium=8192, high=16384, xhigh=32768. "max" is
// deliberately absent: llama.cpp has no maximum level, so a translated
// request for it is reported instead of sending nothing. Native unknown,
// max, non-string and null efforts are consumed without adding controls.
package capability

import (
	"strings"
	"unicode"
)

// LocalRuntime is the closed server engine kind. Unobserved means no live
// probe (or restore) has established an engine; it never becomes non-local.
type LocalRuntime uint8

const (
	// RuntimeUnobserved is a cache-only, restored or never-probed server.
	RuntimeUnobserved LocalRuntime = iota
	// RuntimeLlamaCpp is a live-probed llama.cpp server (the default).
	RuntimeLlamaCpp
	// RuntimeStrata is a live-probed Strata/polyStrata server.
	RuntimeStrata
)

// Valid reports whether the runtime is a closed kind member.
func (r LocalRuntime) Valid() bool {
	return r == RuntimeUnobserved || r == RuntimeLlamaCpp || r == RuntimeStrata
}

// LocalEngine derives the server runtime from raw server props on a live
// successful probe. Only build_info is read; unknown names, model rows,
// metadata.router, override enums and prefixes are never engine evidence.
// Absent/blank/malformed props default to LlamaCpp: the probe itself is
// the llama evidence. No I/O, no locks, no global state.
func LocalEngine(props map[string]any) LocalRuntime {
	var build string
	if props != nil {
		build, _ = props["build_info"].(string)
	}
	first := ""
	if fields := strings.FieldsFunc(build, func(r rune) bool {
		// Python str.split also treats the four information separators as whitespace.
		return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f
	}); len(fields) > 0 {
		first = strings.ToLower(fields[0])
	}
	if first == "strata" || first == "polystrata" {
		return RuntimeStrata
	}
	return RuntimeLlamaCpp
}

// llamaBudgets is the single owned budget table (section 1). It is read
// only through LlamaBudget and LlamaEffortValues below.
var llamaBudgets = map[string]int64{
	"minimal": 1024,
	"low":     2048,
	"medium":  8192,
	"high":    16384,
	"xhigh":   32768,
}

// LlamaBudget maps one literal effort level to its thinking budget.
// Unknown names (including "max") report false; the caller diagnoses the
// loss. No validation, capabilities, headers or body maps are involved.
func LlamaBudget(level string) (int64, bool) {
	b, ok := llamaBudgets[level]
	return b, ok
}

// LlamaEffortValues is the configured effort enum for LlamaCpp routes.
func LlamaEffortValues() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh"}
}

// StrataEffortValues is the inferred effort enum for Strata routes: the
// canonical native levels in PolyStrata's serve/frontend.py effort_kwargs
// (reviewed against 0.1.45). Other accepted spellings are aliases.
func StrataEffortValues() []string {
	return []string{"none", "low", "medium", "high"}
}

// LlamaMapKind is the pure literal mapper result: Absent (no control or
// unknown name), DisableThinking (none) or Budget (recognized level).
type LlamaMapKind uint8

const (
	// LlamaMapAbsent means emit nothing (absent or unknown/max name).
	LlamaMapAbsent LlamaMapKind = iota
	// LlamaMapDisable means render enable_thinking=false.
	LlamaMapDisable
	// LlamaMapBudget means render the mapped thinking budget.
	LlamaMapBudget
)

// LlamaMap is the mapper result. It knows no JSON paths, capabilities,
// HTTP, errors, loss headers or raw body maps.
type LlamaMap struct {
	Kind   LlamaMapKind
	Budget int64
}

// MapLlamaEffort maps one literal effort string. "none" disables;
// recognized budget levels map exactly; unknown names (including "max")
// return Absent with ok=false so the translated planner can diagnose a
// dropped loss. Native callers consume even the Absent case.
func MapLlamaEffort(level string) (LlamaMap, bool) {
	if level == "none" {
		return LlamaMap{Kind: LlamaMapDisable}, true
	}
	if b, ok := LlamaBudget(level); ok {
		return LlamaMap{Kind: LlamaMapBudget, Budget: b}, true
	}
	return LlamaMap{Kind: LlamaMapAbsent}, false
}

// Llama thinking-map provenance (exact wire strings from the contract).
const (
	llamaThinkingSource = "router.llama_thinking_map"
	llamaThinkingVerif  = "router request mapping, not an upstream report"
	strataCompatSource  = "strata.compat"
	strataCompatVerif   = "PolyStrata 0.1.45 serve/frontend.py effort_kwargs; identified by /props.build_info"
)

// ApplyLlamaMapper layers the runtime mapper facts onto a private base
// built from observed evidence. It runs only for live-known or
// stale-retained runtimes (LlamaCpp or Strata); Unobserved, static,
// shadow, losing and restored rows get no mapper base. Callers apply
// canonical operator overrides and Normalize afterwards, through the same
// pinned selector used by listing and request planning.
//
// For LlamaCpp: reasoning.effort.supported=true, values, and the native
// openai-chat/reasoning_effort parameter only (kind configured). For
// Strata: the effort fields (values none/low/medium/high) plus the
// toggle supported/native facts (kind inferred). Neither invents
// reasoning.supported, budget support/bounds/defaults, effort defaults or
// llama native toggle facts. An existing observed-effort false is
// superseded here exactly as in Python (before Normalize); observed times
// are preserved and mapper sources carry verification, never fabricated
// retrieval times.
func ApplyLlamaMapper(c *Contract, rt LocalRuntime, wall float64) {
	switch rt {
	case RuntimeLlamaCpp:
		putMapper(c, "reasoning.effort.supported", BoolValue(true), llamaThinkingSource, "supported", KindConfigured)
		if v, ok := StringListValue(LlamaEffortValues()); ok {
			putMapper(c, "reasoning.effort.values", v, llamaThinkingSource, "values", KindConfigured)
		}
		if np, ok := NewNativeParam("openai-chat", "reasoning_effort", nil); ok {
			putMapper(c, "reasoning.effort.native_parameter", NativeValue(np), llamaThinkingSource, "native_parameter", KindConfigured)
		}
		noteMapperSource(c, llamaThinkingSource, llamaThinkingVerif, wall)
	case RuntimeStrata:
		putMapper(c, "reasoning.effort.supported", BoolValue(true), strataCompatSource, "supported", KindInferred)
		if v, ok := StringListValue(StrataEffortValues()); ok {
			putMapper(c, "reasoning.effort.values", v, strataCompatSource, "values", KindInferred)
		}
		if np, ok := NewNativeParam("openai-chat", "reasoning_effort", nil); ok {
			putMapper(c, "reasoning.effort.native_parameter", NativeValue(np), strataCompatSource, "native_parameter", KindInferred)
		}
		putMapper(c, "reasoning.toggle.supported", BoolValue(true), strataCompatSource, "none", KindInferred)
		off := StringScalar("none")
		if np, ok := NewNativeParam("openai-chat", "reasoning_effort",
			map[string]NativeScalar{"off": off}); ok {
			putMapper(c, "reasoning.toggle.native_parameter", NativeValue(np), strataCompatSource, "none", KindInferred)
		}
		noteMapperSource(c, strataCompatSource, strataCompatVerif, wall)
	default:
		// Unobserved and unknown: no mapper facts. Static shadows, losing
		// rows and restored servers keep their observed/empty base.
	}
}

// putMapper stores one mapper leaf with conflict evidence preserved: an
// observed false effort report keeps its evidence while the mapper fact
// wins, exactly like Python's put before normalize. The leaf source is
// the mapper source plus the field name (doc section 5).
func putMapper(c *Contract, path string, v Value, source, field, kind string) {
	s := source + "." + field
	c.put(path, v, &s, kind)
}

// noteMapperSource records the mapper provenance with verification and no
// fabricated retrieval time.
func noteMapperSource(c *Contract, source, verification string, wall float64) {
	if c.sources == nil {
		return
	}
	v := verification
	c.sources[source] = &Source{verification: &v}
	_ = wall
}
