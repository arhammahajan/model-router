package convstore

import (
	"path/filepath"
	"strings"
)

// Conversation represents one unified coding-agent conversation row, enriched
// with any tmux/hook overlay fields.
type Conversation struct {
	RowID        int64  `json:"-"`
	Agent        string `json:"agent"`
	ID           string `json:"id"`
	App          string `json:"app"`
	Profile      string `json:"profile,omitempty"`
	Title        string `json:"title,omitempty"`
	Preview      string `json:"preview,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Model        string `json:"model,omitempty"`
	CreatedAtMs  int64  `json:"created_at_ms,omitempty"`
	UpdatedAtMs  int64  `json:"updated_at_ms"`
	MessageCount int    `json:"message_count,omitempty"`
	Archived     bool   `json:"archived,omitempty"`
	SourceKey    string `json:"-"`
	SourcePath   string `json:"source_path,omitempty"`
	ResumeTarget string `json:"resume_target,omitempty"`

	// Fields merged from the overlay table.
	Space        string `json:"space,omitempty"`
	LastOutcome  string `json:"last_outcome,omitempty"`
	LastOpenedMs int64  `json:"last_opened_ms,omitempty"`
}

// OverlayRecord carries tmux/hook enrichment fields that survive native source rescans.
type OverlayRecord struct {
	Agent        string `json:"agent"`
	SessionID    string `json:"session_id"`
	Profile      string `json:"profile,omitempty"`
	Space        string `json:"space,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	Title        string `json:"title,omitempty"`
	Model        string `json:"model,omitempty"`
	LastOutcome  string `json:"last_outcome,omitempty"`
	LastOpenedMs int64  `json:"last_opened_ms,omitempty"`
	UpdatedAtMs  int64  `json:"updated_at_ms,omitempty"`
}

// SnippetSpan marks a highlighted rune range [Start, End) inside a SearchHit.Snippet.
type SnippetSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// SearchHit is one conversation matched by FTS5 full-message or metadata search.
type SearchHit struct {
	Conversation      Conversation  `json:"conversation"`
	MatchKind         string        `json:"match_kind"` // "message" | "metadata"
	MatchRole         string        `json:"match_role,omitempty"`
	Snippet           string        `json:"snippet,omitempty"`
	SnippetHighlights []SnippetSpan `json:"snippet_highlights,omitempty"`
	Score             float64       `json:"score"`
}

// ListOptions controls Store.List filtering and pagination.
type ListOptions struct {
	Agent           string
	CWD             string
	IncludeArchived bool
	IncludeTmp      bool
	Limit           int
}

// SearchOptions controls Store.Search FTS5 full-text querying.
type SearchOptions struct {
	Query           string
	Agent           string
	CWD             string
	Scope           string // "all" (default) | "user" | "titles"
	IncludeArchived bool
	IncludeTmp      bool
	Limit           int
}

// IndexingStatus summarizes the SQLite index state for UI banners and CLI status.
type IndexingStatus struct {
	DBPath          string `json:"db_path"`
	Conversations   int    `json:"conversations"`
	Sources         int    `json:"sources"`
	PendingSources  int    `json:"pending_sources"`
	IndexedMessages int    `json:"indexed_messages"`
	LastSyncedMs    int64  `json:"last_synced_ms"`
}

// SyncStats reports work done by Stage 1 (metadata) or Stage 2 (messages) sync.
type SyncStats struct {
	SourcesDiscovered int   `json:"sources_discovered"`
	SourcesUpdated    int   `json:"sources_updated"`
	SourcesSkipped    int   `json:"sources_skipped"`
	SourcesPruned     int   `json:"sources_pruned"`
	SourcesFailed     int   `json:"sources_failed"`
	Conversations     int   `json:"conversations"`
	MessagesIndexed   int   `json:"messages_indexed"`
	DurationMs        int64 `json:"duration_ms"`
}

// ListResponse is the JSON payload returned by `spaces __space-conversations` (list/refresh).
type ListResponse struct {
	OK              bool           `json:"ok"`
	Conversations   []Conversation `json:"conversations"`
	SyncedMs        int64          `json:"synced_ms"`
	IndexedMessages int            `json:"indexed_messages"`
	PendingSources  int            `json:"pending_sources"`
}

// SearchResponse is the JSON payload returned by `spaces __space-conversations --search`.
type SearchResponse struct {
	OK    bool        `json:"ok"`
	Query string      `json:"query"`
	Scope string      `json:"scope"`
	Hits  []SearchHit `json:"hits"`
}

// CanonicalAgent normalizes shorthand agent aliases to canonical IDs.
func CanonicalAgent(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	switch s {
	case "claude-code":
		return "claude"
	case "agy":
		return "antigravity"
	case "dsh", "dsh-tui":
		return "deepseek"
	case "kimi-code":
		return "kimi"
	case "cursor":
		return "cursor-agent"
	case "fx-router":
		return "fx"
	default:
		return s
	}
}

// DisplayApp maps a canonical agent ID to its user-facing harness label.
func DisplayApp(agent string) string {
	canon := CanonicalAgent(agent)
	switch canon {
	case "claude":
		return "claude-code"
	case "kimi-cli":
		return "kimi"
	case "deepseek":
		return "deepseek"
	case "cursor-agent":
		return "cursor-agent"
	default:
		return canon
	}
}

// IsEphemeralCWD reports whether cwd points to a temporary directory typically
// created by automated tests or throwaway runs.
func IsEphemeralCWD(cwd string) bool {
	c := filepath.Clean(strings.TrimSpace(cwd))
	if c == "" || c == "." {
		return false
	}
	for _, prefix := range []string{"/tmp", "/var/tmp", "/private/tmp"} {
		if c == prefix || strings.HasPrefix(c, prefix+"/") {
			return true
		}
	}
	return false
}
