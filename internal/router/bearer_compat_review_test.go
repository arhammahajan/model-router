package router

// Adversarial F2-F4/F6-F7 router review: exact-case grammar, all carriers,
// identical-vs-distinct, sessionId normalize + bigints, Chat shape/merge/
// presence incl Connection, fallback order, aliased controls, value-free.
// Hermetic, no env/network.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/config"
)

const (
	revTokA = "router-session-12345678-1234-4234-8234-123456789abc"
	revTokB = "router-session-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func revPub(vals ...string) capability.OptionalStringList {
	return capability.OptionalStringList{Set: true, List: vals}
}

func TestBearerCompatReviewTokenGrammar(t *testing.T) {
	body := []byte(`{"contents":[]}`)
	mk := func() http.Header { return http.Header{} }
	// api-key without x- must be ignored.
	h := mk()
	h.Set("Api-Key", revTokA)
	if _, fb, _, rerr := PrepareGeminiCompat(h, nil, body, revPub("low"), "openai-chat"); rerr != nil || fb.Has {
		t.Fatalf("api-key must not collect: %+v %+v", fb, rerr)
	}
	// Bearer without space, trailing space token, lowercase scheme ignored.
	for _, v := range []string{"Bearer", "Bearer ", "bearer " + revTokA, "Bearer " + revTokA + " ", "Bearer  " + revTokA} {
		h = mk()
		h.Set("Authorization", v)
		if _, fb, _, rerr := PrepareGeminiCompat(h, nil, body, revPub("low"), "openai-chat"); rerr != nil || fb.Has {
			t.Fatalf("%q must be ignored: %+v %+v", v, fb, rerr)
		}
	}
	// x-goog value with Bearer prefix is not a token.
	h = mk()
	h.Set("X-Goog-Api-Key", "Bearer "+revTokA)
	if _, fb, _, rerr := PrepareGeminiCompat(h, nil, body, revPub("low"), "openai-chat"); rerr != nil || fb.Has {
		t.Fatalf("goog Bearer prefix must be ignored: %+v %+v", fb, rerr)
	}
	// Identical across all three sources dedups to one.
	h = mk()
	h.Set("Authorization", "Bearer "+revTokA)
	h.Set("X-Goog-Api-Key", revTokA)
	if _, fb, _, rerr := PrepareGeminiCompat(h, []string{revTokA}, body, revPub("low"), "openai-chat"); rerr != nil || !fb.Has || fb.ID != revTokA {
		t.Fatalf("triple identical must dedup: %+v %+v", fb, rerr)
	}
	// Distinct header-vs-query rejects value-free.
	h = mk()
	h.Set("Authorization", "Bearer "+revTokA)
	if _, _, _, rerr := PrepareGeminiCompat(h, []string{revTokB}, body, revPub("low"), "openai-chat"); rerr == nil || rerr.Code != "invalid_session_id" {
		t.Fatalf("header/query distinct must 400: %+v", rerr)
	} else if strings.Contains(rerr.Msg, revTokA) || strings.Contains(rerr.Msg, revTokB) {
		t.Fatalf("value-free: %q", rerr.Msg)
	}
}

