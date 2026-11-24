package provider

import (
	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/endpoint"
	"github.com/arhammahajan/router/internal/protocol"
)

// StrataPlugin handles [providers.strata] and Strata/PolyStrata runtime behavior.
type StrataPlugin struct{}

func (*StrataPlugin) Kind() string { return KindStrata }

func (*StrataPlugin) ParseConfig(common CommonConfig, raw map[string]any, _ string, env func(string) (string, bool)) (*ParseResult, error) {
	return parseLocalFleet(localFleetOpts{
		kind:          KindStrata,
		namePrefix:    "strata",
		defaultPrefix: "",
		envFallback:   "",
	}, common, raw, env)
}

func (*StrataPlugin) AuthHeaders(p *Spec, destDialect string) map[string]string {
	return DefaultAuthHeaders(p, destDialect)
}

func (*StrataPlugin) GenerationEndpoint(baseURL, destDialect string) (string, error) {
	return endpoint.GenerationEndpoint(baseURL, destDialect)
}

func (*StrataPlugin) PrepareNativeBody(_ string, body []byte) ([]byte, bool) {
	return body, false
}

func (*StrataPlugin) EffortScope(destDialect string) protocol.RouteScope {
	switch destDialect {
	case DialectOpenAIResponses:
		return protocol.LocalResponsesRoute(capability.RuntimeStrata)
	case DialectAnthropic:
		return protocol.CompatAnthropicRoute()
	case DialectGemini:
		return protocol.OrdinaryGeminiRoute()
	default:
		return protocol.LocalRoute(capability.RuntimeStrata)
	}
}

func (*StrataPlugin) RefusesUnobservedReasoning() bool { return false }

func (*StrataPlugin) ApplyCapabilities(c *capability.Contract, wall float64) {
	capability.ApplyLlamaMapper(c, capability.RuntimeStrata, wall)
}
