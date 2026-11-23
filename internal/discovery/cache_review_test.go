// Review probes for the route-only discovery cache contract:
// exact closed disk shapes, identity/timestamps/UTF-8/duplicates, full
// prefixed aliases vs raw upstream IDs (local rename), valid-written-file
// restore, two-pass canonical-before-alias, hidden per-server rows, fresh
// success timestamp rules, Safe-ID admission corpora, fingerprint
// routing-vs-interval separation, digest-only persistence and the
// filesystem reader boundary. New file only; no production edits.
package discovery

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/router"
)

// --- closed shapes: schema_version 2 is the only admitted version ---

func TestReviewCacheClosedShapes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	cfg := cacheTestConfig(t, dir)
	now := float64(wallNow())

	// Float 2.0 is not the integer 2: Python requires type int. Note:
	// json.Marshal(2.0) emits "2", so hand-write the document bytes.
	os.WriteFile(cfg.DiscoveryCache.Path,
		[]byte(`{"schema_version":2.0,"identity":"i","catalog":null,"llama":{}}`), 0o600)
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheIncompatible {
		t.Fatalf("float 2.0 must be incompatible, got %+v", st)
	}
	// String "2" is not the integer 2.
	writeFile(t, cfg.DiscoveryCache.Path, map[string]any{
		"schema_version": "2", "identity": "i", "catalog": nil, "llama": map[string]any{},
	})
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheIncompatible {
		t.Fatalf("string version must be incompatible, got %+v", st)
	}
	// Missing identity member breaks the closed top shape.
	os.WriteFile(cfg.DiscoveryCache.Path,
		[]byte(`{"schema_version":2,"catalog":null,"llama":{}}`), 0o600)
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheIncompatible {
		t.Fatalf("missing identity must be incompatible, got %+v", st)
	}
	// Null identity is present-but-empty: invalid, not incompatible.
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("", nil, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("empty identity must be invalid, got %+v", st)
	}
	// Unknown per-unit keys reject.
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{}, "stale_after_seconds": 1}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("unknown unit key must be invalid, got %+v", st)
	}
	// Unknown llama index shapes reject: empty key, leading zeros, negative, huge.
	for _, key := range []string{"", "01", "-1", "1e3", "99999999999"} {
		writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i", nil,
			map[string]any{key: map[string]any{"succeeded_at": now, "models": []any{}}}))
		if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
			t.Fatalf("index %q must be invalid, got %+v", key, st)
		}
	}
	// Missing llama section breaks the closed top shape.
	os.WriteFile(cfg.DiscoveryCache.Path,
		[]byte(`{"schema_version":2,"identity":"i","catalog":null}`), 0o600)
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheIncompatible {
		t.Fatalf("missing llama section must be incompatible, got %+v", st)
	}
}

// --- timestamps, UTF-8, duplicates, alias prefix rules ---

