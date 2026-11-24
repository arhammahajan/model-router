package routerctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	proxyTestInferKey = "proxy-inference-key"
	proxyTestMgmtKey  = "proxy-management-key-123"
)

// proxyMock is an httptest server speaking the verified upstream surface:
// GET {base}/models (inference key), GET /v0/management/plugins,
// PATCH /v0/management/plugins/:id/enabled, GET /v0/management/auth-files
// (management key via Authorization Bearer or X-Management-Key).
type proxyMock struct {
	t            *testing.T
	srv          *httptest.Server
	gotAuth      []string // Authorization headers seen on management routes
	patchedID    string
	patchedBody  string
	modelsStatus int
}

func newProxyMock(t *testing.T) *proxyMock {
	t.Helper()
	m := &proxyMock{t: t, modelsStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+proxyTestInferKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if m.modelsStatus != http.StatusOK {
			w.WriteHeader(m.modelsStatus)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"opaque/model-a","owned_by":"anthropic"},{"id":"model-b"},{"id":"","owned_by":"x"},{"id":"opaque/model-c","owned_by":"openai"}]}`))
	})
	mux.HandleFunc("/v0/management/plugins", func(w http.ResponseWriter, r *http.Request) {
		m.gotAuth = append(m.gotAuth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Management-Key"))
		if !m.checkMgmt(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"plugins_enabled":true,"plugins_dir":"/tmp/plugins","plugins":[
{"id":"codex-auth","path":"/tmp/plugins/codex-auth","configured":true,"registered":true,"enabled":true,"effective_enabled":true,"supports_oauth":true,"oauth_provider":"codex","oauth_providers":["codex"],"supports_quota":true,"metadata":{"name":"Codex Auth","version":"1.2.3","author":"cpa"}},
{"id":"idle-plugin","configured":false,"registered":false,"enabled":false,"effective_enabled":false,"supports_oauth":false,"supports_quota":false,"menus":[],"config_fields":[]}
]}`))
	})
	mux.HandleFunc("/v0/management/plugins/", func(w http.ResponseWriter, r *http.Request) {
		m.gotAuth = append(m.gotAuth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Management-Key"))
		if !m.checkMgmt(w, r) {
			return
		}
		if r.Method != http.MethodPatch || !strings.HasSuffix(r.URL.Path, "/enabled") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v0/management/plugins/"), "/enabled")
		if id == "missing-plugin" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.patchedID = id
		m.patchedBody = "enabled=" + map[bool]string{true: "true", false: "false"}[*body.Enabled]
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v0/management/auth-files", func(w http.ResponseWriter, r *http.Request) {
		m.gotAuth = append(m.gotAuth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Management-Key"))
		if !m.checkMgmt(w, r) {
			return
		}
		// Deliberately includes server-side-only material (path, size,
		// secrets-shaped metadata) that the projection must drop.
		_, _ = w.Write([]byte(`{"observed_at":"2026-01-01T00:00:00Z","files":[
{"id":"a1","name":"codex-oauth.json","type":"codex","status":"active","disabled":false,"unavailable":false,"email":"op@example.com","account":"acct-1","path":"/secret/auth/dir/codex-oauth.json","size":4242,"quota":{"exceeded":false},"success":10,"failed":0},
{"id":"a2","name":"antigravity.json","provider":"antigravity","status":"error","status_message":"unauthorized","disabled":false,"unavailable":true}
]}`))
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *proxyMock) checkMgmt(w http.ResponseWriter, r *http.Request) bool {
	got := r.Header.Get("Authorization")
	if got == "" {
		got = "Bearer " + r.Header.Get("X-Management-Key")
	}
	if got != "Bearer "+proxyTestMgmtKey {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return false
	}
	return true
}

func writeProxyTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func proxyTestOpts(cfgPath string) Options {
	return Options{RouterConfig: cfgPath, HTTPTimeout: 5 * time.Second}
}

func TestProxySettingsModes(t *testing.T) {
	m := newProxyMock(t)

	external := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"/v1\"\napi_key=\""+proxyTestInferKey+"\"\n")
	ep, err := ProxySettings(proxyTestOpts(external))
	if err != nil {
		t.Fatalf("external: %v", err)
	}
	if ep.Mode != "external" || ep.BaseURL != m.srv.URL+"/v1" {
		t.Fatalf("external endpoint = %+v", ep)
	}
	if ep.ManagementURL != m.srv.URL+"/v0/management" {
		t.Fatalf("management URL = %q", ep.ManagementURL)
	}
	if !ep.HasInferenceKey || ep.InferenceKey != proxyTestInferKey {
		t.Fatalf("inference key not resolved")
	}

	embedded := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"/v1/\"\napi_key=\"k\"\nconfig_path=\"/tmp/proxy.yaml\"\n")
	ep, err = ProxySettings(proxyTestOpts(embedded))
	if err != nil {
		t.Fatalf("embedded: %v", err)
	}
	if ep.Mode != "embedded" || ep.BaseURL != m.srv.URL+"/v1" || ep.EmbeddedConfigPath != "/tmp/proxy.yaml" {
		t.Fatalf("embedded endpoint = %+v", ep)
	}

	// A bare origin without /v1 still yields the origin management root.
	origin := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"\"\n")
	ep, err = ProxySettings(proxyTestOpts(origin))
	if err != nil {
		t.Fatalf("origin: %v", err)
	}
	if ep.ManagementURL != m.srv.URL+"/v0/management" {
		t.Fatalf("origin management URL = %q", ep.ManagementURL)
	}

	disabled := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[providers.p]\ndialect=\"openai-chat\"\nbase_url=\"https://example.com/v1\"\napi_key=\"k\"\n")
	ep, err = ProxySettings(proxyTestOpts(disabled))
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if ep.Mode != "disabled" || ep.BaseURL != "" || ep.ManagementURL != "" {
		t.Fatalf("disabled endpoint = %+v", ep)
	}
}

