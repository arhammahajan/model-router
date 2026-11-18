package capability

import (
	"encoding/json"
	"testing"
)

func llamaObsRow() map[string]any { return map[string]any{} }

func llamaObsProps() map[string]any { return map[string]any{} }

func llamaObsWire(t *testing.T, obs *LlamaModelObs, wall float64) map[string]any {
	t.Helper()
	c := ObservedLlama(obs, wall)
	rendered := Render(c, nil, StaticDiscovery(), wall)
	return mustJSON(t, rendered)
}

func llamaObsFact(t *testing.T, wire map[string]any, path string) map[string]any {
	t.Helper()
	return leaf(t, wire, path)
}

func TestLlamaObsModeGate(t *testing.T) {
	props := map[string]any{
		"modalities": map[string]any{"vision": true},
		"build_info": "llama.cpp",
	}
	row := llamaObsRow()
	single := ExtractLlamaObs(row, props, "single")
	w := llamaObsWire(t, single, 1000)
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != true {
		t.Fatalf("single must use global props, got %v", got)
	}
	srcs := w["sources"].(map[string]any)["llama.props"].(map[string]any)
	if srcs["available"] != true || srcs["retrieved_at"] != float64(1000) {
		t.Fatalf("props source shape: %v", srcs)
	}
	if srcs["server_build"] != "llama.cpp" || srcs["endpoint"] != "/props?autoload=false" {
		t.Fatalf("props provenance: %v", srcs)
	}
	for _, mode := range []string{"router", "llama-swap"} {
		obs := ExtractLlamaObs(row, props, mode)
		w := llamaObsWire(t, obs, 1000)
		if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != nil {
			t.Fatalf("%s must ignore global props, got %v", mode, got)
		}
		srcs := w["sources"].(map[string]any)["llama.props"].(map[string]any)
		if srcs["available"] != false {
			t.Fatalf("%s available must be false: %v", mode, srcs)
		}
		if srcs["retrieved_at"] != nil || srcs["server_build"] != nil {
			t.Fatalf("%s null keys must be present null: %v", mode, srcs)
		}
	}
	legacy := ExtractLlamaObs(row, props, "legacy")
	wl := llamaObsWire(t, legacy, 1000)
	if got := llamaObsFact(t, wl, "modalities.input.image")["value"]; got != true {
		t.Fatalf("legacy must use global props, got %v", got)
	}
}

func TestLlamaObsArch(t *testing.T) {
	mk := func(mods any) map[string]any {
		return map[string]any{"architecture": map[string]any{"input_modalities": mods, "output_modalities": mods}}
	}
	obs := ExtractLlamaObs(mk([]any{"text"}), nil, "single")
	w := llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.text")["value"]; got != true {
		t.Fatalf("text present: %v", got)
	}
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != false {
		t.Fatalf("absent member must be reported false, got %v", got)
	}
	obs = ExtractLlamaObs(mk([]any{"text", "image"}), nil, "single")
	w = llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != true {
		t.Fatalf("image present: %v", got)
	}
	obs = ExtractLlamaObs(mk([]any{}), nil, "single")
	w = llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.text")["value"]; got != false {
		t.Fatalf("empty list must report false, got %v", got)
	}
	obs = ExtractLlamaObs(map[string]any{}, nil, "single")
	w = llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.text")["value"]; got != nil {
		t.Fatalf("absent list must stay unknown, got %v", got)
	}
	obs = ExtractLlamaObs(mk([]any{"text", "text"}), nil, "single")
	w = llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.text")["value"]; got != nil {
		t.Fatalf("duplicate list invalid must stay unknown, got %v", got)
	}
	obs = ExtractLlamaObs(mk([]any{""}), nil, "single")
	w = llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.text")["value"]; got != nil {
		t.Fatalf("empty element invalid must stay unknown, got %v", got)
	}
}

func TestLlamaObsLegacyExact(t *testing.T) {
	row := map[string]any{"capabilities": []any{"multimodal"}}
	obs := ExtractLlamaObs(row, nil, "legacy")
	w := llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != true {
		t.Fatalf("legacy multimodal must infer image, got %v", got)
	}
	if got := llamaObsFact(t, w, "modalities.input.image")["kind"]; got != "inferred" {
		t.Fatalf("legacy kind: %v", got)
	}
	row = map[string]any{"capabilities": []any{"Multimodal"}}
	obs = ExtractLlamaObs(row, nil, "legacy")
	w = llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != nil {
		t.Fatalf("case mismatch must not infer, got %v", got)
	}
	row = map[string]any{"capabilities": []any{"multimodal"}}
	obs = ExtractLlamaObs(row, nil, "single")
	w = llamaObsWire(t, obs, 10)
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != nil {
		t.Fatalf("single must not apply legacy rule, got %v", got)
	}
}

