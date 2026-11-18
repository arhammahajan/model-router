// Package searxng is an explicitly bound, opt-in SearXNG JSON search seam.
//
// Contract (no default activation): this package is imported by nothing in
// the router. Wiring it into config, generation, response shaping or citation
// rendering is later work; constructing a Client performs no I/O and sends no
// request until Search is called.
//
// Wire facts (verified against docs.searxng.org/dev/search_api.html and
// dev/result_types/main/mainresult.html): a GET to {endpoint}/search with
// query parameters q=<query verbatim> and format=json returns a JSON object
// whose "results" array holds main results with string fields url, title and
// content (plus engine-specific extras, which are ignored). POST form search
// and the csv/rss formats are not used. A disabled json format surfaces as a
// non-2xx status. The q string is passed through verbatim: engine-specific
// syntax such as site: is never interpreted or approximated here.
//
// Bounds and hygiene: bounded query length and result count, a bounded body
// before and after gzip decompression, identity/gzip only, redirects refused
// (never followed), no retries, no result-URL fetching, own timeout on top
// of the caller's context, static sanitized errors that never echo the
// query, body, URLs or headers, and no logging of any kind.
package searxng

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds. All are admission gates: violating input is rejected, violating
// payload is a sanitized error, overlong text fields are truncated.
const (
	// DefaultTimeout bounds one Search attempt including body consumption.
	DefaultTimeout = 10 * time.Second
	// MaxQueryRunes bounds a search query.
	MaxQueryRunes = 500
	// DefaultCount applies when Request.Count is 0.
	DefaultCount = 5
	// MaxCount bounds Request.Count and the returned slice.
	MaxCount = 10
	// MaxBodyBytes bounds the response before and after decompression.
	MaxBodyBytes = 1 << 20
	// MaxTitleRunes and MaxSnippetRunes truncate overlong text fields.
	MaxTitleRunes   = 500
	MaxSnippetRunes = 2000
)

// version mirrors the router User-Agent product token.
const version = "model-router-go/0.1.0"

// Static failures. All are value-free sentinels: they never carry the
// endpoint, query, count, headers, status, body or raw errors.
var (
	ErrInvalidEndpoint = errors.New("searxng: invalid endpoint")
	ErrInvalidHeader   = errors.New("searxng: invalid header")
	ErrInvalidQuery    = errors.New("searxng: invalid query")
	ErrInvalidCount    = errors.New("searxng: invalid result count")
	ErrRequest         = errors.New("searxng: request failed")
	ErrRedirect        = errors.New("searxng: redirect refused")
	ErrStatus          = errors.New("searxng: non-2xx response")
	ErrEncoding        = errors.New("searxng: unsupported content encoding")
	ErrTooLarge        = errors.New("searxng: body exceeds the size bound")
	ErrBody            = errors.New("searxng: body could not be read")
	ErrJSON            = errors.New("searxng: malformed result shape")
)

// httpToken mirrors the repo's reviewed header-name rule (config.checkHeaders):
// HTTP token names with printable-ASCII values.
var httpToken = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// reservedSessionHeaders mirrors the repo's static session-header ban:
// conversation identity must never travel as a static configured header.
var reservedSessionHeaders = map[string]bool{
	"x-claude-code-session-id": true, "session-id": true, "session_id": true,
	"thread-id": true, "conversation_id": true, "x-session-id": true,
	"x-mavis-session-id": true, "x-opencode-session": true,
}

// Request is one typed search. Only admission actually enforced by this
// package is exposed: the query and the result count. Engine-specific
// filters (categories, language, time range, safe search) are omitted until
// a continuation enforces them.
type Request struct {
	Query string
	Count int // 0 means DefaultCount; otherwise 1..MaxCount
}

// Result is one normalized main result. ID uses lowercased scheme and
// host with the original escaped path and query, excluding the fragment.
// URL retains the link as served. Title and Snippet are trimmed and bounded.
type Result struct {
	ID      string
	Title   string
	URL     string
	Snippet string
}

// Client is an immutable, explicitly bound SearXNG JSON search client.
// The zero value is not usable; construct with NewClient.
type Client struct {
	searchURL url.URL
	headers   map[string]string
	http      *http.Client
}

