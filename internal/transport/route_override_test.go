package transport

import (
	"net/http"
	"testing"

	"github.com/arhammahajan/router/internal/config"
)

func overrideProvider(dialect string) *config.Provider {
	return &config.Provider{
		Name: "up", Dialect: dialect, BaseURL: "https://up.example",
		APIKey: "k", Headers: map[string]string{}, Timeout: 5,
		TranslatedHeaders: map[string]string{"X-Legacy": "legacy"},
	}
}

func TestOverrideNativeVsBridgeClassification(t *testing.T) {
	p := overrideProvider(config.DialectOpenAIChat)
	in := http.Header{}
	in.Set("X-Custom", "keep")
	in.Set("Anthropic-Beta", "opt")
	// Native: chat ingress + chat dest preserves unknown headers, no legacy.
	native := OutboundHeaders(in, p, "chat", config.DialectOpenAIChat, "", false)
	if native["X-Custom"] != "keep" {
		t.Fatalf("native must preserve unknown header: %v", native)
	}
	if _, ok := native["X-Legacy"]; ok {
		t.Fatalf("native must not carry legacy: %v", native)
	}
	// Bridged: same ingress, anthropic dest -> only opt-ins, legacy on.
	bridged := OutboundHeaders(in, p, "chat", config.DialectAnthropic, "", false)
	if _, ok := bridged["X-Custom"]; ok {
		t.Fatalf("bridged must drop unknown: %v", bridged)
	}
	if bridged["Anthropic-Beta"] != "opt" {
		t.Fatalf("dest opt-in must survive: %v", bridged)
	}
	if bridged["X-Legacy"] != "legacy" {
		t.Fatalf("translated must carry legacy: %v", bridged)
	}
}

func TestOverrideAuthPreservedWithoutExplicitHeader(t *testing.T) {
	// Provider defaults must stay even when dest differs; no silent adapt.
	cases := []struct {
		dialect string
		dest    string
		wantKey string
		wantVal string
	}{
		{config.DialectAnthropic, config.DialectOpenAIChat, "X-Api-Key", "k"},
		{config.DialectGemini, config.DialectAnthropic, "X-Goog-Api-Key", "k"},
		{config.DialectOpenAIChat, config.DialectAnthropic, "Authorization", "Bearer k"},
	}
	for _, tc := range cases {
		p := overrideProvider(tc.dialect)
		out := OutboundHeaders(http.Header{}, p, "chat", tc.dest, "", false)
		if out[tc.wantKey] != tc.wantVal {
			t.Fatalf("%s->%s auth = %v, want %s=%s", tc.dialect, tc.dest, out, tc.wantKey, tc.wantVal)
		}
		n := 0
		for name := range out {
			switch name {
			case "X-Api-Key", "Authorization", "Api-Key", "X-Goog-Api-Key":
				n++
			}
		}
		if n != 1 {
			t.Fatalf("exactly one credential required: %v", out)
		}
	}
}

func TestOverrideOptinsAndVersionFromDest(t *testing.T) {
	p := overrideProvider(config.DialectOpenAIChat)
	in := http.Header{}
	in.Set("Openai-Beta", "chat-opt")
	in.Set("Anthropic-Beta", "anthropic-opt")
	// Dest chat: only openai-beta, no anthropic-version.
	out := OutboundHeaders(in, p, "messages", config.DialectOpenAIChat, "", false)
	if out["Openai-Beta"] != "chat-opt" {
		t.Fatalf("chat opt-in lost: %v", out)
	}
	if _, ok := out["Anthropic-Beta"]; ok {
		t.Fatalf("wrong-dialect opt-in must drop: %v", out)
	}
	if _, ok := out["Anthropic-Version"]; ok {
		t.Fatalf("version default must follow dest, not provider: %v", out)
	}
	// Dest anthropic from non-anthropic provider: version default added, chat opt-in dropped.
	out2 := OutboundHeaders(in, p, "chat", config.DialectAnthropic, "", false)
	if _, ok := out2["Openai-Beta"]; ok {
		t.Fatalf("wrong-dialect opt-in must drop for anthropic dest: %v", out2)
	}
	if out2["Anthropic-Version"] == "" {
		t.Fatalf("anthropic dest must default version: %v", out2)
	}
}

func TestOverrideSessionAndFraming(t *testing.T) {
	p := overrideProvider(config.DialectAnthropic)
	p.SessionHeader = "x-sess-out"
	p.SessionSourceHeaders = []string{"session-id"}
	in := http.Header{"Session-Id": {"s1"}}
	native := OutboundHeaders(in, p, "messages", config.DialectAnthropic, "s1", true)
	if native["X-Sess-Out"] != "s1" {
		t.Fatalf("native session: %v", native)
	}
	bridged := OutboundHeaders(in, p, "chat", config.DialectOpenAIChat, "s1", true)
	// Session emission is provider-owned and identical on both modes.
	if bridged["X-Sess-Out"] != "s1" {
		t.Fatalf("bridged session: %v", bridged)
	}
	if native["Content-Type"] != "application/json" || bridged["Accept-Encoding"] != "identity" {
		t.Fatalf("framing must be enforced: %v %v", native, bridged)
	}
}