func TestLlamaObsToolsConjunction(t *testing.T) {
	mk := func(m map[string]any) map[string]any {
		return map[string]any{"chat_template_caps": m}
	}
	obs := ExtractLlamaObs(nil, mk(map[string]any{"supports_tools": true}), "single")
	if got := llamaObsFact(t, llamaObsWire(t, obs, 1), "tools.supported")["value"]; got != true {
		t.Fatalf("one-sided true: %v", got)
	}
	obs = ExtractLlamaObs(nil, mk(map[string]any{"supports_tools": true, "supports_tool_calls": false}), "single")
	if got := llamaObsFact(t, llamaObsWire(t, obs, 1), "tools.supported")["value"]; got != false {
		t.Fatalf("true+false must be false: %v", got)
	}
	obs = ExtractLlamaObs(nil, mk(map[string]any{}), "single")
	if got := llamaObsFact(t, llamaObsWire(t, obs, 1), "tools.supported")["value"]; got != nil {
		t.Fatalf("both absent unknown: %v", got)
	}
	obs = ExtractLlamaObs(nil, mk(map[string]any{"supports_tools": "yes"}), "single")
	if got := llamaObsFact(t, llamaObsWire(t, obs, 1), "tools.supported")["value"]; got != nil {
		t.Fatalf("non-bool ignored: %v", got)
	}
	obs = ExtractLlamaObs(nil, mk(map[string]any{"supports_parallel_tool_calls": false}), "single")
	if got := llamaObsFact(t, llamaObsWire(t, obs, 1), "tools.parallel")["value"]; got != false {
		t.Fatalf("parallel false: %v", got)
	}
}

func TestLlamaObsReasoningNativeOnly(t *testing.T) {
	props := map[string]any{"chat_template_caps": map[string]any{"supports_reasoning_effort": true}}
	obs := ExtractLlamaObs(map[string]any{}, props, "single")
	w := llamaObsWire(t, obs, 7)
	if got := llamaObsFact(t, w, "reasoning.effort.supported")["value"]; got != true {
		t.Fatalf("effort supported: %v", got)
	}
	if got := llamaObsFact(t, w, "reasoning.supported")["value"]; got != true {
		t.Fatalf("reasoning supported: %v", got)
	}
	np := llamaObsFact(t, w, "reasoning.effort.native_parameter")["value"].(map[string]any)
	if np["protocol"] != "openai-chat" || np["path"] != "reasoning_effort" {
		t.Fatalf("native param: %v", np)
	}
	// No configured values/toggle/budget follow.
	for _, p := range []string{"reasoning.effort.values", "reasoning.toggle.supported", "reasoning.budget.supported"} {
		if got := llamaObsFact(t, w, p)["value"]; got != nil {
			t.Fatalf("%s must stay unknown, got %v", p, got)
		}
	}
	if _, has := w["sources"].(map[string]any)["router.llama_thinking_map"]; has {
		t.Fatalf("configured map must be absent")
	}
	props = map[string]any{"chat_template_caps": map[string]any{"supports_reasoning_effort": false}}
	obs = ExtractLlamaObs(map[string]any{}, props, "single")
	w = llamaObsWire(t, obs, 7)
	if got := llamaObsFact(t, w, "reasoning.effort.supported")["value"]; got != false {
		t.Fatalf("effort false: %v", got)
	}
	if got := llamaObsFact(t, w, "reasoning.effort.native_parameter")["value"]; got != nil {
		t.Fatalf("false must not invent native: %v", got)
	}
}

func TestLlamaObsStreamPresence(t *testing.T) {
	mk := func(stream any) map[string]any {
		return map[string]any{"default_generation_settings": map[string]any{"params": map[string]any{"stream": stream}}}
	}
	for _, v := range []any{true, false} {
		obs := ExtractLlamaObs(map[string]any{}, mk(v), "single")
		if got := llamaObsFact(t, llamaObsWire(t, obs, 1), "streaming")["value"]; got != true {
			t.Fatalf("stream %v must prove streaming, got %v", v, got)
		}
	}
	obs := ExtractLlamaObs(map[string]any{}, map[string]any{}, "single")
	if got := llamaObsFact(t, llamaObsWire(t, obs, 1), "streaming")["value"]; got != nil {
		t.Fatalf("absent stream unknown, got %v", got)
	}
}