func TestBearerCompatReviewSessionIDNormalize(t *testing.T) {
	pub := revPub("low")
	// Short escape vs unicode escape normalize.
	a, fbA, _, _ := PrepareGeminiCompat(http.Header{}, nil, []byte("{\"sessionId\":\"\\b\"}"), pub, "openai-chat")
	_ = a
	_, fbB, _, _ := PrepareGeminiCompat(http.Header{}, nil, []byte("{\"sessionId\":\"\\u0008\"}"), pub, "openai-chat")
	if fbA.ID != fbB.ID || fbA.ID != "agy-acp-764b71f1436ed52909511f169b4a41282426dc60382846132cd6c2a02f4df149" {
		t.Fatalf("backspace normalize: %q vs %q", fbA.ID, fbB.ID)
	}
	// Uppercase lone surrogate normalizes to frozen golden.
	_, fbU, _, rerr := PrepareGeminiCompat(http.Header{}, nil, []byte("{\"sessionId\":\"\\uD800\"}"), pub, "openai-chat")
	if rerr != nil || fbU.ID != "agy-acp-8c0c59dd0d275aadcd462a5fe12eb352cbdfeaf961eae4f85a4660521df7d2f5" {
		t.Fatalf("uppercase lone must normalize: %+v %+v", fbU, rerr)
	}
	// Lone low surrogate golden from Python ensure_ascii.
	_, fbL, _, rerr := PrepareGeminiCompat(http.Header{}, nil, []byte("{\"sessionId\":\"\\udc00\"}"), pub, "openai-chat")
	if rerr != nil || fbL.ID != "agy-acp-353c7370beca95e64c258c908edac60c2ab30d355ca1b5b7fc31c5bce4a4c65a" {
		t.Fatalf("lone low golden: %+v %+v", fbL, rerr)
	}
	// Backslash and quote normalize.
	_, fbBs, _, _ := PrepareGeminiCompat(http.Header{}, nil, []byte("{\"sessionId\":\"\\\\\"}"), pub, "openai-chat")
	if fbBs.ID != "agy-acp-687a9ed5a2b7019504648f874bb683f56e7787e1a4738493d84349c8d4e72b23" {
		t.Fatalf("backslash golden: %q", fbBs.ID)
	}
	// Bigints survive exactly beside stripped sessionId.
	out, _, _, rerr := PrepareGeminiCompat(http.Header{}, nil, []byte(`{"sessionId":"abc","n":9007199254740993,"m":-9007199254740993}`), pub, "openai-chat")
	if rerr != nil {
		t.Fatal(rerr)
	}
	s := string(out)
	if !strings.Contains(s, "9007199254740993") || strings.Contains(s, "sessionId") {
		t.Fatalf("bigint/strip: %s", s)
	}
	// Boolean and float sessionId reject; null falls back.
	for _, b := range []string{`{"sessionId":false}`, `{"sessionId":0.5}`} {
		_, _, _, rerr := PrepareGeminiCompat(http.Header{}, nil, []byte(b), pub, "openai-chat")
		if rerr == nil || rerr.Code != "invalid_session_id" {
			t.Fatalf("%s must 400: %+v", b, rerr)
		}
	}
}

