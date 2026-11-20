package convstore

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
	"github.com/arhammahajan/model-router/internal/harness/profiles"
)

// SourceKind identifies the backing storage format of a DiscoveredSource.
type SourceKind string

const (
	SourceFile   SourceKind = "file"
	SourceSQLite SourceKind = "sqlite"
	SourceDir    SourceKind = "dir"
	SourceCLI    SourceKind = "cli"
)

// ProfileHome pairs a named profile with its resolved shadow home directory.
type ProfileHome struct {
	Agent      string
	Profile    string
	ShadowHome string
}

// ScanEnv passes resolved home/XDG paths and loaded profiles to adapters
// without letting adapters read global OS state directly.
type ScanEnv struct {
	Paths    harnessenv.Paths
	Profiles []profiles.NamedProfile

	mu            sync.Mutex
	seenRealPaths map[string]bool
}

// NewScanEnv constructs a ScanEnv from harnessenv.Paths, loading any configured profiles.
func NewScanEnv(paths harnessenv.Paths) *ScanEnv {
	env := &ScanEnv{
		Paths:         paths,
		seenRealPaths: make(map[string]bool),
	}
	if store, err := profiles.Open(paths); err == nil && store != nil {
		env.Profiles = store.List()
	}
	return env
}

// MarkRealPath resolves symlinks on path and returns true if this real path has
// not yet been claimed during the current scan. Adapters call this when scanning
// both default and shadow profile homes so overlay-symlinked stores are not
// scanned twice.
func (e *ScanEnv) MarkRealPath(path string) bool {
	if e == nil || path == "" {
		return true
	}
	realPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != "" {
		realPath = resolved
	}
	realPath = filepath.Clean(realPath)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seenRealPaths == nil {
		e.seenRealPaths = make(map[string]bool)
	}
	if e.seenRealPaths[realPath] {
		return false
	}
	e.seenRealPaths[realPath] = true
	return true
}

// ProfileShadowHomes returns all shadow homes configured for agent (both from
// profiles.Store and any on-disk directories under ~/.model-router/profiles/<agent>/*/home).
func (e *ScanEnv) ProfileShadowHomes(agent string) []ProfileHome {
	if e == nil {
		return nil
	}
	canon := CanonicalAgent(agent)
	seen := make(map[string]bool)
	var out []ProfileHome

	add := func(prof, shadow string) {
		if prof == "" || prof == profiles.DefaultProfileName || shadow == "" {
			return
		}
		clean := filepath.Clean(shadow)
		key := prof + "\x00" + clean
		if seen[key] {
			return
		}
		if fi, err := os.Stat(clean); err != nil || !fi.IsDir() {
			return
		}
		seen[key] = true
		out = append(out, ProfileHome{
			Agent:      canon,
			Profile:    prof,
			ShadowHome: clean,
		})
	}

	for _, np := range e.Profiles {
		if CanonicalAgent(np.Profile.Agent) != canon {
			continue
		}
		add(np.Name, np.Profile.ShadowHome)
	}

	// Also scan ~/.model-router/profiles/<agent>/*/home for profiles created outside profiles.json.
	profDir := filepath.Join(profiles.ProfilesDir(e.Paths), canon)
	if entries, err := os.ReadDir(profDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() || entry.Name() == profiles.DefaultProfileName {
				continue
			}
			add(entry.Name(), filepath.Join(profDir, entry.Name(), "home"))
		}
	}
	return out
}

// DiscoveredSource is one native file, directory, SQLite database, or CLI source to inspect.
type DiscoveredSource struct {
	SourceKey  string     // stable unique key: "<agent>:<profile>:<clean_path>"
	Agent      string     // canonical agent id
	Profile    string     // owning profile when discovered in an isolated/antigravity shadow home
	Path       string     // primary file/dir/db path
	Kind       SourceKind // SourceFile | SourceSQLite | SourceDir | SourceCLI
	SizeBytes  int64
	MtimeNs    int64
	WALSize    int64
	WALMtimeNs int64
}

// SourceCursor holds previously persisted Stage 1 and Stage 2 watermarks for one source.
type SourceCursor struct {
	SourceKey         string
	MetaSizeBytes     int64
	MetaMtimeNs       int64
	MetaWALSize       int64
	MetaWALMtimeNs    int64
	MetaCheckedMs     int64
	MsgSizeBytes      int64
	MsgMtimeNs        int64
	MsgWALSize        int64
	MsgWALMtimeNs     int64
	MsgByteOffset     int64
	MsgPrefixHash     string
	MsgIndexedMs      int64
	FailCount         int
	FailedMtimeNs     int64
	ExistingUpdatedMs map[string]int64 // conv ID -> previously stored updated_at_ms
}

// Message is one normalized user or assistant turn for full-text FTS5 indexing.
type Message struct {
	Seq       int
	Role      string // "user" | "assistant"
	Text      string // cleaned plain text, capped at MaxMessageTextBytes
	TsMs      int64
	Truncated bool
}

// MetadataResult is the fast Stage-1 output for one changed source.
type MetadataResult struct {
	Conversations []Conversation
}

// MessagesResult is the Stage-2 full-body extraction output for one source.
type MessagesResult struct {
	// Messages keyed by conversation ID.
	Messages map[string][]Message
	// Append indicates JSONL prefix_hash matched and messages should append
	// rather than replace existing rows for those conversations.
	Append     bool
	ByteOffset int64
	PrefixHash string
}

// Adapter encapsulates all native-store parsing logic for one coding agent.
type Adapter interface {
	// Agent returns the canonical agent id (e.g. "claude", "codex", "antigravity").
	Agent() string
	// Discover locates all native conversation stores (default + isolated profiles).
	Discover(ctx context.Context, env *ScanEnv) ([]DiscoveredSource, error)
	// ParseMetadata extracts fast conversation metadata for Stage 1.
	ParseMetadata(ctx context.Context, src DiscoveredSource, prev *SourceCursor) (*MetadataResult, error)
	// ParseMessages streams user/assistant messages for Stage 2 FTS5 indexing.
	// Returning nil, nil indicates an adapter has no separate message store.
	ParseMessages(ctx context.Context, src DiscoveredSource, prev *SourceCursor) (*MessagesResult, error)
}

// MakeSourceKey builds a canonical, stable source key.
func MakeSourceKey(agent, profile, path string) string {
	return CanonicalAgent(agent) + ":" + profile + ":" + filepath.Clean(path)
}

// StatFile returns (size, mtimeNs, true) for a regular file, or (0, 0, false).
func StatFile(path string) (int64, int64, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0, 0, false
	}
	return fi.Size(), fi.ModTime().UnixNano(), true
}
