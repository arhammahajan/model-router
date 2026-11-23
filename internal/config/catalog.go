// Package config catalog support: the only accepted discovery table is
// [catalog] (plus its [[catalog.rules]] entries). Llama, discovery-cache
// and capability tables remain startup-rejected by buildConfig's top-level
// schema; the rejection paths there stay as-is.
package config

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"

	"github.com/arhammahajan/router/internal/capability"
	"github.com/arhammahajan/router/internal/catalog"
)

// Catalog defaults mirror the Python baseline: timeout 30s, refresh 300s.
const (
	DefaultCatalogTimeout = 30.0
	DefaultCatalogRefresh = 300.0
)

// CatalogRule is one [[catalog.rules]] entry: Match is the original text
// (for error context), Provider names a trusted configured provider, and
// Pattern is the portable compiled program. Compilation runs ONLY through
// internal/catalog.Compile, the reviewed admission gate; Go compilation
// alone does not prove Python re parity.
type CatalogRule struct {
	Match    string
	Provider string
	Pattern  *regexp.Regexp
}

// Catalog is the validated [catalog] table.
type Catalog struct {
	URL            string
	Headers        map[string]string // trusted fetch headers only (§5)
	Timeout        float64           // seconds, whole-attempt bound
	RefreshSeconds float64           // >=0; 0 means exactly one startup attempt
	Rules          []CatalogRule     // first-match wins, search semantics
	ModelProviders map[string]string // exact ID -> provider, wins over rules
	// MetadataSource is "" when absent (null/omitted) or "opencode-go-v1"
	// when the bundled review is selected. Any other value fails startup.
	MetadataSource string
	// ReviewAsset is the validated immutable bundled asset, present only
	// when MetadataSource is admitted. HasReview distinguishes absent.
	ReviewAsset capability.ReviewAsset
	HasReview   bool
}

// Review returns a deep copy of the validated asset when present.
func (c *Catalog) Review() (capability.ReviewAsset, bool) {
	if c == nil || !c.HasReview {
		return capability.ReviewAsset{}, false
	}
	return c.ReviewAsset.Clone(), true
}

var catalogKeys = map[string]bool{
	"url": true, "headers": true, "timeout": true,
	"refresh_seconds": true, "model_providers": true,
	"rules": true, "metadata_source": true,
}

var catalogRuleKeys = map[string]bool{"match": true, "provider": true}

func finiteNonNegative(v any, where, field string) (float64, error) {
	n, ok := asNum(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("%s: '%s' must be a finite number", where, field)
	}
	if n < 0 || n > maxTimeoutSeconds {
		return 0, fmt.Errorf("%s: '%s' must be >= 0 and <= %v", where, field, maxTimeoutSeconds)
	}
	return n, nil
}

