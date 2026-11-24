package provider

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/endpoint"
	"github.com/arhammahajan/router/internal/protocol"
)

// GenericPlugin handles standard [providers.<name>] tables.
type GenericPlugin struct{}

var genericAllowedKeys = map[string]bool{
	"dialect": true, "base_url": true, "api_key": true, "api_key_env": true,
	"api_key_file": true, "headers": true, "timeout": true, "compatibility": true,
	"drop_params": true, "auth_header": true, "count_tokens": true,
	"litellm_provider": true, "litellm_kwargs": true,
	"session_header": true, "session_source_headers": true,
	"session_prompt_cache_key": true, "session_missing": true,
}

var nativeSessionSources = map[string]bool{
	"x-claude-code-session-id": true, "session-id": true, "session_id": true,
	"thread-id": true, "conversation_id": true, "x-session-id": true,
	"x-mavis-session-id": true,
}

func (*GenericPlugin) Kind() string { return KindGeneric }

func (*GenericPlugin) ParseConfig(common CommonConfig, raw map[string]any, _ string, _ func(string) (string, bool)) (*ParseResult, error) {
	where := "providers." + common.Name
	if err := RejectUnknown(raw, genericAllowedKeys, where,
		" (Supported litellm_kwargs are headers/extra_headers and equivalent api_base/timeout aliases; "+
			"use compatibility/timeout/headers/auth_header for other supported controls)"); err != nil {
		return nil, err
	}
	v, ok := raw["dialect"]
	if !ok {
		return nil, fmt.Errorf("%s.dialect is required", where)
	}
	dialect, ok := AsString(v)
	if !ok {
		return nil, fmt.Errorf("%s: 'dialect' must be one of openai-chat, openai-responses, anthropic, gemini", where)
	}
	switch dialect {
	case DialectOpenAIChat, DialectOpenAIResponses, DialectAnthropic, DialectGemini:
	default:
		return nil, fmt.Errorf("%s: 'dialect' must be one of openai-chat, openai-responses, anthropic, gemini", where)
	}
	if !common.HasBaseURL {
		return nil, fmt.Errorf("%s.base_url is required", where)
	}

	p := &Spec{
		Name:              common.Name,
		Kind:              KindGeneric,
		DefaultDialect:    dialect,
		SupportedDialects: []string{dialect},
		BaseURL:           common.BaseURL,
		APIKey:            common.APIKey,
		AuthHeader:        common.AuthHeader,
		Headers:           CopyHeaders(common.Headers),
		Timeout:           common.Timeout,
		CompatibilityRaw:  common.CompatibilityRaw,
	}
	if common.Lossy != nil {
		p.Lossy = *common.Lossy
	}

	dropPresent := false
	var drop bool
	if v, ok := raw["drop_params"]; ok {
		b, ok := AsBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'drop_params' must be boolean", where)
		}
		dropPresent, drop = true, b
	}
	if common.CompatibilityRaw != "" {
		if dropPresent && drop != p.Lossy {
			return nil, fmt.Errorf("%s: 'drop_params' must be boolean and agree with 'compatibility'", where)
		}
	} else if drop {
		// Legacy LiteLLM-era spelling selects the diagnosed lossy policy.
		p.CompatibilityRaw = "lossy"
		p.Lossy = true
	}

	if v, ok := raw["count_tokens"]; ok {
		b, ok := AsBool(v)
		if !ok {
			return nil, fmt.Errorf("%s: 'count_tokens' must be boolean", where)
		}
		if b && p.DefaultDialect != DialectAnthropic {
			return nil, fmt.Errorf("%s: 'count_tokens' can only be enabled for the anthropic dialect", where)
		}
		p.CountTokens = &b
	}
	if err := fillLitellmProvider(p, raw, where); err != nil {
		return nil, err
	}
	if err := fillLitellmKwargs(p, raw, where); err != nil {
		return nil, err
	}
	if err := fillSessionPolicy(p, raw, where); err != nil {
		return nil, err
	}
	return &ParseResult{Provider: p}, nil
}

func (*GenericPlugin) AuthHeaders(p *Spec, destDialect string) map[string]string {
	return DefaultAuthHeaders(p, destDialect)
}

func (*GenericPlugin) GenerationEndpoint(baseURL, destDialect string) (string, error) {
	return endpoint.GenerationEndpoint(baseURL, destDialect)
}

func (*GenericPlugin) PrepareNativeBody(_ string, body []byte) ([]byte, bool) {
	return body, false
}

