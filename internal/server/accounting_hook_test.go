// Integration tests for the opt-in usage accounting hook: records on the
// native and translated Chat paths (JSON and streams), token extraction
// from the captured downstream bytes, nil-gating when [accounting] is
// absent, error statuses, and fail-open on an unwritable target.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/accounting"
	"github.com/arhammahajan/model-router/internal/config"
)

// meteredHarness builds a fake upstream plus router. accPath empty means
// no [accounting] section. The returned Server is the real construction
// so nil-gating can be asserted.
func meteredHarness(t *testing.T, accPath string, upstream http.HandlerFunc) (*httptest.Server, *httptest.Server, *Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	body := `
[server]
auth_mode = "none"
allow_dynamic_routes = false

[retry]
max_attempts = 1

[providers.local]
dialect = "openai-chat"
base_url = "` + up.URL + `"

[providers.anth]
dialect = "anthropic"
base_url = "` + up.URL + `"

[providers.resp]
dialect = "openai-responses"
base_url = "` + up.URL + `"

[providers.gem]
dialect = "gemini"
base_url = "` + up.URL + `"

[[models]]
name = "native-llama"
provider = "local"
model = "llama3.1"

[[models]]
name = "claude-haiku"
provider = "anth"
model = "claude-haiku-4-5"

[[models]]
name = "claude-via-llama"
provider = "local"
dialect = "openai-chat"
model = "llama3.1"

[[models]]
name = "resp-model"
provider = "resp"
model = "resp-1"

[[models]]
name = "gem-model"
provider = "gem"
model = "gem-1"
`
	if accPath != "" {
		body += "\n[accounting]\npath = \"" + accPath + "\"\n"
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "router.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	router := httptest.NewServer(srv.Handler())
	t.Cleanup(router.Close)
	return up, router, srv
}

func meteredRecords(t *testing.T, path string) []accounting.Record {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read usage log: %v", err)
	}
	var out []accounting.Record
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec accounting.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

const nativeChatJSON = `{"id":"c1","object":"chat.completion","model":"llama3.1",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},` +
	`"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":7,"completion_tokens":3,` +
	`"prompt_tokens_details":{"cached_tokens":2},"total_tokens":10}}`

func TestAccountingHookNativeJSON(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	_, router, srv := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeChatJSON))
	})
	if srv.acc == nil {
		t.Fatal("recorder must exist when [accounting] is configured")
	}
	status, _, data := post(t, router.URL+"/v1/chat/completions",
		`{"model":"native-llama","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if string(data) != nativeChatJSON {
		t.Fatalf("accounting must not alter the response: %s", data)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Model != "native-llama" || rec.Provider != "local" {
		t.Fatalf("route fields wrong: %+v", rec)
	}
	if rec.Ingress != config.DialectOpenAIChat || rec.Upstream != config.DialectOpenAIChat {
		t.Fatalf("dialect fields wrong: %+v", rec)
	}
	if rec.Status != 200 || rec.Stream {
		t.Fatalf("status/stream wrong: %+v", rec)
	}
	if rec.InputTokens != 7 || rec.OutputTokens != 3 || rec.CachedTokens != 2 {
		t.Fatalf("tokens not extracted: %+v", rec)
	}
	if rec.TS <= 0 || rec.DurationMS < 0 {
		t.Fatalf("ts/duration wrong: %+v", rec)
	}
}

func TestAccountingHookTranslatedJSON(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":7,"output_tokens":3}}`))
	})
	status, _, data := post(t, router.URL+"/v1/chat/completions",
		`{"model":"claude-haiku","messages":[{"role":"user","content":"Hi"}],"max_tokens":16}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Model != "claude-haiku" || rec.Provider != "anth" {
		t.Fatalf("route fields wrong: %+v", rec)
	}
	if rec.Ingress != config.DialectOpenAIChat || rec.Upstream != config.DialectAnthropic {
		t.Fatalf("translated path must record the upstream dialect: %+v", rec)
	}
	if rec.Status != 200 || rec.Stream {
		t.Fatalf("status/stream wrong: %+v", rec)
	}
	if rec.InputTokens != 7 || rec.OutputTokens != 3 || rec.CachedTokens != 0 {
		t.Fatalf("translated tokens wrong: %+v", rec)
	}
}

func TestAccountingHookNativeStream(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4,\"prompt_tokens_details\":{\"cached_tokens\":5}}}\n\n" +
		"data: [DONE]\n\n"
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	status, _, data := post(t, router.URL+"/v1/chat/completions",
		`{"model":"native-llama","messages":[{"role":"user","content":"Hi"}],"stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if !strings.Contains(string(data), "[DONE]") {
		t.Fatalf("stream not flushed through the capture: %q", data)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if !rec.Stream {
		t.Fatalf("stream requests must be flagged: %+v", rec)
	}
	if rec.InputTokens != 11 || rec.OutputTokens != 4 || rec.CachedTokens != 5 {
		t.Fatalf("terminal chunk tokens not extracted: %+v", rec)
	}
}

func TestAccountingHookUpstreamErrorStatus(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"api_error"}}`))
	})
	status, _, _ := post(t, router.URL+"/v1/chat/completions",
		`{"model":"native-llama","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 500 {
		t.Fatalf("native upstream status must pass through: %d", status)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].Status != 500 {
		t.Fatalf("error status must be recorded: %+v", recs[0])
	}
}

