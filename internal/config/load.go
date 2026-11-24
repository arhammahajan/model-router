// Config file loading: TOML/JSON decoding, credential files and options.
//
// Split from config.go to keep the strict schema small. TOML semantics and
// defaults are unchanged; JSON is normalized to the same TOML types before
// validation. Errors are value-free: they name keys/paths, never secrets.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// LoadOptions controls credential-file loading.
// Explicit EnvFiles load in argument order BEFORE default files; the existing
// process environment (including present-empty) always wins and the first
// file wins. NoDefaultEnv skips the automatic default files.
type LoadOptions struct {
	NoDefaultEnv bool
	EnvFiles     []string
}

// Load reads, parses and validates path with default credential behavior
// (real embedder convenience).
func Load(path string) (*Config, error) {
	return LoadWithOptions(path, LoadOptions{})
}

// LoadWithOptions reads explicit env files, then default files unless
// disabled, then parses and validates the config document.
func LoadWithOptions(path string, opts LoadOptions) (*Config, error) {
	for _, ef := range opts.EnvFiles {
		if err := loadEnvFileStrict(ef); err != nil {
			return nil, err
		}
	}
	if !opts.NoDefaultEnv {
		loadDefaultCredentials()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	doc, err := decodeDocument(path, raw)
	if err != nil {
		return nil, err
	}
	if err := expandEnvValues(doc, "config"); err != nil {
		return nil, err
	}
	return buildConfig(doc, filepath.Dir(path))
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// cutInlineComment strips a trailing ` # comment` (any whitespace before
// `#`), mirroring the baseline. A `#` without preceding whitespace is kept.
func cutInlineComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return s
}

// applyEnvData is the single stdlib env-line parser used by explicit and
// default credential files: blank lines, # comments, optional leading
// `export`, single/double quoted values, else trailing ` #` comments.
// Existing variables (including present-empty) win; malformed lines skip.
func applyEnvData(data []byte) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || !envKeyPattern.MatchString(key) {
			continue
		}
		value := strings.TrimSpace(raw)
		if len(value) >= 2 && value[0] == value[len(value)-1] &&
			(value[0] == '\'' || value[0] == '"') {
			value = value[1 : len(value)-1]
		} else {
			value = cutInlineComment(value)
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

// defaultEnvFiles mirrors the Python baseline: ~/.config/ai/credentials.env
// (or $XDG_CONFIG_HOME/ai/credentials.env) is loaded automatically; existing
// environment variables win.
func defaultEnvFiles() []string {
	var out []string
	seen := map[string]bool{}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		p := filepath.Join(xdg, "ai", "credentials.env")
		seen[p] = true
		out = append(out, p)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".config", "ai", "credentials.env")
		if !seen[p] {
			out = append(out, p)
		}
	}
	return out
}

func loadDefaultCredentials() {
	for _, path := range defaultEnvFiles() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		applyEnvData(data)
	}
}

func loadEnvFileStrict(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read env file")
	}
	applyEnvData(data)
	return nil
}

// decodeDocument selects TOML/JSON by casefolded suffix; schemeless or
// unknown suffixes try JSON then TOML; YAML is an explicit migration error.
func decodeDocument(path string, raw []byte) (map[string]any, error) {
	suffix := strings.ToLower(filepath.Ext(path))
	switch suffix {
	case ".json":
		return parseJSONStrict(raw)
	case ".toml":
		return parseTOML(raw)
	case ".yaml", ".yml":
		return nil, fmt.Errorf("config: YAML is not supported in this build; use TOML/JSON")
	case "":
		if doc, err := parseJSONStrict(raw); err == nil {
			return doc, nil
		}
		return parseTOML(raw)
	default:
		if doc, err := parseJSONStrict(raw); err == nil {
			return doc, nil
		}
		return parseTOML(raw)
	}
}

func parseTOML(raw []byte) (map[string]any, error) {
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("config: invalid TOML: %v", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func parseJSONStrict(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("config: invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("config: invalid JSON")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("config: invalid JSON: trailing data")
	}
	doc, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config: must contain an object/table at the top level")
	}
	normalized, err := normalizeJSONValue(doc, "")
	if err != nil {
		return nil, err
	}
	out, ok := normalized.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config: must contain an object/table at the top level")
	}
	return out, nil
}

func isMetadataPath(path string) bool {
	if path == "metadata" || strings.HasSuffix(path, ".metadata") {
		return true
	}
	return strings.Contains(path, ".metadata.")
}

func normalizeJSONValue(v any, path string) (any, error) {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, item := range n {
			child := k
			if path != "" {
				child = path + "." + k
			}
			nv, err := normalizeJSONValue(item, child)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(n))
		for i, item := range n {
			nv, err := normalizeJSONValue(item, path)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	case json.Number:
		s := string(n)
		if strings.ContainsAny(s, ".eE") {
			f, err := n.Float64()
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, fmt.Errorf("config: unsupported numeric value")
			}
			return f, nil
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		} else {
			if isMetadataPath(path) {
				return n, nil
			}
			return nil, fmt.Errorf("config: unsupported numeric value")
		}
	default:
		return v, nil
	}
}