func TestLlamaObsSamplingNumerics(t *testing.T) {
	mkparams := func(v any) map[string]any {
		return map[string]any{"default_generation_settings": map[string]any{"params": map[string]any{"temperature": v}}}
	}
	obs := ExtractLlamaObs(map[string]any{}, mkparams(json.Number("2")), "single")
	w := llamaObsWire(t, obs, 1)
	if got := llamaObsFact(t, w, "sampling.temperature.default")["value"]; got != float64(2) {
		t.Fatalf("int default: %v", got)
	}
	obs = ExtractLlamaObs(map[string]any{}, mkparams(json.Number("2.0")), "single")
	w = llamaObsWire(t, obs, 1)
	if got := llamaObsFact(t, w, "sampling.temperature.default")["value"]; got != float64(2) {
		t.Fatalf("float default: %v", got)
	}
	for _, bad := range []any{json.Number("2.5x"), true, "3"} {
		_ = bad
	}
	obs = ExtractLlamaObs(map[string]any{}, mkparams(json.Number("2.5")), "single")
	w = llamaObsWire(t, obs, 1)
	if got := llamaObsFact(t, w, "sampling.temperature.default")["value"]; got != float64(2.5) {
		t.Fatalf("2.5 valid sampling number: %v", got)
	}
	obs = ExtractLlamaObs(map[string]any{}, mkparams(true), "single")
	w = llamaObsWire(t, obs, 1)
	if got := llamaObsFact(t, w, "sampling.temperature.supported")["value"]; got != nil {
		t.Fatalf("bool sampling invalid must be unknown: %v", got)
	}
	obs = ExtractLlamaObs(map[string]any{}, mkparams("3"), "single")
	w = llamaObsWire(t, obs, 1)
	if got := llamaObsFact(t, w, "sampling.temperature.supported")["value"]; got != nil {
		t.Fatalf("string sampling invalid: %v", got)
	}
}

func TestLlamaObsLimitsProvenance(t *testing.T) {
	row := map[string]any{"meta": map[string]any{"n_ctx": json.Number("4096")}}
	obs := ExtractLlamaObs(row, nil, "single")
	w := llamaObsWire(t, obs, 5)
	f := llamaObsFact(t, w, "limits.context_window")
	if f["value"] != float64(4096) || f["source"] != "/v1/models.meta.n_ctx" || f["kind"] != "reported" {
		t.Fatalf("ctx provenance: %v", f)
	}
	row = map[string]any{
		"meta":   map[string]any{"n_ctx": json.Number("4096")},
		"status": map[string]any{"args": []any{"--ctx-size", "8192"}},
	}
	obs = ExtractLlamaObs(row, nil, "single")
	w = llamaObsWire(t, obs, 5)
	f = llamaObsFact(t, w, "limits.context_window")
	if f["value"] != float64(4096) {
		t.Fatalf("meta beats args: %v", f)
	}
	row = map[string]any{"status": map[string]any{"args": []any{"--ctx-size", "8192"}}}
	obs = ExtractLlamaObs(row, nil, "single")
	w = llamaObsWire(t, obs, 5)
	f = llamaObsFact(t, w, "limits.context_window")
	if f["value"] != float64(8192) || f["source"] != "/v1/models.status.args.ctx-size" || f["kind"] != "inferred" {
		t.Fatalf("args inferred: %v", f)
	}
	// Legacy winner.
	row = map[string]any{"max_model_len": json.Number("2048"), "meta": map[string]any{"n_ctx": json.Number("4096")}}
	obs = ExtractLlamaObs(row, nil, "legacy")
	w = llamaObsWire(t, obs, 5)
	f = llamaObsFact(t, w, "limits.context_window")
	if f["source"] != "/v1/models.max_model_len" {
		t.Fatalf("legacy winner: %v", f)
	}
	// Bigint exact, out-of-range unknown.
	row = map[string]any{"meta": map[string]any{"n_ctx": json.Number("9223372036854775807")}}
	obs = ExtractLlamaObs(row, nil, "single")
	w = llamaObsWire(t, obs, 5)
	if got := llamaObsFact(t, w, "limits.context_window")["value"]; got == nil {
		t.Fatalf("max int64 must be exact")
	}
	row = map[string]any{"meta": map[string]any{"n_ctx": json.Number("9223372036854775808")}}
	obs = ExtractLlamaObs(row, nil, "single")
	w = llamaObsWire(t, obs, 5)
	if got := llamaObsFact(t, w, "limits.context_window")["value"]; got != nil {
		t.Fatalf("out-of-range must stay unknown, got %v", got)
	}
}

