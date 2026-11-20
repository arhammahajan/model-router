package convstore

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxMessageTextBytes caps each indexed message body at 16 KB.
	MaxMessageTextBytes = 16 << 10
	// MaxTitleRunes caps normalized conversation titles.
	MaxTitleRunes = 200
	// MaxPreviewRunes caps normalized conversation previews.
	MaxPreviewRunes = 240
	// MaxIdentifiersBytes caps the FTS5 identifiers shadow column per message.
	MaxIdentifiersBytes = 4 << 10
)

var (
	base64DataURIRe = regexp.MustCompile(`data:[a-zA-Z0-9/+.-]+;base64,[A-Za-z0-9+/=]{64,}`)
	multiBlankRe    = regexp.MustCompile(`\n{3,}`)
	identTokenRe    = regexp.MustCompile(`[A-Za-z0-9_./+-]{3,}`)
)

var syntheticTagPrefixes = []string{
	"system-reminder",
	"environment_context",
	"session_context",
	"loaded_context",
	"t3_code_instructions",
	"runtime_info",
	"permissions",
	"instructions",
	"context",
	"ide_",
}

// stripSyntheticBlocks removes paired synthetic XML-like blocks such as
// <session_context>...</session_context> by matching each opening tag to its
// exact closing tag (so nested tags like <loaded_context> inside
// <session_context> are removed completely rather than leaving stray tails).
func stripSyntheticBlocks(s string) string {
	for {
		earliestStart := -1
		var matchedClose string
		for i := 0; i < len(s); i++ {
			if s[i] != '<' || i+1 >= len(s) || s[i+1] == '/' {
				continue
			}
			// Extract tag name immediately after '<'.
			j := i + 1
			for j < len(s) && (s[j] == '_' || s[j] == '-' || (s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j == i+1 || j >= len(s) {
				continue
			}
			// Tag name must be followed by '>' or whitespace.
			if s[j] != '>' && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != '\r' {
				continue
			}
			tagName := s[i+1 : j]
			lowerTag := strings.ToLower(tagName)
			matched := false
			for _, pref := range syntheticTagPrefixes {
				if strings.HasSuffix(pref, "_") {
					if strings.HasPrefix(lowerTag, pref) {
						matched = true
						break
					}
				} else if lowerTag == pref {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			// Find end of opening tag '>'.
			gt := strings.IndexByte(s[j:], '>')
			if gt < 0 {
				continue
			}
			closeTag := "</" + tagName + ">"
			closeIdx := strings.Index(s[j+gt+1:], closeTag)
			if closeIdx < 0 {
				continue
			}
			earliestStart = i
			matchedClose = closeTag
			endPos := j + gt + 1 + closeIdx + len(matchedClose)
			s = s[:earliestStart] + " " + s[endPos:]
			break
		}
		if earliestStart < 0 {
			return s
		}
	}
}

// CleanTitle normalizes a conversation title or initial prompt into a clean
// single-line string, returning "" for empty/placeholder/synthetic text.
func CleanTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, tag := range []string{"user_input", "USER_REQUEST", "user_request"} {
		openIdx := strings.Index(s, "<"+tag)
		if openIdx >= 0 {
			if gt := strings.IndexByte(s[openIdx:], '>'); gt >= 0 {
				inner := s[openIdx+gt+1:]
				if closeIdx := strings.Index(inner, "</"+tag+">"); closeIdx >= 0 {
					s = strings.TrimSpace(inner[:closeIdx])
				}
			}
		}
	}
	s = strings.TrimSpace(stripSyntheticBlocks(s))
	if s == "" {
		return ""
	}
	for _, prefix := range []string{
		"<local-command-", "<command-name>", "<environment_context>",
		"<session_context>", "<loaded_context>", "<system-reminder>",
		"<t3_code_instructions>", "<runtime_info>",
		"<permissions", "<instructions>", "<ide_", "<context>",
		"# AGENTS.md instructions for ",
	} {
		if strings.HasPrefix(s, prefix) {
			return ""
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if lower == "new session" || strings.HasPrefix(lower, "new session - ") || lower == "untitled" {
		return ""
	}
	if strings.HasPrefix(s, "/") {
		fields := strings.Fields(s)
		if len(fields) <= 1 {
			return ""
		}
		switch strings.ToLower(fields[0]) {
		case "/model", "/login", "/logout", "/clear", "/compact", "/help", "/status", "/config":
			return ""
		default:
			s = strings.Join(fields[1:], " ")
		}
	}
	return TruncateRunes(s, MaxTitleRunes)
}

// CleanPreview normalizes a short conversation preview string.
func CleanPreview(s string) string {
	t := CleanTitle(s)
	if t == "" {
		return ""
	}
	return TruncateRunes(t, MaxPreviewRunes)
}

// CleanMessageText prepares a user or assistant message body for full-text
// FTS5 indexing: unwraps <USER_REQUEST>/<user_input>, strips synthetic system
// blocks and base64 data URIs, collapses excess blank lines, and caps at 16 KB.
func CleanMessageText(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for _, tag := range []string{"user_input", "USER_REQUEST", "user_request"} {
		openIdx := strings.Index(s, "<"+tag)
		if openIdx >= 0 {
			if gt := strings.IndexByte(s[openIdx:], '>'); gt >= 0 {
				inner := s[openIdx+gt+1:]
				if closeIdx := strings.Index(inner, "</"+tag+">"); closeIdx >= 0 {
					s = strings.TrimSpace(inner[:closeIdx])
				}
			}
		}
	}
	s = stripSyntheticBlocks(s)
	s = base64DataURIRe.ReplaceAllString(s, "[data]")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = multiBlankRe.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for _, prefix := range []string{
		"<local-command-", "<command-name>", "<environment_context>",
		"<session_context>", "<loaded_context>", "<system-reminder>",
		"<t3_code_instructions>", "<runtime_info>",
		"<permissions", "<instructions>", "<ide_", "<context>",
		"# AGENTS.md instructions for ",
	} {
		if strings.HasPrefix(s, prefix) {
			return "", false
		}
	}
	if len(s) <= MaxMessageTextBytes {
		return s, false
	}
	cut := MaxMessageTextBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]), true
}

// ExtractPlainText extracts plain user/assistant text from polymorphic JSON
// message content (string, array of content blocks, or nested object), skipping
// thinking, tool_use, and tool_result blocks.
func ExtractPlainText(content any) string {
	var parts []string
	collectPlainText(content, &parts)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func collectPlainText(content any, out *[]string) {
	switch v := content.(type) {
	case string:
		if s := strings.TrimSpace(v); s != "" {
			*out = append(*out, s)
		}
	case []any:
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				if s := strings.TrimSpace(elem); s != "" {
					*out = append(*out, s)
				}
			case map[string]any:
				if typ, _ := elem["type"].(string); typ == "thinking" || typ == "reasoning" ||
					typ == "tool_use" || typ == "tool_result" || typ == "tool-call" ||
					typ == "tool-result" || typ == "function_call" || typ == "function_call_output" ||
					typ == "image" || typ == "image_url" {
					continue
				}
				for _, k := range []string{"text", "input_text", "output_text", "content", "value"} {
					if raw, ok := elem[k]; ok {
						collectPlainText(raw, out)
						break
					}
				}
			}
		}
	case map[string]any:
		if typ, _ := v["type"].(string); typ == "thinking" || typ == "reasoning" ||
			typ == "tool_use" || typ == "tool_result" || typ == "function_call" {
			return
		}
		for _, k := range []string{"text", "input_text", "output_text", "content", "parts", "message"} {
			if raw, ok := v[k]; ok {
				collectPlainText(raw, out)
			}
		}
	}
}

// ExtractIdentifiers splits camelCase, PascalCase, snake_case, kebab-case,
// dotted names, and file paths into lowercase shadow tokens for FTS5 indexing.
func ExtractIdentifiers(text string) string {
	if text == "" {
		return ""
	}
	matches := identTokenRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, 64)
	var sb strings.Builder
	for _, tok := range matches {
		parts := splitIdentifierParts(tok)
		if len(parts) <= 1 {
			continue
		}
		for _, p := range parts {
			if len(p) < 2 {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			if sb.Len()+len(p)+1 > MaxIdentifiersBytes {
				return sb.String()
			}
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(p)
		}
	}
	return sb.String()
}

func splitIdentifierParts(tok string) []string {
	var parts []string
	var cur []rune
	runes := []rune(tok)
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range runes {
		if r == '_' || r == '-' || r == '.' || r == '/' || r == '+' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return parts
}

// TruncateRunes truncates s to at most maxRunes runes, appending "…" if truncated.
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(r[:maxRunes-1]) + "…"
}

// ParseTimestampMs converts ISO-8601/RFC3339 strings or Unix s/ms/us/ns numbers
// into Unix milliseconds. Returns 0 if unparseable.
func ParseTimestampMs(v any) int64 {
	switch t := v.(type) {
	case int64:
		return normalizeEpochMs(t)
	case int:
		return normalizeEpochMs(int64(t))
	case float64:
		if t <= 0 {
			return 0
		}
		return normalizeEpochMs(int64(t))
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return normalizeEpochMs(i)
		}
		if f, err := t.Float64(); err == nil && f > 0 {
			return normalizeEpochMs(int64(f))
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normalizeEpochMs(i)
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return normalizeEpochMs(int64(f))
		}
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.999999999",
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05",
		} {
			if parsed, err := time.Parse(layout, s); err == nil {
				return parsed.UnixMilli()
			}
		}
	}
	return 0
}

func normalizeEpochMs(v int64) int64 {
	if v <= 0 {
		return 0
	}
	switch {
	case v >= 1e17: // nanoseconds
		return v / 1e6
	case v >= 1e14: // microseconds
		return v / 1e3
	case v >= 1e11: // milliseconds
		return v
	default: // seconds
		return v * 1000
	}
}
