// Native LlamaCpp applier matrix: strict/lossy never validate, absent
// returns byte identity, consumption of null/non-string/empty/max/unknown,
// none kwargs precedence, budget setdefault, and exact subvalue/bigint
// preservation. Hermetic stdlib only.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func applyNative(t *testing.T, body string) (map[string]any, bool) {
	t.Helper()
	out, consumed := ApplyLlamaNative([]byte(body))
	if !consumed {
		t.Fatalf("expected consumption: %s", body)
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m, consumed
}

func TestNativeAbsentByteIdentity(t *testing.T) {
	body := `{"model":"m","messages":[],"temperature":0.5}`
	out, consumed := ApplyLlamaNative([]byte(body))
	if consumed {
		t.Fatalf("absent effort must not consume")
	}
	if string(out) != body {
		t.Fatalf("absent must return original bytes: %q", out)
	}
}

func TestNativeMalformedUntouched(t *testing.T) {
	for _, bad := range []string{"", "{", "[1,2]", `"str"`} {
		out, consumed := ApplyLlamaNative([]byte(bad))
		if consumed || string(out) != bad {
			t.Fatalf("malformed must pass through: %q -> %q %v", bad, out, consumed)
		}
	}
}

func TestNativeNoneAbsentAndNullKwargs(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","reasoning_effort":"none"}`,
		`{"model":"m","reasoning_effort":"none","chat_template_kwargs":null}`,
	} {
		m, _ := applyNative(t, body)
		kwargs, ok := m["chat_template_kwargs"].(map[string]any)
		if !ok || kwargs["enable_thinking"] != false || len(kwargs) != 1 {
			t.Fatalf("kwargs default: %v", m)
		}
		if _, has := m["reasoning_effort"]; has {
			t.Fatalf("effort must be consumed: %v", m)
		}
	}
}

func TestNativeNoneObjectKwargsPrecedence(t *testing.T) {
	// Explicit values win, including explicit true and null; other keys
	// survive; false is added only when the key is absent.
	m, _ := applyNative(t, `{"model":"m","reasoning_effort":"none","chat_template_kwargs":{"enable_thinking":true,"other":1}}`)
	kwargs := m["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != true || kwargs["other"] != json.Number("1") {
		t.Fatalf("explicit wins: %v", kwargs)
	}
	m, _ = applyNative(t, `{"model":"m","reasoning_effort":"none","chat_template_kwargs":{"enable_thinking":null}}`)
	if m["chat_template_kwargs"].(map[string]any)["enable_thinking"] != nil {
		t.Fatalf("explicit null wins: %v", m)
	}
	m, _ = applyNative(t, `{"model":"m","reasoning_effort":"none","chat_template_kwargs":{"other":[]}}`)
	kwargs = m["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != false || len(kwargs) != 2 {
		t.Fatalf("missing key gains false: %v", kwargs)
	}
}

func TestNativeNoneNonObjectKwargsUntouched(t *testing.T) {
	// Non-object/non-null kwargs remain exactly as sent.
	for _, kv := range []string{`"k"`, `42`, `true`, `[1]`, `1.5`} {
		m, _ := applyNative(t, `{"model":"m","reasoning_effort":"none","chat_template_kwargs":`+kv+`}`)
		raw, _ := json.Marshal(m["chat_template_kwargs"])
		if string(raw) != kv {
			t.Fatalf("kwargs %s must stay, got %s", kv, raw)
		}
		if _, has := m["reasoning_effort"]; has {
			t.Fatalf("effort must be consumed")
		}
	}
}

