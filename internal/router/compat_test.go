package router

// Bearer compat unit tests: strict token grammar, sessionId ensure_ascii
// hashing, Chat marked handling and fallback precedence. Hermetic: no env,
// no network, fixed tokens only. Error assertions check codes and
// value-freedom (no token echo).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/config"
)

const (
	compatTokA = "router-session-12345678-1234-4234-8234-123456789abc"
	compatTokB = "router-session-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func compatPublished(vals ...string) capability.OptionalStringList {
	return capability.OptionalStringList{Set: true, List: vals}
}

func TestCompatTokenSources(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	mk := func() http.Header { return http.Header{} }
	// Authorization exact Bearer prefix.
	h := mk()
	h.Set("Authorization", "Bearer "+compatTokA)
	_, fb, _, rerr := PrepareGeminiCompat(h, nil, body, compatPublished("low"), "openai-chat")
	if rerr != nil || !fb.Has || fb.ID != compatTokA {
		t.Fatalf("auth token must win fallback: %+v %+v", fb, rerr)
	}
	// Lowercase scheme is not a token.
	h = mk()
	h.Set("Authorization", "bearer "+compatTokA)
	_, fb, _, rerr = PrepareGeminiCompat(h, nil, body, compatPublished("low"), "openai-chat")
	if rerr != nil || fb.Has {
		t.Fatalf("lowercase scheme must be ignored: %+v %+v", fb, rerr)
	}
	// Uppercase hex never matches.
	h = mk()
	h.Set("Authorization", "Bearer router-session-12345678-1234-4234-8234-123456789ABC")
	_, fb, _, rerr = PrepareGeminiCompat(h, nil, body, compatPublished("low"), "openai-chat")
	if rerr != nil || fb.Has {
		t.Fatalf("uppercase hex must be ignored: %+v %+v", fb, rerr)
	}
	// x-goog-api-key and x-api-key headers collect.
	for _, name := range []string{"x-goog-api-key", "X-API-KEY"} {
		h = mk()
		h.Set(name, compatTokA)
		_, fb, _, rerr = PrepareGeminiCompat(h, nil, body, compatPublished("low"), "openai-chat")
		if rerr != nil || !fb.Has || fb.ID != compatTokA {
			t.Fatalf("%s must collect: %+v %+v", name, fb, rerr)
		}
	}
	// Ordinary credentials are ignored, not rejected.
	h = mk()
	h.Set("Authorization", "Bearer sk-ordinary")
	h.Set("X-Api-Key", "ordinary-key")
	_, fb, _, rerr = PrepareGeminiCompat(h, nil, body, compatPublished("low"), "openai-chat")
	if rerr != nil || fb.Has {
		t.Fatalf("ordinary credentials must be ignored: %+v %+v", fb, rerr)
	}
	// Query keys collect.
	_, fb, _, rerr = PrepareGeminiCompat(mk(), []string{compatTokA}, body, compatPublished("low"), "openai-chat")
	if rerr != nil || !fb.Has || fb.ID != compatTokA {
		t.Fatalf("query key must collect: %+v %+v", fb, rerr)
	}
	// Identical tokens across sources dedup to one.
	h = mk()
	h.Set("Authorization", "Bearer "+compatTokA)
	_, fb, _, rerr = PrepareGeminiCompat(h, []string{compatTokA}, body, compatPublished("low"), "openai-chat")
	if rerr != nil || !fb.Has {
		t.Fatalf("identical tokens must dedup: %+v %+v", fb, rerr)
	}
	// Two distinct tokens reject, even with an ACP/body winner present.
	h = mk()
	h.Set("Authorization", "Bearer "+compatTokA)
	h.Add("X-Goog-Api-Key", compatTokB)
	_, _, _, rerr = PrepareGeminiCompat(h, nil, []byte(`{"sessionId":"abc"}`), compatPublished("low"), "openai-chat")
	if rerr == nil || rerr.Code != "invalid_session_id" || rerr.Status != 400 {
		t.Fatalf("distinct tokens must 400: %+v", rerr)
	}
	if strings.Contains(rerr.Msg, compatTokA) || strings.Contains(rerr.Msg, compatTokB) {
		t.Fatalf("token errors must be value-free: %q", rerr.Msg)
	}
}

