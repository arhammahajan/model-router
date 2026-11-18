package capability

import (
	"encoding/json"
	"math"
	"testing"
)

// Independent review of the observed-only llama vertical (contract 4/5).
// Each case fails if a source-observed rule or boundary fidelity breaks.

func TestLlamaCapRevArchOutputIndependent(t *testing.T) {
	row := map[string]any{
		"architecture": map[string]any{
			"input_modalities":  []any{"text"},
			"output_modalities": []any{"text", "image"},
		},
	}
	w := llamaObsWire(t, ExtractLlamaObs(row, nil, "single"), 10)
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != false {
		t.Fatalf("input image absent must be reported false, got %v", got)
	}
	if got := llamaObsFact(t, w, "modalities.output.image")["value"]; got != true {
		t.Fatalf("output image present must be true, got %v", got)
	}
	// Invalid input list stays unknown while valid output still reports.
	row = map[string]any{
		"architecture": map[string]any{
			"input_modalities":  []any{"text", "text"},
			"output_modalities": []any{"text"},
		},
	}
	w = llamaObsWire(t, ExtractLlamaObs(row, nil, "single"), 10)
	if got := llamaObsFact(t, w, "modalities.input.text")["value"]; got != nil {
		t.Fatalf("dup input must stay unknown, got %v", got)
	}
	if got := llamaObsFact(t, w, "modalities.output.text")["value"]; got != true {
		t.Fatalf("valid output must still report, got %v", got)
	}
}

func TestLlamaCapRevEmptyPropsSingleUnknown(t *testing.T) {
	obs := ExtractLlamaObs(map[string]any{}, map[string]any{}, "single")
	w := llamaObsWire(t, obs, 1000)
	srcs := w["sources"].(map[string]any)["llama.props"].(map[string]any)
	if srcs["available"] != false {
		t.Fatalf("empty props available must be false: %v", srcs)
	}
	if srcs["retrieved_at"] != nil {
		t.Fatalf("empty props retrieved_at must be null: %v", srcs)
	}
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != nil {
		t.Fatalf("empty props must leave image unknown, got %v", got)
	}
}

func TestLlamaCapRevBuildInfoNonString(t *testing.T) {
	props := map[string]any{"build_info": json.Number("123"), "x": 1}
	w := llamaObsWire(t, ExtractLlamaObs(map[string]any{}, props, "single"), 5)
	srcs := w["sources"].(map[string]any)["llama.props"].(map[string]any)
	if srcs["available"] != true {
		t.Fatalf("nonempty props available must be true: %v", srcs)
	}
	if srcs["server_build"] != nil {
		t.Fatalf("non-string build_info must be null: %v", srcs)
	}
}

func TestLlamaCapRevPropsModalitiesNonBool(t *testing.T) {
	props := map[string]any{"modalities": map[string]any{"vision": "yes", "audio": 1}}
	w := llamaObsWire(t, ExtractLlamaObs(map[string]any{}, props, "single"), 5)
	if got := llamaObsFact(t, w, "modalities.input.image")["value"]; got != nil {
		t.Fatalf("non-bool vision must stay unknown, got %v", got)
	}
	if got := llamaObsFact(t, w, "modalities.input.audio")["value"]; got != nil {
		t.Fatalf("non-bool audio must stay unknown, got %v", got)
	}
}

func TestLlamaCapRevSamplingFlatAndList(t *testing.T) {
	// Flat defaults carry usable sampling with a flat source path.
	props := map[string]any{"default_generation_settings": map[string]any{"temperature": json.Number("0.7")}}
	w := llamaObsWire(t, ExtractLlamaObs(map[string]any{}, props, "single"), 1)
	f := llamaObsFact(t, w, "sampling.temperature.default")
	if f["value"] != float64(0.7) {
		t.Fatalf("flat temperature default: %v", f)
	}
	if f["source"] != "llama.props.default_generation_settings.temperature" {
		t.Fatalf("flat sampling source: %v", f)
	}
	// Flat stream:false still proves streaming.
	props = map[string]any{"default_generation_settings": map[string]any{"stream": false}}
	w = llamaObsWire(t, ExtractLlamaObs(map[string]any{}, props, "single"), 1)
	if got := llamaObsFact(t, w, "streaming")["value"]; got != true {
		t.Fatalf("flat stream:false must prove streaming, got %v", got)
	}
	// Valid samplers list reports; duplicates stay unknown.
	props = map[string]any{"default_generation_settings": map[string]any{
		"params": map[string]any{"samplers": []any{"a", "b"}},
	}}
	w = llamaObsWire(t, ExtractLlamaObs(map[string]any{}, props, "single"), 1)
	if got := llamaObsFact(t, w, "sampling.samplers.supported")["value"]; got != true {
		t.Fatalf("samplers list must report supported, got %v", got)
	}
	props = map[string]any{"default_generation_settings": map[string]any{
		"params": map[string]any{"samplers": []any{"a", "a"}},
	}}
	w = llamaObsWire(t, ExtractLlamaObs(map[string]any{}, props, "single"), 1)
	if got := llamaObsFact(t, w, "sampling.samplers.supported")["value"]; got != nil {
		t.Fatalf("dup samplers must stay unknown, got %v", got)
	}
	props = map[string]any{"default_generation_settings": map[string]any{
		"params": map[string]any{"samplers": []any{"a", ""}},
	}}
	w = llamaObsWire(t, ExtractLlamaObs(map[string]any{}, props, "single"), 1)
	if got := llamaObsFact(t, w, "sampling.samplers.supported")["value"]; got != nil {
		t.Fatalf("empty samplers element must stay unknown, got %v", got)
	}
}