func TestProxyStatusReachable(t *testing.T) {
	m := newProxyMock(t)
	t.Setenv(ProxyManagementKeyEnv, proxyTestMgmtKey)
	cfg := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"/v1\"\napi_key=\""+proxyTestInferKey+"\"\n")
	st, err := FetchProxyStatus(context.Background(), proxyTestOpts(cfg))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Mode != "external" || !st.Reachable || st.Models != 3 {
		t.Fatalf("status = %+v", st)
	}
	if st.Management != "ok" || !st.HasManagementKey || !st.HasInferenceKey {
		t.Fatalf("management/status flags = %+v", st)
	}
	if len(st.ModelIDs) != 3 || st.ModelIDs[0] != "model-b" {
		t.Fatalf("model ids = %q", st.ModelIDs)
	}
	// Secret isolation: marshaled status carries presence flags, not keys.
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), proxyTestInferKey) || strings.Contains(string(raw), proxyTestMgmtKey) {
		t.Fatalf("status JSON leaks a key: %s", raw)
	}
	// The management probe used the management key, never the inference key.
	for _, seen := range m.gotAuth {
		if strings.Contains(seen, proxyTestInferKey) {
			t.Fatalf("inference key sent to management route: %q", seen)
		}
	}
}

func TestProxyStatusUnreachableAndDisabled(t *testing.T) {
	closed := "http://127.0.0.1:1/v1"
	cfg := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+closed+"\"\napi_key=\""+proxyTestInferKey+"\"\n")
	t.Setenv(ProxyManagementKeyEnv, proxyTestMgmtKey)
	st, err := FetchProxyStatus(context.Background(), proxyTestOpts(cfg))
	if err != nil {
		t.Fatalf("unreachable status should degrade, not fail: %v", err)
	}
	if st.Reachable || st.Management != "unreachable" {
		t.Fatalf("unreachable status = %+v", st)
	}
	if st.Note == "" || strings.Contains(st.Note, proxyTestInferKey) {
		t.Fatalf("note missing or leaks key: %q", st.Note)
	}
	if !strings.Contains(st.Note, "connection refused or unreachable") {
		t.Fatalf("unreachable note = %q, want connection refused or unreachable", st.Note)
	}

	disabled := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n")
	st, err = FetchProxyStatus(context.Background(), proxyTestOpts(disabled))
	if err != nil {
		t.Fatalf("disabled status: %v", err)
	}
	if st.Mode != "disabled" || st.Reachable || st.Management != "disabled" || st.Note == "" {
		t.Fatalf("disabled status = %+v", st)
	}
}

func TestProxyStatusMissingManagementKey(t *testing.T) {
	m := newProxyMock(t)
	t.Setenv(ProxyManagementKeyEnv, "")
	cfg := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"/v1\"\napi_key=\""+proxyTestInferKey+"\"\n")
	st, err := FetchProxyStatus(context.Background(), proxyTestOpts(cfg))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Reachable || st.Management != "missing-key" || st.HasManagementKey {
		t.Fatalf("status = %+v", st)
	}
	if len(m.gotAuth) != 0 {
		t.Fatalf("management probed without a key: %v", m.gotAuth)
	}
}

