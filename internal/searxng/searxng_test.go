package searxng

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustClient(t *testing.T, endpoint string, headers map[string]string) *Client {
	t.Helper()
	c, err := NewClient(endpoint, headers)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func jsonResults(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// Fake asserts the wire contract: GET on /search with q verbatim (no
// site: approximation), format=json, and the framing plus operator headers.
func TestMethodParamsHeaders(t *testing.T) {
	var method, path, q, format, accept, encoding, agent, custom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		q, format = r.URL.Query().Get("q"), r.URL.Query().Get("format")
		accept, encoding, agent = r.Header.Get("Accept"), r.Header.Get("Accept-Encoding"), r.Header.Get("User-Agent")
		custom = r.Header.Get("X-Search-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"query":"x","number_of_results":1,"results":[{"url":"https://example.com/a","title":"A","content":"snip"}]}`))
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL, map[string]string{"X-Search-Token": "opaque"})
	res, err := c.Search(context.Background(), Request{Query: " site:example.com SearXNG ", Count: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if method != http.MethodGet || path != "/search" {
		t.Fatalf("method/path = %s %s, want GET /search", method, path)
	}
	if q != " site:example.com SearXNG " {
		t.Fatalf("q = %q, want verbatim passthrough", q)
	}
	if format != "json" {
		t.Fatalf("format = %q, want json", format)
	}
	if accept != "application/json" || encoding != "gzip" || agent != version {
		t.Fatalf("framing = %q %q %q", accept, encoding, agent)
	}
	if custom != "opaque" {
		t.Fatalf("operator header = %q, want opaque", custom)
	}
	if len(res) != 1 || res[0].ID != "https://example.com/a" || res[0].Title != "A" || res[0].URL != "https://example.com/a" || res[0].Snippet != "snip" {
		t.Fatalf("result = %+v", res)
	}
}

func TestSubPathEndpoint(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL+"/searxng/", nil)
	if _, err := c.Search(context.Background(), Request{Query: "q", Count: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if path != "/searxng/search" {
		t.Fatalf("path = %s, want /searxng/search", path)
	}
}

// Auth isolation: the operator header channel touches only the search
// request, and result URLs are never fetched (no second request anywhere).
func TestAuthIsolation(t *testing.T) {
	var auth string
	var searchHits int64
	var resultHits int64
	results := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&resultHits, 1)
	}))
	defer results.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&searchHits, 1)
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"results":[{"url":"` + results.URL + `/p","title":"T","content":"C"}]}`))
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL, map[string]string{"Authorization": "Bearer operator-secret"})
	if _, err := c.Search(context.Background(), Request{Query: "q", Count: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if auth != "Bearer operator-secret" {
		t.Fatalf("auth = %q", auth)
	}
	if got := atomic.LoadInt64(&searchHits); got != 1 {
		t.Fatalf("search hits = %d, want exactly 1 (no retries)", got)
	}
	if got := atomic.LoadInt64(&resultHits); got != 0 {
		t.Fatalf("result hits = %d, want 0 (never fetch result URLs)", got)
	}
}

func TestConstructorValidation(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	cases := []struct {
		name     string
		endpoint string
		headers  map[string]string
		want     error
	}{
		{"empty", "", nil, ErrInvalidEndpoint},
		{"non-http", "ftp://example.com", nil, ErrInvalidEndpoint},
		{"no-host", "https://", nil, ErrInvalidEndpoint},
		{"userinfo", "https://user:pass@example.com", nil, ErrInvalidEndpoint},
		{"query", ok.URL + "?key=secret", nil, ErrInvalidEndpoint},
		{"fragment", ok.URL + "#frag", nil, ErrInvalidEndpoint},
		{"empty-fragment", ok.URL + "#", nil, ErrInvalidEndpoint},
		{"port-without-host", "http://:80", nil, ErrInvalidEndpoint},
		{"opaque", "mailto:box@example.com", nil, ErrInvalidEndpoint},
		{"bad-header-name", ok.URL, map[string]string{"not a header": "v"}, ErrInvalidHeader},
		{"bad-header-value", ok.URL, map[string]string{"X-T": "v\x01"}, ErrInvalidHeader},
		{"empty-header-value", ok.URL, map[string]string{"X-T": "  "}, ErrInvalidHeader},
		{"session-header", ok.URL, map[string]string{"Session-Id": "s"}, ErrInvalidHeader},
		{"duplicate-header", ok.URL, map[string]string{"X-T": "a", "x-t": "b"}, ErrInvalidHeader},
		{"hop-header", ok.URL, map[string]string{"Connection": "X-Search-Token"}, ErrInvalidHeader},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.endpoint, tc.headers); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := NewClient(ok.URL, nil); err != nil {
		t.Fatalf("valid endpoint: %v", err)
	}
}

func TestRequestValidation(t *testing.T) {
	srv := jsonResults(t, `{"results":[]}`)
	defer srv.Close()
	c := mustClient(t, srv.URL, nil)
	cases := []struct {
		name string
		req  Request
		want error
	}{
		{"empty-query", Request{Query: "  ", Count: 1}, ErrInvalidQuery},
		{"invalid-utf8-query", Request{Query: "q\xff", Count: 1}, ErrInvalidQuery},
		{"overlong-query", Request{Query: strings.Repeat("q", MaxQueryRunes+1), Count: 1}, ErrInvalidQuery},
		{"negative-count", Request{Query: "q", Count: -1}, ErrInvalidCount},
		{"over-count", Request{Query: "q", Count: MaxCount + 1}, ErrInvalidCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Search(context.Background(), tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// Blocking cancellation: a hung upstream plus caller cancel must fail
// promptly with the static error, not the hung body or the query.
func TestBlockingCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	c := mustClient(t, srv.URL, nil)
	start := time.Now()
	_, err := c.Search(ctx, Request{Query: "q", Count: 1})
	if !errors.Is(err, ErrRequest) {
		t.Fatalf("err = %v, want %v", err, ErrRequest)
	}
	if time.Since(start) > DefaultTimeout {
		t.Fatalf("cancellation took %v", time.Since(start))
	}
}

func TestRedirectRefused(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	c := mustClient(t, srv.URL, nil)
	if _, err := c.Search(context.Background(), Request{Query: "q", Count: 1}); !errors.Is(err, ErrRedirect) {
		t.Fatalf("err = %v, want %v", err, ErrRedirect)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1 (never follow)", got)
	}
}

func TestOversizedBodies(t *testing.T) {
	big := `{"results":[{"url":"https://example.com/a","title":"` + strings.Repeat("T", MaxBodyBytes) + `","content":"x"}]}`
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer plain.Close()
	if _, err := mustClient(t, plain.URL, nil).Search(context.Background(), Request{Query: "q", Count: 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("plain err = %v, want %v", err, ErrTooLarge)
	}

	var gzipped bytes.Buffer
	gz := gzip.NewWriter(&gzipped)
	if _, err := gz.Write(bytes.Repeat([]byte("0123456789abcdef"), MaxBodyBytes/16+1024)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	bomb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gzipped.Bytes())
	}))
	defer bomb.Close()
	if _, err := mustClient(t, bomb.URL, nil).Search(context.Background(), Request{Query: "q", Count: 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("gzip err = %v, want %v", err, ErrTooLarge)
	}
}

func TestUnsupportedEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	if _, err := mustClient(t, srv.URL, nil).Search(context.Background(), Request{Query: "q", Count: 1}); !errors.Is(err, ErrEncoding) {
		t.Fatalf("err = %v, want %v", err, ErrEncoding)
	}
}

func TestNon2xx(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		srv := jsonResults(t, `{"results":[]}`)
		srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		_, err := mustClient(t, srv.URL, nil).Search(context.Background(), Request{Query: "q", Count: 1})
		srv.Close()
		if !errors.Is(err, ErrStatus) {
			t.Fatalf("status %d err = %v, want %v", status, err, ErrStatus)
		}
	}
}

// Malformed top-level shapes fail; entry-level garbage is skipped while
// valid entries survive.
func TestMalformedResultShapes(t *testing.T) {
	bad := []string{
		`not json`,
		`{"query":"q"}`,
		`{"results":null}`,
		`{"results":"nope"}`,
		`{"results":{}}`,
		`{"results":[]} trailing`,
		`[1,2,3]`,
	}
	for _, body := range bad {
		srv := jsonResults(t, body)
		_, err := mustClient(t, srv.URL, nil).Search(context.Background(), Request{Query: "q", Count: 1})
		srv.Close()
		if !errors.Is(err, ErrJSON) {
			t.Fatalf("body %q err = %v, want %v", body, err, ErrJSON)
		}
	}

	srv := jsonResults(t, `{"results":[
		{"url":42,"title":"T","content":"C"},
		"just-a-string",
		null,
		{"url":"https://example.com/kept","title":"K","content":"C"}
	]}`)
	defer srv.Close()
	res, err := mustClient(t, srv.URL, nil).Search(context.Background(), Request{Query: "q", Count: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].URL != "https://example.com/kept" {
		t.Fatalf("res = %+v, want only the valid entry", res)
	}
}

// Unsafe result URLs are skipped: only bare http(s) without userinfo survive.
func TestUnsafeResultURLsSkipped(t *testing.T) {
	srv := jsonResults(t, `{"results":[
		{"url":"javascript:alert(1)","title":"x","content":"x"},
		{"url":"data:text/plain,hi","title":"x","content":"x"},
		{"url":"ftp://example.com/f","title":"x","content":"x"},
		{"url":"/relative/path","title":"x","content":"x"},
		{"url":"https://user:pass@example.com/","title":"x","content":"x"},
		{"url":"  https://example.com/ok  ","title":"  T  ","content":"  S  "}
	]}`)
	defer srv.Close()
	res, err := mustClient(t, srv.URL, nil).Search(context.Background(), Request{Query: "q", Count: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("res = %+v, want exactly the safe entry", res)
	}
	if res[0].ID != "https://example.com/ok" || res[0].Title != "T" || res[0].Snippet != "S" {
		t.Fatalf("res = %+v, want trimmed normalization", res)
	}
}

// Count caps, duplicates collapse to first-seen order, IDs are stable.
func TestResultCountAndStableIDs(t *testing.T) {
	body := `{"query":"q","number_of_results":5,"results":[
		{"url":"HTTPS://EXAMPLE.COM/a#section","title":"A","content":"sa"},
		{"url":"https://example.com/b","title":"B","content":"sb"},
		{"url":"https://example.com/a","title":"A2","content":"sa2"},
		{"url":"HTTPS://EXAMPLE.COM/b","title":"B2","content":"sb2"},
		{"url":"https://example.com/c","title":"C","content":"sc"}
	]}`
	srv := jsonResults(t, body)
	defer srv.Close()
	c := mustClient(t, srv.URL, nil)

	res, err := c.Search(context.Background(), Request{Query: "q", Count: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 || res[0].ID != "https://example.com/a" || res[1].ID != "https://example.com/b" {
		t.Fatalf("res = %+v, want first-seen dedup capped at 2", res)
	}
	if res[0].Title != "A" || !strings.HasSuffix(res[0].URL, "#section") {
		t.Fatalf("duplicate kept %+v, want first-seen values", res[0])
	}

	full, err := c.Search(context.Background(), Request{Query: "q", Count: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(full) != 3 {
		t.Fatalf("full = %+v, want 3 deduped results", full)
	}
	again, err := c.Search(context.Background(), Request{Query: "q", Count: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i := range full {
		if full[i].ID != again[i].ID {
			t.Fatalf("IDs unstable: %+v vs %+v", full, again)
		}
	}

	def, err := c.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(def) != 3 {
		t.Fatalf("default count res = %+v, want all 3 (DefaultCount=%d)", def, DefaultCount)
	}
}

func TestGzipSuccess(t *testing.T) {
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	if _, err := gz.Write([]byte(`{"results":[{"url":"https://example.com/a","title":"A","content":"C"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(raw.Bytes())
	}))
	defer srv.Close()
	res, err := mustClient(t, srv.URL, nil).Search(context.Background(), Request{Query: "q", Count: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].ID != "https://example.com/a" {
		t.Fatalf("res = %+v", res)
	}
}

func TestEscapedEndpointPathRemainsEscaped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/proxy%2Fsearch-zone/search" {
			t.Errorf("escaped endpoint path changed on the wire: %s", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	c := mustClient(t, srv.URL+"/proxy%2Fsearch-zone/", nil)
	if _, err := c.Search(context.Background(), Request{Query: "q"}); err != nil {
		t.Fatal(err)
	}
}
