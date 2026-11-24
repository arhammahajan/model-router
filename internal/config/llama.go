// Package config local fleet support: [providers.llama_cpp] and
// [providers.strata] expand into validated local discovery servers merged into
// Config.Llama.
package config

import (
	"fmt"
	"strings"

	"github.com/arhammahajan/router/internal/provider"
)

// Local discovery defaults and bounds re-exported from internal/provider.
const (
	DefaultLlamaURL            = provider.DefaultLlamaURL
	DefaultLlamaProbeTimeout   = provider.DefaultLlamaProbeTimeout
	DefaultLlamaRefreshSeconds = provider.DefaultLlamaRefresh
	MaxLlamaServers            = provider.MaxLlamaServers
	MaxLlamaConcurrentProbes   = provider.MaxLlamaConcurrentProbes
)

// LlamaServer is an alias for provider.LocalServer.
type LlamaServer = provider.LocalServer

// LlamaConfig holds the merged local discovery fleet from [providers.llama_cpp]
// and/or [providers.strata]. Per-server Kind, Prefix, ExposeAliases,
// ProbeTimeout, AuthHeader, Timeout, Lossy, ContextFallback, and
// LimitOverrides allow both fleets to coexist cleanly.
type LlamaConfig struct {
	Servers         []LlamaServer
	Headers         map[string]string // trusted global headers (probe + generation)
	ProbeTimeout    float64           // seconds, whole-probe bound per server
	RefreshSeconds  float64           // >=0; 0 means exactly one startup attempt
	Prefix          string
	ExposeAliases   bool
	Compatibility   string // "strict" | "lossy"
	ContextFallback *int64
	LimitOverrides  map[string]map[string]*int64
}

// LlamaProviders builds the trusted per-server generated providers from
// config only: no I/O, no payload input.
func (l *LlamaConfig) LlamaProviders() map[string]*Provider {
	out := map[string]*Provider{}
	if l == nil {
		return out
	}
	for _, s := range l.Servers {
		kind := s.Kind
		if kind == "" {
			kind = provider.KindLlamaCpp
		}
		base := s.BaseURL
		if base == "" {
			base = provider.StripV1Suffix(s.URL) + "/v1"
		}
		timeout := s.Timeout
		if timeout <= 0 {
			timeout = DefaultProviderTimeout
		}
		compat := s.CompatibilityRaw
		lossy := s.Lossy
		if compat == "" && l.Compatibility != "" {
			compat = l.Compatibility
			lossy = l.Compatibility == "lossy"
		}
		out[s.Name] = &Provider{
			Name:              s.Name,
			Kind:              kind,
			DefaultDialect:    DialectOpenAIChat,
			SupportedDialects: []string{DialectOpenAIChat, DialectAnthropic},
			BaseURL:           base,
			APIKey:            s.APIKey,
			AuthHeader:        s.AuthHeader,
			Headers:           provider.CopyHeaders(s.Headers),
			Timeout:           timeout,
			Lossy:             lossy,
			LitellmProvider:   "openai",
			CompatibilityRaw:  compat,
		}
	}
	return out
}

// LlamaProviders builds the trusted per-server generated providers from
// config only: no I/O, no payload input.
func (c *Config) LlamaProviders() map[string]*Provider {
	if c == nil || c.Llama == nil {
		return map[string]*Provider{}
	}
	return c.Llama.LlamaProviders()
}

type localFleetParse struct {
	tableName string
	result    *provider.ParseResult
}

func mergeLocalFleets(fleets []localFleetParse, staticProviders map[string]*Provider) (*LlamaConfig, error) {
	if len(fleets) == 0 {
		return nil, nil
	}
	taken := map[string]bool{
		provider.KindLlamaCpp: true,
		provider.KindStrata:   true,
	}
	for name := range staticProviders {
		taken[name] = true
	}

	// First pass: validate all explicit names across static providers and all local fleets.
	explicitOwners := map[string]string{}
	for _, f := range fleets {
		where := "providers." + f.tableName
		for i, s := range f.result.LocalServers {
			if !s.ExplicitName {
				continue
			}
			entryWhere := fmt.Sprintf("%s.servers[%d]", where, i)
			if staticProviders[s.Name] != nil {
				return nil, fmt.Errorf("%s.name: provider name %q is already configured "+
					"in providers; choose a unique server name or omit it.", entryWhere, s.Name)
			}
			if prev, dup := explicitOwners[s.Name]; dup {
				return nil, fmt.Errorf("%s.name: duplicate explicit provider name %q; "+
					"already used by %s. Assign each server a unique name or omit names to generate them.",
					entryWhere, s.Name, prev)
			}
			explicitOwners[s.Name] = entryWhere
			taken[s.Name] = true
		}
	}

	// Second pass: merge servers and ensure generated names never collide with
	// static providers or servers from another local fleet.
	var servers []LlamaServer
	mergedHeaders := map[string]string{}
	var mergedOverrides map[string]map[string]*int64
	for _, f := range fleets {
		mergedHeaders = provider.MergeLegacyHeaders(mergedHeaders, f.result.Headers)
		if len(f.result.LimitOverrides) > 0 {
			if mergedOverrides == nil {
				mergedOverrides = map[string]map[string]*int64{}
			}
			for modelID, fields := range f.result.LimitOverrides {
				cp := make(map[string]*int64, len(fields))
				for k, v := range fields {
					cp[k] = provider.CopyInt64Ptr(v)
				}
				mergedOverrides[modelID] = cp
			}
		}
		namePrefix := "llama"
		if f.tableName == provider.KindStrata {
			namePrefix = "strata"
		}
		for _, s := range f.result.LocalServers {
			if !s.ExplicitName {
				if taken[s.Name] || strings.TrimSpace(s.Name) == "" {
					s.Name = provider.LocalProviderName(namePrefix, s.URL, taken)
				}
				taken[s.Name] = true
			}
			servers = append(servers, s)
		}
	}
	if mergedHeaders == nil {
		mergedHeaders = map[string]string{}
	}

	first := fleets[0].result
	probeTimeout := first.ProbeTimeout
	refreshSeconds := first.RefreshSeconds
	for _, f := range fleets[1:] {
		if f.result.ProbeTimeout > probeTimeout {
			probeTimeout = f.result.ProbeTimeout
		}
		if f.result.RefreshSeconds > 0 && (refreshSeconds == 0 || f.result.RefreshSeconds < refreshSeconds) {
			refreshSeconds = f.result.RefreshSeconds
		}
	}

	return &LlamaConfig{
		Servers:         servers,
		Headers:         mergedHeaders,
		ProbeTimeout:    probeTimeout,
		RefreshSeconds:  refreshSeconds,
		Prefix:          first.Prefix,
		ExposeAliases:   first.ExposeAliases,
		Compatibility:   first.Compatibility,
		ContextFallback: provider.CopyInt64Ptr(first.ContextFallback),
		LimitOverrides:  mergedOverrides,
	}, nil
}