func TestListProxyModels(t *testing.T) {
	m := newProxyMock(t)
	cfg := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"/v1\"\napi_key=\""+proxyTestInferKey+"\"\n")
	models, err := ListProxyModels(context.Background(), proxyTestOpts(cfg))
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 3 || models[0].ID != "opaque/model-a" || models[0].OwnedBy != "anthropic" {
		t.Fatalf("models = %+v", models)
	}

	m.modelsStatus = http.StatusUnauthorized
	_, err = ListProxyModels(context.Background(), proxyTestOpts(cfg))
	if err == nil || !strings.Contains(err.Error(), "inference key rejected") {
		t.Fatalf("auth failure err = %v", err)
	}
	if strings.Contains(err.Error(), proxyTestInferKey) {
		t.Fatalf("error leaks inference key: %v", err)
	}
}

func TestListProxyPluginsRequiresSeparateKey(t *testing.T) {
	m := newProxyMock(t)
	cfg := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"/v1\"\napi_key=\""+proxyTestInferKey+"\"\n")

	t.Setenv(ProxyManagementKeyEnv, "")
	_, err := ListProxyPlugins(context.Background(), proxyTestOpts(cfg))
	if err == nil || !strings.Contains(err.Error(), ProxyManagementKeyEnv) {
		t.Fatalf("missing key err = %v", err)
	}
	if strings.Contains(err.Error(), proxyTestInferKey) {
		t.Fatalf("missing-key error echoes inference key: %v", err)
	}

	t.Setenv(ProxyManagementKeyEnv, proxyTestMgmtKey)
	plugins, err := ListProxyPlugins(context.Background(), proxyTestOpts(cfg))
	if err != nil {
		t.Fatalf("plugins: %v", err)
	}
	if len(plugins) != 2 || plugins[0].ID != "codex-auth" {
		t.Fatalf("plugins = %+v", plugins)
	}
	got := plugins[0]
	if !got.Enabled || !got.EffectiveEnabled || !got.Registered || !got.Configured {
		t.Fatalf("codex-auth flags = %+v", got)
	}
	if !got.SupportsOAuth || len(got.OAuthProviders) != 1 || got.OAuthProviders[0] != "codex" {
		t.Fatalf("codex-auth oauth = %+v", got)
	}
	if got.Name != "Codex Auth" || got.Version != "1.2.3" || !got.SupportsQuota {
		t.Fatalf("codex-auth meta = %+v", got)
	}
	for _, seen := range m.gotAuth {
		if !strings.Contains(seen, proxyTestMgmtKey) {
			t.Fatalf("management auth missing mgmt key: %q", seen)
		}
		if strings.Contains(seen, proxyTestInferKey) {
			t.Fatalf("inference key used for management: %q", seen)
		}
	}

	raw, _ := json.Marshal(plugins)
	if strings.Contains(string(raw), proxyTestMgmtKey) || strings.Contains(string(raw), proxyTestInferKey) {
		t.Fatalf("plugins JSON leaks a key")
	}
}

func TestSetProxyPluginEnabled(t *testing.T) {
	m := newProxyMock(t)
	t.Setenv(ProxyManagementKeyEnv, proxyTestMgmtKey)
	cfg := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"/v1\"\napi_key=\""+proxyTestInferKey+"\"\n")
	opts := proxyTestOpts(cfg)

	if err := SetProxyPluginEnabled(context.Background(), opts, "idle-plugin", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if m.patchedID != "idle-plugin" || m.patchedBody != "enabled=true" {
		t.Fatalf("enable sent id=%q body=%q", m.patchedID, m.patchedBody)
	}
	if err := SetProxyPluginEnabled(context.Background(), opts, "codex-auth", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if m.patchedID != "codex-auth" || m.patchedBody != "enabled=false" {
		t.Fatalf("disable sent id=%q body=%q", m.patchedID, m.patchedBody)
	}

	if err := SetProxyPluginEnabled(context.Background(), opts, "missing-plugin", true); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing plugin err = %v", err)
	}
	for _, id := range []string{"", "a/b", "a?b"} {
		if err := SetProxyPluginEnabled(context.Background(), opts, id, true); err == nil {
			t.Fatalf("invalid id %q accepted", id)
		}
	}
	t.Setenv(ProxyManagementKeyEnv, "")
	if err := SetProxyPluginEnabled(context.Background(), opts, "idle-plugin", true); err == nil ||
		!strings.Contains(err.Error(), ProxyManagementKeyEnv) {
		t.Fatalf("no-key control err = %v", err)
	}
}

