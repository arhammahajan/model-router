// Package routerenv resolves the router CLI runtime environment, including XDG paths
// and the shared ai/router.env endpoint settings.
//
// All functions that touch the user's home read their locations from the
// process environment through Paths, so tests can point every lookup at a
// temporary directory with t.Setenv.
package routerenv

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Paths holds the resolved home and XDG base directories used by every
// router CLI subsystem.
type Paths struct {
	Home       string
	ConfigHome string // XDG_CONFIG_HOME
	DataHome   string // XDG_DATA_HOME
	StateHome  string // XDG_STATE_HOME
	CacheHome  string // XDG_CACHE_HOME
}

// DefaultPaths resolves the XDG base directories from the process
// environment with the standard fallbacks. Relative XDG values are ignored
// in favor of the home-based defaults, matching the common systemd
// convention. Home falls back to os.UserHomeDir when HOME is unset.
func DefaultPaths() Paths {
	home := os.Getenv("HOME")
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = dir
		}
	}
	pick := func(env, fallback string) string {
		if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
			return v
		}
		return fallback
	}
	return Paths{
		Home:       home,
		ConfigHome: pick("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
		DataHome:   pick("XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
		StateHome:  pick("XDG_STATE_HOME", filepath.Join(home, ".local", "state")),
		CacheHome:  pick("XDG_CACHE_HOME", filepath.Join(home, ".cache")),
	}
}

// RouterSettings is the resolved router endpoint used by router status commands.
type RouterSettings struct {
	URL    string // no trailing slash
	APIKey string
}

// RouterEnvFile is the shared router endpoint env file, relative to ConfigHome.
func RouterEnvFile(paths Paths) string {
	return filepath.Join(paths.ConfigHome, "ai", "router.env")
}

// RouterEnv resolves the router endpoint. Pre-existing process values win
// over the shared env file, which wins over the documented defaults. This
// preserves the shared router environment contract.
func RouterEnv(paths Paths) RouterSettings {
	url := firstNonEmpty(os.Getenv("ROUTER_URL"), os.Getenv("MODEL_ROUTER_URL"))
	key := firstNonEmpty(os.Getenv("ROUTER_API_KEY"), os.Getenv("MODEL_ROUTER_API_KEY"))
	data, err := os.ReadFile(RouterEnvFile(paths))
	if err != nil {
		data, err = os.ReadFile(filepath.Join(paths.ConfigHome, "ai", "model-router.env"))
	}
	if err == nil {
		fileEnv := ParseEnvFile(string(data), func(name string) (string, bool) {
			if v, ok := os.LookupEnv(name); ok {
				return v, true
			}
			return "", false
		})
		if url == "" {
			url = firstNonEmpty(fileEnv["ROUTER_URL"], fileEnv["MODEL_ROUTER_URL"])
		}
		if key == "" {
			key = firstNonEmpty(fileEnv["ROUTER_API_KEY"], fileEnv["MODEL_ROUTER_API_KEY"])
		}
	}
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		url = "http://127.0.0.1:4000"
	}
	if key == "" {
		key = "unused"
	}
	return RouterSettings{URL: url, APIKey: key}
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnvFile parses the small export-file dialect used by
// ai/router.env: optional `export `, NAME=VALUE, where VALUE is a
// single-quoted literal, a double-quoted string with ${VAR} and
// ${VAR:-default} expansion, or a bare unquoted token. Blank lines and
// comments are skipped; anything else is ignored. lookup supplies values for
// ${VAR} expansion; file values accumulate so later lines can reference
// earlier ones.
func ParseEnvFile(data string, lookup func(name string) (string, bool)) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:eq])
		if !envName.MatchString(name) {
			continue
		}
		out[name] = expandValue(strings.TrimSpace(line[eq+1:]), func(v string) (string, bool) {
			if got, ok := out[v]; ok {
				return got, true
			}
			if lookup != nil {
				return lookup(v)
			}
			return "", false
		})
	}
	return out
}

// expandValue evaluates one env-file value: single quotes are literal,
// double quotes and bare tokens expand ${VAR} and ${VAR:-default}.
func expandValue(value string, lookup func(string) (string, bool)) string {
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2 {
		return value[1 : len(value)-1]
	}
	if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 2 {
		value = value[1 : len(value)-1]
	}
	return expandSubstitutions(value, lookup)
}

var substRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

func expandSubstitutions(value string, lookup func(string) (string, bool)) string {
	return substRe.ReplaceAllStringFunc(value, func(match string) string {
		parts := substRe.FindStringSubmatch(match)
		name, fallback := parts[1], parts[2]
		if got, ok := lookup(name); ok && got != "" {
			return got
		}
		if len(parts) > 2 { // ${VAR:-default}
			return fallback
		}
		return ""
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