func TestAccountingAbsentSectionIsNilSafe(t *testing.T) {
	dir := t.TempDir()
	accPath := filepath.Join(dir, "usage.jsonl")
	_, router, srv := meteredHarness(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeChatJSON))
	})
	if srv.acc != nil {
		t.Fatal("absent section must leave the recorder nil")
	}
	status, _, data := post(t, router.URL+"/v1/chat/completions",
		`{"model":"native-llama","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if _, err := os.Stat(accPath); !os.IsNotExist(err) {
		t.Fatal("no records must be written when the section is absent")
	}
}

func TestAccountingHookFailOpenOnUnwritablePath(t *testing.T) {
	// The target path exists as a directory, so every append fails; the
	// request must still succeed and no error may surface.
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.Mkdir(accPath, 0o755); err != nil {
		t.Fatal(err)
	}
	_, router, srv := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeChatJSON))
	})
	if srv.acc == nil {
		t.Fatal("recorder must exist")
	}
	status, _, data := post(t, router.URL+"/v1/chat/completions",
		`{"model":"native-llama","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 || string(data) != nativeChatJSON {
		t.Fatalf("metering failure must never fail the request: %d %s", status, data)
	}
}

// Metering must survive the reader side as well: the written file parses
// with the tolerant CLI loader.
func TestAccountingFileParsesWithUsageLoader(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeChatJSON))
	})
	for i := 0; i < 2; i++ {
		if status, _, _ := post(t, router.URL+"/v1/chat/completions",
			`{"model":"native-llama","messages":[{"role":"user","content":"Hi"}]}`, nil); status != 200 {
			t.Fatalf("request %d failed", i)
		}
	}
	data, err := os.ReadFile(accPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 2 {
		t.Fatalf("want 2 newline-terminated records, got %d", got)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("final line must be newline-terminated")
	}
}

const nativeAnthropicJSON = `{"id":"msg_1","type":"message","role":"assistant",` +
	`"model":"claude-haiku-4-5","content":[{"type":"text","text":"hi"}],` +
	`"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":4,` +
	`"cache_read_input_tokens":2}}`

func TestAccountingHookAnthropicNativeJSON(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeAnthropicJSON))
	})
	status, _, data := post(t, router.URL+"/v1/messages",
		`{"model":"claude-haiku","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if string(data) != nativeAnthropicJSON {
		t.Fatalf("accounting must not alter the response: %s", data)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Model != "claude-haiku" || rec.Provider != "anth" {
		t.Fatalf("route fields wrong: %+v", rec)
	}
	if rec.Ingress != config.DialectAnthropic || rec.Upstream != config.DialectAnthropic {
		t.Fatalf("dialect fields wrong: %+v", rec)
	}
	if rec.Status != 200 || rec.Stream {
		t.Fatalf("status/stream wrong: %+v", rec)
	}
	if rec.InputTokens != 9 || rec.OutputTokens != 4 || rec.CachedTokens != 2 {
		t.Fatalf("tokens not extracted: %+v", rec)
	}
}

func TestAccountingHookAnthropicNativeStream(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":8,"cache_read_input_tokens":3,"output_tokens":1}}}` + "\n\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	status, _, _ := post(t, router.URL+"/v1/messages",
		`{"model":"claude-haiku","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if !rec.Stream || rec.Ingress != config.DialectAnthropic {
		t.Fatalf("stream/ingress wrong: %+v", rec)
	}
	if rec.InputTokens != 8 || rec.OutputTokens != 6 || rec.CachedTokens != 3 {
		t.Fatalf("stream tokens not extracted: %+v", rec)
	}
}

func TestAccountingHookAnthropicTranslated(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeChatJSON))
	})
	status, _, data := post(t, router.URL+"/v1/messages",
		`{"model":"claude-via-llama","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Ingress != config.DialectAnthropic || rec.Upstream != config.DialectOpenAIChat {
		t.Fatalf("translated path must record both dialects: %+v", rec)
	}
	// The Anthropic destination reports input_tokens NET of the cached
	// share (7 prompt - 2 cached), mirroring Anthropic wire semantics.
	if rec.InputTokens != 5 || rec.OutputTokens != 3 || rec.CachedTokens != 2 {
		t.Fatalf("translated tokens wrong: %+v", rec)
	}
}

