// Catalog fetch policy: schema, bounds, redirects, encodings and the
// trust boundary (catalog headers only, never provider auth).
package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arhammahajan/model-router/internal/catalog"
	"github.com/arhammahajan/model-router/internal/config"
)

func testProviders() map[string]*config.Provider {
	return map[string]*config.Provider{
		"up": {Name: "up", Dialect: config.DialectAnthropic, BaseURL: "http://127.0.0.1:9", APIKey: "provider-secret",
			Headers: map[string]string{"Authorization": "Bearer provider-secret"}},
		"chat": {Name: "chat", Dialect: config.DialectOpenAIChat, BaseURL: "http://127.0.0.1:9"},
	}
}

func testCatalog(url string) *config.Catalog {
	return &config.Catalog{
		URL:            url,
		Headers:        map[string]string{},
		Timeout:        5,
		RefreshSeconds: 60,
		ModelProviders: map[string]string{"exact-model": "up"},
	}
}

func TestFetchMappingSchema(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	re, err := catalog.Compile("^acme/.*$")
	if err != nil {
		t.Fatal(err)
	}
	cat := testCatalog(srv.URL)
	cat.Rules = []config.CatalogRule{{Match: "^acme/.*$", Provider: "chat", Pattern: re}}
	body = `{"data": [` +
		`{"id": "exact-model", "metadata": {"context_window": 200000}},` +
		`{"id": "acme/fast", "metadata": {"a": 1}},` +
		`{"id": "ghost-model"},` +
		`{"id": "  "},` +
		`{"id": 7},` +
		`"bare-string",` +
		`{"noid": true}` +
		`]}`

	entries, unmapped, err := FetchMapped(context.Background(), fetchClient(5*time.Second), cat, testProviders())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(entries) != 2 || entries[0].Name != "exact-model" || entries[1].Name != "acme/fast" {
		t.Fatalf("entries: %+v", entries)
	}
	if entries[0].Provider != "up" || entries[0].Upstream != "exact-model" {
		t.Fatalf("exact mapping wrong: %+v", entries[0])
	}
	if entries[1].Provider != "chat" {
		t.Fatalf("rule mapping wrong: %+v", entries[1])
	}
	if !strings.Contains(string(entries[0].Metadata), "200000") {
		t.Fatalf("metadata lost: %s", entries[0].Metadata)
	}
	if len(unmapped) != 2 || unmapped[0] != "bare-string" || unmapped[1] != "ghost-model" {
		t.Fatalf("unmapped: %v", unmapped)
	}
}

func TestFetchBareListAndDuplicates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`["dup", {"id": "dup", "metadata": {}}, "exact-model"]`))
	}))
	defer srv.Close()
	re, err := catalog.Compile("^acme/.*$")
	if err != nil {
		t.Fatal(err)
	}
	cat := testCatalog(srv.URL)
	cat.Rules = []config.CatalogRule{{Match: "^acme/.*$", Provider: "chat", Pattern: re}}
	entries, unmapped, err := FetchMapped(context.Background(), fetchClient(5*time.Second), cat, testProviders())
	if err != nil {
		t.Fatalf("bare fetch: %v", err)
	}
	// "dup" matches nothing (exact miss, rule miss): unmapped; the
	// duplicate object row collapses to the same unmapped ID.
	// "exact-model" maps exactly.
	if len(entries) != 1 || entries[0].Name != "exact-model" {
		t.Fatalf("entries: %+v", entries)
	}
	if len(unmapped) != 1 || unmapped[0] != "dup" {
		t.Fatalf("unmapped: %v", unmapped)
	}
}

func TestFetchFailuresAreValueFree(t *testing.T) {
	secret := "secret-canary-4d2a"
	payloads := map[string]string{
		"non-list mapping": `{"data": {"id": "` + secret + `"}}`,
		"non-list bare":    `"` + secret + `"`,
		"invalid json":     `{oops`,
		"oversize raw":     strings.Repeat("x", maxCatalogBodyBytes+1),
	}
	for name, payload := range payloads {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(payload))
		}))
		cat := testCatalog(srv.URL)
		_, _, err := FetchMapped(context.Background(), fetchClient(5*time.Second), cat, testProviders())
		srv.Close()
		if err == nil {
			t.Fatalf("%s: must fail", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: error echoes payload: %v", name, err)
		}
	}
	statusCases := map[string]func(w http.ResponseWriter, r *http.Request){
		"non-2xx": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"redirect 3xx": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(302)
		},
		"bad encoding": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write([]byte("xxx"))
		},
	}
	for name, handler := range statusCases {
		srv := httptest.NewServer(http.HandlerFunc(handler))
		cat := testCatalog(srv.URL)
		_, _, err := FetchMapped(context.Background(), fetchClient(5*time.Second), cat, testProviders())
		srv.Close()
		if err == nil {
			t.Fatalf("%s: must fail", name)
		}
		if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "elsewhere") {
			t.Fatalf("%s: error echoes transport detail: %v", name, err)
		}
	}
}

func TestFetchOversizeGzipDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		var buf bytes.Buffer
		g := gzip.NewWriter(&buf)
		_, _ = g.Write([]byte(`{"data": [`))
		for i := 0; i < 250000; i++ {
			_, _ = g.Write([]byte(`"aaaaaaaaaaaaaaaaaaaa",`))
		}
		_, _ = g.Write([]byte(`"end"]}`))
		_ = g.Close()
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	cat := testCatalog(srv.URL)
	_, _, err := FetchMapped(context.Background(), fetchClient(10*time.Second), cat, testProviders())
	if err != ErrCatalogTooLarge {
		t.Fatalf("oversize decoded body must fail with TooLarge, got %v", err)
	}
}

func TestFetchGzipAndIdentity(t *testing.T) {
	payload := `[{"id": "exact-model"}]`
	gzipped := gzipOf(t, payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gz" {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gzipped)
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	for _, path := range []string{"", "/gz"} {
		cat := testCatalog(srv.URL + path)
		entries, _, err := FetchMapped(context.Background(), fetchClient(5*time.Second), cat, testProviders())
		if err != nil || len(entries) != 1 || entries[0].Name != "exact-model" {
			t.Fatalf("%q: %+v %v", path, entries, err)
		}
	}
}

func TestFetchTrustBoundary(t *testing.T) {
	var gotUA, gotCustom, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotCustom, gotAuth = r.Header.Get("User-Agent"), r.Header.Get("X-Catalog-Token"), r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cat := testCatalog(srv.URL)
	cat.Headers = map[string]string{"X-Catalog-Token": "trusted"}
	entries, _, err := FetchMapped(context.Background(), fetchClient(5*time.Second), cat, testProviders())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty valid catalog must replace with zero entries: %+v", entries)
	}
	if !strings.HasPrefix(gotUA, "model-router-go/") {
		t.Fatalf("user-agent wrong: %q", gotUA)
	}
	if gotCustom != "trusted" {
		t.Fatalf("trusted catalog header missing: %q", gotCustom)
	}
	if gotAuth != "" {
		t.Fatalf("provider auth must never ride the catalog GET: %q", gotAuth)
	}
}

func TestFetchTimeoutAndCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cat := testCatalog(srv.URL)
	_, _, err := FetchMapped(context.Background(), fetchClient(50*time.Millisecond), cat, testProviders())
	if err == nil {
		t.Fatal("slow catalog must time out")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = FetchMapped(ctx, fetchClient(5*time.Second), testCatalog(srv.URL), testProviders())
	if err == nil {
		t.Fatal("canceled context must fail")
	}
}

func TestStripModelIDMatchesPython(t *testing.T) {
	// Python str.strip set from the portability doc: tab/CR/LF/VT/FF,
	// space, U+001C-1F separators, NEL, NBSP, Ogham, U+2000-200A,
	// U+2028/2029, U+202F, U+205F and U+3000 strip; NUL and DEL stay.
	pad := " \t\n\x0b\x0c\r" +
		"\u001c\u001d\u001e\u001f" +
		"\u0085\u00a0\u1680" +
		"\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a" +
		"\u2028\u2029\u202f\u205f\u3000"
	if got := catalog.StripModelID(pad + "abc" + pad); got != "abc" {
		t.Fatalf("strip set incomplete: %q", got)
	}
	if got := catalog.StripModelID("abc"); got != "abc" {
		t.Fatalf("plain ID must survive: %q", got)
	}
	if got := catalog.StripModelID("\x00abc\x7f"); got != "\x00abc\x7f" {
		t.Fatalf("NUL/DEL must be kept: %q", got)
	}
	// Plain TrimSpace would keep U+001C; exactness is the requirement.
	if got := catalog.StripModelID("\u001cmodel\u001f"); got != "model" {
		t.Fatalf("U+001C-1F must strip: %q", got)
	}
	// NBSP-only padding strips too.
	if got := catalog.StripModelID("\u00a0\u00a0"); got != "" {
		t.Fatalf("NBSP padding must strip: %q", got)
	}
}

func gzipOf(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
