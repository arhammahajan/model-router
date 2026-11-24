package provider_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/protocol"
	"github.com/arhammahajan/router/internal/provider"
)

func TestSpecSupportsAndResolveDialect(t *testing.T) {
	// Empty SupportedDialects falls back to DefaultDialect.
	single := &provider.Spec{
		Name:           "anth",
		DefaultDialect: provider.DialectAnthropic,
	}
	if !single.SupportsDialect(provider.DialectAnthropic) {
		t.Fatal("single-dialect spec must support DefaultDialect")
	}
	if single.SupportsDialect(provider.DialectOpenAIChat) {
		t.Fatal("single-dialect spec must not support other dialects")
	}
	if got := single.ResolveDialect(provider.DialectOpenAIChat, ""); got != provider.DialectAnthropic {
		t.Fatalf("ResolveDialect fallback = %q, want %q", got, provider.DialectAnthropic)
	}
	if got := single.ResolveDialect(provider.DialectOpenAIChat, provider.DialectGemini); got != provider.DialectGemini {
		t.Fatalf("modelOverride must win, got %q", got)
	}

	// Multi-dialect spec passes through supported ingress dialects.
	multi := &provider.Spec{
		Name:              "cliproxyapi",
		DefaultDialect:    provider.DialectOpenAIChat,
		SupportedDialects: []string{provider.DialectOpenAIChat, provider.DialectOpenAIResponses, provider.DialectAnthropic},
	}
	if got := multi.ResolveDialect(provider.DialectAnthropic, ""); got != provider.DialectAnthropic {
		t.Fatalf("supported ingress must pass through, got %q", got)
	}
	if got := multi.ResolveDialect(provider.DialectGemini, ""); got != provider.DialectOpenAIChat {
		t.Fatalf("unsupported ingress must fall back to DefaultDialect, got %q", got)
	}
	if got := multi.ResolveDialect(provider.DialectAnthropic, provider.DialectOpenAIResponses); got != provider.DialectOpenAIResponses {
		t.Fatalf("modelOverride must beat supported ingress, got %q", got)
	}
}

func TestDefaultAuthHeaders(t *testing.T) {
	// Multi-dialect without explicit AuthHeader selects per destDialect.
	multi := &provider.Spec{
		Name:              "cliproxyapi",
		DefaultDialect:    provider.DialectOpenAIChat,
		SupportedDialects: []string{provider.DialectOpenAIChat, provider.DialectOpenAIResponses, provider.DialectAnthropic},
		APIKey:            "secret-k",
	}
	if got := provider.DefaultAuthHeaders(multi, provider.DialectOpenAIChat); got["Authorization"] != "Bearer secret-k" {
		t.Fatalf("chat auth = %+v", got)
	}
	if got := provider.DefaultAuthHeaders(multi, provider.DialectAnthropic); got["x-api-key"] != "secret-k" {
		t.Fatalf("anthropic auth = %+v", got)
	}
	// Unsupported destDialect falls back to DefaultDialect.
	if got := provider.DefaultAuthHeaders(multi, provider.DialectGemini); got["Authorization"] != "Bearer secret-k" {
		t.Fatalf("unsupported destDialect must fall back to DefaultDialect: %+v", got)
	}

	// Explicit AuthHeader always wins regardless of destDialect.
	pinned := &provider.Spec{
		Name:              "cliproxyapi",
		DefaultDialect:    provider.DialectOpenAIChat,
		SupportedDialects: []string{provider.DialectOpenAIChat, provider.DialectAnthropic},
		APIKey:            "secret-k",
		AuthHeader:        "bearer",
	}
	if got := provider.DefaultAuthHeaders(pinned, provider.DialectAnthropic); got["Authorization"] != "Bearer secret-k" {
		t.Fatalf("explicit bearer must win: %+v", got)
	}

	// Gemini default without explicit AuthHeader uses x-goog-api-key.
	gem := &provider.Spec{
		Name:           "gem",
		DefaultDialect: provider.DialectGemini,
		APIKey:         "gem-k",
	}
	if got := provider.DefaultAuthHeaders(gem, ""); got["x-goog-api-key"] != "gem-k" {
		t.Fatalf("gemini auth = %+v", got)
	}

	// Blank APIKey uses UnauthenticatedUpstreamKey.
	unauth := &provider.Spec{Name: "local", DefaultDialect: provider.DialectOpenAIChat}
	if got := provider.DefaultAuthHeaders(unauth, ""); got["Authorization"] != "Bearer "+provider.UnauthenticatedUpstreamKey {
		t.Fatalf("unauth header = %+v", got)
	}
}