func TestBearerCompatReviewChatPresence(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},{"role":"user","content":"hi"}]}`)
	// Unmarked with native present stays untouched (no strip, no inject).
	h := http.Header{"Authorization": {"Bearer ordinary"}, "X-Session-Id": {"keep"}}
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out, hout, rerr := PrepareChatCompat(h, raw)
	if rerr != nil || string(out) != string(raw) {
		t.Fatalf("unmarked must be byte-identical: %s %+v", out, rerr)
	}
	if hout.Get("X-Session-Id") != "keep" {
		t.Fatalf("unmarked must not inject: %v", hout)
	}
	// Connection nominates an absent name: compat still injects (headers stay
	// intact path), but affinity later skips it -> no identity. Prove both halves.
	h2 := http.Header{"Authorization": {"Bearer " + revTokA}, "Connection": {"x-session-id"}}
	_, hout2, rerr := PrepareChatCompat(h2, body)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if hout2.Get("x-session-id") != revTokA {
		t.Fatalf("absent nominated still injects at compat layer: %v", hout2)
	}
	p := &config.Provider{Name: "up", Dialect: config.DialectOpenAIChat, BaseURL: "http://127.0.0.1:9", SessionHeader: "x-session-out", SessionSourceHeaders: []string{"x-session-id"}, SessionMissing: "omit"}
	sess, _, rerr := PrepareSession(p, hout2, []byte(`{}`), false, NoCompatFallback())
	if rerr != nil {
		t.Fatal(rerr)
	}
	// x-session-id is validated-but-losing (custom dest x-session-out, no fallback).
	if sess.Has {
		t.Fatalf("nominated injected header must be skipped: %+v", sess)
	}
	// Empty-string native blocks injection but auth still strips.
	h3 := http.Header{"Authorization": {"Bearer " + revTokA}, "Conversation_Id": {""}}
	_, hout3, rerr := PrepareChatCompat(h3, body)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if hout3.Get("x-session-id") == revTokA {
		t.Fatalf("empty native must block: %v", hout3)
	}
	for k := range hout3 {
		if strings.EqualFold(k, "authorization") {
			t.Fatalf("auth must strip even when blocked")
		}
	}
}

func TestBearerCompatReviewFallbackOrder(t *testing.T) {
	mk := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(pairs); i += 2 {
			h.Add(pairs[i], pairs[i+1])
		}
		return h
	}
	// Custom destination beats configured sources and body and fallback.
	p := &config.Provider{Name: "up", Dialect: config.DialectGemini, BaseURL: "http://127.0.0.1:9", SessionHeader: "x-custom", SessionSourceHeaders: []string{"session-id"}, SessionPromptCacheKey: true, SessionMissing: "omit"}
	fb := CompatFallback{ID: revTokA, Has: true}
	sess, _, rerr := PrepareSession(p, mk("X-Custom", "dest1", "Session-Id", "src1"), []byte(`{"prompt_cache_key":"body1"}`), true, fb)
	if rerr != nil || sess.ID != "dest1" {
		t.Fatalf("dest must win: %+v %+v", sess, rerr)
	}
	// Source beats body beats fallback.
	sess, _, _ = PrepareSession(p, mk("Session-Id", "src1"), []byte(`{"prompt_cache_key":"body1"}`), true, fb)
	if sess.ID != "src1" {
		t.Fatalf("source must beat body: %+v", sess)
	}
	// Invalid native still rejects even when it loses to dest.
	if _, _, rerr := PrepareSession(p, mk("X-Custom", "dest1", "Session-Id", "bad value!"), []byte(`{}`), false, NoCompatFallback()); rerr == nil {
		t.Fatalf("losing invalid native must 400")
	}
}

func TestBearerCompatReviewEffortAliases(t *testing.T) {
	h := http.Header{"X-Model-Router-Reasoning-Effort": {"low"}}
	// thinking_level snake null conflicts.
	if _, _, _, rerr := PrepareGeminiCompat(h, nil, []byte(`{"generationConfig":{"thinkingConfig":{"thinking_level":null}}}`), revPub("low"), "openai-chat"); rerr == nil {
		t.Fatalf("snake null control must conflict")
	}
	// thinking_config collision rejects.
	if _, _, _, rerr := PrepareGeminiCompat(h, nil, []byte(`{"generationConfig":{"thinkingConfig":{},"thinking_config":{}}}`), revPub("low"), "openai-chat"); rerr == nil {
		t.Fatalf("thinking collision must 400")
	}
	// Null thinkingConfig alone passes (no control).
	if _, _, effort, rerr := PrepareGeminiCompat(h, nil, []byte(`{"generationConfig":{"thinkingConfig":null}}`), revPub("low"), "openai-chat"); rerr != nil || !effort.IsPresent() {
		t.Fatalf("null thinkingConfig must pass with carrier: %+v %+v", effort, rerr)
	}
	// Null generationConfig alone passes.
	if _, _, effort, rerr := PrepareGeminiCompat(h, nil, []byte(`{"generationConfig":null}`), revPub("low"), "openai-chat"); rerr != nil || !effort.IsPresent() {
		t.Fatalf("null generationConfig must pass: %+v %+v", effort, rerr)
	}
	// Native preserves snake container spelling.
	hs := http.Header{"X-Model-Router-Reasoning-Effort": {"medium"}}
	mut, _, _, rerr := PrepareGeminiCompat(hs, nil, []byte(`{"generation_config":{"thinking_config":{}}}`), revPub("medium"), "gemini")
	if rerr != nil || !strings.Contains(string(mut), `"generation_config"`) || !strings.Contains(string(mut), `"thinkingLevel":"MEDIUM"`) {
		t.Fatalf("snake spelling must be preserved: %s %+v", mut, rerr)
	}
	// Without carrier, aliased bodies stay byte-identical.
	raw := `{"generation_config":{"thinking_config":{"thinkingBudget":5}}}`
	out, _, effort, rerr := PrepareGeminiCompat(http.Header{}, nil, []byte(raw), revPub("low"), "openai-chat")
	if rerr != nil || effort.IsPresent() || string(out) != raw {
		t.Fatalf("no-carrier snake must be untouched: %s %+v", out, rerr)
	}
}