func TestLlamaCapRevLimitsFullChain(t *testing.T) {
	// defaults.n_ctx beats props.n_ctx when meta absent.
	row := map[string]any{}
	props := map[string]any{
		"default_generation_settings": map[string]any{"n_ctx": json.Number("2048")},
		"n_ctx":                       json.Number("4096"),
	}
	w := llamaObsWire(t, ExtractLlamaObs(row, props, "single"), 5)
	if got := llamaObsFact(t, w, "limits.context_window")["value"]; got != float64(2048) {
		t.Fatalf("defaults.n_ctx must beat props.n_ctx, got %v", got)
	}
	// Trained: meta beats props; router row.context_length is gated.
	row = map[string]any{
		"meta":           map[string]any{"n_ctx_train": json.Number("100")},
		"context_length": json.Number("200"),
	}
	props = map[string]any{"n_ctx_train": json.Number("300")}
	w = llamaObsWire(t, ExtractLlamaObs(row, props, "single"), 5)
	f := llamaObsFact(t, w, "limits.trained_context_window")
	if f["value"] != float64(100) || f["source"] != "/v1/models.meta.n_ctx_train" {
		t.Fatalf("trained meta must win for single: %v", f)
	}
	w = llamaObsWire(t, ExtractLlamaObs(row, props, "router"), 5)
	f = llamaObsFact(t, w, "limits.trained_context_window")
	if f["value"] != float64(100) {
		t.Fatalf("trained meta still wins for router: %v", f)
	}
	row = map[string]any{"context_length": json.Number("200")}
	w = llamaObsWire(t, ExtractLlamaObs(row, nil, "router"), 5)
	f = llamaObsFact(t, w, "limits.trained_context_window")
	if f["value"] != float64(200) || f["source"] != "/v1/models.context_length" {
		t.Fatalf("router uses context_length: %v", f)
	}
	w = llamaObsWire(t, ExtractLlamaObs(row, nil, "single"), 5)
	if got := llamaObsFact(t, w, "limits.trained_context_window")["value"]; got != nil {
		t.Fatalf("single must ignore row context_length, got %v", got)
	}
	// Hard chain meta > row > metadata > props.
	row = map[string]any{
		"meta":     map[string]any{"max_input_tokens": json.Number("10")},
		"metadata": map[string]any{"max_input_tokens": json.Number("30")},
	}
	row["max_input_tokens"] = json.Number("20")
	props = map[string]any{"max_input_tokens": json.Number("40")}
	w = llamaObsWire(t, ExtractLlamaObs(row, props, "single"), 5)
	f = llamaObsFact(t, w, "limits.max_input_tokens")
	if f["value"] != float64(10) || f["source"] != "/v1/models.meta.max_input_tokens" {
		t.Fatalf("hard meta must win: %v", f)
	}
	// Default output precedence: nested n_predict > flat n_predict >
	// nested max_tokens > flat max_tokens > arg.
	mkProps := func(nestedN, flatN, nestedM, flatM any) map[string]any {
		d := map[string]any{}
		if flatN != nil {
			d["n_predict"] = flatN
		}
		if flatM != nil {
			d["max_tokens"] = flatM
		}
		if nestedN != nil || nestedM != nil {
			p := map[string]any{}
			if nestedN != nil {
				p["n_predict"] = nestedN
			}
			if nestedM != nil {
				p["max_tokens"] = nestedM
			}
			d["params"] = p
		}
		return map[string]any{"default_generation_settings": d}
	}
	w = llamaObsWire(t, ExtractLlamaObs(nil, mkProps(json.Number("11"), json.Number("22"), json.Number("33"), json.Number("44")), "single"), 1)
	f = llamaObsFact(t, w, "limits.default_output_tokens")
	if f["value"] != float64(11) || f["source"] != "/props.default_generation_settings.params.n_predict" {
		t.Fatalf("nested n_predict must win: %v", f)
	}
	w = llamaObsWire(t, ExtractLlamaObs(nil, mkProps(nil, json.Number("22"), json.Number("33"), json.Number("44")), "single"), 1)
	f = llamaObsFact(t, w, "limits.default_output_tokens")
	if f["value"] != float64(22) || f["source"] != "/props.default_generation_settings.n_predict" {
		t.Fatalf("flat n_predict must beat nested max_tokens: %v", f)
	}
	// Integral float exactness; bool/string/NaN/Inf stay unknown.
	row = map[string]any{"meta": map[string]any{"n_ctx": json.Number("4096.0")}}
	w = llamaObsWire(t, ExtractLlamaObs(row, nil, "single"), 5)
	if got := llamaObsFact(t, w, "limits.context_window")["value"]; got != float64(4096) {
		t.Fatalf("integral float must be exact, got %v", got)
	}
	for _, bad := range []any{true, "4096", math.NaN(), math.Inf(1)} {
		row = map[string]any{"meta": map[string]any{"n_ctx": bad}}
		w = llamaObsWire(t, ExtractLlamaObs(row, nil, "single"), 5)
		if got := llamaObsFact(t, w, "limits.context_window")["value"]; got != nil {
			t.Fatalf("bad limit %v must stay unknown, got %v", bad, got)
		}
	}
	// -3 below minimum stays unknown; absent chain stays unknown (no fallback).
	row = map[string]any{"status": map[string]any{"args": []any{"--predict", "-3"}}}
	w = llamaObsWire(t, ExtractLlamaObs(row, nil, "single"), 5)
	if got := llamaObsFact(t, w, "limits.default_output_tokens")["value"]; got != nil {
		t.Fatalf("-3 must stay unknown, got %v", got)
	}
	w = llamaObsWire(t, ExtractLlamaObs(nil, nil, "single"), 5)
	f = llamaObsFact(t, w, "limits.context_window")
	if f["value"] != nil || f["kind"] != KindUnknown {
		t.Fatalf("absent context must stay unknown without fallback: %v", f)
	}
	if _, hasFallback := w["sources"].(map[string]any)["llama_cpp.context_fallback"]; hasFallback {
		t.Fatalf("fallback source must never appear")
	}
}

