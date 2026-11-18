// Usage metering hooks for every generation ingress: an opt-in response
// capture that lets the server append one accounting record per
// completed generation after the response ends.
//
// The [accounting] section is nil-gated like [search]/[catalog]: without
// it no Recorder exists, no writer is wrapped and the hot path is
// byte-identical. With it, each ingress handler wraps the downstream
// writer once via beginAccounting; native and translated dispatch both
// flow through the wrapper, so a single deferred finish covers both
// paths and fires only after the response is finished (streams return
// from their copy loops after the terminal chunk). Token counts come
// from the captured downstream bytes, whose shape is the ingress
// dialect's on every exit of that ingress; anything unparsable yields
// zeros. Capture failures can never fail a request.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/arhammahajan/model-router/internal/accounting"
	"github.com/arhammahajan/model-router/internal/config"
)

// beginAccounting wraps the downstream writer for metering when the
// [accounting] section is configured. It returns the writer to dispatch
// through plus a finish closure that records one entry for the request;
// callers defer finish with the final response status. The disabled path
// returns the original writer and a no-op, keeping the hot path
// byte-identical. Recording never fails the request.
func (s *Server) beginAccounting(w http.ResponseWriter, ingress, upstream, model, provider string, wantStream bool, start time.Time) (http.ResponseWriter, func(status int)) {
	if s.acc == nil {
		return w, func(int) {}
	}
	capture := &responseCapture{ResponseWriter: w}
	return capture, func(status int) {
		in, out, cached := capture.usageFor(ingress)
		s.acc.Complete(accounting.Record{
			TS:           float64(time.Now().UnixNano()) / 1e9,
			Model:        model,
			Provider:     provider,
			Ingress:      ingress,
			Upstream:     upstream,
			Status:       status,
			InputTokens:  in,
			OutputTokens: out,
			CachedTokens: cached,
			DurationMS:   time.Since(start).Milliseconds(),
			Stream:       wantStream,
		})
	}
}

// accountingTailBytes bounds the retained downstream tail. Usage rides
// the terminal chunk of a stream and the tail of a JSON body, so a
// bounded tail is enough; oversize responses meter with zero tokens
// rather than unbounded buffering.
const accountingTailBytes = 128 * 1024

// responseCapture tees downstream response bytes into a bounded rolling
// tail and stays transparent to the rest of the stack: Header and
// WriteHeader pass through the embedded writer, and Unwrap exposes it so
// http.ResponseController keeps finding the real Flusher and write
// deadlines exactly as before.
type responseCapture struct {
	http.ResponseWriter
	tail []byte
}