func TestCompatSessionIDHashGoldens(t *testing.T) {
	vectors := map[string]string{
		`"abc"`:          "agy-acp-6cc43f858fbb763301637b5af970e2a46b46f461f27e5a0f41e009c59b827b25",
		`"\u00e9"`:       "agy-acp-abc3932c9ec58f042750738c49034e7ee67cf556d6be86832ac3dda38decb703",
		`"\ud83d\ude00"`: "agy-acp-e30439cb87e140c0b998ad3095285abfec0294e2a8916f9a8f30d85734e6f82b",
		`"<>&"`:          "agy-acp-de0b609d2f9f5af97658414422d275ed44984bc2eaa807e7ba5f308b4e436365",
		`"\ud800"`:       "agy-acp-8c0c59dd0d275aadcd462a5fe12eb352cbdfeaf961eae4f85a4660521df7d2f5",
	}
	for raw, want := range vectors {
		body := []byte(`{"contents":[],"sessionId":` + raw + `}`)
		_, fb, _, rerr := PrepareGeminiCompat(http.Header{}, nil, body, compatPublished("low"), "openai-chat")
		if rerr != nil {
			t.Fatalf("%s: %v", raw, rerr)
		}
		if !fb.Has || fb.ID != want {
			t.Fatalf("%s: got %q want %q", raw, fb.ID, want)
		}
	}
	// Null, absent and decoded-empty select fallback; escaped NUL is nonempty.
	for _, b := range []string{
		`{"contents":[],"sessionId":null}`,
		`{"contents":[]}`,
		`{"contents":[],"sessionId":""}`,
	} {
		_, fb, _, rerr := PrepareGeminiCompat(http.Header{}, []string{compatTokA}, []byte(b), compatPublished("low"), "openai-chat")
		if rerr != nil || !fb.Has || fb.ID != compatTokA {
			t.Fatalf("%s must fall back: %+v %+v", b, fb, rerr)
		}
	}
	_, fb, _, rerr := PrepareGeminiCompat(http.Header{}, nil, []byte(`{"sessionId":"\u0000"}`), compatPublished("low"), "openai-chat")
	if rerr != nil || !fb.Has || fb.ID == "" || strings.HasPrefix(fb.ID, compatTokA) {
		t.Fatalf("escaped NUL must hash nonempty: %+v %+v", fb, rerr)
	}
	// Escape normalization: \/ == / and \u00e9 == literal é.
	_, slashA, _, _ := PrepareGeminiCompat(http.Header{}, nil, []byte(`{"sessionId":"a\/b"}`), compatPublished("low"), "openai-chat")
	_, slashB, _, _ := PrepareGeminiCompat(http.Header{}, nil, []byte(`{"sessionId":"a/b"}`), compatPublished("low"), "openai-chat")
	if slashA.ID != slashB.ID {
		t.Fatalf("escaped slash must normalize: %q vs %q", slashA.ID, slashB.ID)
	}
	_, upper, _, _ := PrepareGeminiCompat(http.Header{}, nil, []byte(`{"sessionId":"\u00E9"}`), compatPublished("low"), "openai-chat")
	_, lower, _, _ := PrepareGeminiCompat(http.Header{}, nil, []byte(`{"sessionId":"é"}`), compatPublished("low"), "openai-chat")
	if upper.ID != lower.ID {
		t.Fatalf("uppercase escape must normalize: %q vs %q", upper.ID, lower.ID)
	}
	// Non-string/non-null rejects; invalid UTF8 rejects value-free.
	for _, b := range []string{`{"sessionId":42}`, `{"sessionId":{}}`, `{"sessionId":["x"]}`, `{"sessionId":true}`} {
		_, _, _, rerr := PrepareGeminiCompat(http.Header{}, nil, []byte(b), compatPublished("low"), "openai-chat")
		if rerr == nil || rerr.Code != "invalid_session_id" {
			t.Fatalf("%s must 400 invalid_session_id: %+v", b, rerr)
		}
	}
	bad := append([]byte(`{"sessionId":"`), 0xff, 0xfe)
	bad = append(bad, []byte(`"}`)...)
	if _, _, _, rerr := PrepareGeminiCompat(http.Header{}, nil, bad, compatPublished("low"), "openai-chat"); rerr == nil {
		t.Fatalf("invalid UTF8 must reject")
	}
}