func TestLlamaCapRevPredictFinalInvalid(t *testing.T) {
	row := map[string]any{"status": map[string]any{"args": []any{"--predict", "100", "--predict", "auto"}}}
	w := llamaObsWire(t, ExtractLlamaObs(row, nil, "single"), 1)
	if got := llamaObsFact(t, w, "limits.default_output_tokens")["value"]; got != nil {
		t.Fatalf("final invalid predict must not revive earlier, got %v", got)
	}
	row = map[string]any{"status": map[string]any{"args": []any{"--predict=50"}}}
	w = llamaObsWire(t, ExtractLlamaObs(row, nil, "single"), 1)
	f := llamaObsFact(t, w, "limits.default_output_tokens")
	if f["value"] != float64(50) || f["kind"] != KindInferred {
		t.Fatalf("predict equals syntax: %v", f)
	}
}

func TestLlamaCapRevNormalizeToolsFalse(t *testing.T) {
	obs := ExtractLlamaObs(nil, map[string]any{
		"chat_template_caps": map[string]any{
			"supports_tools":               true,
			"supports_tool_calls":          false,
			"supports_parallel_tool_calls": true,
		},
	}, "single")
	w := llamaObsWire(t, obs, 1)
	if got := llamaObsFact(t, w, "tools.supported")["value"]; got != false {
		t.Fatalf("conjunction must be false, got %v", got)
	}
	par := llamaObsFact(t, w, "tools.parallel")
	if par["value"] != false {
		t.Fatalf("false tools must suppress parallel to false, got %v", par)
	}
	if par["source"] != "llama.props.chat_template_caps.supports_tools/supports_tool_calls" {
		t.Fatalf("suppression must carry false evidence source, got %v", par)
	}
}