func (c *responseCapture) Write(p []byte) (int, error) {
	c.keep(p)
	return c.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer's
// Flush/SetWriteDeadline support, preserving wrapped behavior.
func (c *responseCapture) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// keep appends p to the rolling tail, dropping the oldest bytes once the
// bound is exceeded.
func (c *responseCapture) keep(p []byte) {
	if len(p) >= accountingTailBytes {
		c.tail = append(c.tail[:0], p[len(p)-accountingTailBytes:]...)
		return
	}
	if len(c.tail)+len(p) > accountingTailBytes {
		drop := len(c.tail) + len(p) - accountingTailBytes
		copy(c.tail, c.tail[drop:])
		c.tail = c.tail[:len(c.tail)-drop]
	}
	c.tail = append(c.tail, p...)
}

// usage extracts terminal token counts from the captured tail. The bytes
// are Chat-dialect on every handleChat exit: native passthrough and all
// translated responses normalize to Chat JSON or Chat SSE, whose usage
// member is prompt_tokens/completion_tokens with cached_tokens under
// prompt_tokens_details. Anything malformed yields zeros.
func (c *responseCapture) usage() (input, output, cached int64) {
	return extractChatUsage(c.tail)
}

// usageFor extracts terminal token counts in the shape of the given
// ingress dialect: every exit of an ingress is downstream-shaped in that
// dialect, whatever the upstream was. Long streams can evict early
// chunks from the bounded tail; the affected counters then meter as zero
// rather than inventing numbers.
func (c *responseCapture) usageFor(ingress string) (input, output, cached int64) {
	switch ingress {
	case config.DialectAnthropic:
		return extractAnthropicUsage(c.tail)
	case config.DialectOpenAIResponses:
		return extractResponsesUsage(c.tail)
	case config.DialectGemini:
		return extractGeminiUsage(c.tail)
	default:
		return extractChatUsage(c.tail)
	}
}

// anthropicUsageEnvelope is the top-level "usage" member of an Anthropic
// JSON response; message_start and message_delta chunks repeat the shape.
type anthropicUsage struct {
	InputTokens              float64 `json:"input_tokens"`
	OutputTokens             float64 `json:"output_tokens"`
	CacheReadInputTokens     float64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens float64 `json:"cache_creation_input_tokens"`
}

type anthropicEvent struct {
	Type  string          `json:"type"`
	Usage *anthropicUsage `json:"usage"`
	// message_delta carries usage next to the delta member.
	Message *struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
}

type anthropicUsageEnvelope struct {
	Usage *anthropicUsage `json:"usage"`
}

func extractAnthropicUsage(tail []byte) (int64, int64, int64) {
	trimmed := bytes.TrimSpace(tail)
	if len(trimmed) == 0 {
		return 0, 0, 0
	}
	var in, out, cached int64
	if isSSE(trimmed) {
		// message_start holds input tokens (and cache reads) inside its
		// message member; the last message_delta holds the authoritative
		// output total at its top level.
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			payload, ok := sseDataPayload(line)
			if !ok {
				continue
			}
			var event anthropicEvent
			if json.Unmarshal(payload, &event) != nil {
				continue
			}
			switch event.Type {
			case "message_start":
				if event.Message != nil && event.Message.Usage != nil {
					in = clampTokens(event.Message.Usage.InputTokens)
					cached = clampTokens(event.Message.Usage.CacheReadInputTokens)
				}
			case "message_delta":
				if event.Usage != nil && event.Usage.OutputTokens > 0 {
					out = clampTokens(event.Usage.OutputTokens)
				}
			}
		}
		return in, out, cached
	}
	var env anthropicUsageEnvelope
	if json.Unmarshal(trimmed, &env) != nil || env.Usage == nil {
		return 0, 0, 0
	}
	return clampTokens(env.Usage.InputTokens), clampTokens(env.Usage.OutputTokens),
		clampTokens(env.Usage.CacheReadInputTokens)
}

// responsesUsageBody mirrors the Responses usage member;
// response.completed events repeat it under response.usage.
type responsesUsageBody struct {
	InputTokens  float64                `json:"input_tokens"`
	OutputTokens float64                `json:"output_tokens"`
	Details      *responsesUsageDetails `json:"input_tokens_details"`
}

type responsesUsageDetails struct {
	CachedTokens float64 `json:"cached_tokens"`
}

type responsesUsageEnvelope struct {
	Usage *responsesUsageBody `json:"usage"`
}

type responsesCompletedEnvelope struct {
	Response *struct {
		Usage *responsesUsageBody `json:"usage"`
	} `json:"response"`
}

func extractResponsesUsage(tail []byte) (int64, int64, int64) {
	trimmed := bytes.TrimSpace(tail)
	if len(trimmed) == 0 {
		return 0, 0, 0
	}
	var usage *responsesUsageBody
	if isSSE(trimmed) {
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			payload, ok := sseDataPayload(line)
			if !ok {
				continue
			}
			var completed responsesCompletedEnvelope
			if json.Unmarshal(payload, &completed) != nil {
				continue
			}
			if completed.Response != nil && completed.Response.Usage != nil {
				usage = completed.Response.Usage
			}
		}
	} else {
		var env responsesUsageEnvelope
		if json.Unmarshal(trimmed, &env) == nil {
			usage = env.Usage
		}
	}
	if usage == nil {
		return 0, 0, 0
	}
	var cached int64
	if usage.Details != nil {
		cached = clampTokens(usage.Details.CachedTokens)
	}
	return clampTokens(usage.InputTokens), clampTokens(usage.OutputTokens), cached
}

