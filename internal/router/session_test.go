package router

// Session affinity selection contracts: explicit/native/body precedence,
// default source order, disabled-native validation, duplicates, hop
// nomination, omit/reject, translated consumption and no-policy passthrough.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
)

func testProvider() *config.Provider {
	return &config.Provider{
		Name:                 "up",
		Dialect:              config.DialectAnthropic,
		BaseURL:              "http://127.0.0.1:9",
		SessionHeader:        "x-session-out",
		SessionSourceHeaders: []string{"session-id", "thread-id"},
		SessionMissing:       "omit",
	}
}

func hdr(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Add(pairs[i], pairs[i+1])
	}
	return h
}

func mustOK(t *testing.T, p *config.Provider, h http.Header, body string, translated bool) (Session, []byte) {
	t.Helper()
	sess, eff, rerr := PrepareSession(p, h, []byte(body), translated, NoCompatFallback())
	if rerr != nil {
		t.Fatalf("unexpected error %+v", rerr)
	}
	return sess, eff
}

func mustFail(t *testing.T, p *config.Provider, h http.Header, body string, translated bool, code string) {
	t.Helper()
	_, _, rerr := PrepareSession(p, h, []byte(body), translated, NoCompatFallback())
	if rerr == nil {
		t.Fatalf("want %s, got success", code)
	}
	if rerr.Code != code {
		t.Fatalf("want %s got %+v", code, rerr)
	}
	if rerr.Status != 400 {
		t.Fatalf("session failures are 400, got %+v", rerr)
	}
}

func TestSessionExplicitBeatsNative(t *testing.T) {
	p := testProvider()
	sess, eff := mustOK(t, p, hdr("X-Session-Out", "explicit1", "Session-Id", "native1"), `{"model":"m"}`, false)
	if !sess.Has || sess.ID != "explicit1" {
		t.Fatalf("want explicit1 got %+v", sess)
	}
	if string(eff) != `{"model":"m"}` {
		t.Fatalf("native body must be untouched: %s", eff)
	}
}

func TestSessionNativeOrderAndValidation(t *testing.T) {
	p := testProvider()
	sess, _ := mustOK(t, p, hdr("Thread-Id", "t1", "Session-Id", "s1"), `{"model":"m"}`, false)
	if sess.ID != "s1" {
		t.Fatalf("configured source order must win: %+v", sess)
	}
}

func TestSessionDuplicateRejected(t *testing.T) {
	p := testProvider()
	// Case variants with identical values are still duplicates.
	mustFail(t, p, hdr("Session-Id", "abc", "SESSION-ID", "abc"), `{"model":"m"}`, false, "invalid_session_id")
	// Repeated header lines under one casing are duplicates too.
	h := http.Header{"Session-Id": {"abc", "abc"}}
	mustFail(t, p, h, `{"model":"m"}`, false, "invalid_session_id")
	// A duplicate on a disabled/losing native still fails.
	p2 := testProvider()
	p2.SessionSourceHeaders = []string{}
	mustFail(t, p2, hdr("Thread-Id", "a", "THREAD-ID", "a"), `{"model":"m"}`, false, "invalid_session_id")
}

func TestSessionInvalidLosingValueRejected(t *testing.T) {
	p := testProvider()
	// Losing native with bad grammar fails even though the winner is valid.
	mustFail(t, p, hdr("X-Session-Out", "good1", "Thread-Id", "bad value!"), `{"model":"m"}`, false, "invalid_session_id")
	// Empty value is invalid, not missing.
	mustFail(t, p, hdr("Session-Id", ""), `{"model":"m"}`, false, "invalid_session_id")
	// 257 chars too long.
	mustFail(t, p, hdr("Session-Id", "a"+strings.Repeat("b", 256)), `{"model":"m"}`, false, "invalid_session_id")
	// Leading dash rejected.
	mustFail(t, p, hdr("Session-Id", "-abc"), `{"model":"m"}`, false, "invalid_session_id")
}

func TestSessionDefaultOrderExact(t *testing.T) {
	p := testProvider()
	p.SessionSourceHeaders = nil // absent: default all seven in baseline order
	sess, _ := mustOK(t, p, hdr("Session-Id", "s1", "X-Claude-Code-Session-Id", "c1"), `{"model":"m"}`, false)
	if sess.ID != "c1" {
		t.Fatalf("default order must prefer x-claude-code-session-id: %+v", sess)
	}
	sess, _ = mustOK(t, p, hdr("X-Mavis-Session-Id", "m1", "Conversation_Id", "v1"), `{"model":"m"}`, false)
	if sess.ID != "v1" {
		t.Fatalf("default order must prefer conversation_id over x-mavis-session-id: %+v", sess)
	}
}