func TestReviewCacheTimeAndIDShapes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	cfg := cacheTestConfig(t, dir)
	now := float64(wallNow())

	// Negative times reject (NaN/Inf cannot appear in strict JSON, so
	// negative stands in for the invalid-time class).
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": -1.0, "models": []any{}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("negative time must be invalid, got %+v", st)
	}
	// succeeded_at as a string rejects.
	os.WriteFile(cfg.DiscoveryCache.Path,
		[]byte(`{"schema_version":2,"identity":"i","catalog":{"succeeded_at":"now","models":[]},"llama":{}}`), 0o600)
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("string time must be invalid, got %+v", st)
	}
	// Empty ID, over-long ID reject.
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "", "aliases": []any{}}}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("empty id must be invalid, got %+v", st)
	}
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": strings.Repeat("m", 2049), "aliases": []any{}}}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("over-long id must be invalid, got %+v", st)
	}
	// Invalid UTF-8 bytes on disk reject (hand-built: json.Marshal would
	// replace them with U+FFFD and hide the case).
	rawNum, _ := json.Marshal(now)
	raw := `{"schema_version":2,"identity":"i","catalog":{"succeeded_at":` +
		string(rawNum) + `,"models":[{"id":"m` + "\xff" + `","aliases":[]}]},"llama":{}}`
	os.WriteFile(cfg.DiscoveryCache.Path, []byte(raw), 0o600)
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("invalid UTF-8 must be invalid, got %+v", st)
	}
	// Duplicate canonical IDs reject.
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{
			map[string]any{"id": "m", "aliases": []any{}},
			map[string]any{"id": "m", "aliases": []any{}},
		}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("duplicate canonical must be invalid, got %+v", st)
	}
	// Duplicate aliases and alias-vs-canonical collisions reject.
	dupAlias := map[string]any{"succeeded_at": now, "models": []any{
		map[string]any{"id": "a", "aliases": []any{"pre-x"}},
		map[string]any{"id": "b", "aliases": []any{"pre-x"}},
	}}
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i", nil, map[string]any{"0": dupAlias}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("duplicate alias must be invalid, got %+v", st)
	}
	fwd := map[string]any{"succeeded_at": now, "models": []any{
		map[string]any{"id": "a", "aliases": []any{"pre-b"}},
		map[string]any{"id": "b", "aliases": []any{}},
	}}
	// Alias collisions must be checked in full public-name space with
	// trusted prefix/local-rename policy, not against raw upstream IDs.
	fwdStore := NewCacheStore(cfg, router.NewRegistry(cfg))
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(fwdStore.identity, nil, map[string]any{"0": fwd}))
	if st := fwdStore.Restore(context.Background(), nil); st.Load != CacheInvalid {
		t.Fatalf("forward alias-vs-canonical collision must be invalid, got %+v", st)
	}
	// Catalog rows must carry no aliases.
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "m1", "aliases": []any{"pre-a"}}}}, map[string]any{}))
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("catalog alias must be invalid, got %+v", st)
	}
	// A llama row whose alias lacks the exposed prefix must fail restore
	// (file-level admission is prefix-agnostic; trusted-config resolve is
	// where the prefix rule applies).
	bare := map[string]any{"succeeded_at": now, "models": []any{
		map[string]any{"id": "m1", "aliases": []any{"bare-alias"}},
	}}
	reg := router.NewRegistry(cfg)
	store := NewCacheStore(cfg, reg)
	writeFile(t, cfg.DiscoveryCache.Path, cacheDoc(store.identity, nil,
		map[string]any{"0": bare}))
	if st := store.Restore(context.Background(), nil); st.Load != CacheInvalid {
		t.Fatalf("bare alias must fail restore, got %+v", st)
	}
}

// --- valid written files must restore, incl. local rename + hidden rows ---

func TestReviewCacheWrittenFileRestores(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	names := []string{}
	for _, s := range cfg.Llama.Servers {
		names = append(names, s.Name)
	}
	if len(names) < 2 {
		t.Fatal("need two llama servers")
	}
	now := float64(wallNow())
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Provider: "up", Upstream: "m1"}}, nil, now); err != nil {
		t.Fatal(err)
	}
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		names[0]: {State: router.LlamaReachable, Entries: []router.LlamaEntry{
			{Name: cfg.Llama.Prefix + "m1", Upstream: "m1", Aliases: []string{cfg.Llama.Prefix + "m1-alias"}},
			{Name: cfg.Llama.Prefix + "local-llama", Upstream: "local"},
		}},
		names[1]: {State: router.LlamaReachable, Entries: []router.LlamaEntry{
			{Name: cfg.Llama.Prefix + "hidden", Upstream: "m1", Aliases: []string{cfg.Llama.Prefix + "h2"}},
		}},
	}}
	if err := commitLlamaShim(reg, layer, now); err != nil {
		t.Fatal(err)
	}
	store := NewCacheStore(cfg, reg)
	payload, ok := store.encode(reg.ExportDiscovery())
	if !ok {
		t.Fatal("encode must succeed")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DiscoveryCache.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.DiscoveryCache.Path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["schema_version"] != float64(2) {
		t.Fatalf("must write v2, got %v", doc["schema_version"])
	}
	llama := doc["llama"].(map[string]any)
	if len(llama) != 2 {
		t.Fatalf("hidden per-server rows must stay per-index, got %d sections", len(llama))
	}
	// Raw upstream IDs on disk, never staged names: "local" stays "local".
	found := false
	for _, unit := range llama {
		for _, row := range unit.(map[string]any)["models"].([]any) {
			if row.(map[string]any)["id"] == "local" {
				found = true
			}
			for _, a := range row.(map[string]any)["aliases"].([]any) {
				if !strings.HasPrefix(a.(string), cfg.Llama.Prefix) {
					t.Fatalf("stored alias must carry full prefix: %v", a)
				}
			}
		}
	}
	if !found {
		t.Fatal("raw upstream id local must be stored, not the renamed staged name")
	}
	reg2 := router.NewRegistry(cfg)
	store2 := NewCacheStore(cfg, reg2)
	if st := store2.Restore(context.Background(), nil); st.Load != CacheLoaded {
		t.Fatalf("written file must restore, got %+v", st)
	}
	if m, _, fault := reg2.Current().Resolve(cfg.Llama.Prefix + "local-llama"); fault != router.Found || m.Upstream != "local" {
		t.Fatalf("local rename broken after restore: %+v %v", m, fault)
	}
	if m, _, fault := reg2.Current().Resolve(cfg.Llama.Prefix + "m1-alias"); fault != router.Found || m.Upstream != "m1" {
		t.Fatalf("alias broken after restore: %+v %v", m, fault)
	}
	inv := reg2.ExportDiscovery()
	if len(inv.Llama) != 2 {
		t.Fatalf("restore must keep hidden per-server rows separate: %+v", inv.Llama)
	}
}

