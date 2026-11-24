package transport

import (
	"net/http"
	"testing"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/provider"
)

func TestAuthHeadersNilProvider(t *testing.T) {
	h := AuthHeaders(nil, config.DialectOpenAIChat)
	if h == nil || len(h) != 0 {
		t.Fatalf("AuthHeaders(nil, ...) = %v, want empty non-nil map", h)
	}
}

func TestAuthHeadersGenericProviderAndModelOverride(t *testing.T) {
	cases := []struct {
		name           string
		defaultDialect string
		destDialect    string
		authHeader     string
		wantKey        string
		wantVal        string
	}{
		{
			name:           "openai-chat default",
			defaultDialect: config.DialectOpenAIChat,
			destDialect:    config.DialectOpenAIChat,
			wantKey:        "Authorization",
			wantVal:        "Bearer k",
		},
		{
			name:           "openai-chat with anthropic model override stays bearer",
			defaultDialect: config.DialectOpenAIChat,
			destDialect:    config.DialectAnthropic,
			wantKey:        "Authorization",
			wantVal:        "Bearer k",
		},
		{
			name:           "anthropic with openai-chat model override stays x-api-key",
			defaultDialect: config.DialectAnthropic,
			destDialect:    config.DialectOpenAIChat,
			wantKey:        "x-api-key",
			wantVal:        "k",
		},
		{
			name:           "gemini with anthropic model override stays x-goog-api-key",
			defaultDialect: config.DialectGemini,
			destDialect:    config.DialectAnthropic,
			wantKey:        "x-goog-api-key",
			wantVal:        "k",
		},
		{
			name:           "explicit auth_header always wins",
			defaultDialect: config.DialectAnthropic,
			destDialect:    config.DialectAnthropic,
			authHeader:     "bearer",
			wantKey:        "Authorization",
			wantVal:        "Bearer k",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &config.Provider{
				Name:              "gen",
				Kind:              provider.KindGeneric,
				DefaultDialect:    tc.defaultDialect,
				SupportedDialects: []string{tc.defaultDialect},
				BaseURL:           "https://gen.example",
				APIKey:            "k",
				AuthHeader:        tc.authHeader,
			}
			got := AuthHeaders(p, tc.destDialect)
			if len(got) != 1 || got[tc.wantKey] != tc.wantVal {
				t.Fatalf("AuthHeaders(%q -> %q) = %v, want %s=%q",
					tc.defaultDialect, tc.destDialect, got, tc.wantKey, tc.wantVal)
			}
		})
	}
}

func TestAuthHeadersCLIProxyAPIMultiDialect(t *testing.T) {
	supported := []string{
		config.DialectOpenAIChat,
		config.DialectOpenAIResponses,
		config.DialectAnthropic,
	}

	p := &config.Provider{
		Name:              provider.CLIProxyAPIProviderName,
		Kind:              provider.KindCLIProxyAPI,
		DefaultDialect:    config.DialectOpenAIChat,
		SupportedDialects: supported,
		BaseURL:           "http://127.0.0.1:8317/v1",
		APIKey:            "cli-secret",
	}

	for _, dest := range []string{config.DialectOpenAIChat, config.DialectOpenAIResponses} {
		got := AuthHeaders(p, dest)
		if len(got) != 1 || got["Authorization"] != "Bearer cli-secret" {
			t.Fatalf("cliproxyapi dest=%s: got %v, want Authorization=Bearer cli-secret", dest, got)
		}
		out := OutboundHeaders(http.Header{}, p, "chat", dest, "", false)
		if out["Authorization"] != "Bearer cli-secret" {
			t.Fatalf("cliproxyapi OutboundHeaders dest=%s: got %v", dest, out)
		}
		if _, ok := out["X-Api-Key"]; ok {
			t.Fatalf("cliproxyapi OutboundHeaders dest=%s must not set X-Api-Key: %v", dest, out)
		}
	}

	got := AuthHeaders(p, config.DialectAnthropic)
	if len(got) != 1 || got["x-api-key"] != "cli-secret" {
		t.Fatalf("cliproxyapi dest=anthropic: got %v, want x-api-key=cli-secret", got)
	}
	out := OutboundHeaders(http.Header{}, p, "messages", config.DialectAnthropic, "", false)
	if out["X-Api-Key"] != "cli-secret" {
		t.Fatalf("cliproxyapi OutboundHeaders dest=anthropic: got %v", out)
	}
	if _, ok := out["Authorization"]; ok {
		t.Fatalf("cliproxyapi OutboundHeaders dest=anthropic must not set Authorization: %v", out)
	}

	// Explicit AuthHeader="bearer" override stays Bearer even on anthropic.
	pBearer := &config.Provider{
		Name:              provider.CLIProxyAPIProviderName,
		Kind:              provider.KindCLIProxyAPI,
		DefaultDialect:    config.DialectOpenAIChat,
		SupportedDialects: supported,
		BaseURL:           "http://127.0.0.1:8317/v1",
		APIKey:            "cli-secret",
		AuthHeader:        "bearer",
	}
	gotOverride := AuthHeaders(pBearer, config.DialectAnthropic)
	if len(gotOverride) != 1 || gotOverride["Authorization"] != "Bearer cli-secret" {
		t.Fatalf("cliproxyapi bearer override dest=anthropic: got %v, want Authorization=Bearer cli-secret", gotOverride)
	}
	outOverride := OutboundHeaders(http.Header{}, pBearer, "messages", config.DialectAnthropic, "", false)
	if outOverride["Authorization"] != "Bearer cli-secret" {
		t.Fatalf("cliproxyapi OutboundHeaders bearer override dest=anthropic: got %v", outOverride)
	}
	if _, ok := outOverride["X-Api-Key"]; ok {
		t.Fatalf("cliproxyapi OutboundHeaders bearer override must not set X-Api-Key: %v", outOverride)
	}
}

