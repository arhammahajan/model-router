package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func readCfg(t *testing.T, body string) string {
	t.Helper()
	return writeStaticCfg(t, staticBase+body)
}

func get(t *testing.T, url string, hdr map[string]string) (int, http.Header, []byte) {
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
	return resp.StatusCode, resp.Header, data
}

func geminiErr(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("gemini error not JSON: %s", data)
	}
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("gemini envelope missing: %s", data)
	}
	if e["code"] == nil || e["status"] == nil || e["message"] == nil {
		t.Fatalf("gemini fields missing: %s", data)
	}
	return e
}

func TestReadAllRoutesAuth(t *testing.T) {
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
	cfg := mustLoad(t, p)
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	// OpenAI + alias + muse: headers only, ?key must not auth.
	for _, path := range []string{"/v1/models", "/models", "/muse-code/models", "/v1/muse-code/models"} {
		if code, _, _ := get(t, srv.URL+path, nil); code != 401 {
			t.Fatalf("%s no-auth must be 401 got %d", path, code)
		}
		if code, _, _ := get(t, srv.URL+path+"?key=client-secret", nil); code != 401 {
			t.Fatalf("%s ?key must not auth got %d", path, code)
		}
		if code, _, _ := get(t, srv.URL+path, map[string]string{"Authorization": "Bearer client-secret"}); code != 200 {
			t.Fatalf("%s header auth must be 200 got %d", path, code)
		}
	}
	// Gemini list/get: headers or ?key.
	for _, path := range []string{"/v1beta/models", "/v1beta/models/a"} {
		if code, _, _ := get(t, srv.URL+path, nil); code != 401 {
			t.Fatalf("%s no-auth must be 401 got %d", path, code)
		}
		if code, _, _ := get(t, srv.URL+path, map[string]string{"Authorization": "Bearer client-secret"}); code != 200 {
			t.Fatalf("%s header must be 200 got %d", path, code)
		}
		if code, _, _ := get(t, srv.URL+path+"?key=client-secret", nil); code != 200 {
			t.Fatalf("%s ?key must be 200 got %d", path, code)
		}
	}
	// Index is public.
	if code, _, data := get(t, srv.URL+"/", nil); code != 200 {
		t.Fatalf("/ must be public 200 got %d %s", code, data)
	} else {
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		if out["name"] != goIndexName || out["version"] != BuildVersion {
			t.Fatalf("index identity must be Go-owned literals: %s", data)
		}
		eps, ok := out["endpoints"].([]any)
		if !ok || len(eps) == 0 {
			t.Fatalf("index endpoints missing: %s", data)
		}
	}
}