const nativeResponsesJSON = `{"id":"resp_1","object":"response","status":"completed",` +
	`"model":"resp-1","output":[],"usage":{"input_tokens":5,"output_tokens":2,` +
	`"input_tokens_details":{"cached_tokens":1},"total_tokens":7}}`

func TestAccountingHookResponsesNativeJSON(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeResponsesJSON))
	})
	status, _, data := post(t, router.URL+"/v1/responses",
		`{"model":"resp-model","input":"Hi"}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Ingress != config.DialectOpenAIResponses || rec.Upstream != config.DialectOpenAIResponses {
		t.Fatalf("dialect fields wrong: %+v", rec)
	}
	if rec.InputTokens != 5 || rec.OutputTokens != 2 || rec.CachedTokens != 1 {
		t.Fatalf("tokens not extracted: %+v", rec)
	}
}

func TestAccountingHookResponsesNativeStream(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	sse := `data: {"type":"response.output_item.added","output_index":0}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed",` +
		`"usage":{"input_tokens":13,"output_tokens":6,"input_tokens_details":{"cached_tokens":2}}}}` + "\n\n"
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	status, _, _ := post(t, router.URL+"/v1/responses",
		`{"model":"resp-model","input":"Hi","stream":true}`, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if !rec.Stream || rec.Ingress != config.DialectOpenAIResponses {
		t.Fatalf("stream/ingress wrong: %+v", rec)
	}
	if rec.InputTokens != 13 || rec.OutputTokens != 6 || rec.CachedTokens != 2 {
		t.Fatalf("stream tokens not extracted: %+v", rec)
	}
}

const nativeGeminiJSON = `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},` +
	`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,` +
	`"candidatesTokenCount":6,"cachedContentTokenCount":4,"totalTokenCount":18}}`

func TestAccountingHookGeminiNativeJSON(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeGeminiJSON))
	})
	status, _, data := post(t, router.URL+"/v1beta/models/gem-model:generateContent", `{}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, data)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Model != "gem-model" || rec.Provider != "gem" {
		t.Fatalf("route fields wrong: %+v", rec)
	}
	if rec.Ingress != config.DialectGemini || rec.Upstream != config.DialectGemini {
		t.Fatalf("dialect fields wrong: %+v", rec)
	}
	if rec.InputTokens != 12 || rec.OutputTokens != 6 || rec.CachedTokens != 4 {
		t.Fatalf("tokens not extracted: %+v", rec)
	}
}

func TestAccountingHookGeminiNativeStream(t *testing.T) {
	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	sse := `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3,` +
		`"cachedContentTokenCount":1,"totalTokenCount":12}}` + "\n\n"
	_, router, _ := meteredHarness(t, accPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	status, _, _ := post(t, router.URL+"/v1beta/models/gem-model:streamGenerateContent?alt=sse", `{}`, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	recs := meteredRecords(t, accPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if !rec.Stream || rec.Ingress != config.DialectGemini {
		t.Fatalf("stream/ingress wrong: %+v", rec)
	}
	if rec.InputTokens != 9 || rec.OutputTokens != 3 || rec.CachedTokens != 1 {
		t.Fatalf("stream tokens not extracted: %+v", rec)
	}
}