func TestAuthHeadersLlamaCppAndStrataMultiDialect(t *testing.T) {
	supported := []string{config.DialectOpenAIChat, config.DialectAnthropic}
	for _, kind := range []string{provider.KindLlamaCpp, provider.KindStrata} {
		t.Run(kind, func(t *testing.T) {
			p := &config.Provider{
				Name:              kind + "-local",
				Kind:              kind,
				DefaultDialect:    config.DialectOpenAIChat,
				SupportedDialects: supported,
				BaseURL:           "http://127.0.0.1:9931/v1",
				APIKey:            "local-key",
			}

			gotChat := AuthHeaders(p, config.DialectOpenAIChat)
			if len(gotChat) != 1 || gotChat["Authorization"] != "Bearer local-key" {
				t.Fatalf("%s dest=openai-chat: got %v, want Authorization=Bearer local-key", kind, gotChat)
			}

			gotAnth := AuthHeaders(p, config.DialectAnthropic)
			if len(gotAnth) != 1 || gotAnth["x-api-key"] != "local-key" {
				t.Fatalf("%s dest=anthropic: got %v, want x-api-key=local-key", kind, gotAnth)
			}

			outAnth := OutboundHeaders(http.Header{}, p, "messages", config.DialectAnthropic, "", false)
			if outAnth["X-Api-Key"] != "local-key" {
				t.Fatalf("%s OutboundHeaders dest=anthropic: got %v", kind, outAnth)
			}
			if _, ok := outAnth["Authorization"]; ok {
				t.Fatalf("%s OutboundHeaders dest=anthropic must not set Authorization: %v", kind, outAnth)
			}

			// Explicit AuthHeader="bearer" wins on anthropic too.
			p.AuthHeader = "bearer"
			gotOverride := AuthHeaders(p, config.DialectAnthropic)
			if len(gotOverride) != 1 || gotOverride["Authorization"] != "Bearer local-key" {
				t.Fatalf("%s bearer override dest=anthropic: got %v", kind, gotOverride)
			}
		})
	}
}

func TestCountTokensDisabledForChatDefaultMultiDialect(t *testing.T) {
	tr := true
	for _, kind := range []string{provider.KindCLIProxyAPI, provider.KindLlamaCpp, provider.KindStrata} {
		p := &config.Provider{
			Name:              kind,
			Kind:              kind,
			DefaultDialect:    config.DialectOpenAIChat,
			SupportedDialects: []string{config.DialectOpenAIChat, config.DialectAnthropic},
			BaseURL:           "https://api.anthropic.com",
			CountTokens:       &tr,
		}
		if CountTokensEnabled(p) {
			t.Fatalf("%s chat-default provider must not enable count_tokens even when anthropic is in SupportedDialects", kind)
		}
	}
}

func TestGenerationEndpointDelegatesToProviderPlugin(t *testing.T) {
	if _, rerr := GenerationEndpoint(nil, config.DialectOpenAIChat); rerr == nil || rerr.Status != 500 {
		t.Fatalf("nil provider must return RouterError 500, got %v", rerr)
	}
	for _, kind := range []string{provider.KindGeneric, provider.KindCLIProxyAPI, provider.KindLlamaCpp, provider.KindStrata} {
		p := &config.Provider{
			Name:           kind,
			Kind:           kind,
			DefaultDialect: config.DialectOpenAIChat,
			BaseURL:        "http://127.0.0.1:8080/v1",
		}
		gotDefault, rerr := GenerationEndpoint(p, "")
		if rerr != nil || gotDefault != "http://127.0.0.1:8080/v1/chat/completions" {
			t.Fatalf("%s default endpoint = %q err=%v", kind, gotDefault, rerr)
		}
		gotAnth, rerr := GenerationEndpoint(p, config.DialectAnthropic)
		if rerr != nil || gotAnth != "http://127.0.0.1:8080/v1/messages" {
			t.Fatalf("%s anthropic endpoint = %q err=%v", kind, gotAnth, rerr)
		}
		if _, rerr := GenerationEndpoint(p, "bogus"); rerr == nil || rerr.Status != 500 {
			t.Fatalf("%s bogus dialect must return RouterError 500, got %v", kind, rerr)
		}
	}
}