func TestLlamaObsDefaultSpecialFitCtx(t *testing.T) {
	mk := func(v any) map[string]any {
		return map[string]any{"default_generation_settings": map[string]any{"params": map[string]any{"n_predict": v}}}
	}
	obs := ExtractLlamaObs(map[string]any{}, mk(json.Number("0")), "single")
	w := llamaObsWire(t, obs, 1)
	f := llamaObsFact(t, w, "limits.default_output_tokens")
	if f["value"] != float64(0) {
		t.Fatalf("zero stays 0: %v", f)
	}
	obs = ExtractLlamaObs(map[string]any{}, mk(json.Number("-1")), "single")
	w = llamaObsWire(t, obs, 1)
	f = llamaObsFact(t, w, "limits.default_output_tokens")
	if f["value"] != nil || f["special"] != "unlimited" {
		t.Fatalf("-1 special: %v", f)
	}
	obs = ExtractLlamaObs(map[string]any{}, mk(json.Number("-2")), "single")
	w = llamaObsWire(t, obs, 1)
	f = llamaObsFact(t, w, "limits.default_output_tokens")
	if f["value"] != nil || f["special"] != "until_context" {
		t.Fatalf("-2 special: %v", f)
	}
	row := map[string]any{"status": map[string]any{"args": []any{"--fit-ctx", "100"}}}
	obs = ExtractLlamaObs(row, nil, "single")
	w = llamaObsWire(t, obs, 1)
	lims := w["limits"].(map[string]any)
	cons, ok := lims["constraints"].(map[string]any)
	if !ok {
		t.Fatalf("constraints missing: %v", lims)
	}
	fit, ok := cons["fit_context_min"].(map[string]any)
	if !ok || fit["value"] != float64(100) || fit["kind"] != "constraint" {
		t.Fatalf("fit ctx: %v", cons)
	}
	// Last-occurrence invalid never revives earlier.
	row = map[string]any{"status": map[string]any{"args": []any{"--ctx-size", "100", "--ctx-size", "auto"}}}
	obs = ExtractLlamaObs(row, nil, "single")
	w = llamaObsWire(t, obs, 1)
	if got := llamaObsFact(t, w, "limits.context_window")["value"]; got != nil {
		t.Fatalf("final invalid must not revive earlier, got %v", got)
	}
	row = map[string]any{"status": map[string]any{"args": []any{"--ctx-size=200"}}}
	obs = ExtractLlamaObs(row, nil, "single")
	w = llamaObsWire(t, obs, 1)
	if got := llamaObsFact(t, w, "limits.context_window")["value"]; got != float64(200) {
		t.Fatalf("equals syntax: %v", got)
	}
}

func TestLlamaObsClone(t *testing.T) {
	obs := ExtractLlamaObs(map[string]any{"meta": map[string]any{"n_ctx": json.Number("10")}}, map[string]any{"build_info": "b"}, "single")
	cp := obs.Clone()
	if cp == obs {
		t.Fatalf("clone must copy")
	}
	w1 := llamaObsWire(t, obs, 3)
	w2 := llamaObsWire(t, cp, 3)
	if llamaObsFact(t, w1, "limits.context_window")["value"] != llamaObsFact(t, w2, "limits.context_window")["value"] {
		t.Fatalf("clone mismatch")
	}
	if obs := ExtractLlamaObs(map[string]any{}, nil, "single"); obs.Clone() == nil {
		t.Fatalf("clone non-nil")
	}
	var nilObs *LlamaModelObs
	if nilObs.Clone() != nil {
		t.Fatalf("nil clone must stay nil")
	}
	c := ObservedLlama(nil, 9)
	if got := mustJSON(t, c)["sources"].(map[string]any); len(got) != 1 {
		t.Fatalf("nil obs must carry no llama sources: %v", got)
	}
}