func fillCatalog(tab map[string]any, providers map[string]*Provider) (*Catalog, error) {
	if err := rejectUnknown(tab, catalogKeys, "catalog",
		" (supported: url, headers, timeout, refresh_seconds, model_providers, rules)"); err != nil {
		return nil, err
	}
	v, ok := tab["url"]
	if !ok {
		return nil, fmt.Errorf("catalog.url is required")
	}
	rawURL, ok := asString(v)
	if !ok || catalog.StripModelID(rawURL) == "" {
		return nil, fmt.Errorf("catalog: 'url' must be a non-empty string")
	}
	rawURL = catalog.StripModelID(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("catalog: 'url' must start with http:// or https://")
	}
	headers := map[string]string{}
	if v, ok := tab["headers"]; ok {
		hdrs, err := checkHeaders(v, "catalog.headers", "")
		if err != nil {
			return nil, err
		}
		headers = hdrs
	}
	timeout := DefaultCatalogTimeout
	if v, ok := tab["timeout"]; ok {
		n, err := finitePositive(v, "catalog", "timeout")
		if err != nil {
			return nil, err
		}
		timeout = n
	}
	refresh := DefaultCatalogRefresh
	if v, ok := tab["refresh_seconds"]; ok {
		n, err := finiteNonNegative(v, "catalog", "refresh_seconds")
		if err != nil {
			return nil, err
		}
		refresh = n
	}
	modelProviders := map[string]string{}
	if v, ok := tab["model_providers"]; ok {
		mp, ok := asTable(v)
		if !ok {
			return nil, fmt.Errorf("catalog.model_providers: must be a table")
		}
		keys := make([]string, 0, len(mp))
		for k := range mp {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if k == "" || k != catalog.StripModelID(k) {
				return nil, fmt.Errorf("catalog.model_providers: model IDs must be nonempty strings without surrounding whitespace")
			}
			s, ok := asString(mp[k])
			if !ok {
				return nil, fmt.Errorf("catalog.model_providers[%d]: unknown provider", i)
			}
			if _, known := providers[s]; !known {
				return nil, fmt.Errorf("catalog.model_providers[%d]: unknown provider", i)
			}
			modelProviders[k] = s
		}
	}
	var rules []CatalogRule
	if v, ok := tab["rules"]; ok {
		arr, ok := asArray(v)
		if !ok {
			return nil, fmt.Errorf("catalog.rules: must be an array")
		}
		for i, item := range arr {
			where := fmt.Sprintf("catalog.rules[%d]", i)
			rtab, ok := asTable(item)
			if !ok {
				return nil, fmt.Errorf("%s: must be a table", where)
			}
			if err := rejectUnknown(rtab, catalogRuleKeys, where, " (supported: match, provider)"); err != nil {
				return nil, err
			}
			mv, ok := rtab["match"]
			if !ok {
				return nil, fmt.Errorf("%s: missing %q", where, "match")
			}
			match, ok := asString(mv)
			if !ok || catalog.StripModelID(match) == "" {
				return nil, fmt.Errorf("%s: %q must be a non-empty string", where, "match")
			}
			match = catalog.StripModelID(match)
			pv, ok := rtab["provider"]
			if !ok {
				return nil, fmt.Errorf("%s: missing %q", where, "provider")
			}
			provider, ok := asString(pv)
			if !ok || catalog.StripModelID(provider) == "" {
				return nil, fmt.Errorf("%s: %q must be a non-empty string", where, "provider")
			}
			provider = catalog.StripModelID(provider)
			if _, known := providers[provider]; !known {
				return nil, fmt.Errorf("%s.provider: unknown provider", where)
			}
			pattern, err := catalog.Compile(match)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid 'match' regex: %v", where, err)
			}
			rules = append(rules, CatalogRule{Match: match, Provider: provider, Pattern: pattern})
		}
	}
	// Admission mirrors Python config.py: metadata_source in
	// (None, "opencode-go-v1"), otherwise a value-free startup error.
	// Absent or JSON null means no enrichment; the exact string opts in
	// and validates the bundled asset once per config construction.
	metadataSource := ""
	var review capability.ReviewAsset
	hasReview := false
	if value, ok := tab["metadata_source"]; ok && value != nil {
		s, ok := asString(value)
		if !ok || s != "opencode-go-v1" {
			return nil, fmt.Errorf("catalog.metadata_source: expected 'opencode-go-v1' or null")
		}
		metadataSource = s
		ra, err := capability.LoadReviewAsset(capability.EmbeddedReviewBytes())
		if err != nil {
			return nil, fmt.Errorf("catalog.metadata_source: invalid bundled review asset")
		}
		review = ra
		hasReview = true
	}
	if len(modelProviders) == 0 && len(rules) == 0 {
		return nil, fmt.Errorf("catalog: at least one model_providers entry or [[catalog.rules]] is required")
	}
	return &Catalog{
		URL:            rawURL,
		Headers:        headers,
		Timeout:        timeout,
		RefreshSeconds: refresh,
		Rules:          rules,
		ModelProviders: modelProviders,
		MetadataSource: metadataSource,
		ReviewAsset:    review,
		HasReview:      hasReview,
	}, nil
}