// NewClient binds an operator-trusted endpoint with an optional separate
// credential/header channel. The endpoint must be a bare http(s) URL with a
// host, an optional path, and no userinfo, query or fragment; credentials
// travel only in headers, never in the URL. Headers follow the repo's
// reviewed static-header standard (token names, printable-ASCII values, no
// static session headers) and touch only this client's search requests.
// Framing (Accept, Accept-Encoding, User-Agent) is enforced at send time and
// cannot be overridden by statics.
func NewClient(endpoint string, headers map[string]string) (*Client, error) {
	trimmed := strings.TrimSpace(endpoint)
	u, err := url.Parse(trimmed)
	if err != nil || u.Opaque != "" {
		return nil, ErrInvalidEndpoint
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return nil, ErrInvalidEndpoint
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(trimmed, "#") {
		return nil, ErrInvalidEndpoint
	}
	escaped := strings.TrimRight(u.EscapedPath(), "/") + "/search"
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, ErrInvalidEndpoint
	}
	u.Path, u.RawPath = path, escaped
	copied := make(map[string]string, len(headers))
	for name, value := range headers {
		lowerName := strings.ToLower(name)
		if !httpToken.MatchString(name) || !printableASCII(value) || strings.TrimSpace(value) == "" ||
			reservedSessionHeaders[lowerName] || forbiddenSearchHeader(lowerName) {
			return nil, ErrInvalidHeader
		}
		canonical := http.CanonicalHeaderKey(name)
		if _, exists := copied[canonical]; exists {
			return nil, ErrInvalidHeader
		}
		copied[canonical] = value
	}
	var transport *http.Transport
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.DisableCompression = true
	transport.MaxResponseHeaderBytes = 64 << 10
	return &Client{
		searchURL: *u,
		headers:   copied,
		http: &http.Client{
			Timeout:   DefaultTimeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// The client owns hop headers, HTTP authority, and body framing.
func forbiddenSearchHeader(name string) bool {
	switch name {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade",
		"host", "content-length":
		return true
	}
	return false
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 32 || s[i] > 126 {
			return false
		}
	}
	return true
}

// CloseIdleConnections releases pooled connections owned by this client.
func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

// Search performs exactly one GET {endpoint}/search?q=...&format=json attempt:
// no retries, no redirect following, no result-URL fetching. It honors
// context cancellation and enforces the client's own timeout over it.
func (c *Client) Search(ctx context.Context, req Request) ([]Result, error) {
	query := req.Query
	if strings.TrimSpace(query) == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > MaxQueryRunes {
		return nil, ErrInvalidQuery
	}
	count := req.Count
	if count == 0 {
		count = DefaultCount
	}
	if count < 1 || count > MaxCount {
		return nil, ErrInvalidCount
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return nil, ErrRequest
	}
	base := c.searchURL
	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	base.RawQuery = params.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, ErrRequest
	}
	for name, value := range c.headers {
		httpReq.Header.Set(name, value)
	}
	// Framing is enforced last so statics cannot override the wire contract.
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Accept-Encoding", "gzip")
	httpReq.Header.Set("User-Agent", version)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, ErrRequest
	}
	defer resp.Body.Close()
	if isRedirect(resp.StatusCode) {
		return nil, ErrRedirect
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, ErrStatus
	}
	raw, err := readBounded(resp)
	if err != nil {
		return nil, err
	}
	return parseResults(ctx, raw, count)
}

func isRedirect(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

// readBounded reads the body capped before and after decompression. Only
// identity (or absent) and gzip are admitted.
func readBounded(resp *http.Response) ([]byte, error) {
	enc := strings.ToLower(strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ",")))
	switch enc {
	case "", "identity":
	case "gzip":
	default:
		return nil, ErrEncoding
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, ErrBody
	}
	if len(raw) > MaxBodyBytes {
		return nil, ErrTooLarge
	}
	if enc != "gzip" {
		return raw, nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrBody
	}
	defer gr.Close()
	decoded, err := io.ReadAll(io.LimitReader(gr, MaxBodyBytes+1))
	if err != nil {
		return nil, ErrBody
	}
	if len(decoded) > MaxBodyBytes {
		return nil, ErrTooLarge
	}
	return decoded, nil
}

// rawEntry consumes only the protocol actually used downstream: the main
// result url/title/content strings. Every other field (engines, score,
// answers, infoboxes, suggestions) is ignored.
type rawEntry struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// parseResults enforces the top-level shape strictly (object with a results
// array, no trailing data) while skipping entry-level garbage: entries that
// are not objects, carry non-string url/title/content, or fail safe-URL
// validation are dropped. Results are deduped by normalized identity,
// capped at count, in first-seen order.
func parseResults(ctx context.Context, raw []byte, count int) ([]Result, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var doc struct {
		Results json.RawMessage `json:"results"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, ErrJSON
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, ErrJSON
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(doc.Results, &entries); err != nil {
		return nil, ErrJSON
	}
	if entries == nil {
		return nil, ErrJSON
	}
	out := []Result{}
	seen := map[string]bool{}
	for _, item := range entries {
		if ctx.Err() != nil {
			return nil, ErrRequest
		}
		var entry rawEntry
		if err := json.Unmarshal(item, &entry); err != nil {
			continue
		}
		trimmed := strings.TrimSpace(entry.URL)
		u, err := url.Parse(trimmed)
		if err != nil {
			continue
		}
		if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
			continue
		}
		if u.Hostname() == "" || u.User != nil {
			continue
		}
		key := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath()
		if u.RawQuery != "" {
			key += "?" + u.RawQuery
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Result{
			ID:      key,
			Title:   truncateRunes(strings.TrimSpace(entry.Title), MaxTitleRunes),
			URL:     trimmed,
			Snippet: truncateRunes(strings.TrimSpace(entry.Content), MaxSnippetRunes),
		})
		if len(out) == count {
			break
		}
	}
	return out, nil
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}
