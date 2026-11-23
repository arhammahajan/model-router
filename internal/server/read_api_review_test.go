package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
)

func testReviewContract(t *testing.T, dialect string) capability.Contract {
	t.Helper()
	return capability.Empty(dialect)
}

func reviewInt(n int64) capability.Value { return capability.IntValue(n) }

func mustReviewPut(t *testing.T, c *capability.Contract, path string, v capability.Value) {
	t.Helper()
	if err := c.Put(path, v, nil, capability.KindConfigured); err != nil {
		t.Fatal(err)
	}
}

func mustReviewPutNull(t *testing.T, c *capability.Contract, path string) {
	t.Helper()
	if err := c.Put(path, capability.Null(), nil, capability.KindConfigured); err != nil {
		t.Fatal(err)
	}
}

// Independent read-API review: actual GET paths only. No production edits.
// Stable Python compared via git show HEAD:model_router/{http,discovery,gemini}.py
// + router_metadata only (working tree files never consulted).

func reviewGet(t *testing.T, url string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func reviewGeminiErr(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("gemini error not JSON: %s", data)
	}
	e, ok := out["error"].(map[string]any)
	if !ok || e["code"] == nil || e["status"] == nil || e["message"] == nil {
		t.Fatalf("gemini envelope missing fields: %s", data)
	}
	return e
}