func TestSessionDisabledSourcesStillValidate(t *testing.T) {
	p := testProvider()
	p.SessionSourceHeaders = []string{}
	// No native selection...
	sess, _ := mustOK(t, p, hdr("Session-Id", "s1"), `{"model":"m"}`, false)
	if sess.Has {
		t.Fatalf("empty sources must disable native selection: %+v", sess)
	}
	// ...but a losing native with bad grammar still fails.
	mustFail(t, p, hdr("Session-Id", "bad value!"), `{"model":"m"}`, false, "invalid_session_id")
}

func TestSessionHopNominatedSkipped(t *testing.T) {
	p := testProvider()
	h := hdr("Session-Id", "bad value!", "Connection", "session-id")
	sess, _ := mustOK(t, p, h, `{"model":"m"}`, false)
	if sess.Has {
		t.Fatalf("nominated header must be skipped: %+v", sess)
	}
}

func TestSessionMissingOmitReject(t *testing.T) {
	p := testProvider()
	sess, _ := mustOK(t, p, hdr(), `{"model":"m"}`, false)
	if sess.Has {
		t.Fatalf("omit must yield no identity: %+v", sess)
	}
	p.SessionMissing = "reject"
	mustFail(t, p, hdr(), `{"model":"m"}`, false, "missing_session_id")
}

func TestSessionBodyFallbackTranslatedConsumed(t *testing.T) {
	p := testProvider()
	p.SessionPromptCacheKey = true
	sess, eff := mustOK(t, p, hdr(), `{"model":"m","prompt_cache_key":"body1","max_tokens":9007199254740993}`, true)
	if !sess.Has || sess.ID != "body1" {
		t.Fatalf("want body1 got %+v", sess)
	}
	if strings.Contains(string(eff), "prompt_cache_key") {
		t.Fatalf("translated must consume prompt_cache_key: %s", eff)
	}
	if !strings.Contains(string(eff), "9007199254740993") {
		t.Fatalf("bigint must survive consumption: %s", eff)
	}
	// Header beats body.
	sess, _ = mustOK(t, p, hdr("Session-Id", "s1"), `{"model":"m","prompt_cache_key":"body1"}`, true)
	if sess.ID != "s1" {
		t.Fatalf("header must beat body: %+v", sess)
	}
	// Invalid body value fails even with a header winner.
	mustFail(t, p, hdr("Session-Id", "s1"), `{"model":"m","prompt_cache_key":"bad value!"}`, true, "invalid_session_id")
	// Non-string body value fails.
	mustFail(t, p, hdr(), `{"model":"m","prompt_cache_key":42}`, true, "invalid_session_id")
}

func TestSessionBodyNativePreserved(t *testing.T) {
	p := testProvider()
	p.SessionPromptCacheKey = true
	raw := `{"model":"m","prompt_cache_key":"body1"}`
	sess, eff := mustOK(t, p, hdr(), raw, false)
	if !sess.Has || sess.ID != "body1" {
		t.Fatalf("want body1 got %+v", sess)
	}
	if string(eff) != raw {
		t.Fatalf("native must preserve exact body: %s", eff)
	}
}

func TestSessionBodyIgnoredWhenDisabled(t *testing.T) {
	p := testProvider() // prompt_cache_key disabled
	sess, eff := mustOK(t, p, hdr(), `{"model":"m","prompt_cache_key":"body1"}`, true)
	if sess.Has {
		t.Fatalf("disabled fallback must not select body: %+v", sess)
	}
	if !strings.Contains(string(eff), "prompt_cache_key") {
		t.Fatalf("disabled option must not consume the field: %s", eff)
	}
}

func TestSessionNoPolicyPassthrough(t *testing.T) {
	p := &config.Provider{Name: "up", Dialect: config.DialectAnthropic, BaseURL: "http://127.0.0.1:9"}
	sess, eff := mustOK(t, p, hdr("Session-Id", "s1", "X-Session-Out", "x1"), `{"model":"m","prompt_cache_key":"k"}`, true)
	if sess.Has {
		t.Fatalf("no policy must select nothing: %+v", sess)
	}
	if string(eff) != `{"model":"m","prompt_cache_key":"k"}` {
		t.Fatalf("no policy must not touch the body: %s", eff)
	}
}

func TestSessionErrorsCarryNoValues(t *testing.T) {
	p := testProvider()
	_, _, rerr := PrepareSession(p, hdr("Session-Id", "sekretABC"), []byte(`{"model":"m"}`), false, NoCompatFallback())
	_ = rerr
	// Duplicate path message check happens via code; assert messages are static.
	_, _, dup := PrepareSession(p, hdr("Session-Id", "sekretABC", "SESSION-ID", "sekretABC"), []byte(`{"model":"m"}`), false, NoCompatFallback())
	if strings.Contains(dup.Msg, "sekretABC") {
		t.Fatalf("error must not echo identity value: %q", dup.Msg)
	}
}
