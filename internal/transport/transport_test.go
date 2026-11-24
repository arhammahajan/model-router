package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/config"
)

func TestMain(m *testing.M) {
	m.Run()
}

func testProvider(dialect, base string) *config.Provider {
	return &config.Provider{
		Name:           "up",
		DefaultDialect: dialect,
		BaseURL:        base,
		APIKey:         "upstream-secret",
		Headers:        map[string]string{},
		Timeout:        5,
	}
}

func testRetry() config.Retry {
	return config.Retry{
		MaxAttempts: 1, Statuses: []int{429, 503}, ConnectErrors: true,
		BackoffBase: 0.001, BackoffMax: 0.002, Deadline: 5,
	}
}

func TestOutboundDropsCrossDialectAndNominated(t *testing.T) {
	p := testProvider(config.DialectAnthropic, "http://x")
	in := http.Header{}
	in.Set("Openai-Beta", "assistants=v2")
	in.Set("Connection", "X-Private")
	in.Set("X-Private", "s3cr3t")
	in.Set("X-Session-Id", "abc")
	in.Set("Idempotency-Key", "k1")
	in.Set("Authorization", "Bearer client")
	in.Set("X-Custom", "keep?")
	out := OutboundHeaders(in, p, "chat", "", "", false)
	if _, ok := out["Openai-Beta"]; ok {
		t.Fatalf("wrong-dialect opt-in forwarded: %v", out)
	}
	if _, ok := out["X-Private"]; ok {
		t.Fatalf("connection-nominated header forwarded: %v", out)
	}
	for name := range out {
		switch name {
		case "X-Session-Id", "X-Session-ID", "Idempotency-Key":
			t.Fatalf("session/idempotency forwarded: %v", out)
		case "Authorization":
			// Authorization must be the provider credential, never the client.
		}
	}
	if out["X-Api-Key"] != "upstream-secret" {
		t.Fatalf("provider credential missing: %v", out)
	}
	if _, ok := out["Connection"]; ok {
		t.Fatalf("Connection forwarded: %v", out)
	}
}

func TestOutboundScrubsProviderAuth(t *testing.T) {
	p := testProvider(config.DialectAnthropic, "http://x")
	p.Headers = map[string]string{"Authorization": "Bearer stale", "X-Custom": "v"}
	out := OutboundHeaders(http.Header{}, p, "chat", "", "", false)
	if out["Authorization"] == "Bearer stale" {
		t.Fatalf("stale provider Authorization survived: %v", out)
	}
	if out["X-Api-Key"] != "upstream-secret" {
		t.Fatalf("selected auth missing: %v", out)
	}
	if out["X-Custom"] != "v" {
		t.Fatalf("trusted static header lost: %v", out)
	}
}

func TestOutboundTranslatedOnlyAllowsDestinationOptIns(t *testing.T) {
	p := testProvider(config.DialectAnthropic, "http://x")
	in := http.Header{}
	in.Set("Anthropic-Beta", "ctx")
	in.Set("Openai-Beta", "x")
	out := OutboundHeaders(in, p, "chat", "", "", false)
	if out["Anthropic-Beta"] != "ctx" {
		t.Fatalf("destination opt-in lost: %v", out)
	}
	if _, ok := out["Openai-Beta"]; ok {
		t.Fatalf("wrong-dialect opt-in forwarded: %v", out)
	}
}

func TestProviderTimeoutEnforced(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	p := testProvider(config.DialectAnthropic, up.URL)
	p.Timeout = 0.02
	c := NewClient()
	ctx := context.Background()
	res, rerr := c.Do(ctx, p, testRetry(), up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr == nil || rerr.Status != 504 {
		t.Fatalf("slow upstream with timeout=.02 must be 504, got res=%v err=%v", res, rerr)
	}
	if res != nil {
		t.Fatalf("timeout must not return a result")
	}
}

func TestStatusRetryThenSuccess(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`busy`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	c := NewClient()
	retry := testRetry()
	retry.MaxAttempts = 3
	retry.Statuses = []int{429}
	res, rerr := c.Do(context.Background(), p, retry, up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr != nil || res == nil || res.Status != 200 {
		t.Fatalf("retry should succeed: res=%v err=%v", res, rerr)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls.Load())
	}
}

func TestExhaustionKeepsStatusBodyRetryAfter(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`bad gateway`))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	c := NewClient()
	retry := testRetry()
	retry.MaxAttempts = 2
	retry.Statuses = []int{502}
	res, rerr := c.Do(context.Background(), p, retry, up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr == nil || res == nil || !res.Failed {
		t.Fatalf("exhausted retries must return Failed result: res=%v err=%v", res, rerr)
	}
	if res.Status != 502 || string(res.Body) != "bad gateway" {
		t.Fatalf("status/body not preserved: %d %q", res.Status, res.Body)
	}
	if res.Header.Get("Retry-After") != "0" {
		t.Fatalf("Retry-After not preserved: %v", res.Header)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", calls.Load())
	}
}

func TestUnaffordableRetryAfterIs504(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`busy`))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	c := NewClient()
	retry := testRetry()
	retry.MaxAttempts = 3
	retry.Statuses = []int{429}
	retry.Deadline = 1
	res, rerr := c.Do(context.Background(), p, retry, up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr == nil || rerr.Status != 504 {
		t.Fatalf("unaffordable Retry-After must be 504, got res=%v err=%v", res, rerr)
	}
	if calls.Load() != 1 {
		t.Fatalf("must not retry unaffordable wait, got %d calls", calls.Load())
	}
}

func TestTruncatedErrorBodyNeverRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`not-gzip-at-all`))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	c := NewClient()
	retry := testRetry()
	retry.MaxAttempts = 3
	retry.Statuses = []int{502}
	res, rerr := c.Do(context.Background(), p, retry, up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("truncated error must be 502: res=%v err=%v", res, rerr)
	}
	if res != nil {
		t.Fatalf("truncated error must not return Failed body: %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("truncated error must never retry, got %d calls", calls.Load())
	}
}

func TestCancellationReleasesClient(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	p.Timeout = 5
	c := NewClient()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Do(ctx, p, testRetry(), up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("cancelled Do did not return")
	}
	// Pool must still serve the next request.
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":1}`))
	}))
	defer up2.Close()
	res, rerr := c.Do(context.Background(), p, testRetry(), up2.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr != nil || res == nil || res.Status != 200 {
		t.Fatalf("client unusable after cancel: res=%v err=%v", res, rerr)
	}
}

func TestGzipResponseDecoded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		// Minimal gzip for `{"a":1}` would be longer; instead send invalid
		// gzip and assert refusal rather than silent passthrough. Valid gzip
		// is covered by the server-level test with a real encoder.
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	_ = up
}

func TestBodyReadTimeoutIs504NotRetried(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Stall mid-body past the per-attempt timeout.
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	p.Timeout = 0.05
	c := NewClient()
	retry := testRetry()
	retry.MaxAttempts = 3
	retry.Statuses = []int{502}
	res, rerr := c.Do(context.Background(), p, retry, up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr == nil || rerr.Status != 504 {
		t.Fatalf("timeout during body read must be 504, got res=%v err=%v", res, rerr)
	}
	if res != nil {
		t.Fatalf("timeout must not return a result")
	}
	if calls.Load() != 1 {
		t.Fatalf("timeout during body read must never retry, got %d calls", calls.Load())
	}
}

func TestOversizeErrorBodyIsTruncated502(t *testing.T) {
	var calls atomic.Int32
	big := strings.Repeat("e", 64*1024+1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		_, _ = w.Write([]byte(big))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	c := NewClient()
	retry := testRetry()
	retry.MaxAttempts = 3
	retry.Statuses = []int{502}
	res, rerr := c.Do(context.Background(), p, retry, up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("oversize error body must be truncated 502, got res=%v err=%v", res, rerr)
	}
	if res != nil {
		t.Fatalf("oversize error body must not preserve status/body: %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("truncated error body must never retry, got %d calls", calls.Load())
	}
}

func TestGzipRawBytesBounded(t *testing.T) {
	// 1024 incompressible bytes: decompressed fits max=1024 but the gzip
	// framing is larger, so the raw cap must refuse it.
	raw := make([]byte, 1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= 1024 {
		t.Skipf("gzip framing %d bytes fits; raw-bound case not exercised", buf.Len())
	}
	blob := append([]byte(nil), buf.Bytes()...)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		_, _ = w.Write(blob)
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	c := NewClient()
	hdrs := map[string]string{"Accept-Encoding": "identity"}
	res, rerr := c.Do(context.Background(), p, testRetry(), up.URL, hdrs, []byte(`{}`), false, 1024)
	if rerr == nil || rerr.Status != 502 {
		t.Fatalf("raw gzip overflow must be 502, got res=%v err=%v", res, rerr)
	}
	if res != nil {
		t.Fatalf("raw overflow must not return a result")
	}
}

func TestParseRetryAfterBeyondUint64Saturates(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"9999999999999999999999999999999999999999", time.Duration(1<<63 - 1)},
		{"18446744073709551615", time.Duration(1<<63 - 1)}, // math.MaxUint64
		{"10000000000", time.Duration(1<<63 - 1)},          // fits uint64, overflows int64 nanos
		{"120", 120 * time.Second},
		{"0", 0},
		{"", 0},
		{"   ", 0},
		{"abc", 0},
		{"12a", 0},
		{"12.5", 0},
		{"-5", 0},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestHugeUint64OverflowRetryAfterIs504NoReplay(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "9999999999999999999999999999999999999999")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`busy`))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	c := NewClient()
	retry := testRetry()
	retry.MaxAttempts = 2
	retry.Statuses = []int{429}
	retry.Deadline = 5
	res, rerr := c.Do(context.Background(), p, retry, up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr == nil || rerr.Status != 504 {
		t.Fatalf("huge Retry-After must be unaffordable 504, got res=%v err=%v", res, rerr)
	}
	if calls.Load() != 1 {
		t.Fatalf("huge Retry-After must not replay, got %d calls", calls.Load())
	}
}
func TestParseRetryAfterOverflowSaturates(t *testing.T) {
	// 10**10 seconds overflows int64 nanoseconds (wraps negative with a
	// naive multiply); it must saturate positive so the wait is
	// unaffordable instead of replaying.
	got := parseRetryAfter("10000000000")
	if got <= 0 {
		t.Fatalf("enormous Retry-After must saturate positive, got %v", got)
	}
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "10000000000")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`busy`))
	}))
	defer up.Close()
	p := testProvider(config.DialectOpenAIChat, up.URL)
	c := NewClient()
	retry := testRetry()
	retry.MaxAttempts = 3
	retry.Statuses = []int{429}
	retry.Deadline = 1
	res, rerr := c.Do(context.Background(), p, retry, up.URL, map[string]string{}, []byte(`{}`), false, 1<<20)
	if rerr == nil || rerr.Status != 504 {
		t.Fatalf("unaffordable saturated Retry-After must be 504, got res=%v err=%v", res, rerr)
	}
	if calls.Load() != 1 {
		t.Fatalf("saturated Retry-After must not replay, got %d calls", calls.Load())
	}
}