func TestCompatSessionIDStripsAndPreserves(t *testing.T) {
	// sessionId consumed on this path; bigints survive via RawMessage.
	body := []byte(`{"model":"m","max_tokens":9007199254740993,"sessionId":"abc","z":1}`)
	out, fb, _, rerr := PrepareGeminiCompat(http.Header{}, nil, body, compatPublished("low"), "openai-chat")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !fb.Has || fb.ID != "agy-acp-6cc43f858fbb763301637b5af970e2a46b46f461f27e5a0f41e009c59b827b25" {
		t.Fatalf("hash fallback: %+v", fb)
	}
	if strings.Contains(string(out), "sessionId") {
		t.Fatalf("sessionId must be stripped: %s", out)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("bigint must survive: %s", out)
	}
	// Untouched bodies stay byte-identical.
	raw := []byte(`{"b":1,"a":2}`)
	out, fb, _, rerr = PrepareGeminiCompat(http.Header{}, nil, raw, compatPublished("low"), "openai-chat")
	if rerr != nil || fb.Has || string(out) != string(raw) {
		t.Fatalf("untouched body must be byte-identical: %s %+v %+v", out, fb, rerr)
	}
}

func TestCompatFallbackPrecedence(t *testing.T) {
	p := &config.Provider{
		Name: "up", DefaultDialect: config.DialectGemini, BaseURL: "http://127.0.0.1:9",
		SessionHeader: "x-session-out", SessionSourceHeaders: []string{"session-id"},
		SessionPromptCacheKey: true, SessionMissing: "omit",
	}
	mk := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(pairs); i += 2 {
			h.Add(pairs[i], pairs[i+1])
		}
		return h
	}
	fb := CompatFallback{ID: compatTokA, Has: true}
	// Explicit header beats fallback.
	sess, _, rerr := PrepareSession(p, mk("X-Session-Out", "explicit1"), []byte(`{}`), false, fb)
	if rerr != nil || sess.ID != "explicit1" {
		t.Fatalf("explicit must win: %+v %+v", sess, rerr)
	}
	// Native beats fallback; body beats fallback.
	sess, _, rerr = PrepareSession(p, mk("Session-Id", "native1"), []byte(`{"prompt_cache_key":"body1"}`), true, fb)
	if rerr != nil || sess.ID != "native1" {
		t.Fatalf("native must beat fallback: %+v %+v", sess, rerr)
	}
	sess, _, _ = PrepareSession(p, mk(), []byte(`{"prompt_cache_key":"body1"}`), true, fb)
	if sess.ID != "body1" {
		t.Fatalf("body must beat fallback: %+v", sess)
	}
	// Fallback wins alone and is validated then.
	sess, _, rerr = PrepareSession(p, mk(), []byte(`{}`), false, fb)
	if rerr != nil || sess.ID != compatTokA {
		t.Fatalf("fallback must win alone: %+v %+v", sess, rerr)
	}
	// Invalid fallback is ignored when an earlier candidate won...
	sess, _, rerr = PrepareSession(p, mk("Session-Id", "good1"), []byte(`{}`), false, CompatFallback{ID: "bad value!", Has: true})
	if rerr != nil || sess.ID != "good1" {
		t.Fatalf("losing fallback must not validate: %+v %+v", sess, rerr)
	}
	// ...but rejects when it would win.
	if _, _, rerr := PrepareSession(p, mk(), []byte(`{}`), false, CompatFallback{ID: "bad value!", Has: true}); rerr == nil || rerr.Code != "invalid_session_id" {
		t.Fatalf("winning invalid fallback must 400: %+v", rerr)
	}
	// Explicit [] disables native selection but not fallback.
	p2 := &config.Provider{Name: "up", DefaultDialect: config.DialectGemini, BaseURL: "http://127.0.0.1:9",
		SessionHeader: "x-session-out", SessionSourceHeaders: []string{}, SessionMissing: "omit"}
	sess, _, rerr = PrepareSession(p2, mk("Session-Id", "native1"), []byte(`{}`), false, fb)
	if rerr != nil || !sess.Has || sess.ID != compatTokA {
		t.Fatalf("[] must disable natives, keep fallback: %+v %+v", sess, rerr)
	}
	// No policy ignores the fallback entirely.
	p3 := &config.Provider{Name: "up", DefaultDialect: config.DialectGemini, BaseURL: "http://127.0.0.1:9"}
	sess, eff, rerr := PrepareSession(p3, mk("Session-Id", "x"), []byte(`{"a":1}`), false, fb)
	if rerr != nil || sess.Has || string(eff) != `{"a":1}` {
		t.Fatalf("no policy must ignore fallback: %+v %s %+v", sess, eff, rerr)
	}
	// Connection-nominated identity headers skip selection but do not
	// hide compat credential collection (separate path, same header).
	h := mk("Authorization", "Bearer "+compatTokA, "Connection", "authorization")
	_, fb2, _, rerr := PrepareGeminiCompat(h, nil, []byte(`{}`), compatPublished("low"), "openai-chat")
	if rerr != nil || !fb2.Has {
		t.Fatalf("Connection must not hide compat carriers: %+v %+v", fb2, rerr)
	}
	sess, _, rerr = PrepareSession(p, mk("Session-Id", "bad value!", "Connection", "session-id"), []byte(`{}`), false, NoCompatFallback())
	if rerr != nil || sess.Has {
		t.Fatalf("Connection must skip identity selection: %+v %+v", sess, rerr)
	}
}