// --- encode must never write a file the reader would reject ---

func TestReviewCacheEncodeSuppressesForwardCollisions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	cfg := cacheTestConfig(t, dir)
	reg := router.NewRegistry(cfg)
	var srv string
	for _, s := range cfg.Llama.Servers {
		srv = s.Name
		break
	}
	p := cfg.Llama.Prefix
	// Row 1 claims alias "pre-x"; row 2 claims canonical "pre-x".
	// Live publication two-passes this (canonical wins); the encoder must
	// suppress the losing alias instead of writing an alias-vs-canonical
	// collision the reader rejects.
	layer := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		srv: {State: router.LlamaReachable, Entries: []router.LlamaEntry{
			{Name: p + "c1", Upstream: "c1", Aliases: []string{p + "x"}},
			{Name: p + "x", Upstream: "x"},
		}},
	}}
	if err := commitLlamaShim(reg, layer, float64(wallNow())); err != nil {
		t.Fatal(err)
	}
	if m, _, fault := reg.Current().Resolve(p + "x"); fault != router.Found || m.Upstream != "x" {
		t.Fatalf("canonical must win over earlier alias: %+v %v", m, fault)
	}
	store := NewCacheStore(cfg, reg)
	payload, ok := store.encode(reg.ExportDiscovery())
	if !ok {
		t.Fatal("encode must succeed")
	}
	// The written file must self-validate: reload it strictly.
	if _, _, err := validateEncoded(payload, 86400, float64(wallNow()), store.sanitize, cfg); err != nil {
		t.Fatalf("encoder wrote a file the reader rejects: %v", err)
	}
	if err := os.WriteFile(cfg.DiscoveryCache.Path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	reg2 := router.NewRegistry(cfg)
	store2 := NewCacheStore(cfg, reg2)
	if st := store2.Restore(context.Background(), nil); st.Load != CacheLoaded {
		t.Fatalf("forward-collision save must restore, got %+v", st)
	}
	if m, _, fault := reg2.Current().Resolve(p + "x"); fault != router.Found || m.Upstream != "x" {
		t.Fatalf("restored canonical must still win: %+v %v", m, fault)
	}
	// Suppressed rows contribute no aliases: an unsafe row vanishes wholly.
	reg3 := router.NewRegistry(cfg)
	bad := router.LlamaLayer{Servers: map[string]router.LlamaServerSnapshot{
		srv: {State: router.LlamaReachable, Entries: []router.LlamaEntry{
			{Name: p + "secret-key-1", Upstream: "secret-key-1", Aliases: []string{p + "leak"}},
			{Name: p + "ok", Upstream: "ok", Aliases: []string{p + "leak2"}},
		}},
	}}
	_ = commitLlamaShim(reg3, bad, float64(wallNow()))
	store3 := NewCacheStore(cfg, reg3)
	payload3, ok := store3.encode(reg3.ExportDiscovery())
	if !ok {
		t.Fatal("encode with unsafe row must still succeed by omission")
	}
	if strings.Contains(string(payload3), "secret-key-1") {
		t.Fatal("unsafe row must be omitted, never rewritten or persisted")
	}
}

// --- Safe-ID admission: secret corpora, quoting, JSON, generic patterns ---
//
// URL userinfo/query values cannot be expressed in validated config (the
// config layer rejects non-bare URLs), so they are injected into the
// validated struct before the store is built: this exercises the corpus
// logic in addURLSecrets exactly as live-resolved values would.

