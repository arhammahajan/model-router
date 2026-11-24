// Package provider defines the unified provider specification, plugin
// interface, and built-in provider implementations (generic, cliproxyapi,
// llama_cpp, strata, and runtime-only unobserved).
//
// It sits below internal/config and may only import the standard library plus
// internal/capability, internal/catalog, internal/endpoint, and
// internal/protocol.
package provider

import (
	"strings"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/protocol"
)

// Dialects spoken by upstreams and ingress endpoints.
const (
	DialectOpenAIChat      = "openai-chat"
	DialectOpenAIResponses = "openai-responses"
	DialectAnthropic       = "anthropic"
	DialectGemini          = "gemini"
)

// Dialects lists all supported wire dialects in canonical order.
var Dialects = []string{
	DialectOpenAIChat,
	DialectOpenAIResponses,
	DialectAnthropic,
	DialectGemini,
}

// UnauthenticatedUpstreamKey is sent when a provider config has no key,
// using a placeholder to avoid sending an empty credential.
const UnauthenticatedUpstreamKey = "router-no-auth"

// Shared provider defaults and bounds.
const (
	DefaultProviderTimeout           = 600.0
	DefaultCLIProxyAPIBaseURL        = "http://127.0.0.1:8317/v1"
	CLIProxyAPIProviderName          = "cliproxyapi"
	DefaultCLIProxyAPIRefreshSeconds = 30.0
	DefaultCLIProxyAPIProbeTimeout   = 30.0
	DefaultLlamaURL                  = "http://127.0.0.1:9931"
	DefaultLlamaProbeTimeout         = 2.0
	DefaultLlamaRefresh              = 30.0
	MaxLlamaServers                  = 16
	MaxLlamaConcurrentProbes         = 8
	MaxTimeoutSeconds                = 9223372036.0
)

// Provider kinds identify the plugin backing a runtime provider Spec.
const (
	KindGeneric     = "generic"
	KindLlamaCpp    = "llama_cpp"
	KindStrata      = "strata"
	KindCLIProxyAPI = "cliproxyapi"
	KindUnobserved  = "unobserved"
)

// BuiltinProviders returns the canonical list of built-in provider plugin
// names exposed by the router substrate.
func BuiltinProviders() []string {
	return []string{KindCLIProxyAPI, KindLlamaCpp, KindStrata}
}

// Spec is the runtime provider configuration shared across config, router,
// discovery, and transport layers.
type Spec struct {
	Name              string
	Kind              string
	DefaultDialect    string
	SupportedDialects []string
	BaseURL           string
	APIKey            string // resolved, stripped at use; "" means unauthenticated
	AuthHeader        string
	CountTokens       *bool
	Headers           map[string]string
	// TranslatedHeaders is the merged legacy litellm_kwargs.headers <
	// extra_headers subset, applied to translated traffic only. Nil when
	// unconfigured.
	TranslatedHeaders map[string]string
	Timeout           float64
	Lossy             bool // compatibility = "lossy" (or legacy drop_params = true)
	SessionHeader     string
	// Session affinity routing: SessionHeader plus an ordered subset of the
	// reviewed native source headers, an optional prompt_cache_key fallback
	// and omit|reject missing policy. Nil SessionSourceHeaders with a header
	// set means the baseline default order; explicit [] disables native selection.
	SessionSourceHeaders  []string
	SessionPromptCacheKey bool
	SessionMissing        string
	CompatibilityRaw      string
	LitellmProvider       string
}

// EffectiveKey returns the stripped API key or the unauthenticated placeholder.
func (p *Spec) EffectiveKey() string {
	if p == nil {
		return ""
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return UnauthenticatedUpstreamKey
	}
	return strings.TrimSpace(p.APIKey)
}

// IsLossy reports whether the provider opts into diagnosed approximation.
func (p *Spec) IsLossy() bool {
	return p != nil && p.Lossy
}

// SupportsDialect reports whether the provider natively accepts dialect.
// When SupportedDialects is empty, only DefaultDialect is supported.
func (p *Spec) SupportsDialect(dialect string) bool {
	if p == nil || dialect == "" {
		return false
	}
	if len(p.SupportedDialects) == 0 {
		return dialect == p.DefaultDialect
	}
	for _, d := range p.SupportedDialects {
		if d == dialect {
			return true
		}
	}
	return false
}

// ResolveDialect selects the egress dialect for a request: an explicit model
// override always wins; otherwise if ingress is natively supported it passes
// through; otherwise DefaultDialect is used.
func (p *Spec) ResolveDialect(ingress, modelOverride string) string {
	if modelOverride != "" {
		return modelOverride
	}
	if p == nil {
		return ""
	}
	if ingress != "" && p.SupportsDialect(ingress) {
		return ingress
	}
	return p.DefaultDialect
}