func TestCompatChatMarked(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},{"role":"user","content":"hi"}]}`)
	// Marked token strips every Authorization casing and injects.
	h := http.Header{"Authorization": {"Bearer " + compatTokA}, "X-Other": {"v"}}
	out, hout, rerr := PrepareChatCompat(h, body)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for k := range hout {
		if strings.EqualFold(k, "authorization") {
			t.Fatalf("auth must be stripped: %v", hout)
		}
	}
	if hout.Get("x-session-id") != compatTokA {
		t.Fatalf("must inject x-session-id: %v", hout)
	}
	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	msgs := top["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("systems must merge: %v", top)
	}
	if msgs[0].(map[string]any)["content"] != "a\n\nb" {
		t.Fatalf("merge separator: %v", msgs[0])
	}
	// Input headers are cloned, never mutated.
	if _, ok := h["Authorization"]; !ok {
		t.Fatalf("input header must not be mutated")
	}
	// Native presence (even empty or nominated) blocks injection but auth still strips.
	for _, extra := range []http.Header{
		{"Authorization": {"Bearer " + compatTokA}, "X-Session-Id": {""}},
		{"Authorization": {"Bearer " + compatTokA}, "Session-Id": {"s"}, "Connection": {"session-id"}},
		{"Authorization": {"Bearer " + compatTokA}, "X-Opencode-Session": {"o"}},
	} {
		_, hout, rerr := PrepareChatCompat(extra, body)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if hout.Get("x-session-id") == compatTokA {
			t.Fatalf("native presence must block injection: %v", hout)
		}
		for k := range hout {
			if strings.EqualFold(k, "authorization") {
				t.Fatalf("auth must strip even when blocked: %v", hout)
			}
		}
	}
	// Distinct marked tokens reject with hardening; unmarked is a no-op.
	h2 := http.Header{}
	h2["Authorization"] = []string{"Bearer " + compatTokA}
	h2.Add("authorization", "Bearer "+compatTokB)
	if _, _, rerr := PrepareChatCompat(h2, body); rerr == nil || rerr.Code != "invalid_session_id" {
		t.Fatalf("distinct marked tokens must 400: %+v", rerr)
	}
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	h3 := http.Header{"Authorization": {"Bearer ordinary"}}
	out, hout, rerr = PrepareChatCompat(h3, raw)
	if rerr != nil || string(out) != string(raw) || len(hout) != len(h3) {
		t.Fatalf("unmarked must be a no-op: %s %+v", out, rerr)
	}
}

func TestCompatChatMergeEdges(t *testing.T) {
	// Named or structured systems stop the merge; single system stays.
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"named stops", `{"messages":[{"role":"system","content":"a","name":"n"},{"role":"system","content":"b"}]}`, 2},
		{"structured stops", `{"messages":[{"role":"system","content":"a"},{"role":"system","content":[{"type":"text","text":"x"}]}]}`, 2},
		{"single stays", `{"messages":[{"role":"system","content":"a"},{"role":"user","content":"hi"}]}`, 2},
		{"nonleading stops", `{"messages":[{"role":"user","content":"q"},{"role":"system","content":"a"},{"role":"system","content":"b"}]}`, 3},
	} {
		h := http.Header{"Authorization": {"Bearer " + compatTokA}}
		out, _, rerr := PrepareChatCompat(h, []byte(tc.body))
		if rerr != nil {
			t.Fatal(rerr)
		}
		var top map[string]any
		if err := json.Unmarshal(out, &top); err != nil {
			t.Fatal(err)
		}
		if len(top["messages"].([]any)) != tc.want {
			t.Fatalf("%s: got %v", tc.name, top)
		}
	}
	// Marked bodies validate message shapes.
	for _, b := range []string{
		`{"messages":"nope"}`,
		`{"messages":[42]}`,
		`{"messages":[{"role":42}]}`,
		`{"messages":[{"content":"x"}]}`,
	} {
		h := http.Header{"Authorization": {"Bearer " + compatTokA}}
		if _, _, rerr := PrepareChatCompat(h, []byte(b)); rerr == nil {
			t.Fatalf("%s must 400", b)
		}
	}
}

func TestCompatEffortValidation(t *testing.T) {
	base := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	// Unpublished effort rejects value-free; empty published set rejects all.
	h := http.Header{"X-Model-Router-Reasoning-Effort": {"medium"}}
	if _, _, _, rerr := PrepareGeminiCompat(h, nil, []byte(base), compatPublished("low"), "openai-chat"); rerr == nil {
		t.Fatalf("unpublished must 400")
	} else if strings.Contains(rerr.Msg, "medium") {
		t.Fatalf("effort errors must be value-free: %q", rerr.Msg)
	}
	if _, _, _, rerr := PrepareGeminiCompat(h, nil, []byte(base), compatPublished(), "openai-chat"); rerr == nil {
		t.Fatalf("empty set must reject")
	}
	if _, _, _, rerr := PrepareGeminiCompat(h, nil, []byte(base), capability.OptionalStringList{}, "openai-chat"); rerr == nil {
		t.Fatalf("unknown set must reject")
	}
	// Repeated headers (even identical) reject rather than choose one.
	h2 := http.Header{}
	h2["X-Model-Router-Reasoning-Effort"] = []string{"low", "low"}
	if _, _, _, rerr := PrepareGeminiCompat(h2, nil, []byte(base), compatPublished("low"), "openai-chat"); rerr == nil {
		t.Fatalf("repeated effort must 400")
	}
	h3 := http.Header{"X-Model-Router-Reasoning-Effort": {"low"}}
	h3.Add("x-model-router-REASONING-effort", "low")
	if _, _, _, rerr := PrepareGeminiCompat(h3, nil, []byte(base), compatPublished("low"), "openai-chat"); rerr == nil {
		t.Fatalf("case-variant effort fields must 400")
	}
	// Non-object configs reject; null is allowed; control presence (even
	// null, either spelling) conflicts with a carrier.
	for _, b := range []string{
		`{"generationConfig":[]}`,
		`{"generationConfig":{"thinkingConfig":7}}`,
		`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`,
		`{"generationConfig":{"thinkingConfig":{"thinkingLevel":null}}}`,
		`{"generationConfig":{"thinking_config":{"thinking_budget":null}}}`,
		`{"generation_config":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`,
	} {
		if _, _, _, rerr := PrepareGeminiCompat(h, nil, []byte(b), compatPublished("medium"), "openai-chat"); rerr == nil {
			t.Fatalf("%s must 400 with carrier", b)
		}
	}
	// Same bodies pass without a carrier (byte-identical).
	for _, b := range []string{
		`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}}}`,
		`{"generationConfig":[]}`,
	} {
		raw := []byte(b)
		out, _, effort, rerr := PrepareGeminiCompat(http.Header{}, nil, raw, compatPublished("medium"), "openai-chat")
		if rerr != nil || effort.IsPresent() || string(out) != b {
			t.Fatalf("%s must pass untouched without carrier: %s %+v", b, out, rerr)
		}
	}
	// Alias collision rejects.
	if _, _, _, rerr := PrepareGeminiCompat(h, nil, []byte(`{"generationConfig":{},"generation_config":{}}`), compatPublished("medium"), "openai-chat"); rerr == nil {
		t.Fatalf("generationConfig collision must 400")
	}
	// Native mutation preserves container spelling and maps levels.
	out, _, effort, rerr := PrepareGeminiCompat(h, nil, []byte(base), compatPublished("medium", "low"), "gemini")
	_ = out
	_ = effort
	_ = rerr
	hMed := http.Header{"X-Model-Router-Reasoning-Effort": {"medium"}}
	mut, _, eff, rerr := PrepareGeminiCompat(hMed, nil, []byte(base), compatPublished("medium"), "gemini")
	if rerr != nil || eff.IsPresent() {
		t.Fatalf("native must consume carrier: %+v %+v", eff, rerr)
	}
	if !strings.Contains(string(mut), `"thinkingLevel":"MEDIUM"`) {
		t.Fatalf("native must map medium: %s", mut)
	}
	hNone := http.Header{"X-Model-Router-Reasoning-Effort": {"none"}}
	mut, _, _, rerr = PrepareGeminiCompat(hNone, nil, []byte(base), compatPublished("none"), "gemini")
	if rerr != nil || !strings.Contains(string(mut), `"thinkingBudget":0`) {
		t.Fatalf("native none must set budget 0: %s %+v", mut, rerr)
	}
	hX := http.Header{"X-Model-Router-Reasoning-Effort": {"xhigh"}}
	if _, _, _, rerr := PrepareGeminiCompat(hX, nil, []byte(base), compatPublished("xhigh"), "gemini"); rerr == nil {
		t.Fatalf("native xhigh must 400")
	}
	// Bridged xhigh/max stay literal through the compat constructor.
	_, _, intent, rerr := PrepareGeminiCompat(hX, nil, []byte(base), compatPublished("xhigh"), "openai-chat")
	if rerr != nil || !intent.IsPresent() {
		t.Fatalf("bridged xhigh must plan: %+v", rerr)
	}
	if lvl, _ := intent.Level(); lvl != "xhigh" {
		t.Fatalf("literal must be preserved: %q", lvl)
	}
	if intent.Origin() != "x-model-router-reasoning-effort" {
		t.Fatalf("origin must be the header name: %q", intent.Origin())
	}
}