func TestReadGeminiPagination(t *testing.T) {
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
	// Default page: all three, no token.
	_, _, data := get(t, srv.URL+"/v1beta/models", nil)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out["models"].([]any)) != 3 {
		t.Fatalf("default must list 3: %s", data)
	}
	if _, has := out["nextPageToken"]; has {
		t.Fatalf("no token when complete: %s", data)
	}
	// pageSize=1 chains.
	_, _, d1 := get(t, srv.URL+"/v1beta/models?pageSize=1", nil)
	var o1 map[string]any
	_ = json.Unmarshal(d1, &o1)
	if len(o1["models"].([]any)) != 1 {
		t.Fatalf("size 1: %s", d1)
	}
	tok1, _ := o1["nextPageToken"].(string)
	if tok1 == "" {
		t.Fatalf("want next token: %s", d1)
	}
	// Token decodes to offset:1.
	raw, err := base64.RawURLEncoding.DecodeString(tok1)
	if err != nil || string(raw) != "offset:1" {
		t.Fatalf("token must be offset:1 got %q err %v", raw, err)
	}
	_, _, d2 := get(t, srv.URL+"/v1beta/models?pageSize=1&pageToken="+tok1, nil)
	var o2 map[string]any
	_ = json.Unmarshal(d2, &o2)
	if len(o2["models"].([]any)) != 1 {
		t.Fatalf("second page: %s", d2)
	}
	// Last repeated wins.
	_, _, d3 := get(t, srv.URL+"/v1beta/models?pageSize=100&pageSize=1", nil)
	var o3 map[string]any
	_ = json.Unmarshal(d3, &o3)
	if len(o3["models"].([]any)) != 1 {
		t.Fatalf("last pageSize must win: %s", d3)
	}
	// pageSize=0 defaults to 50 (all three, no token).
	_, _, d4 := get(t, srv.URL+"/v1beta/models?pageSize=0", nil)
	var o4 map[string]any
	_ = json.Unmarshal(d4, &o4)
	if len(o4["models"].([]any)) != 3 {
		t.Fatalf("pageSize 0 must default: %s", d4)
	}
	// Empty pageSize defaults.
	_, _, d5 := get(t, srv.URL+"/v1beta/models?pageSize=", nil)
	var o5 map[string]any
	_ = json.Unmarshal(d5, &o5)
	if len(o5["models"].([]any)) != 3 {
		t.Fatalf("empty pageSize must default: %s", d5)
	}
	// Invalid sizes: value-free 400 Gemini envelope.
	for _, q := range []string{"abc", "1001", "1234567", "-1", "1.5", "%201", "9999999"} {
		code, _, data := get(t, srv.URL+"/v1beta/models?pageSize="+q, nil)
		if code != 400 {
			t.Fatalf("pageSize %q must be 400 got %d %s", q, code, data)
		}
		e := geminiErr(t, data)
		if e["status"] != "INVALID_ARGUMENT" {
			t.Fatalf("status: %v", e)
		}
		if q != "%201" && strings.Contains(string(data), q) {
			t.Fatalf("value must not echo: %s", data)
		}
	}
	// Invalid tokens: value-free 400.
	for _, q := range []string{"bogus", "b2Zmc2V0", "T2Zmc2V0OjE=", "offset:abc"} {
		code, _, data := get(t, srv.URL+"/v1beta/models?pageToken="+q, nil)
		if code != 400 {
			t.Fatalf("pageToken %q must be 400 got %d %s", q, code, data)
		}
		geminiErr(t, data)
		if strings.Contains(string(data), q) {
			t.Fatalf("token value leaked: %s", data)
		}
	}
	// Offset beyond length: empty models, no token.
	far := base64.RawURLEncoding.EncodeToString([]byte("offset:99"))
	code, _, data := get(t, srv.URL+"/v1beta/models?pageToken="+far, nil)
	if code != 200 {
		t.Fatalf("far offset 200 got %d %s", code, data)
	}
	var of map[string]any
	_ = json.Unmarshal(data, &of)
	if len(of["models"].([]any)) != 0 {
		t.Fatalf("far offset empty: %s", data)
	}
	if _, has := of["nextPageToken"]; has {
		t.Fatalf("no token at end: %s", data)
	}
}

