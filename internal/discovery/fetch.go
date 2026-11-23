// Catalog fetch: GET the exact configured URL and map rows to trusted
// routes. The payload never supplies provider dialects, endpoints,
// keys or capabilities: provider resolution is exact model_providers
// first, then first-rule search, and the provider's trusted config
// supplies the dialect.
package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/arhammahajan/router/internal/catalog"
	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// maxCatalogBodyBytes caps one catalog body before AND after
// decompression. Intentional hardening (root review): Python imposes no
// catalog cap; an unbounded catalog could OOM the router.
const maxCatalogBodyBytes = 4 * 1024 * 1024

// routerVersion feeds the catalog User-Agent. Keep in sync with
// cmd/routerd's version constant.
const routerVersion = "0.1.0"

// Static fetch failures. All are value-free sentinels: ordinary logs
// carry static reasons and counts only, never payloads, IDs, URLs,
// headers, bodies or raw errors.
var (
	ErrCatalogHTTP     = errors.New("catalog: fetch failed")
	ErrCatalogStatus   = errors.New("catalog: non-2xx response")
	ErrCatalogEncoding = errors.New("catalog: unsupported content encoding; supported: identity, gzip")
	ErrCatalogTooLarge = errors.New("catalog: body exceeds the size bound")
	ErrCatalogBody     = errors.New("catalog: body could not be read")
	ErrCatalogJSON     = errors.New("catalog: response has no model list")
)

// fetchClient builds the catalog HTTP client: whole-attempt timeout,
// redirects disabled (any 3xx is an attempt failure, matching the
// generation transport), and no automatic decompression so both the
// compressed and decoded sizes are bounded explicitly.
func fetchClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// FetchMapped performs one catalog attempt: GET, decode, map to trusted
// routes. Entries come back in fetch order with duplicates last-wins at
// first position; unmapped IDs come back sorted. The trusted providers
// map is read-only and never mutated.
func FetchMapped(ctx context.Context, client *http.Client, cat *config.Catalog, providers map[string]*config.Provider) ([]router.CatalogEntry, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cat.Timeout*float64(time.Second)))
	defer cancel()
	if ctx.Err() != nil {
		return nil, nil, ErrCatalogHTTP
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cat.URL, nil)
	if err != nil {
		return nil, nil, ErrCatalogHTTP
	}
	// Trusted catalog headers touch ONLY this GET: they are never merged
	// into generation requests, and provider AuthHeaders are untouched
	// (client creds never become upstream creds).
	req.Header.Set("User-Agent", "routerd/"+routerVersion)
	for k, v := range cat.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, ErrCatalogHTTP
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, ErrCatalogStatus
	}
	raw, err := readBoundedBody(resp)
	if err != nil {
		return nil, nil, err
	}
	rows, err := decodeRows(raw)
	if err != nil {
		return nil, nil, err
	}
	return mapRows(ctx, rows, cat, providers)
}

// readBoundedBody reads the body capped before and after decompression.
// Only identity and gzip are admitted; anything else is an attempt
// failure with no intra-attempt retry or fallback.
func readBoundedBody(resp *http.Response) ([]byte, error) {
	enc := strings.TrimSpace(strings.ToLower(strings.Join(resp.Header.Values("Content-Encoding"), ",")))
	switch enc {
	case "", "identity":
	case "gzip":
	default:
		return nil, ErrCatalogEncoding
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBodyBytes+1))
	if err != nil {
		return nil, ErrCatalogBody
	}
	if len(raw) > maxCatalogBodyBytes {
		return nil, ErrCatalogTooLarge
	}
	if enc != "gzip" {
		return raw, nil
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrCatalogBody
	}
	defer gr.Close()
	decoded, err := io.ReadAll(io.LimitReader(gr, maxCatalogBodyBytes+1))
	if err != nil {
		return nil, ErrCatalogBody
	}
	if len(decoded) > maxCatalogBodyBytes {
		return nil, ErrCatalogTooLarge
	}
	return decoded, nil
}

// decodeRows enforces the JSON shape: a mapping yields payload["data"],
// a bare value is the rows themselves, and the result must be a list.
func decodeRows(raw []byte) ([]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // preserve raw numeric literals in metadata
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return nil, ErrCatalogJSON
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, ErrCatalogJSON
	}
	if m, ok := payload.(map[string]any); ok {
		payload = m["data"]
	}
	rows, ok := payload.([]any)
	if !ok {
		return nil, ErrCatalogJSON
	}
	return rows, nil
}

// mapRows strips IDs exactly (never TrimSpace), skips blank/non-string
// rows, keeps duplicate first positions with last values, resolves the
// provider (exact first, first-rule search next) and copies mapping
// metadata objects verbatim ({} when the row carries none).
func mapRows(ctx context.Context, rows []any, cat *config.Catalog, providers map[string]*config.Provider) ([]router.CatalogEntry, []string, error) {
	type slot struct {
		entry router.CatalogEntry
		order int
	}
	slots := map[string]*slot{}
	order := []string{}
	unmappedSet := map[string]struct{}{}
	for _, row := range rows {
		if ctx.Err() != nil {
			return nil, nil, ErrCatalogHTTP
		}
		var id any
		var meta any
		if m, ok := row.(map[string]any); ok {
			id, meta = m["id"], m["metadata"]
		} else {
			id = row
		}
		name, ok := id.(string)
		if !ok {
			continue
		}
		name = catalog.StripModelID(name)
		if name == "" {
			continue
		}
		provider := ""
		if p, ok := cat.ModelProviders[name]; ok {
			provider = p
		} else {
			for _, rule := range cat.Rules {
				if ctx.Err() != nil {
					return nil, nil, ErrCatalogHTTP
				}
				if rule.Pattern != nil && rule.Pattern.MatchString(name) {
					provider = rule.Provider
					break
				}
			}
		}
		if provider == "" {
			unmappedSet[name] = struct{}{}
			continue
		}
		if _, known := providers[provider]; !known {
			continue // validated at startup; fail closed, never publish
		}
		delete(unmappedSet, name)
		entry := router.CatalogEntry{Name: name, Provider: provider, Upstream: name}
		if m, ok := meta.(map[string]any); ok {
			if raw, err := json.Marshal(m); err == nil {
				entry.Metadata = raw
			}
		}
		if prev, dup := slots[name]; dup {
			prev.entry = entry // last value wins, first position kept
			continue
		}
		slots[name] = &slot{entry: entry, order: len(order)}
		order = append(order, name)
	}
	entries := make([]router.CatalogEntry, 0, len(order))
	for _, name := range order {
		entries = append(entries, slots[name].entry)
	}
	unmapped := make([]string, 0, len(unmappedSet))
	for id := range unmappedSet {
		unmapped = append(unmapped, id)
	}
	sort.Strings(unmapped)
	if ctx.Err() != nil {
		return nil, nil, ErrCatalogHTTP
	}
	return entries, unmapped, nil
}
