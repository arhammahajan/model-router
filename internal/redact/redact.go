// Package redact owns app-secret text redaction for diagnostics.
//
// Pure and neutral: no config imports, no filesystem, no environment, no
// process-wide registry. Callers build the raw corpus with
// config.ConfigSecrets (app-owned only) and expand it here, including
// quote/JSON encodings, before redacting diagnostic text.
//
// Mirrors Python common.ErrorSanitizer + _sanitize_details: known-secret
// replacement (longest first), Bearer tail, k=v credential shapes,
// userinfo host redaction, sk-/AIza/gsk_ patterns, control
// normalization, 4000-char truncation. Keys and values are both sanitized.
package redact

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
)

// MaxText is the diagnostic text bound (Python MAX_ERROR_MESSAGE).
const MaxText = 4000

var (
	bearerRe = regexp.MustCompile(`(?i)(bearer\s+)[^\s"'` + "`" + `,;<>\[\]{}]+`)
	credRe   = regexp.MustCompile(`(?i)((?:[\w-]*(?:key|token|secret|password|credential)|authorization|cookie)["']?\s*[:=]\s*["']?)[^\s"'` + "`" + `,;&<>\[\]{}]+`)
	userRe   = regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`)
	keyRe    = regexp.MustCompile(`\b(?:sk-[\w.\-]+|AIza[\w\-]{20,}|gsk_[\w]{20,})`)
)

// ExpandCorpus adds URL/JSON encoded forms for each raw secret, longest
// first. Matches Python ErrorSanitizer: quote(safe=""), quote_plus
// (%20 form), JSON inner strings (unicode + ascii escapes).
func ExpandCorpus(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range raw {
		if s == "" {
			continue
		}
		add(s)
		add(url.PathEscape(s))
		add(url.QueryEscape(s))
		add(strings.ReplaceAll(url.QueryEscape(s), "+", "%20"))
		if inner, ok := jsonInner(s); ok {
			add(inner)
		}
		if inner, ok := jsonInnerASCII(s); ok {
			add(inner)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func jsonInner(s string) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", false
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	if len(raw) < 2 {
		return "", false
	}
	return string(raw[1 : len(raw)-1]), true
}

func jsonInnerASCII(s string) (string, bool) {
	inner, ok := jsonInner(s)
	if !ok {
		return "", false
	}
	var b strings.Builder
	for _, r := range inner {
		if r > 0xffff {
			hi, lo := utf16.EncodeRune(r)
			b.WriteString(sprintfU4(hi) + sprintfU4(lo))
		} else if r > 127 {
			b.WriteString(sprintfU4(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String(), true
}

func sprintfU4(r rune) string {
	const hexd = "0123456789abcdef"
	var out [6]byte
	out[0] = '\\'
	out[1] = 'u'
	out[2] = hexd[(r>>12)&0xf]
	out[3] = hexd[(r>>8)&0xf]
	out[4] = hexd[(r>>4)&0xf]
	out[5] = hexd[r&0xf]
	return string(out[:])
}

// Redact renders one diagnostic string with an expanded corpus.
func Redact(s string, corpus []string) string {
	for _, secret := range corpus {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "<redacted>")
		}
	}
	s = bearerRe.ReplaceAllString(s, `${1}<redacted>`)
	s = credRe.ReplaceAllString(s, `${1}<redacted>`)
	s = userRe.ReplaceAllString(s, `${1}<redacted>@`)
	s = keyRe.ReplaceAllString(s, "<redacted>")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	out := b.String()
	if len(out) > MaxText {
		characters := 0
		for i := range out {
			if characters == MaxText {
				out = out[:i]
				break
			}
			characters++
		}
	}
	return out
}

// SanitizeDetails recurses over report values, sanitizing mapping keys and
// values like Python _sanitize_details. Non-strings pass through.
func SanitizeDetails(v any, corpus []string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, part := range t {
			out[Redact(k, corpus)] = SanitizeDetails(part, corpus)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(t))
		for k, part := range t {
			out[Redact(k, corpus)] = Redact(part, corpus)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, part := range t {
			out[i] = SanitizeDetails(part, corpus)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, part := range t {
			out[i] = Redact(part, corpus)
		}
		return out
	case string:
		return Redact(t, corpus)
	default:
		return v
	}
}

// URLSecrets extracts userinfo (decoded once) and query values from a raw
// URL for corpus building. Parse failures yield nothing.
func URLSecrets(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return nil
	}
	var out []string
	if u.User != nil {
		if name := u.User.Username(); name != "" {
			out = append(out, name)
		}
		if pw, ok := u.User.Password(); ok && pw != "" {
			out = append(out, pw)
		}
	}
	for _, vals := range u.Query() {
		for _, v := range vals {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}