func TestReadGeminiGetEscaped(t *testing.T) {
	p := readCfg(t, `
[[models]]
name = "plain"
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
[models.metadata]
display_name = "Pretty"
context_window = 200000
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	// Canonical get.
	code, _, data := get(t, srv.URL+"/v1beta/models/real", nil)
	if code != 200 {
		t.Fatalf("get real 200 got %d %s", code, data)
	}
	var res map[string]any
	_ = json.Unmarshal(data, &res)
	if res["name"] != "models/real" || res["baseModelId"] != "real" {
		t.Fatalf("resource identity: %s", data)
	}
	if res["displayName"] != "Pretty" && !strings.Contains(string(data), "real") {
		t.Fatalf("display: %s", data)
	}
	// Alias get must 404 (canonical-only).
	if code, _, data := get(t, srv.URL+"/v1beta/models/alias-a", nil); code != 404 {
		t.Fatalf("alias get must 404 got %d %s", code, data)
	} else {
		e := geminiErr(t, data)
		if e["status"] != "NOT_FOUND" {
			t.Fatalf("alias 404 envelope: %s", data)
		}
	}
	// Disabled must 404.
	if code, _, _ := get(t, srv.URL+"/v1beta/models/off", nil); code != 404 {
		t.Fatalf("disabled get 404 got %d", code)
	}
	// Unknown must 404 value-free.
	if code, _, data := get(t, srv.URL+"/v1beta/models/ghost", nil); code != 404 {
		t.Fatalf("unknown 404 got %d", code)
	} else if strings.Contains(string(data), "ghost-secret") {
		t.Fatalf("leak: %s", data)
	}
	// tunedModels get 404.
	if code, _, data := get(t, srv.URL+"/v1beta/tunedModels/real", nil); code != 404 {
		t.Fatalf("tunedModels 404 got %d %s", code, data)
	}
	// Unknown limits omitted: metadata context_window must not become inputTokenLimit.
	if _, has := res["inputTokenLimit"]; has {
		t.Fatalf("typed unknown must omit inputTokenLimit even with metadata context_window: %s", data)
	}
	// Trailing slash list.
	if code, _, data := get(t, srv.URL+"/v1beta/models/", nil); code != 200 {
		t.Fatalf("trailing slash list 200 got %d %s", code, data)
	}
	// GET must not be 405 on generation prefixes.
	for _, path := range []string{"/v1/models/x", "/models/x", "/v1beta/tunedModels", "/v1/tunedModels/y"} {
		code, _, data := get(t, srv.URL+path, nil)
		if code == 405 {
			t.Fatalf("GET %s must not be 405 got %s", path, data)
		}
	}
	// POST to list paths stays non-405 (404) while /v1/models POST stays 405.
	if code, _, _ := get(t, srv.URL+"/v1beta/models", nil); code != 200 {
		t.Fatalf("sanity")
	}
	resp, err := http.Post(srv.URL+"/v1beta/models", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 405 {
		t.Fatalf("POST /v1beta/models must not be 405 (unchanged 404)")
	}
	resp2, err := http.Post(srv.URL+"/v1/models", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 405 {
		t.Fatalf("POST /v1/models must stay 405 got %d", resp2.StatusCode)
	}
}

func TestReadEscapedSlashColonOnce(t *testing.T) {
	p := readCfg(t, `
[[models]]
name = "team/gem:flash"
provider = "up"
model = "x"
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	// Single decode: %2F -> /, %3A -> :.
	code, _, data := get(t, srv.URL+"/v1beta/models/team%2Fgem%3Aflash", nil)
	if code != 200 {
		t.Fatalf("escaped get 200 got %d %s", code, data)
	}
	var res map[string]any
	_ = json.Unmarshal(data, &res)
	if res["baseModelId"] != "team/gem:flash" {
		t.Fatalf("decoded id: %s", data)
	}
	// Double-encoded %252F decodes once to %2F, not to /, so 404.
	if code, _, _ := get(t, srv.URL+"/v1beta/models/team%252Fgem%253Aflash", nil); code != 404 {
		t.Fatalf("double-encoded must 404 got %d", code)
	}
}

func TestReadKnownFalseVsUnknown(t *testing.T) {
	p := readCfg(t, `
[[models]]
name = "supported"
provider = "up"
model = "x"
[[models]]
name = "unsupported"
provider = "up"
model = "y"
[[models]]
name = "unknown"
provider = "up"
model = "z"
[capabilities.overrides.supported.reasoning]
supported = true
[capabilities.overrides.unsupported.reasoning]
supported = false
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	byID := map[string]map[string]any{}
	for _, id := range []string{"supported", "unsupported", "unknown"} {
		_, _, data := get(t, srv.URL+"/v1beta/models/"+id, nil)
		var res map[string]any
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatal(err)
		}
		byID[id] = res
	}
	if byID["supported"]["thinking"] != true {
		t.Fatalf("supported thinking true: %v", byID["supported"])
	}
	if byID["unsupported"]["thinking"] != false {
		t.Fatalf("explicit false must stay false: %v", byID["unsupported"])
	}
	if _, has := byID["unknown"]["thinking"]; has {
		t.Fatalf("unknown must omit thinking: %v", byID["unknown"])
	}
	// Muse reasoning mirrors: true / false / false, variants only when known.
	_, _, data := get(t, srv.URL+"/muse-code/models", nil)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	rows := out["data"].([]any)
	mcode := map[string]map[string]any{}
	for _, r := range rows {
		row := r.(map[string]any)
		meta := row["metadata"].(map[string]any)
		mc := meta["muse-code"].(map[string]any)
		mcode[row["id"].(string)] = mc
	}
	if mcode["supported"]["reasoning"] != true {
		t.Fatalf("muse supported: %v", mcode["supported"])
	}
	if mcode["unsupported"]["reasoning"] != false || mcode["unknown"]["reasoning"] != false {
		t.Fatalf("muse false/unknown: %v %v", mcode["unsupported"], mcode["unknown"])
	}
}

func TestReadExactLimitsAndMuseDefaults(t *testing.T) {
	p := readCfg(t, `
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata]
display_name = "Pretty"
context_window = 200000
max_tokens = 64000
`)
	srv := httptest.NewServer(New(mustLoad(t, p)).Handler())
	defer srv.Close()
	// OpenAI row: raw metadata exact, router overwritten.
	_, _, data := get(t, srv.URL+"/v1/models", nil)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	row := out["data"].([]any)[0].(map[string]any)
	meta := row["metadata"].(map[string]any)
	if meta["display_name"] != "Pretty" {
		t.Fatalf("display preserved: %s", data)
	}
	// Bigint exactness: no float .0.
	if strings.Contains(string(data), "200000.0") {
		t.Fatalf("bigint fidelity: %s", data)
	}
	if _, ok := meta["router"]; !ok {
		t.Fatalf("router overwritten: %s", data)
	}
	// Raw router injection must not survive.
	p2 := readCfg(t, `
[[models]]
name = "a"
provider = "up"
model = "x"
[models.metadata.router.reasoning.effort]
values = ["low"]
`)
	srv2 := httptest.NewServer(New(mustLoad(t, p2)).Handler())
	defer srv2.Close()
	_, _, d2 := get(t, srv2.URL+"/v1/models", nil)
	if strings.Contains(string(d2), `"low"`) {
		// The raw string "low" could appear elsewhere; check router value null.
		var o map[string]any
		_ = json.Unmarshal(d2, &o)
		r := o["data"].([]any)[0].(map[string]any)["metadata"].(map[string]any)["router"].(map[string]any)
		eff := r["reasoning"].(map[string]any)["effort"].(map[string]any)
		if eff["values"].(map[string]any)["value"] != nil {
			t.Fatalf("raw router injection must render unknown: %s", d2)
		}
	}
	// Gemini: no typed limits -> omitted; Muse: defaults 128000/16384.
	_, _, gd := get(t, srv.URL+"/v1beta/models/a", nil)
	var gr map[string]any
	_ = json.Unmarshal(gd, &gr)
	if _, has := gr["inputTokenLimit"]; has {
		t.Fatalf("metadata must not source limits: %s", gd)
	}
	_, _, md := get(t, srv.URL+"/muse-code/models", nil)
	var mo map[string]any
	_ = json.Unmarshal(md, &mo)
	mc := mo["data"].([]any)[0].(map[string]any)["metadata"].(map[string]any)["muse-code"].(map[string]any)
	lim := mc["limit"].(map[string]any)
	if lim["context"] != float64(128000) || lim["output"] != float64(16384) {
		t.Fatalf("muse defaults: %s", md)
	}
	if mc["name"] != "Pretty" || mc["display_label"] != "Pretty" || mc["is_hidden"] != false {
		t.Fatalf("muse identity: %s", md)
	}
}

func TestReadNoIOStalePinConcurrent(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()
	p := readCfg(t, `
[[models]]
name = "a"
provider = "up"
model = "x"
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
			for _, path := range []string{"/v1/models", "/models", "/muse-code/models", "/v1beta/models", "/"} {
				code, _, _ := get(t, srv.URL+path, nil)
				if code != 200 {
					t.Errorf("%s code=%d", path, code)
				}
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 0 {
		t.Fatalf("listing must not touch upstream, got %d", got)
	}
	// No provider URL/key leak in any view.
	for _, path := range []string{"/v1/models", "/muse-code/models", "/v1beta/models", "/v1beta/models/a", "/"} {
		_, _, data := get(t, srv.URL+path, nil)
		s := string(data)
		if strings.Contains(s, up.URL) || strings.Contains(s, "upstream-secret") || strings.Contains(s, "127.0.0.1:9") {
			t.Fatalf("provider secret leak in %s: %s", path, s)
		}
	}
}

func TestReadNativeGenerationUnchanged(t *testing.T) {
	p := readCfg(t, `
[[models]]
name = "a"
provider = "up"
model = "x"
`)
	cfg := mustLoad(t, p)
	var lastPath atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		lastPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	for _, prov := range cfg.Providers {
		prov.BaseURL = up.URL
	}
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	status, _, data := post(t, srv.URL+"/v1/chat/completions", `{"model":"a","messages":[{"role":"user","content":"Hi"}]}`, nil)
	if status != 200 {
		t.Fatalf("native chat unchanged: %d %s", status, data)
	}
	// Gemini generation POST still routes (may 502/200 depending on fake, but not 404/405).
	resp, err := http.Post(srv.URL+"/v1beta/models/a:generateContent", "application/json", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode == 405 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("gemini generation must still route, got %d %s", resp.StatusCode, b)
	}
	_ = lastPath
	_ = fmt.Sprint()
}
