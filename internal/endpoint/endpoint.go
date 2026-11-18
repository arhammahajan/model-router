// Package endpoint owns the single pure generation-endpoint derivation used
// by both transport URL building and config startup comparison.
//
// Rules mirror the Python router's suffix-tolerant behavior: trailing slashes
// are stripped, then a dialect suffix is appended unless already present.
// No host/port/path normalization beyond that is implied.
package endpoint

import (
	"fmt"
	"strings"
)

// Dialect spellings (duplicated as plain strings to keep this package
// dependency-free and avoid a config↔transport import cycle).
const (
	DialectOpenAIChat      = "openai-chat"
	DialectOpenAIResponses = "openai-responses"
	DialectAnthropic       = "anthropic"
	DialectGemini          = "gemini"
)

// GenerationEndpoint joins baseURL and the generation path for a dialect.
func GenerationEndpoint(baseURL, dialect string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch dialect {
	case DialectAnthropic:
		if strings.HasSuffix(base, "/v1/messages") {
			return base, nil
		}
		if strings.HasSuffix(base, "/v1") {
			return base + "/messages", nil
		}
		return base + "/v1/messages", nil
	case DialectOpenAIChat:
		if strings.HasSuffix(base, "/chat/completions") {
			return base, nil
		}
		return base + "/chat/completions", nil
	case DialectOpenAIResponses:
		if strings.HasSuffix(base, "/responses") {
			return base, nil
		}
		return base + "/responses", nil
	case DialectGemini:
		if strings.HasSuffix(base, "/v1beta") || strings.HasSuffix(base, "/v1") {
			return base, nil
		}
		return base + "/v1beta", nil
	default:
		return "", fmt.Errorf("unsupported upstream dialect %q in this build", dialect)
	}
}