func reviewSanitizerStore(t *testing.T, dir string, secret string) *CacheStore {
	t.Helper()
	path := filepath.Join(dir, "router.toml")
	body := `
[server]
[retry]
[providers.up]
dialect = "openai-chat"
base_url = "http://127.0.0.1:9"
api_key = "` + secret + `"
headers = { X-Custom = "hunter2-hdr", Authorization = "Bearer static-bogus" }
[providers.up.litellm_kwargs]
headers = { X-Extra = "translated-secret" }
[providers.other]
dialect = "openai-chat"
base_url = "http://127.0.0.1:10"
[[models]]
name = "a"
provider = "up"
[catalog]
url = "http://127.0.0.1:9/models"
headers = { X-Cat = "catalog-secret" }
model_providers = { "m1" = "up" }
[llama_cpp]
prefix = "pre-"
servers = [{ url = "http://127.0.0.1:9931", api_key = "llama-secret", headers = { X-Srv = "server-secret" } }]
[discovery_cache]
path = "` + filepath.Join(dir, "cache.json") + `"
` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Inject URL-carried secrets post-validation (config admits bare URLs
	// only; resolved runtime values with userinfo/query still reach the
	// corpus through the same struct fields).
	cfg.Providers["up"].BaseURL = "http://user1:pass1@127.0.0.1:9/v1?tok=qsecret1&other=x"
	cfg.Catalog.URL = "http://catuser:catpass@127.0.0.1:9/models?ckey=cqval"
	cfg.Llama.Servers[0].URL = "http://luser:lpass@127.0.0.1:9931"
	reg := router.NewRegistry(cfg)
	return NewCacheStore(cfg, reg)
}

func TestReviewCacheSafeIDKnownCorpora(t *testing.T) {
	dir := t.TempDir()
	store := reviewSanitizerStore(t, dir, "secret-key-1")
	// Every resolved secret echo must fail admission (save omits, load rejects).
	for _, secret := range []string{
		"secret-key-1", "hunter2-hdr", "translated-secret",
		"catalog-secret", "server-secret", "llama-secret",
		"qsecret1", "cqval", "pass1", "user1", "catpass", "catuser", "lpass", "luser",
		"static-bogus",
	} {
		if store.sanitize.safeID(secret) {
			t.Fatalf("secret echo %q must fail admission", secret)
		}
	}
	// URL-quoted forms (both escapes) of a secret are covered.
	for _, enc := range []string{url.PathEscape("server-secret"), url.QueryEscape("server-secret")} {
		if store.sanitize.safeID(enc) {
			t.Fatalf("url-quoted secret %q must fail admission", enc)
		}
	}
	// JSON-escaped BMP form of a secret is covered.
	bmpStore := reviewSanitizerStore(t, t.TempDir(), "caf\u00e9-secret")
	if bmpStore.sanitize.safeID("caf\u00e9-secret") {
		t.Fatal("bmp secret echo must fail admission")
	}
	if bmpStore.sanitize.safeID(`caf\u00e9-secret`) {
		t.Fatalf("json-escaped bmp echo %q must fail admission", `caf\u00e9-secret`)
	}
	// Full URLs reject even with unknown credential parameter names.
	for _, id := range []string{"https://evil.example/x?k=v", "HTTP://x.y/", "model https://x"} {
		if store.sanitize.safeID(id) {
			t.Fatalf("full URL %q must fail admission", id)
		}
	}
	// Benign IDs pass.
	for _, id := range []string{"m1", "acme/model-2", "pre-m1-alias", "local"} {
		if !store.sanitize.safeID(id) {
			t.Fatalf("benign id %q must pass admission", id)
		}
	}
}

func TestReviewCacheSafeIDGenericPatterns(t *testing.T) {
	dir := t.TempDir()
	store := reviewSanitizerStore(t, dir, "secret-key-1")
	// Python ErrorSanitizer generic classes: bearer scheme, key/token
	// assignment shapes, sk-/AIza-style tokens, and control characters.
	// These carry no known corpus value, so only the generic patterns (plus
	// control mapping) catch them.
	for _, id := range []string{
		"Bearer abc123",
		"bearer abc123",
		"api_key=supersecret",
		"token:abc",
		"sk-abc123XYZ",
		"model\x00name",
		"a\nb",
		"a\x1fb",
	} {
		if store.sanitize.safeID(id) {
			t.Fatalf("generic/control id %q must fail admission", id)
		}
	}
}

func TestReviewCacheSafeIDJSONAstral(t *testing.T) {
	dir := t.TempDir()
	// Secret containing an astral character: both JSON escapes of the
	// secret must be admitted as secret echoes.
	astralSecret := "k-\U0001F600-key"
	store := reviewSanitizerStore(t, dir, astralSecret)
	if store.sanitize.safeID(astralSecret) {
		t.Fatal("astral secret echo must fail admission")
	}
	// Python ensure_ascii emits the surrogate pair \ud83d\ude00; that
	// encoded shape must also fail admission.
	surrogateEscaped := `k-\ud83d\ude00-key`
	if store.sanitize.safeID(surrogateEscaped) {
		t.Fatalf("astral surrogate-escaped echo %q must fail admission", surrogateEscaped)
	}
}

func TestReviewCacheFailWriteOmitVsReadReject(t *testing.T) {
	dir := t.TempDir()
	store := reviewSanitizerStore(t, dir, "secret-key-1")
	reg := router.NewRegistry(store.cfg)
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "secret-key-1", Provider: "up", Upstream: "secret-key-1"}}, nil, float64(wallNow())); err != nil {
		t.Fatal(err)
	}
	payload, ok := store.encode(reg.ExportDiscovery())
	if !ok {
		t.Fatal("encode must succeed by omission")
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc["catalog"].(map[string]any)["models"].([]any)) != 0 {
		t.Fatal("unsafe row must be omitted, never sanitize-rewritten")
	}
	if strings.Contains(string(payload), "secret-key-1") {
		t.Fatal("payload must not carry the secret in any form")
	}
	now := float64(wallNow())
	writeFile(t, store.cfg.DiscoveryCache.Path, cacheDoc(store.identity,
		map[string]any{"succeeded_at": now, "models": []any{map[string]any{"id": "secret-key-1", "aliases": []any{}}}}, map[string]any{}))
	if st := store.Restore(context.Background(), nil); st.Load != CacheInvalid {
		t.Fatalf("secret file must be rejected whole, got %+v", st)
	}
}

