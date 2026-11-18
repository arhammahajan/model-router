package config

import (
	"net/url"
	"regexp"
	"strings"
)

var sensitiveName = regexp.MustCompile(`(?i)key|token|secret|password|credential|headers`)

// ConfigSecrets returns the app-owned raw secret corpus: router keys,
// provider keys and sensitive header/kwarg values, plus URL userinfo
// (decoded once via net/url) and query values. No environment is read;
// callers resolve env into cfg before calling. Expansion into encoded
// forms lives in internal/redact.
func ConfigSecrets(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	for _, k := range cfg.Server.APIKeys {
		add(k)
	}
	collectURL := func(raw string) {
		u, err := url.Parse(raw)
		if err != nil || u == nil {
			return
		}
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
		for _, vals := range u.Query() {
			for _, v := range vals {
				add(v)
			}
		}
	}
	collectMap := func(m map[string]string, force bool) {
		for name, v := range m {
			if force || sensitiveName.MatchString(name) {
				add(v)
				if strings.HasPrefix(strings.ToLower(v), "bearer ") {
					add(strings.TrimSpace(v[7:]))
				}
			}
		}
	}
	for _, p := range cfg.Providers {
		if p == nil {
			continue
		}
		add(p.APIKey)
		collectURL(p.BaseURL)
		collectMap(p.Headers, true)
		collectMap(p.TranslatedHeaders, true)
	}
	if cfg.Catalog != nil {
		collectURL(cfg.Catalog.URL)
		collectMap(cfg.Catalog.Headers, true)
	}
	if cfg.Llama != nil {
		collectMap(cfg.Llama.Headers, true)
		for _, s := range cfg.Llama.Servers {
			add(s.APIKey)
			collectURL(s.URL)
			collectMap(s.Headers, true)
		}
	}
	return out
}
