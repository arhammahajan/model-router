package capability

import (
	"encoding/json"
	"testing"
)

// Focused limit-policy facts: observed vs fallback vs null vs 0, exact
// bigints, -1/-2 specials, override clearing, absent preservation.

func limPolicy(fallback *int64, ov map[string]map[string]*int64) *LlamaLimitPolicy {
	return NewLlamaLimitPolicy(fallback, ov)
}

func i64(v int64) *int64 { return &v }

func limWire(t *testing.T, c Contract) map[string]any {
	t.Helper()
	raw, err := c.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func limFact(t *testing.T, wire map[string]any, key string) map[string]any {
	t.Helper()
	lims, ok := wire["limits"].(map[string]any)
	if !ok {
		t.Fatalf("no limits: %v", wire)
	}
	f, ok := lims[key].(map[string]any)
	if !ok {
		t.Fatalf("no %s: %v", key, lims)
	}
	return f
}

func TestLlamaFallbackOnlyWhenUnknown(t *testing.T) {
	fb := int64(64000)
	// Observed wins; fallback ignored.
	obs := ExtractLlamaObs(map[string]any{"meta": map[string]any{"n_ctx": json.Number("4096")}}, nil, "single")
	base := ObservedLlama(obs, 10)
	ApplyLlamaLimits(&base, "m", limPolicy(&fb, nil))
	w := limWire(t, Render(base, nil, StaticDiscovery(), 10))
	f := limFact(t, w, "context_window")
	if f["value"] != float64(4096) || f["source"] != "/v1/models.meta.n_ctx" || f["kind"] != "reported" {
		t.Fatalf("observed must win: %v", f)
	}
	// Unknown observes fallback with fallback kind/source.
	obs = ExtractLlamaObs(map[string]any{}, nil, "single")
	base = ObservedLlama(obs, 10)
	ApplyLlamaLimits(&base, "m", limPolicy(&fb, nil))
	w = limWire(t, Render(base, nil, StaticDiscovery(), 10))
	f = limFact(t, w, "context_window")
	if f["value"] != float64(64000) || f["source"] != LlamaFallbackSource || f["kind"] != KindFallback {
		t.Fatalf("fallback fact wrong: %v", f)
	}
	// Absent fallback keeps unknown (no implicit default).
	obs = ExtractLlamaObs(map[string]any{}, nil, "single")
	base = ObservedLlama(obs, 10)
	ApplyLlamaLimits(&base, "m", limPolicy(nil, nil))
	w = limWire(t, Render(base, nil, StaticDiscovery(), 10))
	if got := limFact(t, w, "context_window")["value"]; got != nil {
		t.Fatalf("absent fallback must stay unknown, got %v", got)
	}
}

func TestLlamaOverrideBeatsObservedAndFallback(t *testing.T) {
	fb := int64(64000)
	ov := map[string]map[string]*int64{"m": {"context_window": i64(111)}}
	obs := ExtractLlamaObs(map[string]any{"meta": map[string]any{"n_ctx": json.Number("4096")}}, nil, "single")
	base := ObservedLlama(obs, 10)
	ApplyLlamaLimits(&base, "m", limPolicy(&fb, ov))
	w := limWire(t, Render(base, nil, StaticDiscovery(), 10))
	f := limFact(t, w, "context_window")
	if f["value"] != float64(111) || f["source"] != LlamaOverrideSource || f["kind"] != KindOverride {
		t.Fatalf("override must win: %v", f)
	}
	// Exact bigint preserved.
	big := int64(9223372036854775807)
	ov = map[string]map[string]*int64{"m": {"context_window": &big}}
	obs = ExtractLlamaObs(map[string]any{}, nil, "single")
	base = ObservedLlama(obs, 10)
	ApplyLlamaLimits(&base, "m", limPolicy(nil, ov))
	w = limWire(t, Render(base, nil, StaticDiscovery(), 10))
	if got := limFact(t, w, "context_window")["value"]; got != float64(9223372036854775807) {
		t.Fatalf("bigint must be exact: %v", got)
	}
	// Other models untouched.
	base = ObservedLlama(obs, 10)
	ApplyLlamaLimits(&base, "other", limPolicy(nil, ov))
	w = limWire(t, Render(base, nil, StaticDiscovery(), 10))
	if got := limFact(t, w, "context_window")["value"]; got != nil {
		t.Fatalf("non-matching upstream must stay unknown, got %v", got)
	}
}

func TestLlamaOverrideNullAndZeroClearSpecial(t *testing.T) {
	mk := func(v any) map[string]any {
		return map[string]any{"default_generation_settings": map[string]any{"params": map[string]any{"n_predict": v}}}
	}
	// Observed -1 carries unlimited special.
	obs := ExtractLlamaObs(map[string]any{}, mk(json.Number("-1")), "single")
	base := ObservedLlama(obs, 1)
	w := limWire(t, Render(base, nil, StaticDiscovery(), 1))
	if got := limFact(t, w, "default_output_tokens"); got["special"] != "unlimited" {
		t.Fatalf("expected unlimited special: %v", got)
	}
	// Override 0 clears special.
	base = ObservedLlama(obs, 1)
	ApplyLlamaLimits(&base, "m", limPolicy(nil, map[string]map[string]*int64{"m": {"default_output_tokens": i64(0)}}))
	w = limWire(t, Render(base, nil, StaticDiscovery(), 1))
	f := limFact(t, w, "default_output_tokens")
	if f["value"] != float64(0) || f["kind"] != KindOverride {
		t.Fatalf("override 0 must clear special: %v", f)
	}
	if _, has := f["special"]; has {
		t.Fatalf("special must be cleared: %v", f)
	}
	// Override null withdraws and clears special.
	base = ObservedLlama(obs, 1)
	ApplyLlamaLimits(&base, "m", limPolicy(nil, map[string]map[string]*int64{"m": {"default_output_tokens": nil}}))
	w = limWire(t, Render(base, nil, StaticDiscovery(), 1))
	f = limFact(t, w, "default_output_tokens")
	if f["value"] != nil || f["kind"] != KindOverride || f["source"] != LlamaOverrideSource {
		t.Fatalf("null override must withdraw: %v", f)
	}
	if _, has := f["special"]; has {
		t.Fatalf("null must clear special: %v", f)
	}
	// Absent override preserves evidence (including special).
	base = ObservedLlama(obs, 1)
	ApplyLlamaLimits(&base, "m", limPolicy(nil, nil))
	w = limWire(t, Render(base, nil, StaticDiscovery(), 1))
	if got := limFact(t, w, "default_output_tokens"); got["special"] != "unlimited" {
		t.Fatalf("absent override must preserve evidence: %v", got)
	}
}

func TestLlamaPolicyCloneIsolation(t *testing.T) {
	fb := int64(5)
	p := limPolicy(&fb, map[string]map[string]*int64{"m": {"context_window": i64(7)}})
	cp := p.Clone()
	*cp.Fallback = 9
	*cp.Overrides["m"]["context_window"] = 8
	if *p.Fallback != 5 || *p.Overrides["m"]["context_window"] != 7 {
		t.Fatal("clone must isolate")
	}
	if p.Empty() || (limPolicy(nil, nil)).Empty() != true {
		t.Fatal("empty mismatch")
	}
	var nilPolicy *LlamaLimitPolicy
	if nilPolicy.Clone() != nil || !nilPolicy.Empty() || nilPolicy.Covers("m") {
		t.Fatal("nil policy must stay nil/empty")
	}
}