// --- fingerprint: routing-affecting inputs invalidate; others do not ---

func TestReviewCacheFingerprintRoutingVsExcluded(t *testing.T) {
	dir := t.TempDir()
	base := reviewSanitizerStore(t, dir, "secret-key-1")
	if len(base.identity) != 64 {
		t.Fatalf("identity must be a hex sha256 digest, got %q", base.identity)
	}
	for _, c := range base.identity {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("identity must be hex, got %q", base.identity)
		}
	}
	// A representative routing flip must change the digest.
	flip := reviewSanitizerStore(t, dir, "rotated-key")
	if flip.identity == base.identity {
		t.Fatal("credential rotation must change the identity digest")
	}
	// Excluded inputs must not change the digest: ingress auth, retry, and
	// per-source intervals (refresh/probe timeouts) never reach the file.
	mut := reviewSanitizerStore(t, dir, "secret-key-1")
	mut.cfg.Server.APIKeys = []string{"ingress-rotated"}
	mut.cfg.Server.AuthMode = "required"
	mut.cfg.Retry.MaxAttempts = 7
	if got := cacheIdentity(mut.cfg); got != base.identity {
		t.Fatal("ingress auth and retry must be excluded from identity")
	}
	mut2 := reviewSanitizerStore(t, dir, "secret-key-1")
	mut2.cfg.Catalog.Timeout = 1
	mut2.cfg.Catalog.RefreshSeconds = 1
	mut2.cfg.Llama.ProbeTimeout = 0.5
	mut2.cfg.Llama.RefreshSeconds = 1
	if got := cacheIdentity(mut2.cfg); got != base.identity {
		t.Fatal("intervals must be excluded from identity")
	}
	// Routing inputs must change the digest.
	mut3 := reviewSanitizerStore(t, dir, "secret-key-1")
	mut3.cfg.Providers["up"].APIKey = "rotated"
	if got := cacheIdentity(mut3.cfg); got == base.identity {
		t.Fatal("provider credential must be fingerprinted")
	}
	mut4 := reviewSanitizerStore(t, dir, "secret-key-1")
	mut4.cfg.Llama.Prefix = "other-"
	if got := cacheIdentity(mut4.cfg); got == base.identity {
		t.Fatal("llama prefix must be fingerprinted")
	}
	mut5 := reviewSanitizerStore(t, dir, "secret-key-1")
	mut5.cfg.Llama.ExposeAliases = !mut5.cfg.Llama.ExposeAliases
	if got := cacheIdentity(mut5.cfg); got == base.identity {
		t.Fatal("expose_aliases must be fingerprinted")
	}
	mut6 := reviewSanitizerStore(t, dir, "secret-key-1")
	mut6.cfg.Providers["up"].TranslatedHeaders = map[string]string{"X-Extra": "changed"}
	if got := cacheIdentity(mut6.cfg); got != base.identity {
		t.Logf("translated headers fingerprinted (ok)")
	} else {
		t.Fatal("translated headers must be fingerprinted")
	}
	mut7 := reviewSanitizerStore(t, dir, "secret-key-1")
	mut7.cfg.Llama.Servers[0].URL += "/x"
	if got := cacheIdentity(mut7.cfg); got == base.identity {
		t.Fatal("llama server URL must be fingerprinted")
	}
	mut8 := reviewSanitizerStore(t, dir, "secret-key-1")
	mut8.cfg.Catalog.ModelProviders["m1"] = "other"
	if got := cacheIdentity(mut8.cfg); got == base.identity {
		t.Fatal("catalog model_providers must be fingerprinted")
	}
	mut9 := reviewSanitizerStore(t, dir, "secret-key-1")
	mut9.cfg.Providers["up"].Lossy = !mut9.cfg.Providers["up"].Lossy
	if got := cacheIdentity(mut9.cfg); got == base.identity {
		t.Fatal("provider lossy flag must be fingerprinted")
	}
}