func (*GenericPlugin) EffortScope(destDialect string) protocol.RouteScope {
	switch destDialect {
	case DialectOpenAIResponses:
		return protocol.OrdinaryResponsesRoute()
	case DialectGemini:
		return protocol.OrdinaryGeminiRoute()
	case DialectAnthropic:
		return protocol.CompatAnthropicRoute()
	default:
		return protocol.OrdinaryRoute()
	}
}

func (*GenericPlugin) RefusesUnobservedReasoning() bool { return false }

func (*GenericPlugin) ApplyCapabilities(_ *capability.Contract, _ float64) {}

// --- generic provider helpers ---

func defaultLitellmProvider(dialect string) string {
	switch dialect {
	case DialectAnthropic:
		return "anthropic"
	case DialectGemini:
		return "gemini"
	default:
		return "openai"
	}
}

func fillLitellmProvider(p *Spec, t map[string]any, where string) error {
	want := defaultLitellmProvider(p.DefaultDialect)
	v, ok := t["litellm_provider"]
	if !ok {
		p.LitellmProvider = want
		return nil
	}
	s, ok := AsString(v)
	if !ok || s != want {
		return fmt.Errorf("%s.litellm_provider: unsupported in this build; use a native route instead", where)
	}
	p.LitellmProvider = s
	return nil
}

var litellmKwargsAllowed = map[string]bool{"headers": true, "extra_headers": true, "api_base": true, "timeout": true}

func litellmKwargsHint(where, key string) string {
	path := where + ".litellm_kwargs." + key
	switch key {
	case "api_key", "api_key_env", "api_key_file":
		return fmt.Sprintf("%s: unsupported in this build; use api_key/api_key_env/api_key_file instead", path)
	case "custom_llm_provider":
		return fmt.Sprintf("%s: unsupported in this build; use a native route instead", path)
	case "num_retries", "max_retries", "retry_policy", "fallbacks", "context_window_fallback_dict":
		return fmt.Sprintf("%s: unsupported in this build; use [retry] instead", path)
	case "additional_drop_params", "drop_params":
		return fmt.Sprintf("%s: unsupported in this build; use compatibility instead", path)
	case "_skip_mcp_handler":
		return fmt.Sprintf("%s: unsupported in this build; router never executes tools", path)
	default:
		return fmt.Sprintf("%s: unsupported in this build (supported: headers, extra_headers, equivalent api_base/timeout aliases)", path)
	}
}

func fillLitellmKwargs(p *Spec, t map[string]any, where string) error {
	v, ok := t["litellm_kwargs"]
	if !ok || v == nil {
		return nil
	}
	tab, ok := AsTable(v)
	if !ok {
		return fmt.Errorf("%s.litellm_kwargs: must be a table", where)
	}
	keys := make([]string, 0, len(tab))
	for k := range tab {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !litellmKwargsAllowed[k] {
			return fmt.Errorf("%s", litellmKwargsHint(where, k))
		}
	}
	if raw, ok := tab["api_base"]; ok {
		if err := checkLegacyAPIBase(p, raw, where); err != nil {
			return err
		}
	}
	if raw, ok := tab["timeout"]; ok {
		if err := checkLegacyTimeout(p, raw, where); err != nil {
			return err
		}
	}
	var kh, keh map[string]string
	if raw, ok := tab["headers"]; ok {
		m, err := CheckHeaders(raw, where+".litellm_kwargs.headers", "")
		if err != nil {
			return err
		}
		kh = m
	}
	if raw, ok := tab["extra_headers"]; ok {
		m, err := CheckHeaders(raw, where+".litellm_kwargs.extra_headers", "")
		if err != nil {
			return err
		}
		keh = m
	}
	merged := MergeLegacyHeaders(kh, keh)
	if len(merged) > 0 {
		p.TranslatedHeaders = merged
	}
	return nil
}

func checkLegacyAPIBase(p *Spec, raw any, where string) error {
	keyPath := where + ".litellm_kwargs.api_base"
	s, ok := AsString(raw)
	if !ok || strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s: must be a non-empty string; use base_url instead", keyPath)
	}
	normalized, err := NormalizeBaseURL(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("%s: must be a bare http(s) origin/path without userinfo, query or fragment; use base_url instead", keyPath)
	}
	want, err1 := endpoint.GenerationEndpoint(p.BaseURL, p.DefaultDialect)
	got, err2 := endpoint.GenerationEndpoint(normalized, p.DefaultDialect)
	if err1 != nil || err2 != nil || want != got {
		return fmt.Errorf("%s: unsupported in this build; use base_url instead", keyPath)
	}
	return nil
}