func TestReviewReadAuthHeadersVsQueryKey(t *testing.T) {
	p := writeStaticCfg(t, `
[server]
auth_mode = "required"
api_keys = ["client-secret"]
allow_dynamic_routes = true
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "x"
[[models]]
name = "a"
provider = "up"
model = "x"
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	for _, path := range []string{"/v1/models", "/models", "/muse-code/models", "/v1/muse-code/models"} {
		if code, _ := reviewGet(t, srv.URL+path, nil); code != 401 {
			t.Fatalf("%s unauthenticated must be 401 got %d", path, code)
		}
		if code, _ := reviewGet(t, srv.URL+path+"?key=client-secret", nil); code != 401 {
			t.Fatalf("%s ?key must never auth non-Gemini reads, got %d", path, code)
		}
		if code, _ := reviewGet(t, srv.URL+path, map[string]string{"Authorization": "Bearer client-secret"}); code != 200 {
			t.Fatalf("%s header auth must be 200 got %d", path, code)
		}
		if code, _ := reviewGet(t, srv.URL+path, map[string]string{"X-Api-Key": "client-secret"}); code != 200 {
			t.Fatalf("%s x-api-key header must be 200 got %d", path, code)
		}
	}
	for _, path := range []string{"/v1beta/models", "/v1beta/models/a"} {
		if code, _ := reviewGet(t, srv.URL+path, nil); code != 401 {
			t.Fatalf("%s unauthenticated Gemini must be 401 got %d", path, code)
		}
		if code, _ := reviewGet(t, srv.URL+path, map[string]string{"Authorization": "Bearer client-secret"}); code != 200 {
			t.Fatalf("%s header auth must be 200 got %d", path, code)
		}
		if code, _ := reviewGet(t, srv.URL+path+"?key=client-secret", nil); code != 200 {
			t.Fatalf("%s ?key must auth Gemini reads, got %d", path, code)
		}
		// Any matching ?key among several still authenticates (not last-wins).
		if code, _ := reviewGet(t, srv.URL+path+"?key=wrong&key=client-secret", nil); code != 200 {
			t.Fatalf("%s any matching ?key must auth, got %d", path, code)
		}
	}
	// Index stays public.
	if code, data := reviewGet(t, srv.URL+"/", nil); code != 200 {
		t.Fatalf("/ must be public 200 got %d %s", code, data)
	}
}

func TestReviewCompatNotActivatedOnReads(t *testing.T) {
	// bearer_session_compat requires keyless access; reads must not run the
	// generation pre-step (conflicting compat tokens stay 200 on GET).
	p := writeStaticCfg(t, `
[server]
auth_mode = "none"
bearer_session_compat = true
[retry]
max_attempts = 1
[providers.up]
dialect = "anthropic"
base_url = "http://127.0.0.1:9"
api_key = "x"
[[models]]
name = "a"
provider = "up"
model = "x"
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	tokA := "router-session-550e8400-e29b-41d4-a716-446655440000"
	tokB := "router-session-6ba7b810-9dad-41d4-a716-446655440001"
	hdr := map[string]string{"Authorization": "Bearer " + tokA}
	if code, _ := reviewGet(t, srv.URL+"/v1beta/models?key="+tokB, hdr); code != 200 {
		t.Fatalf("conflicting compat tokens must not fail reads, got %d", code)
	}
	if code, _ := reviewGet(t, srv.URL+"/v1/models", hdr); code != 200 {
		t.Fatalf("compat-marked header alone must not fail keyless reads, got %d", code)
	}
	// Same two tokens on generation POST would be a 400 conflict; reads ignore it.
}

func TestReviewGeminiPaginationLastWinsAndGrammar(t *testing.T) {
	p := readCfg(t, `
[[models]]
name = "m1"
provider = "up"
model = "x1"
[[models]]
name = "m2"
provider = "up"
model = "x2"
[[models]]
name = "m3"
provider = "up"
model = "x3"
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	_, _, data := get(t, srv.URL+"/v1beta/models?pageSize=1", nil)
	var o1 map[string]any
	_ = json.Unmarshal(data, &o1)
	tok, _ := o1["nextPageToken"].(string)
	if tok == "" {
		t.Fatalf("need next token: %s", data)
	}
	if strings.Contains(tok, "=") {
		t.Fatalf("token must be RawURLEncoding without padding: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || string(raw) != "offset:1" {
		t.Fatalf("token must encode offset:1 got %q err %v", raw, err)
	}
	// Last pageSize wins even when earlier value is malformed.
	if code, _, d := get(t, srv.URL+"/v1beta/models?pageSize=abc&pageSize=1", nil); code != 200 {
		t.Fatalf("last pageSize must win over malformed first, got %d %s", code, d)
	} else {
		var o map[string]any
		_ = json.Unmarshal(d, &o)
		if len(o["models"].([]any)) != 1 {
			t.Fatalf("last pageSize=1 must slice to 1: %s", d)
		}
	}
	// Last pageToken wins even when earlier value is malformed.
	if code, _, d := get(t, srv.URL+"/v1beta/models?pageSize=1&pageToken=bogus&pageToken="+tok, nil); code != 200 {
		t.Fatalf("last pageToken must win, got %d %s", code, d)
	} else {
		var o map[string]any
		_ = json.Unmarshal(d, &o)
		mods := o["models"].([]any)
		if len(mods) != 1 || mods[0].(map[string]any)["baseModelId"] != "m2" {
			t.Fatalf("second page must be m2: %s", d)
		}
	}
	// Boundaries: 1000 allowed, 1001 and 7-digit rejected, 0/empty default.
	if code, _, d := get(t, srv.URL+"/v1beta/models?pageSize=1000", nil); code != 200 {
		t.Fatalf("pageSize 1000 must be 200 got %d %s", code, d)
	}
	for _, q := range []string{"1001", "1234567", "9999999", "-1", "1.5", "+1", " 1"} {
		code, _, d := get(t, srv.URL+"/v1beta/models?pageSize="+q, nil)
		// Space encodes as %20 via get helper? Build manually for " 1".
		if q == " 1" {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1beta/models?pageSize=%201", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			code, d = resp.StatusCode, body
		}
		if code != 400 {
			t.Fatalf("pageSize %q must be 400 got %d %s", q, code, d)
		}
		e := geminiErr(t, d)
		if e["status"] != "INVALID_ARGUMENT" {
			t.Fatalf("pageSize %q status: %v", q, e)
		}
	}
	// Malformed tokens are value-free 400s.
	for _, q := range []string{"bogus", "b2Zmc2V0", "offset:abc", "offset:9999999999"} {
		code, _, d := get(t, srv.URL+"/v1beta/models?pageToken="+q, nil)
		if code != 400 {
			t.Fatalf("pageToken %q must be 400 got %d %s", q, code, d)
		}
		geminiErr(t, d)
		if strings.Contains(string(d), q) && q != "bogus" {
			t.Fatalf("token value leaked: %s", d)
		}
	}
	// Far offset: empty models, no token.
	far := base64.RawURLEncoding.EncodeToString([]byte("offset:99"))
	if code, _, d := get(t, srv.URL+"/v1beta/models?pageToken="+far, nil); code != 200 {
		t.Fatalf("far offset 200 got %d %s", code, d)
	} else {
		var o map[string]any
		_ = json.Unmarshal(d, &o)
		if len(o["models"].([]any)) != 0 {
			t.Fatalf("far offset must be empty: %s", d)
		}
		if _, has := o["nextPageToken"]; has {
			t.Fatalf("no token at end: %s", d)
		}
	}
}

func TestReviewGeminiGetCanonicalDecodeOnce(t *testing.T) {
	p := readCfg(t, `
[[models]]
name = "svc/model:v2"
provider = "up"
model = "x"
[[models]]
name = "real"
provider = "up"
model = "x"
aliases = ["alias-a"]
[[models]]
name = "off"
provider = "up"
model = "y"
enabled = false
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	// Single decode: %2F -> /, %3A -> :.
	code, _, data := get(t, srv.URL+"/v1beta/models/svc%2Fmodel%3Av2", nil)
	if code != 200 {
		t.Fatalf("escaped canonical get 200 got %d %s", code, data)
	}
	var res map[string]any
	_ = json.Unmarshal(data, &res)
	if res["baseModelId"] != "svc/model:v2" || res["name"] != "models/svc/model:v2" {
		t.Fatalf("decoded identity wrong: %s", data)
	}
	// Double-encoded decodes once only: %252F -> literal %2F, not /.
	if code, _, _ := get(t, srv.URL+"/v1beta/models/svc%252Fmodel%253Av2", nil); code != 404 {
		t.Fatalf("double-encoded must be 404 got %d", code)
	}
	// Control characters reject the path.
	if code, _, data := get(t, srv.URL+"/v1beta/models/real%00", nil); code != 404 {
		t.Fatalf("control char must be 404 got %d %s", code, data)
	}
	// Invalid escape never reaches the handler as a decoded name.
	if _, _, _, ok := parseGeminiGetPath("/v1beta/models/%ZZ"); ok {
		t.Fatalf("invalid escape must not parse as a resource path")
	}
	// Canonical-only: alias and disabled 404 with Gemini envelope.
	for _, path := range []string{"/v1beta/models/alias-a", "/v1beta/models/off", "/v1beta/models/ghost"} {
		code, _, data := get(t, srv.URL+path, nil)
		if code != 404 {
			t.Fatalf("%s must be 404 got %d", path, code)
		}
		if e := geminiErr(t, data); e["status"] != "NOT_FOUND" {
			t.Fatalf("%s envelope: %s", path, data)
		}
	}
	// tunedModels always 404.
	if code, _, data := get(t, srv.URL+"/v1beta/tunedModels/real", nil); code != 404 {
		t.Fatalf("tunedModels must be 404 got %d %s", code, data)
	}
	// Trailing slash on a get still resolves the resource.
	if code, _, data := get(t, srv.URL+"/v1beta/models/real/", nil); code != 200 {
		t.Fatalf("trailing slash get must be 200 got %d %s", code, data)
	} else {
		var g map[string]any
		_ = json.Unmarshal(data, &g)
		if g["baseModelId"] != "real" {
			t.Fatalf("trailing get identity: %s", data)
		}
	}
	// Trailing slash list still lists.
	if code, _, _ := get(t, srv.URL+"/v1beta/models/", nil); code != 200 {
		t.Fatalf("trailing slash list must be 200 got %d", code)
	}
	// Generation POST still routes (not 404/405) on the same prefix family.
	resp, err := http.Post(srv.URL+"/v1beta/models/real:generateContent", "application/json", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode == 405 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("gemini generation must still route, got %d %s", resp.StatusCode, b)
	}
	// List POST stays non-405; OpenAI list POST stays 405.
	resp2, err := http.Post(srv.URL+"/v1beta/models", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == 405 {
		t.Fatalf("POST /v1beta/models must not be 405")
	}
	resp3, err := http.Post(srv.URL+"/v1/models", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 405 {
		t.Fatalf("POST /v1/models must stay 405 got %d", resp3.StatusCode)
	}
}

func TestReviewTypedLimitsFallbackPresentNull(t *testing.T) {
	// Explicit Null max_input_tokens must still fall back to context_window.
	// Distinct from the implicit-absent regression: the fact is present as
	// Null (typed unknown), not merely missing.
	c := testReviewContract(t, "openai-chat")
	mustReviewPut(t, &c, "limits.context_window", reviewInt(9007199254740993))
	mustReviewPutNull(t, &c, "limits.max_input_tokens")
	row := listingRow{name: "m", contract: c}
	if got := geminiResourceFor(row)["inputTokenLimit"]; got != int64(9007199254740993) {
		t.Fatalf("present-Null max_input must fall back exactly, got %v", got)
	}
	mustReviewPut(t, &c, "limits.max_input_tokens", reviewInt(32000))
	row.contract = c
	if got := geminiResourceFor(row)["inputTokenLimit"]; got != int64(32000) {
		t.Fatalf("explicit max_input must take priority, got %v", got)
	}
	// Raw metadata never sources typed limits: HTTP-level check.
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "lim-raw"
provider = "up"
model = "z"
[models.metadata]
display_name = "Pretty"
context_window = 200000
[[models]]
name = "lim-nodisp"
provider = "up"
model = "w"
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	_, _, d3 := get(t, srv.URL+"/v1beta/models/lim-raw", nil)
	var r3 map[string]any
	_ = json.Unmarshal(d3, &r3)
	if _, has := r3["inputTokenLimit"]; has {
		t.Fatalf("raw metadata context_window must not become inputTokenLimit: %s", d3)
	}
	if _, has := r3["outputTokenLimit"]; has {
		t.Fatalf("raw metadata must not become outputTokenLimit: %s", d3)
	}
	// Display falls back to canonical when metadata display_name absent.
	_, _, d4 := get(t, srv.URL+"/v1beta/models/lim-nodisp", nil)
	var r4 map[string]any
	_ = json.Unmarshal(d4, &r4)
	if r4["displayName"] != "lim-nodisp" {
		t.Fatalf("display fallback must be canonical id, got %s", d4)
	}
	if r3["displayName"] != "Pretty" {
		t.Fatalf("display_name metadata must be honored, got %s", d3)
	}
	// Exact bigint fidelity through the shared listing renderer.
	if strings.Contains(string(d4), ".0") && strings.Contains(string(d4), "9007199254740993") {
		t.Fatalf("bigint fidelity: %s", d4)
	}
}

func TestReviewMuseVariantsEmptyVsUnknownAndDefaults(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "m-empty"
provider = "up"
model = "x"
[models.metadata]
display_name = "Pretty Empty"
[capabilities.overrides.m-empty.reasoning]
supported = true
[capabilities.overrides.m-empty.reasoning.effort]
values = []
[[models]]
name = "m-unknown"
provider = "up"
model = "y"
[[models]]
name = "m-false"
provider = "up"
model = "z"
[capabilities.overrides.m-false.reasoning]
supported = false
[[models]]
name = "m-vals"
provider = "up"
model = "w"
[capabilities.overrides.m-vals.reasoning]
supported = true
[capabilities.overrides.m-vals.reasoning.effort]
values = ["low", "high"]
default = "low"
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	_, _, data := get(t, srv.URL+"/muse-code/models", nil)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	rows := out["data"].([]any)
	mc := map[string]map[string]any{}
	ids := map[string]bool{}
	for _, r := range rows {
		row := r.(map[string]any)
		ids[row["id"].(string)] = true
		meta := row["metadata"].(map[string]any)
		mc[row["id"].(string)] = meta["muse-code"].(map[string]any)
		if row["owned_by"] != "router" || row["object"] != "model" {
			t.Fatalf("muse row envelope must stay OpenAI-shaped: %v", row)
		}
	}
	for _, id := range []string{"m-empty", "m-unknown", "m-false", "m-vals"} {
		if !ids[id] {
			t.Fatalf("missing muse row %s: %s", id, data)
		}
	}
	// Known empty values -> variants present as {}, no options.
	empty := mc["m-empty"]
	if v, ok := empty["variants"]; !ok {
		t.Fatalf("known empty effort values must present variants {}: %v", empty)
	} else if m, ok := v.(map[string]any); !ok || len(m) != 0 {
		t.Fatalf("known empty variants must be {}: %v", empty)
	}
	if _, has := empty["options"]; has {
		t.Fatalf("empty values must not set options: %v", empty)
	}
	if empty["reasoning"] != true {
		t.Fatalf("m-empty reasoning must be true: %v", empty)
	}
	// Unknown -> no variants key, reasoning false (presentation default).
	unk := mc["m-unknown"]
	if _, has := unk["variants"]; has {
		t.Fatalf("unknown effort must omit variants: %v", unk)
	}
	if unk["reasoning"] != false {
		t.Fatalf("unknown reasoning presents as false: %v", unk)
	}
	// Explicit false -> reasoning false, no variants.
	if mc["m-false"]["reasoning"] != false {
		t.Fatalf("explicit false: %v", mc["m-false"])
	}
	if _, has := mc["m-false"]["variants"]; has {
		t.Fatalf("false must omit variants: %v", mc["m-false"])
	}
	// Defaults apply when typed limits unknown (static routes carry no typed
	// limits); typed facts replace them via the shared renderer, not metadata.
	limEmpty := empty["limit"].(map[string]any)
	if limEmpty["context"] != float64(128000) || limEmpty["output"] != float64(16384) {
		t.Fatalf("m-empty must use presentation defaults 128000/16384: %v", empty)
	}
	limVals := mc["m-vals"]["limit"].(map[string]any)
	if limVals["context"] != float64(128000) || limVals["output"] != float64(16384) {
		t.Fatalf("static typed-unknown must use defaults: %v", mc["m-vals"])
	}
	// Typed limits replace defaults at the renderer (not via raw metadata).
	tc := testReviewContract(t, "openai-chat")
	mustReviewPut(t, &tc, "limits.context_window", reviewInt(50000))
	mustReviewPut(t, &tc, "limits.max_output_tokens", reviewInt(777))
	got := museCodeFor(listingRow{name: "x", contract: tc})
	lim := got["limit"].(map[string]any)
	if lim["context"] != int64(50000) || lim["output"] != int64(777) {
		t.Fatalf("typed limits must replace defaults, got %v", lim)
	}
	variants := mc["m-vals"]["variants"].(map[string]any)
	if len(variants) != 2 || variants["low"] == nil || variants["high"] == nil {
		t.Fatalf("variants must enumerate values: %v", mc["m-vals"])
	}
	opts := mc["m-vals"]["options"].(map[string]any)
	if opts["reasoningEffort"] != "low" {
		t.Fatalf("explicit default must be honored: %v", mc["m-vals"])
	}
	if empty["name"] != "Pretty Empty" || empty["display_label"] != "Pretty Empty" || empty["is_hidden"] != false {
		t.Fatalf("muse identity wrong: %v", empty)
	}
	if unk["name"] != "m-unknown" {
		t.Fatalf("muse display fallback must be canonical: %v", unk)
	}
}

func TestReviewProviderIdentityIndexEnvelope(t *testing.T) {
	p := readCfg(t, `
[[models]]
name = "a"
provider = "up"
model = "x-upstream"
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	// Gemini resource description carries provider + effective dialect only.
	_, _, gd := get(t, srv.URL+"/v1beta/models/a", nil)
	var gr map[string]any
	_ = json.Unmarshal(gd, &gr)
	desc, _ := gr["description"].(string)
	if desc != "Routed to provider 'up' (anthropic) by router." {
		t.Fatalf("description must be provider+EffectiveDialect, got %q (%s)", desc, gd)
	}
	if strings.Contains(string(gd), "127.0.0.1:9") || strings.Contains(string(gd), "upstream-secret") || strings.Contains(string(gd), "http") {
		t.Fatalf("provider URL/key leak in Gemini resource: %s", gd)
	}
	if gr["displayName"] != "a" {
		t.Fatalf("display fallback: %s", gd)
	}
	methods, _ := gr["supportedGenerationMethods"].([]any)
	if len(methods) != 2 {
		t.Fatalf("generation methods: %s", gd)
	}
	// OpenAI envelope stays Go-owned.
	_, _, od := get(t, srv.URL+"/v1/models", nil)
	var out map[string]any
	_ = json.Unmarshal(od, &out)
	row := out["data"].([]any)[0].(map[string]any)
	if row["id"] != "a" || row["owned_by"] != "router" || row["object"] != "model" {
		t.Fatalf("OpenAI envelope must retain owned_by=router: %s", od)
	}
	meta := row["metadata"].(map[string]any)
	if _, ok := meta["router"]; !ok {
		t.Fatalf("router contract must overwrite raw metadata.router: %s", od)
	}
	// Public index shares the endpoint BuildVersion literal.
	_, _, id := get(t, srv.URL+"/", nil)
	var idx map[string]any
	_ = json.Unmarshal(id, &idx)
	if idx["version"] != BuildVersion {
		t.Fatalf("index version must share server.BuildVersion %q, got %s", BuildVersion, id)
	}
	if idx["name"] != "routerd" {
		t.Fatalf("index must stay Go-owned literal: %s", id)
	}
	// Unknown reasoning is omission, explicit false stays false.
	p2 := readCfg(t, `
[[models]]
name = "sup"
provider = "up"
model = "x"
[[models]]
name = "unsup"
provider = "up"
model = "y"
[capabilities.overrides.unsup.reasoning]
supported = false
`)
	srv2 := httptest.NewServer(New(mustLoad(t, p2)).Handler())
	defer srv2.Close()
	_, _, dsup := get(t, srv2.URL+"/v1beta/models/sup", nil)
	var rsup map[string]any
	_ = json.Unmarshal(dsup, &rsup)
	if _, has := rsup["thinking"]; has {
		t.Fatalf("unknown thinking must be omitted: %s", dsup)
	}
	_, _, duns := get(t, srv2.URL+"/v1beta/models/unsup", nil)
	var runs map[string]any
	_ = json.Unmarshal(duns, &runs)
	if runs["thinking"] != false {
		t.Fatalf("explicit false thinking must stay false: %s", duns)
	}
}

func TestReviewNoUpstreamIONorLeakConcurrent(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[capabilities.overrides.a.tools]
supported = true
`)
	cfg := mustLoad(t, p)
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, path := range []string{"/v1/models", "/models", "/muse-code/models", "/v1/muse-code/models", "/v1beta/models", "/v1beta/models/a", "/"} {
				code, data := reviewGet(t, srv.URL+path, nil)
				if code != 200 {
					t.Errorf("%s code=%d %s", path, code, data)
					return
				}
				// Concurrent pin data: every view lists the same canonical row.
				if path == "/v1/models" && !strings.Contains(string(data), `"id":"a"`) {
					t.Errorf("canonical row missing under concurrency: %s", data)
				}
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 0 {
		t.Fatalf("reads must not touch upstream, got %d", got)
	}
	for _, path := range []string{"/v1/models", "/muse-code/models", "/v1beta/models", "/v1beta/models/a", "/"} {
		_, data := reviewGet(t, srv.URL+path, nil)
		s := string(data)
		if strings.Contains(s, up.URL) || strings.Contains(s, "upstream-secret") || strings.Contains(s, "127.0.0.1:9") {
			t.Fatalf("provider secret leak in %s: %s", path, s)
		}
	}
}