func TestForKindAndForConfigName(t *testing.T) {
	if got := provider.ForConfigName("cliproxyapi").Kind(); got != provider.KindCLIProxyAPI {
		t.Fatalf("ForConfigName(cliproxyapi) = %q", got)
	}
	if got := provider.ForConfigName("llama_cpp").Kind(); got != provider.KindLlamaCpp {
		t.Fatalf("ForConfigName(llama_cpp) = %q", got)
	}
	if got := provider.ForConfigName("strata").Kind(); got != provider.KindStrata {
		t.Fatalf("ForConfigName(strata) = %q", got)
	}
	if got := provider.ForConfigName("openai").Kind(); got != provider.KindGeneric {
		t.Fatalf("ForConfigName(openai) = %q", got)
	}
	if got := provider.ForKind(provider.KindUnobserved).Kind(); got != provider.KindUnobserved {
		t.Fatalf("ForKind(unobserved) = %q", got)
	}
	if !provider.ForKind(provider.KindUnobserved).RefusesUnobservedReasoning() {
		t.Fatal("unobserved must refuse unobserved reasoning")
	}
	if provider.ForKind(provider.KindLlamaCpp).RefusesUnobservedReasoning() {
		t.Fatal("llama_cpp must not refuse unobserved reasoning")
	}
}