// geminiUsageMetadata is the usageMetadata member of a Gemini response
// element; it appears in JSON bodies, SSE chunks and array elements.
type geminiUsageMetadata struct {
	PromptTokenCount        float64 `json:"promptTokenCount"`
	CandidatesTokenCount    float64 `json:"candidatesTokenCount"`
	CachedContentTokenCount float64 `json:"cachedContentTokenCount"`
}

func extractGeminiUsage(tail []byte) (int64, int64, int64) {
	// Gemini streams as SSE data frames or an incremental JSON array, and
	// completes as a plain JSON object; the terminal element carries
	// usageMetadata. Scan every occurrence and keep the last parseable
	// one: evicted early chunks simply contribute nothing.
	const key = `"usageMetadata"`
	var in, out, cached int64
	for offset := 0; ; {
		at := bytes.Index(tail[offset:], []byte(key))
		if at < 0 {
			break
		}
		at += offset
		open := bytes.IndexByte(tail[at:], '{')
		if open < 0 {
			break
		}
		open += at
		if end := braceMatch(tail, open); end > open {
			var meta geminiUsageMetadata
			if json.Unmarshal(tail[open:end+1], &meta) == nil {
				in = clampTokens(meta.PromptTokenCount)
				out = clampTokens(meta.CandidatesTokenCount)
				cached = clampTokens(meta.CachedContentTokenCount)
			}
		}
		offset = at + len(key)
	}
	return in, out, cached
}

// braceMatch returns the index of the closing brace balancing the opening
// one at open, honoring JSON string escapes; -1 when unbalanced.
func braceMatch(b []byte, open int) int {
	depth := 0
	inString := false
	escaped := false
	for i := open; i < len(b); i++ {
		if inString {
			if escaped {
				escaped = false
			} else if b[i] == '\\' {
				escaped = true
			} else if b[i] == '"' {
				inString = false
			}
			continue
		}
		switch b[i] {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// chatUsageEnvelope is the top-level "usage" member of a Chat response
// body or data chunk.
type chatUsageEnvelope struct {
	Usage *chatUsage `json:"usage"`
}

type chatUsage struct {
	PromptTokens     float64            `json:"prompt_tokens"`
	CompletionTokens float64            `json:"completion_tokens"`
	PromptDetails    *chatPromptDetails `json:"prompt_tokens_details"`
}

type chatPromptDetails struct {
	CachedTokens float64 `json:"cached_tokens"`
}

func (u *chatUsage) totals() (int64, int64, int64) {
	cached := float64(0)
	if u.PromptDetails != nil {
		cached = u.PromptDetails.CachedTokens
	}
	return clampTokens(u.PromptTokens), clampTokens(u.CompletionTokens), clampTokens(cached)
}

// clampTokens converts a parsed counter to int64, flooring negatives at
// zero: malformed counters never invent usage.
func clampTokens(v float64) int64 {
	if v <= 0 {
		return 0
	}
	return int64(v)
}

func extractChatUsage(tail []byte) (int64, int64, int64) {
	trimmed := bytes.TrimSpace(tail)
	if len(trimmed) == 0 {
		return 0, 0, 0
	}
	if isSSE(trimmed) {
		// Streamed response: the last data chunk carrying a usage
		// object holds the terminal totals (ordinary chunks say
		// "usage": null or omit it entirely).
		var in, out, cached int64
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			payload, ok := sseDataPayload(line)
			if !ok {
				continue
			}
			var env chatUsageEnvelope
			if json.Unmarshal(payload, &env) != nil || env.Usage == nil {
				continue
			}
			in, out, cached = env.Usage.totals()
		}
		return in, out, cached
	}
	var env chatUsageEnvelope
	if json.Unmarshal(trimmed, &env) != nil || env.Usage == nil {
		return 0, 0, 0
	}
	return env.Usage.totals()
}

// isSSE reports whether the captured tail is an SSE stream body.
func isSSE(b []byte) bool {
	return bytes.Contains(b, []byte("\ndata:")) || bytes.HasPrefix(b, []byte("data:"))
}

// sseDataPayload returns the JSON payload of one SSE data line, skipping
// blank, non-data and [DONE] lines.
func sseDataPayload(line []byte) ([]byte, bool) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}