// --- digest-only, no live-env reads, filesystem boundary ---

func TestReviewCacheDigestOnlyNoEnv(t *testing.T) {
	dir := t.TempDir()
	store := reviewSanitizerStore(t, dir, "secret-key-1")
	reg := router.NewRegistry(store.cfg)
	if err := commitCatalogShim(reg, []router.CatalogEntry{{Name: "m1", Provider: "up", Upstream: "m1"}}, nil, float64(wallNow())); err != nil {
		t.Fatal(err)
	}
	payload, ok := store.encode(reg.ExportDiscovery())
	if !ok {
		t.Fatal("encode must succeed")
	}
	text := string(payload)
	for _, secret := range []string{
		"secret-key-1", "hunter2-hdr", "translated-secret", "catalog-secret",
		"server-secret", "llama-secret", "qsecret1", "pass1", "lpass",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("payload must carry digest only, leaks %q", secret)
		}
	}
	// cacheSecrets must not read live env: an env-only value is not a secret.
	t.Setenv("MODEL_ROUTER_API_KEYS", "env-only-secret")
	t.Setenv("MODEL_ROUTER_API_KEY", "env-only-secret")
	if !store.sanitize.safeID("env-only-secret") {
		t.Fatal("sanitizer must derive from validated config, not live env")
	}
}

func TestReviewCacheFilesystemReader(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MODEL_ROUTER_API_KEYS", "")
	t.Setenv("MODEL_ROUTER_API_KEY", "")
	cfg := cacheTestConfig(t, dir)
	now := float64(wallNow())
	// Gzip magic is not JSON: invalid, never sniffed as an archive.
	os.WriteFile(cfg.DiscoveryCache.Path, []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}, 0o600)
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("gzip bytes must be invalid, got %+v", st)
	}
	// Trailing JSON after a valid document rejects.
	os.WriteFile(cfg.DiscoveryCache.Path, []byte(`{"schema_version":2,"identity":"i","catalog":null,"llama":{}} {}`), 0o600)
	if _, st, err := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); err == nil || st.Load != CacheInvalid {
		t.Fatalf("trailing JSON must be invalid, got %+v %v", st, err)
	}
	// Oversize file rejects even when the prefix is whitespace.
	big := make([]byte, CacheMaxBytes+1)
	for i := range big {
		big[i] = ' '
	}
	os.WriteFile(cfg.DiscoveryCache.Path, big, 0o600)
	if _, st, _ := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now); st.Load != CacheInvalid {
		t.Fatalf("oversize must be invalid, got %+v", st)
	}
	// Failures are value-free: no bodies or secrets in the error.
	badFile := cacheDoc("i",
		map[string]any{"succeeded_at": now, "models": []any{
			map[string]any{"id": "m", "aliases": []any{}},
			map[string]any{"id": "m", "aliases": []any{}},
		}}, map[string]any{})
	writeFile(t, cfg.DiscoveryCache.Path, badFile)
	_, _, err := LoadDiscoveryCache(cfg.DiscoveryCache.Path, 86400, now)
	if err == nil || err.Error() != "discovery cache: invalid cache file" ||
		strings.Contains(err.Error(), cfg.DiscoveryCache.Path) {
		t.Fatalf("load error must be static and value-free, got %v", err)
	}
}