// CommonConfig holds fields parsed once from every [providers.<name>] table
// before dispatching to the provider plugin.
type CommonConfig struct {
	Name             string
	BaseURL          string
	HasBaseURL       bool
	APIKey           string
	HasAPIKey        bool
	AuthHeader       string
	Headers          map[string]string
	CompatibilityRaw string
	Lossy            *bool
	Timeout          float64
	HasTimeout       bool
}

// LocalServer is one validated local discovery server from [providers.llama_cpp]
// or [providers.strata].
type LocalServer struct {
	Name             string
	ExplicitName     bool
	URL              string // probe URL, trailing slash and /v1 stripped
	BaseURL          string // generation URL, with /v1 suffix
	APIKey           string
	AuthHeader       string
	Headers          map[string]string
	Timeout          float64 // generation timeout (default 600.0)
	ProbeTimeout     float64 // discovery probe timeout (default 2.0)
	RefreshSeconds   float64 // discovery refresh cadence (default 30.0)
	Kind             string  // KindLlamaCpp or KindStrata
	Prefix           string
	ExposeAliases    bool
	Lossy            bool
	CompatibilityRaw string
	ContextFallback  *int64
	LimitOverrides   map[string]map[string]*int64
}

// CLIProxySpec is the validated [providers.cliproxyapi] configuration.
type CLIProxySpec struct {
	BaseURL          string
	APIKey           string
	ConfigPath       string
	Lossy            bool
	Passthrough      bool
	CompatibilityRaw string
	RefreshSeconds   float64
	ProbeTimeout     float64
	AuthHeader       string
	Headers          map[string]string
	Timeout          float64
}

// Provider builds the runtime Spec for this CLIProxyAPI configuration.
func (c *CLIProxySpec) Provider() *Spec {
	if c == nil {
		return nil
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultProviderTimeout
	}
	supported := []string{DialectOpenAIChat}
	if c.Passthrough {
		supported = []string{DialectOpenAIChat, DialectOpenAIResponses, DialectAnthropic}
	}
	return &Spec{
		Name:              CLIProxyAPIProviderName,
		Kind:              KindCLIProxyAPI,
		DefaultDialect:    DialectOpenAIChat,
		SupportedDialects: supported,
		BaseURL:           c.BaseURL,
		APIKey:            c.APIKey,
		AuthHeader:        c.AuthHeader,
		Headers:           CopyHeaders(c.Headers),
		Timeout:           timeout,
		Lossy:             c.Lossy,
		LitellmProvider:   "openai",
		CompatibilityRaw:  c.CompatibilityRaw,
	}
}

// ParseResult is returned by Plugin.ParseConfig.
type ParseResult struct {
	// Provider is set for single/static providers (generic, cliproxyapi).
	Provider *Spec
	// CLIProxy is set for [providers.cliproxyapi].
	CLIProxy *CLIProxySpec
	// LocalServers and fleet timing/metadata are set for [providers.llama_cpp]
	// and [providers.strata]. Parent keys "llama_cpp" and "strata" are never
	// inserted into static Providers.
	LocalServers    []LocalServer
	Headers         map[string]string
	RefreshSeconds  float64
	ProbeTimeout    float64
	Prefix          string
	ExposeAliases   bool
	Compatibility   string
	ContextFallback *int64
	LimitOverrides  map[string]map[string]*int64
}

// Plugin defines config parsing and runtime behavior for one provider kind.
type Plugin interface {
	Kind() string
	ParseConfig(common CommonConfig, raw map[string]any, configDir string, env func(string) (string, bool)) (*ParseResult, error)
	AuthHeaders(p *Spec, destDialect string) map[string]string
	GenerationEndpoint(baseURL, destDialect string) (string, error)
	PrepareNativeBody(destDialect string, body []byte) ([]byte, bool)
	EffortScope(destDialect string) protocol.RouteScope
	RefusesUnobservedReasoning() bool
	ApplyCapabilities(c *capability.Contract, wall float64)
}

var (
	genericPlugin     = &GenericPlugin{}
	cliproxyapiPlugin = &CLIProxyAPIPlugin{}
	llamaCppPlugin    = &LlamaCppPlugin{}
	strataPlugin      = &StrataPlugin{}
	unobservedPlugin  = &UnobservedPlugin{}
)

// ForKind returns the Plugin for a runtime Spec.Kind. Unknown or empty kinds
// fall back to the generic plugin.
func ForKind(kind string) Plugin {
	switch kind {
	case KindCLIProxyAPI:
		return cliproxyapiPlugin
	case KindLlamaCpp:
		return llamaCppPlugin
	case KindStrata:
		return strataPlugin
	case KindUnobserved:
		return unobservedPlugin
	default:
		return genericPlugin
	}
}

// ForConfigName returns the Plugin for a [providers.<name>] table name.
func ForConfigName(name string) Plugin {
	switch name {
	case CLIProxyAPIProviderName:
		return cliproxyapiPlugin
	case KindLlamaCpp:
		return llamaCppPlugin
	case KindStrata:
		return strataPlugin
	case KindUnobserved:
		return unobservedPlugin
	default:
		return genericPlugin
	}
}
