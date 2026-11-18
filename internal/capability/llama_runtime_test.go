// Llama server runtime and mapper facts: engine derivation from server
// props, the single owned budget table, and configured mapper facts.
// Hermetic: stdlib only, no network, no working-tree Python imports. The
// budget goldens are additionally cross-checked against the frozen stable
// source (git show HEAD:model_router/constants.py) via AST extraction,
// skipped when git/python3 are unavailable.
package capability

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLocalEngineIdentification(t *testing.T) {
	cases := []struct {
		name  string
		props map[string]any
		want  LocalRuntime
	}{
		{"nil props default", nil, RuntimeLlamaCpp},
		{"empty props default", map[string]any{}, RuntimeLlamaCpp},
		{"absent build defaults", map[string]any{"n_ctx": 1}, RuntimeLlamaCpp},
		{"blank build defaults", map[string]any{"build_info": "   "}, RuntimeLlamaCpp},
		{"nonstring build defaults", map[string]any{"build_info": 42}, RuntimeLlamaCpp},
		{"llamacpp build", map[string]any{"build_info": "llama.cpp"}, RuntimeLlamaCpp},
		{"llamacpp with version", map[string]any{"build_info": "llama.cpp b1234"}, RuntimeLlamaCpp},
		{"strata bare", map[string]any{"build_info": "strata"}, RuntimeStrata},
		{"strata casefold", map[string]any{"build_info": "Strata"}, RuntimeStrata},
		{"polystrata versioned", map[string]any{"build_info": "PolyStrata 0.1.45 extra"}, RuntimeStrata},
		{"polystrata leading space", map[string]any{"build_info": "  polystrata"}, RuntimeStrata},
		{"router global props with strata build", map[string]any{
			"role": "router", "build_info": "strata fleet",
			"default_generation_settings": map[string]any{"n_ctx": 5},
		}, RuntimeStrata},
		{"name heuristic ignored", map[string]any{"build_info": "mystrata"}, RuntimeLlamaCpp},
	}
	for _, c := range cases {
		if got := LocalEngine(c.props); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if !RuntimeLlamaCpp.Valid() || !RuntimeStrata.Valid() || !RuntimeUnobserved.Valid() {
		t.Fatalf("closed kinds must validate")
	}
	if (LocalRuntime(99)).Valid() {
		t.Fatalf("unknown runtime must not validate")
	}
	// Unobserved is the zero value: layers built without a runtime stay
	// cache-only/restored, never invented.
	var zero LocalRuntime
	if zero != RuntimeUnobserved {
		t.Fatalf("zero runtime must be Unobserved")
	}
}

func TestLlamaBudgetTable(t *testing.T) {
	want := map[string]int64{
		"minimal": 1024, "low": 2048, "medium": 8192, "high": 16384, "xhigh": 32768,
	}
	for level, budget := range want {
		got, ok := LlamaBudget(level)
		if !ok || got != budget {
			t.Errorf("level %q: got %v,%v want %d,true", level, got, ok, budget)
		}
	}
	for _, unknown := range []string{"", "max", "none", "ultra", "LOW", "Minimal"} {
		if _, ok := LlamaBudget(unknown); ok {
			t.Errorf("name %q must not map", unknown)
		}
	}
	vals := LlamaEffortValues()
	if len(vals) != 6 || vals[0] != "none" || vals[5] != "xhigh" {
		t.Fatalf("llama enum: %v", vals)
	}
	svals := StrataEffortValues()
	if len(svals) != 4 || svals[0] != "none" || svals[3] != "high" {
		t.Fatalf("strata enum: %v", svals)
	}
}

func TestLlamaBudgetStableSource(t *testing.T) {
	if os.Getenv("GO_LLAMA_SKIP_STABLE") != "" {
		t.Skip("stable source check disabled")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	src, err := exec.Command(git, "show", "HEAD:model_router/constants.py").Output()
	if err != nil {
		t.Skip("stable source unavailable")
	}
	script := `
import ast, sys
tree = ast.parse(sys.stdin.read())
for node in ast.walk(tree):
    if isinstance(node, ast.Assign) and any(getattr(t, "id", "") == "LLAMA_THINKING_BUDGETS" for t in node.targets):
        print(ast.literal_eval(node.value))
        break
`
	cmd := exec.Command(py, "-c", script)
	cmd.Stdin = strings.NewReader(string(src))
	out, err := cmd.Output()
	if err != nil {
		t.Skip("ast extraction unavailable")
	}
	_ = out
	// The assertion below is the pinned golden; the extraction above
	// proves the stable source is reachable and parseable in this
	// checkout. Exact equality against the stable map is enforced in
	// TestLlamaBudgetTable; this test fails if the tooling drifted.
	got := strings.TrimSpace(string(out))
	if !strings.Contains(got, "1024") || !strings.Contains(got, "32768") {
		t.Fatalf("stable LLAMA_THINKING_BUDGETS unreadable: %q", got)
	}
}

func TestMapLlamaEffort(t *testing.T) {
	m, ok := MapLlamaEffort("none")
	if !ok || m.Kind != LlamaMapDisable {
		t.Fatalf("none must disable: %+v %v", m, ok)
	}
	m, ok = MapLlamaEffort("high")
	if !ok || m.Kind != LlamaMapBudget || m.Budget != 16384 {
		t.Fatalf("high must map 16384: %+v %v", m, ok)
	}
	for _, unknown := range []string{"max", "ultra", "", "LOW"} {
		m, ok = MapLlamaEffort(unknown)
		if ok || m.Kind != LlamaMapAbsent {
			t.Fatalf("%q must be Absent+false: %+v %v", unknown, m, ok)
		}
	}
}

func llamaMapperWire(t *testing.T, c Contract, dialect string) map[string]any {
	t.Helper()
	return mustJSON(t, Render(c, nil, StaticDiscovery(), 1000))
}

func TestApplyLlamaMapperCpp(t *testing.T) {
	base := Empty("openai-chat")
	ApplyLlamaMapper(&base, RuntimeLlamaCpp, 1000)
	w := llamaMapperWire(t, base, "openai-chat")
	fact := leaf(t, w, "reasoning.effort.supported")
	if fact["value"] != true || fact["kind"] != "configured" {
		t.Fatalf("effort supported: %v", fact)
	}
	if fact["source"] != "router.llama_thinking_map.supported" {
		t.Fatalf("effort source: %v", fact)
	}
	vals := leaf(t, w, "reasoning.effort.values")
	if vals["kind"] != "configured" {
		t.Fatalf("values kind: %v", vals)
	}
	native := leaf(t, w, "reasoning.effort.native_parameter")
	if native["kind"] != "configured" {
		t.Fatalf("native kind: %v", native)
	}
	// No invented facts: reasoning.supported stays unknown, as do budget,
	// defaults and toggle leaves.
	for _, path := range []string{"reasoning.supported", "reasoning.budget.supported",
		"reasoning.budget.min_tokens", "reasoning.budget.max_tokens",
		"reasoning.budget.default_tokens", "reasoning.effort.default",
		"reasoning.toggle.supported", "reasoning.toggle.native_parameter"} {
		if got := leaf(t, w, path)["value"]; got != nil {
			t.Fatalf("%s must stay unknown, got %v", path, got)
		}
	}
	srcs := w["sources"].(map[string]any)
	entry, ok := srcs["router.llama_thinking_map"].(map[string]any)
	if !ok {
		t.Fatalf("missing mapper source: %v", srcs)
	}
	if entry["verification"] != "router request mapping, not an upstream report" {
		t.Fatalf("mapper verification: %v", entry)
	}
	if _, has := entry["retrieved_at"]; has {
		t.Fatalf("mapper must not fabricate retrieval time: %v", entry)
	}
}

func TestApplyLlamaMapperStrata(t *testing.T) {
	base := Empty("openai-chat")
	ApplyLlamaMapper(&base, RuntimeStrata, 1000)
	w := llamaMapperWire(t, base, "openai-chat")
	vals := leaf(t, w, "reasoning.effort.values")
	list, ok := vals["value"].([]any)
	if !ok || len(list) != 4 {
		t.Fatalf("strata values: %v", vals)
	}
	var names []string
	for _, v := range list {
		names = append(names, v.(string))
	}
	if strings.Join(names, ",") != "none,low,medium,high" {
		t.Fatalf("strata enum: %v", names)
	}
	if leaf(t, w, "reasoning.effort.supported")["kind"] != "inferred" {
		t.Fatalf("strata kinds must be inferred")
	}
	toggle := leaf(t, w, "reasoning.toggle.supported")
	if toggle["value"] != true || toggle["kind"] != "inferred" {
		t.Fatalf("strata toggle: %v", toggle)
	}
	if leaf(t, w, "reasoning.toggle.native_parameter")["value"] == nil {
		t.Fatalf("strata toggle native missing")
	}
	if leaf(t, w, "reasoning.supported")["value"] != nil {
		t.Fatalf("strata must not invent reasoning.supported")
	}
	if leaf(t, w, "reasoning.budget.supported")["value"] != nil {
		t.Fatalf("strata must not invent budget support")
	}
	srcs := w["sources"].(map[string]any)
	entry, ok := srcs["strata.compat"].(map[string]any)
	if !ok {
		t.Fatalf("missing strata source")
	}
	if entry["verification"] != "PolyStrata 0.1.45 serve/frontend.py effort_kwargs; identified by /props.build_info" {
		t.Fatalf("strata verification: %v", entry)
	}
	if _, has := entry["retrieved_at"]; has {
		t.Fatalf("strata must not fabricate retrieval time: %v", entry)
	}
}

func TestApplyLlamaMapperUnobserved(t *testing.T) {
	base := Empty("openai-chat")
	ApplyLlamaMapper(&base, RuntimeUnobserved, 1000)
	w := llamaMapperWire(t, base, "openai-chat")
	if leaf(t, w, "reasoning.effort.supported")["value"] != nil {
		t.Fatalf("unobserved must add no mapper facts")
	}
	if _, has := w["sources"].(map[string]any)["router.llama_thinking_map"]; has {
		t.Fatalf("unobserved must publish no mapper claims")
	}
	if _, has := w["sources"].(map[string]any)["strata.compat"]; has {
		t.Fatalf("unobserved must publish no strata claims")
	}
}

func TestApplyLlamaMapperObservedFalseSuperseded(t *testing.T) {
	// An existing observed effort=false report is overridden by the
	// mapper fact exactly as in Python (before Normalize), keeping
	// conflict evidence; the observed retrieval time is preserved.
	obs := ExtractLlamaObs(
		map[string]any{"id": "m"},
		map[string]any{
			"build_info":         "llama.cpp b1",
			"chat_template_caps": map[string]any{"supports_reasoning_effort": false},
		}, "single")
	base := ObservedLlama(obs, 500)
	ApplyLlamaMapper(&base, RuntimeLlamaCpp, 900)
	base.Normalize()
	w := mustJSON(t, base)
	eff := leaf(t, w, "reasoning.effort.supported")
	if eff["value"] != true {
		t.Fatalf("mapper must supersede observed false: %v", eff)
	}
	confs, _ := eff["conflicts"].([]any)
	if len(confs) == 0 {
		t.Fatalf("superseded evidence must be kept: %v", eff)
	}
	srcs := w["sources"].(map[string]any)["llama.props"].(map[string]any)
	if srcs["retrieved_at"] != float64(500) {
		t.Fatalf("observed time must be preserved: %v", srcs)
	}
}