func TestNativeBudgetSetdefault(t *testing.T) {
	levels := map[string]string{
		"minimal": "1024", "low": "2048", "medium": "8192", "high": "16384", "xhigh": "32768",
	}
	for level, budget := range levels {
		m, _ := applyNative(t, `{"model":"m","reasoning_effort":"`+level+`"}`)
		if string(mustNum(t, m["thinking_budget_tokens"])) != budget {
			t.Fatalf("%s must map %s: %v", level, budget, m)
		}
		if _, has := m["reasoning_effort"]; has {
			t.Fatalf("effort must be consumed")
		}
	}
	// Any existing member wins, including null.
	m, _ := applyNative(t, `{"model":"m","reasoning_effort":"high","thinking_budget_tokens":7}`)
	if string(mustNum(t, m["thinking_budget_tokens"])) != "7" {
		t.Fatalf("explicit budget wins: %v", m)
	}
	m, _ = applyNative(t, `{"model":"m","reasoning_effort":"high","thinking_budget_tokens":null}`)
	if m["thinking_budget_tokens"] != nil {
		t.Fatalf("explicit null budget wins: %v", m)
	}
}

func mustNum(t *testing.T, v any) json.Number {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("not a number: %T %v", v, v)
	}
	return n
}

func TestNativeConsumedWithoutControl(t *testing.T) {
	// max, unknown, non-string, null and empty are consumed with no added
	// control, no validation and no loss.
	bodies := []string{
		`{"model":"m","reasoning_effort":"max"}`,
		`{"model":"m","reasoning_effort":"ultra"}`,
		`{"model":"m","reasoning_effort":""}`,
		`{"model":"m","reasoning_effort":null}`,
		`{"model":"m","reasoning_effort":42}`,
		`{"model":"m","reasoning_effort":true}`,
		`{"model":"m","reasoning_effort":["low"]}`,
		`{"model":"m","reasoning_effort":{"a":1}}`,
	}
	for _, body := range bodies {
		m, _ := applyNative(t, body)
		if _, has := m["reasoning_effort"]; has {
			t.Fatalf("effort must be consumed: %s", body)
		}
		if _, has := m["thinking_budget_tokens"]; has {
			t.Fatalf("no budget may be added: %s -> %v", body, m)
		}
		if _, has := m["chat_template_kwargs"]; has {
			t.Fatalf("no kwargs may be added: %s -> %v", body, m)
		}
	}
}

func TestNativeExactPreservation(t *testing.T) {
	// Big integers and raw subvalues survive mutation exactly: no
	// float64 round-trip anywhere on the path.
	m, _ := applyNative(t, `{"model":"m","seed":9007199254740993,"reasoning_effort":"low","chat_template_kwargs":{"enable_thinking":null,"big":9007199254740993},"max_tokens":9007199254740993}`)
	if string(mustNum(t, m["seed"])) != "9007199254740993" {
		t.Fatalf("bigint seed: %v", m)
	}
	if string(mustNum(t, m["max_tokens"])) != "9007199254740993" {
		t.Fatalf("bigint member: %v", m)
	}
	kwargs := m["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != nil {
		t.Fatalf("explicit null wins: %v", kwargs)
	}
	if string(mustNum(t, kwargs["big"])) != "9007199254740993" {
		t.Fatalf("bigint subvalue: %v", kwargs)
	}
	m, _ = applyNative(t, `{"model":"m","reasoning_effort":"none","nested":{"a":[1,{"b":9007199254740993}]}}`)
	raw, _ := json.Marshal(m["nested"])
	if string(raw) != `{"a":[1,{"b":9007199254740993}]}` {
		t.Fatalf("nested raw: %s", raw)
	}
}

func TestNativeModelMemberPreserved(t *testing.T) {
	// Alias rewrite and native application compose: the model member and
	// every other member survive consumption.
	m, _ := applyNative(t, `{"model":"upstream-id","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"minimal"}`)
	if m["model"] != "upstream-id" {
		t.Fatalf("model: %v", m)
	}
	if string(mustNum(t, m["thinking_budget_tokens"])) != "1024" {
		t.Fatalf("budget: %v", m)
	}
	msgs, _ := json.Marshal(m["messages"])
	var got []map[string]any
	if err := json.Unmarshal(msgs, &got); err != nil || len(got) != 1 ||
		got[0]["role"] != "user" || got[0]["content"] != "hi" {
		t.Fatalf("messages: %s", msgs)
	}
}
