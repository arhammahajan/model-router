// Discovery persistence: optional route-only disk cache (schema v2).
//
// Go owns schema v2; a Python v1 file loads as incompatible, never parsed.
// Endpoints, keys, headers, metadata and capabilities are never persisted
// and never read. The file carries route IDs and full alias names only;
// providers are re-resolved from trusted config at restore.
package discovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/arhammahajan/router/internal/config"
)

const CacheVersion = 2

// CacheMaxBytes caps one encoded file, shared with Python (8MiB).
const CacheMaxBytes = 8 * 1024 * 1024

// Cache load statuses. All are value-free; no paths, bodies or errors.
const (
	CacheMissing              = "missing"
	CacheIncompatible         = "incompatible"
	CacheConfigurationChanged = "configuration_changed"
	CacheLoaded               = "loaded"
	CacheEmpty                = "empty"
	CacheInvalid              = "invalid"
	CacheSkipped              = "skipped"
)

// CachedRoute is one persisted route: upstream ID plus full alias names.
type CachedRoute struct {
	ID      string
	Aliases []string
}

// CachedUnit is one persisted unit: wall success time plus routes.
type CachedUnit struct {
	At     float64
	Routes []CachedRoute
}

// CachedInventory is the validated file content: catalog unit plus llama
// units keyed by configured server index.
type CachedInventory struct {
	Catalog *CachedUnit
	Llama   map[int]*CachedUnit
}

// CacheStatus is the value-free last load outcome plus expired-source
// count plus the last async write error. Statuses are canonical in the
// registry snapshot; CacheStore.Status is a thin selector over them.
type CacheStatus struct {
	Load           string
	ExpiredSources int
	WriteErr       *string
}

// --- injectable filesystem gates (deterministic unit/race tests) ---

var (
	cacheOpenRead   = openReadNoFollow
	cacheMkdirAll   = os.MkdirAll
	cacheCreateTemp = createTemp0600
	cacheRename     = os.Rename
	cacheFsyncDir   = fsyncDirPath
	cacheGetUID     = os.Getuid
)

func openReadNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func createTemp0600(dir, pattern string) (*os.File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

func fsyncDirPath(dir string) error {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return syscall.Fsync(fd)
}

// LoadDiscoveryCache reads and validates one cache file without touching
// the registry. It returns a file-level inventory (basic ID safety and
// expiry applied); identity, secret-corpus and prefix checks happen in
// Restore, which has the trusted config. Statuses are value-free.
func LoadDiscoveryCache(path string, maxAge float64, nowWall float64) (CachedInventory, CacheStatus, error) {
	inv, _, st, err := loadWithIdentity(path, maxAge, nowWall)
	return inv, st, err
}

func loadWithIdentity(path string, maxAge float64, nowWall float64) (CachedInventory, string, CacheStatus, error) {
	fileIdent := ""
	inv := CachedInventory{Llama: map[int]*CachedUnit{}}
	expanded, err := config.ExpandCachePath(path)
	if err != nil || !(maxAge > 0 && !math.IsNaN(maxAge) && !math.IsInf(maxAge, 0)) || !finiteWallNow(nowWall) {
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	f, err := cacheOpenRead(expanded)
	if err != nil {
		if os.IsNotExist(err) {
			return inv, fileIdent, CacheStatus{Load: CacheMissing}, nil
		}
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != cacheGetUID() {
		_ = f.Close()
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		_ = f.Close()
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	if info.Size() > CacheMaxBytes {
		_ = f.Close()
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	raw, err := readBounded(f, CacheMaxBytes+1)
	_ = f.Close()
	if err != nil || len(raw) > CacheMaxBytes {
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	payload, err := decodeJSONObject(raw)
	if err != nil {
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	if !isClosedTop(payload) || !isCacheVersion(payload) {
		return inv, fileIdent, CacheStatus{Load: CacheIncompatible}, nil
	}
	ident, _ := payload["identity"].(string)
	if ident == "" {
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	fileIdent = ident
	expired := 0
	cat, catExp, err := decodeUnit(payload["catalog"], true)
	if err != nil {
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	if catExp {
		expired++
	} else if cat != nil {
		if cat.At > nowWall || wallExpired(nowWall, cat.At, maxAge) {
			if wallExpired(nowWall, cat.At, maxAge) {
				expired++
				cat = nil
			} else {
				return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
			}
		} else {
			inv.Catalog = cat
		}
	}
	llamaRaw, ok := payload["llama"].(map[string]any)
	if !ok {
		return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
	}
	for key, unitRaw := range llamaRaw {
		idx, ok := parseServerIndex(key)
		if !ok {
			return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
		}
		unit, exp, err := decodeUnit(unitRaw, false)
		if err != nil {
			return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
		}
		if exp {
			expired++
			continue
		}
		if unit == nil {
			continue
		}
		if unit.At > nowWall || wallExpired(nowWall, unit.At, maxAge) {
			if wallExpired(nowWall, unit.At, maxAge) {
				expired++
				continue
			}
			return inv, fileIdent, CacheStatus{Load: CacheInvalid}, fmt.Errorf("discovery cache: invalid cache file")
		}
		inv.Llama[idx] = unit
	}
	status := CacheLoaded
	if inv.Catalog == nil && len(inv.Llama) == 0 {
		status = CacheEmpty
	}
	return inv, fileIdent, CacheStatus{Load: status, ExpiredSources: expired}, nil
}

func finiteWallNow(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

func wallExpired(nowWall, at, maxAge float64) bool {
	return nowWall-at >= maxAge
}

func readBounded(f *os.File, limit int) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 32*1024)
	remaining := limit
	for remaining > 0 {
		n := len(tmp)
		if n > remaining {
			n = remaining
		}
		m, err := f.Read(tmp[:n])
		if m > 0 {
			buf.Write(tmp[:m])
			remaining -= m
		}
		if err != nil {
			if err == io.EOF || m == 0 {
				break
			}
			break
		}
		if m == 0 {
			break
		}
	}
	// Detect oversize: try one more byte.
	one := make([]byte, 1)
	if m, _ := f.Read(one); m > 0 {
		buf.Write(one[:m])
	}
	return buf.Bytes(), nil
}

func decodeJSONObject(raw []byte) (map[string]any, error) {
	if !validCacheJSONStrings(raw) {
		return nil, fmt.Errorf("invalid cache string encoding")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing data")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("not an object")
	}
	return m, nil
}

// encoding/json replaces invalid UTF-8 and unpaired UTF-16 escapes with
// U+FFFD. Cache routing identities must fail rather than be rewritten.
func validCacheJSONStrings(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		} else if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
	}
	return true
}

func isClosedTop(m map[string]any) bool {
	if len(m) != 4 {
		return false
	}
	for _, k := range []string{"schema_version", "identity", "catalog", "llama"} {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

func isCacheVersion(m map[string]any) bool {
	num, ok := m["schema_version"].(json.Number)
	if !ok {
		// JSON numbers may decode as float64 if UseNumber missed? Reject non-numbers.
		switch v := m["schema_version"].(type) {
		case int, int64:
			return fmt.Sprintf("%v", v) == "2"
		case float64:
			return v == 2
		default:
			return false
		}
	}
	return num.String() == "2"
}

func parseServerIndex(key string) (int, bool) {
	if key == "" {
		return 0, false
	}
	if key == "0" {
		return 0, true
	}
	if key[0] == '0' {
		return 0, false
	}
	n := 0
	for i := 0; i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return 0, false
		}
		n = n*10 + int(key[i]-'0')
		if n > 1<<30 {
			return 0, false
		}
	}
	return n, true
}

// decodeUnit validates one unit shape and basic route IDs. expired=true
// means the unit decoded but its time is expired (caller counts it).
// A nil unit with expired=false means JSON null (never succeeded).
func decodeUnit(raw any, isCatalog bool) (*CachedUnit, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	m, ok := raw.(map[string]any)
	if !ok || len(m) != 2 {
		return nil, false, fmt.Errorf("bad unit")
	}
	atRaw, ok := m["succeeded_at"]
	if !ok {
		return nil, false, fmt.Errorf("bad unit")
	}
	modelsRaw, ok := m["models"]
	if !ok {
		return nil, false, fmt.Errorf("bad unit")
	}
	at, ok := jsonNumberFloat(atRaw)
	if !ok || math.IsNaN(at) || math.IsInf(at, 0) || at < 0 {
		return nil, false, fmt.Errorf("bad time")
	}
	list, ok := modelsRaw.([]any)
	if !ok {
		return nil, false, fmt.Errorf("bad models")
	}
	unit := &CachedUnit{At: at}
	seenCanon := map[string]bool{}
	seenAlias := map[string]bool{}
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok || len(row) != 2 {
			return nil, false, fmt.Errorf("bad row")
		}
		idRaw, ok := row["id"]
		if !ok {
			return nil, false, fmt.Errorf("bad row")
		}
		alRaw, ok := row["aliases"]
		if !ok {
			return nil, false, fmt.Errorf("bad row")
		}
		id, ok := idRaw.(string)
		if !ok || !basicSafeID(id) {
			return nil, false, fmt.Errorf("bad id")
		}
		alList, ok := alRaw.([]any)
		if !ok {
			return nil, false, fmt.Errorf("bad aliases")
		}
		if isCatalog && len(alList) > 0 {
			return nil, false, fmt.Errorf("catalog alias")
		}
		var aliases []string
		for _, a := range alList {
			s, ok := a.(string)
			if !ok || !basicSafeID(s) {
				return nil, false, fmt.Errorf("bad alias")
			}
			aliases = append(aliases, s)
		}
		if seenCanon[id] {
			return nil, false, fmt.Errorf("duplicate id")
		}
		seenCanon[id] = true
		for _, a := range aliases {
			// Llama aliases are full public names; raw IDs occupy a
			// different namespace when a prefix or local rename applies.
			// The config-aware restore validates canonical collisions.
			if seenAlias[a] {
				return nil, false, fmt.Errorf("duplicate alias")
			}
			seenAlias[a] = true
		}
		unit.Routes = append(unit.Routes, CachedRoute{ID: id, Aliases: aliases})
	}
	return unit, false, nil
}

func jsonNumberFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// basicSafeID is the file-level admission without the secret corpus:
// valid UTF-8, nonempty, <=2048 bytes, no case-insensitive https?://.
func basicSafeID(s string) bool {
	if s == "" || len(s) > 2048 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	lower := strings.ToLower(s)
	return !strings.Contains(lower, "http://") && !strings.Contains(lower, "https://")
}