func checkLegacyTimeout(p *Spec, raw any, where string) error {
	keyPath := where + ".litellm_kwargs.timeout"
	n, ok := AsNum(raw)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return fmt.Errorf("%s: must be a finite number; use timeout instead", keyPath)
	}
	if n <= 0 || n > MaxTimeoutSeconds {
		return fmt.Errorf("%s: must be a positive number <= %v; use timeout instead", keyPath, MaxTimeoutSeconds)
	}
	if n != p.Timeout {
		return fmt.Errorf("%s: unsupported in this build; use timeout instead", keyPath)
	}
	return nil
}

func fillSessionPolicy(p *Spec, t map[string]any, where string) error {
	var sessionHeader string
	if v, ok := t["session_header"]; ok {
		s, ok := AsString(v)
		if !ok {
			return fmt.Errorf("%s: 'session_header' must be a string", where)
		}
		sessionHeader = strings.ToLower(s)
		if sessionHeader != "" && (!httpToken.MatchString(sessionHeader) || isDropHeader(sessionHeader)) {
			return fmt.Errorf("%s: 'session_header' must be a dedicated identity header, "+
				"separate from credentials, protocol and transport headers", where)
		}
		for name := range p.Headers {
			if IsReservedSessionHeader(name, sessionHeader) {
				return fmt.Errorf("%s: static session headers are forbidden; supply a conversation identity on each request", where)
			}
		}
		for name := range p.TranslatedHeaders {
			if IsReservedSessionHeader(name, sessionHeader) {
				return fmt.Errorf("%s: static session headers are forbidden; supply a conversation identity on each request", where)
			}
		}
	}
	if v, ok := t["session_source_headers"]; ok {
		arr, ok := AsArray(v)
		if !ok {
			return fmt.Errorf("%s: 'session_source_headers' must be an array of reviewed header names", where)
		}
		p.SessionSourceHeaders = []string{}
		seen := map[string]bool{}
		for _, e := range arr {
			s, ok := AsString(e)
			if !ok || !nativeSessionSources[strings.ToLower(s)] {
				return fmt.Errorf("%s: 'session_source_headers' must be an array of reviewed header names", where)
			}
			lower := strings.ToLower(s)
			if seen[lower] {
				return fmt.Errorf("%s: 'session_source_headers' contains duplicate headers", where)
			}
			seen[lower] = true
			p.SessionSourceHeaders = append(p.SessionSourceHeaders, lower)
		}
	}
	if v, ok := t["session_prompt_cache_key"]; ok {
		b, ok := AsBool(v)
		if !ok {
			return fmt.Errorf("%s: 'session_prompt_cache_key' must be boolean", where)
		}
		p.SessionPromptCacheKey = b
	}
	if v, ok := t["session_missing"]; ok {
		s, ok := AsString(v)
		if !ok || (s != "omit" && s != "reject") {
			return fmt.Errorf("%s: 'session_missing' must be \"omit\" or \"reject\"", where)
		}
		p.SessionMissing = s
	} else {
		p.SessionMissing = "omit"
	}
	p.SessionHeader = sessionHeader
	_, hasSources := t["session_source_headers"]
	_, hasCacheKey := t["session_prompt_cache_key"]
	_, hasMissing := t["session_missing"]
	if sessionHeader == "" && (hasSources || hasCacheKey || hasMissing) {
		return fmt.Errorf("%s: session policy requires 'session_header'", where)
	}
	if sessionHeader != "" && !hasSources {
		p.SessionSourceHeaders = append(p.SessionSourceHeaders,
			"x-claude-code-session-id", "session-id", "session_id", "thread-id",
			"conversation_id", "x-session-id", "x-mavis-session-id")
	}
	return nil
}

func isDropHeader(lower string) bool {
	switch lower {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade",
		"host", "content-length", "authorization", "x-api-key", "api-key",
		"x-goog-api-key", "x-model-router-reasoning-effort",
		"x-goog-user-project", "x-goog-quota-user", "cookie", "user-agent",
		"content-type", "content-encoding", "content-md5", "content-digest",
		"repr-digest", "digest", "content-range", "content-location",
		"content-language", "etag", "last-modified", "signature", "signature-input",
		"accept", "accept-encoding", "anthropic-beta", "anthropic-version", "openai-beta":
		return true
	}
	return false
}
