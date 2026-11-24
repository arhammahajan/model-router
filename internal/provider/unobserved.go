package provider

import (
	"fmt"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/endpoint"
	"github.com/arhammahajan/router/internal/protocol"
)

// UnobservedPlugin is a runtime-only plugin for cache-restored or
// not-yet-probed local servers.
type UnobservedPlugin struct{}

func (*UnobservedPlugin) Kind() string { return KindUnobserved }

func (*UnobservedPlugin) ParseConfig(_ CommonConfig, _ map[string]any, _ string, _ func(string) (string, bool)) (*ParseResult, error) {
	return nil, fmt.Errorf("providers.unobserved: unobserved is a runtime-only provider kind and cannot be configured directly")
}

func (*UnobservedPlugin) AuthHeaders(p *Spec, destDialect string) map[string]string {
	return DefaultAuthHeaders(p, destDialect)
}

func (*UnobservedPlugin) GenerationEndpoint(baseURL, destDialect string) (string, error) {
	return endpoint.GenerationEndpoint(baseURL, destDialect)
}

func (*UnobservedPlugin) PrepareNativeBody(_ string, body []byte) ([]byte, bool) {
	return body, false
}

func (*UnobservedPlugin) EffortScope(destDialect string) protocol.RouteScope {
	switch destDialect {
	case DialectOpenAIResponses:
		return protocol.LocalResponsesRoute(capability.RuntimeUnobserved)
	case DialectAnthropic:
		return protocol.CompatAnthropicRoute()
	case DialectGemini:
		return protocol.OrdinaryGeminiRoute()
	default:
		return protocol.LocalRoute(capability.RuntimeUnobserved)
	}
}

func (*UnobservedPlugin) RefusesUnobservedReasoning() bool { return true }

func (*UnobservedPlugin) ApplyCapabilities(_ *capability.Contract, _ float64) {}