func TestLlamaCppAndStrataPluginParseAndHooks(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }

	// llama_cpp base_url shorthand + /v1 stripping.
	raw := map[string]any{
		"base_url":      "http://127.0.0.1:9931/v1/",
		"timeout":       int64(120),
		"probe_timeout": 3.5,
	}
	common, err := provider.ParseCommonConfig("llama_cpp", raw, noEnv)
	if err != nil {
		t.Fatalf("ParseCommonConfig: %v", err)
	}
	res, err := provider.ForConfigName("llama_cpp").ParseConfig(common, raw, "", noEnv)
	if err != nil {
		t.Fatalf("llama_cpp ParseConfig: %v", err)
	}
	if res.Provider != nil {
		t.Fatal("llama_cpp must not produce a static Provider")
	}
	if len(res.LocalServers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(res.LocalServers))
	}
	srv := res.LocalServers[0]
	if srv.Kind != provider.KindLlamaCpp || srv.URL != "http://127.0.0.1:9931" || srv.BaseURL != "http://127.0.0.1:9931/v1" {
		t.Fatalf("server URLs/kind wrong: %+v", srv)
	}
	if srv.Prefix != "" || !srv.ExposeAliases || srv.Timeout != 120 || srv.ProbeTimeout != 3.5 {
		t.Fatalf("server defaults/timeouts wrong: %+v", srv)
	}
	if !strings.HasPrefix(srv.Name, "llama-") {
		t.Fatalf("generated name must have llama- prefix: %q", srv.Name)
	}

	// base_url mutually exclusive with servers/ports.
	badRaw := map[string]any{"base_url": "http://127.0.0.1:9931", "ports": []any{int64(8080)}}
	badCommon, err := provider.ParseCommonConfig("llama_cpp", badRaw, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ForConfigName("llama_cpp").ParseConfig(badCommon, badRaw, "", noEnv); err == nil {
		t.Fatal("base_url + ports must fail")
	}

	// strata defaults: prefix="", aliases=true, strata- generated name, no LLAMA_SERVER_URL fallback.
	withEnv := func(k string) (string, bool) {
		if k == "LLAMA_SERVER_URL" {
			return "http://127.0.0.1:7777", true
		}
		return "", false
	}
	sCommon, err := provider.ParseCommonConfig("strata", map[string]any{}, withEnv)
	if err != nil {
		t.Fatal(err)
	}
	sRes, err := provider.ForConfigName("strata").ParseConfig(sCommon, map[string]any{}, "", withEnv)
	if err != nil {
		t.Fatalf("strata ParseConfig: %v", err)
	}
	if len(sRes.LocalServers) != 1 {
		t.Fatalf("strata servers = %+v", sRes.LocalServers)
	}
	ss := sRes.LocalServers[0]
	if ss.Kind != provider.KindStrata || ss.URL != provider.DefaultLlamaURL || ss.Prefix != "" || !ss.ExposeAliases || !strings.HasPrefix(ss.Name, "strata-") {
		t.Fatalf("strata server wrong: %+v", ss)
	}

	// PrepareNativeBody: llama_cpp transforms openai-chat only, NOT anthropic; strata is no-op.
	chatBody := []byte(`{"model":"m","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
	outChat, changed := provider.ForKind(provider.KindLlamaCpp).PrepareNativeBody(provider.DialectOpenAIChat, chatBody)
	if !changed || !strings.Contains(string(outChat), "thinking_budget_tokens") {
		t.Fatalf("llama_cpp openai-chat PrepareNativeBody expected transform: changed=%v out=%s", changed, outChat)
	}
	outAnth, changedAnth := provider.ForKind(provider.KindLlamaCpp).PrepareNativeBody(provider.DialectAnthropic, chatBody)
	if changedAnth || string(outAnth) != string(chatBody) {
		t.Fatalf("llama_cpp anthropic PrepareNativeBody must be no-op: changed=%v", changedAnth)
	}
	_, changedStrata := provider.ForKind(provider.KindStrata).PrepareNativeBody(provider.DialectOpenAIChat, chatBody)
	if changedStrata {
		t.Fatal("strata PrepareNativeBody must be no-op")
	}

	// EffortScope & ApplyCapabilities.
	if sc := provider.ForKind(provider.KindLlamaCpp).EffortScope(provider.DialectOpenAIChat); sc != protocol.LocalRoute(capability.RuntimeLlamaCpp) {
		t.Fatalf("llama_cpp EffortScope = %+v", sc)
	}
	if sc := provider.ForKind(provider.KindStrata).EffortScope(provider.DialectOpenAIChat); sc != protocol.LocalRoute(capability.RuntimeStrata) {
		t.Fatalf("strata EffortScope = %+v", sc)
	}
	if sc := provider.ForKind(provider.KindUnobserved).EffortScope(provider.DialectOpenAIChat); sc != protocol.LocalRoute(capability.RuntimeUnobserved) {
		t.Fatalf("unobserved EffortScope = %+v", sc)
	}
	if sc := provider.ForKind(provider.KindGeneric).EffortScope(provider.DialectOpenAIChat); sc != protocol.OrdinaryRoute() {
		t.Fatalf("generic EffortScope = %+v", sc)
	}

	cLlama := capability.Empty(provider.DialectOpenAIChat)
	provider.ForKind(provider.KindLlamaCpp).ApplyCapabilities(&cLlama, 1000)
	if v, ok := cLlama.Get("reasoning.effort.supported"); !ok || v.IsNull() {
		t.Fatalf("llama_cpp ApplyCapabilities did not set reasoning.effort.supported")
	}
	cStrata := capability.Empty(provider.DialectOpenAIChat)
	provider.ForKind(provider.KindStrata).ApplyCapabilities(&cStrata, 1000)
	if v, ok := cStrata.Get("reasoning.toggle.supported"); !ok || v.IsNull() {
		t.Fatalf("strata ApplyCapabilities did not set reasoning.toggle.supported")
	}
	cUnobs := capability.Empty(provider.DialectOpenAIChat)
	provider.ForKind(provider.KindUnobserved).ApplyCapabilities(&cUnobs, 1000)
	if v, ok := cUnobs.Get("reasoning.effort.supported"); ok && !v.IsNull() {
		t.Fatalf("unobserved ApplyCapabilities must be no-op")
	}
}

func TestCLIProxyAPIPluginParse(t *testing.T) {
	raw := map[string]any{
		"passthrough":     true,
		"refresh_seconds": int64(45),
		"probe_timeout":   int64(15),
		"config_path":     " /etc/cliproxy.yaml ",
	}
	common, err := provider.ParseCommonConfig("cliproxyapi", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := provider.ForConfigName("cliproxyapi").ParseConfig(common, raw, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.CLIProxy == nil || res.Provider == nil {
		t.Fatalf("expected both CLIProxy and Provider: %+v", res)
	}
	if res.Provider.AuthHeader != "" {
		t.Fatalf("AuthHeader must stay empty, got %q", res.Provider.AuthHeader)
	}
	wantDialects := []string{provider.DialectOpenAIChat, provider.DialectOpenAIResponses, provider.DialectAnthropic}
	if !reflect.DeepEqual(res.Provider.SupportedDialects, wantDialects) {
		t.Fatalf("SupportedDialects = %v, want %v", res.Provider.SupportedDialects, wantDialects)
	}
	if res.CLIProxy.ConfigPath != "/etc/cliproxy.yaml" || res.CLIProxy.RefreshSeconds != 45 || res.CLIProxy.ProbeTimeout != 15 {
		t.Fatalf("CLIProxy fields wrong: %+v", res.CLIProxy)
	}
}

func TestResolveCredentialPrecedence(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(keyFile, []byte(" from-file \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == "K_ENV" {
			return "from-env", true
		}
		return "", false
	}
	if got, err := provider.ResolveCredential("p", map[string]any{"api_key": "direct", "api_key_env": "K_ENV", "api_key_file": keyFile}, env); err != nil || got != "direct" {
		t.Fatalf("api_key must win: got=%q err=%v", got, err)
	}
	if got, err := provider.ResolveCredential("p", map[string]any{"api_key_env": "K_ENV", "api_key_file": keyFile}, env); err != nil || got != "from-env" {
		t.Fatalf("api_key_env must win over file: got=%q err=%v", got, err)
	}
	if got, err := provider.ResolveCredential("p", map[string]any{"api_key_file": keyFile}, env); err != nil || got != "from-file" {
		t.Fatalf("api_key_file failed: got=%q err=%v", got, err)
	}
}
