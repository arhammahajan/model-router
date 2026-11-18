// Discovery cache table: optional [discovery_cache] with exact path and
// max_age_seconds schema. Absent table means disabled (nil).
package config

import (
	"fmt"
	"math"
	"os"
	"strings"
)

// DiscoveryCacheConfig is the validated [discovery_cache] table.
type DiscoveryCacheConfig struct {
	Path          string
	MaxAgeSeconds float64
}

// DefaultDiscoveryCacheMaxAge mirrors Python's 86400 default, applied only
// once the table exists.
const DefaultDiscoveryCacheMaxAge = 86400.0

var discoveryCacheKeys = map[string]bool{"path": true, "max_age_seconds": true}

// ExpandCachePath expands a leading ~/ via the home directory only. ~other
// rejects; no environment expansion happens here (config-layer ${VAR}
// expansion already ran before validation, if at all).
func ExpandCachePath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("discovery_cache.path must be a nonempty path")
	}
	if strings.HasPrefix(trimmed, "~") {
		if trimmed == "~" || strings.HasPrefix(trimmed, "~/") {
			home, err := os.UserHomeDir()
			if err != nil || strings.TrimSpace(home) == "" {
				return "", fmt.Errorf("discovery_cache.path: cannot resolve home directory")
			}
			rest := strings.TrimPrefix(trimmed, "~")
			rest = strings.TrimPrefix(rest, "/")
			if rest == "" {
				return home, nil
			}
			return home + "/" + rest, nil
		}
		return "", fmt.Errorf("discovery_cache.path: only ~/ home expansion is supported")
	}
	return trimmed, nil
}

func fillDiscoveryCache(tab map[string]any) (*DiscoveryCacheConfig, error) {
	if err := rejectUnknown(tab, discoveryCacheKeys, "discovery_cache",
		" (supported: path, max_age_seconds)"); err != nil {
		return nil, err
	}
	rawPath, ok := tab["path"]
	if !ok {
		return nil, fmt.Errorf("discovery_cache.path is required")
	}
	pathStr, ok := asString(rawPath)
	if !ok || strings.TrimSpace(pathStr) == "" {
		return nil, fmt.Errorf("discovery_cache.path must be a nonempty path")
	}
	expanded, err := ExpandCachePath(pathStr)
	if err != nil {
		return nil, err
	}
	maxAge := DefaultDiscoveryCacheMaxAge
	if v, ok := tab["max_age_seconds"]; ok {
		n, ok := asNum(v)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("discovery_cache: 'max_age_seconds' must be a finite number")
		}
		if n <= 0 || n > maxTimeoutSeconds {
			return nil, fmt.Errorf("discovery_cache: 'max_age_seconds' must be a positive number <= %v", maxTimeoutSeconds)
		}
		maxAge = n
	}
	return &DiscoveryCacheConfig{Path: expanded, MaxAgeSeconds: maxAge}, nil
}
