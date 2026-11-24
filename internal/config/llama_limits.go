// Llama limit config: re-exported from internal/provider.
package config

import "github.com/arhammahajan/router/internal/provider"

// LlamaLimitFields is the closed limit vocabulary.
var LlamaLimitFields = provider.LocalLimitFields

// CloneLlamaLimitOverrides deep-copies the override table.
func CloneLlamaLimitOverrides(in map[string]map[string]*int64) map[string]map[string]*int64 {
	return provider.CloneLimitOverrides(in)
}