func TestListProxyAccountsProjection(t *testing.T) {
	m := newProxyMock(t)
	_ = m
	t.Setenv(ProxyManagementKeyEnv, proxyTestMgmtKey)
	cfg := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+m.srv.URL+"/v1\"\napi_key=\""+proxyTestInferKey+"\"\n")
	accounts, err := ListProxyAccounts(context.Background(), proxyTestOpts(cfg))
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v", accounts)
	}
	first := accounts[0]
	if first.Name != "antigravity.json" || first.Type != "antigravity" || first.Status != "error" || !first.Unavailable {
		t.Fatalf("first account = %+v", first)
	}
	second := accounts[1]
	if second.Name != "codex-oauth.json" || second.Type != "codex" || second.Email != "op@example.com" || second.Account != "acct-1" {
		t.Fatalf("second account = %+v", second)
	}
	raw, _ := json.Marshal(accounts)
	for _, leak := range []string{"/secret/auth/dir", "4242", proxyTestMgmtKey, proxyTestInferKey} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("accounts JSON leaks %q: %s", leak, raw)
		}
	}
}

func TestProxyClientDoesNotFollowRedirectsWithCredentials(t *testing.T) {
	var evilAuth string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer evil.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/v1/models", http.StatusFound)
	}))
	defer redirector.Close()

	cfg := writeProxyTestConfig(t, "[server]\nhost=\"127.0.0.1\"\nport=4000\n[cliproxyapi]\nbase_url=\""+redirector.URL+"/v1\"\napi_key=\""+proxyTestInferKey+"\"\n")
	_, err := ListProxyModels(context.Background(), proxyTestOpts(cfg))
	if err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("redirect err = %v", err)
	}
	if evilAuth != "" {
		t.Fatalf("credentials followed redirect: %q", evilAuth)
	}
}

func TestProxySettingsRejectsMissingConfig(t *testing.T) {
	_, err := ProxySettings(proxyTestOpts(""))
	if err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("missing config err = %v", err)
	}
}

func TestProxyStatusRejectsInvalidModelListing(t *testing.T) {
	t.Setenv(ProxyManagementKeyEnv, "")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`null`))
	}))
	defer proxy.Close()
	path := writeProxyTestConfig(t, "[cliproxyapi]\nbase_url = \""+proxy.URL+"/v1\"\n")
	status, err := FetchProxyStatus(context.Background(), proxyTestOpts(path))
	if err != nil {
		t.Fatal(err)
	}
	if status.Reachable || status.Note == "" {
		t.Fatalf("invalid model list reported healthy: %+v", status)
	}
}

func TestProxyEndpointJSONOmitsCredentials(t *testing.T) {
	body, err := json.Marshal(ProxyEndpoint{InferenceKey: proxyTestInferKey, ManagementKey: proxyTestMgmtKey})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), proxyTestInferKey) || strings.Contains(string(body), proxyTestMgmtKey) {
		t.Fatal("serialized proxy endpoint contains credentials")
	}
}

func TestSanitizedClientError(t *testing.T) {
	dialErr := &url.Error{
		Op:  "Get",
		URL: "http://user:secret@127.0.0.1:8317/v1/models",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
	}
	if got := sanitizedClientError(dialErr); got != "http://127.0.0.1:8317/v1/models: connection refused or unreachable" {
		t.Fatalf("dial error = %q", got)
	}

	dialTimeout := &url.Error{
		Op:  "Get",
		URL: "http://127.0.0.1:8317/v1/models",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded},
	}
	if got := sanitizedClientError(dialTimeout); got != "http://127.0.0.1:8317/v1/models: timed out" {
		t.Fatalf("dial timeout = %q", got)
	}

	otherErr := &url.Error{
		Op:  "Get",
		URL: "http://127.0.0.1:8317/v1/models",
		Err: errors.New("unexpected EOF"),
	}
	if got := sanitizedClientError(otherErr); got != "http://127.0.0.1:8317/v1/models: Get failed" {
		t.Fatalf("generic error = %q", got)
	}
}

func TestProxyStatusModelIDsTruncated(t *testing.T) {
	t.Setenv(ProxyManagementKeyEnv, "")
	type item struct {
		ID string `json:"id"`
	}
	items := make([]item, maxStatusModelIDs+5)
	for i := range items {
		items[i] = item{ID: fmt.Sprintf("model-%03d", i)}
	}
	payload, err := json.Marshal(map[string]any{"data": items})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer proxy.Close()

	cfg := writeProxyTestConfig(t, "[cliproxyapi]\nbase_url = \""+proxy.URL+"/v1\"\n")
	st, err := FetchProxyStatus(context.Background(), proxyTestOpts(cfg))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Models != maxStatusModelIDs+5 {
		t.Fatalf("Models = %d, want %d", st.Models, maxStatusModelIDs+5)
	}
	if len(st.ModelIDs) != maxStatusModelIDs {
		t.Fatalf("len(ModelIDs) = %d, want %d", len(st.ModelIDs), maxStatusModelIDs)
	}
	if !st.ModelIDsTruncated {
		t.Fatal("ModelIDsTruncated = false, want true")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"model_ids_truncated":true`) {
		t.Fatalf("JSON missing model_ids_truncated: %s", raw)
	}
}
