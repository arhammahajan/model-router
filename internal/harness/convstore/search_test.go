package convstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func seedSearchStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	claudeAd := &mockAdapter{
		agent: "claude",
		sources: []DiscoveredSource{
			{SourceKey: "claude::/a", Agent: "claude", Path: "/a", Kind: SourceFile, SizeBytes: 10, MtimeNs: 100},
		},
		metaByKey: map[string][]Conversation{
			"claude::/a": {
				{ID: "c1", Title: "OAuth token refresh pipeline", Preview: "fixing jwt expiry", CWD: "/repo/auth", Branch: "feat/oauth", UpdatedAtMs: 5000},
				{ID: "c2", Title: "Database migration notes", Preview: "postgres schema", CWD: "/repo/db", UpdatedAtMs: 4000},
				{ID: "c3", Title: "C++ allocator benchmark", Preview: "arena memory", CWD: "/repo/cpp", UpdatedAtMs: 3000},
				{ID: "c4", Title: "Archived secret notes", CWD: "/repo/auth", UpdatedAtMs: 6000, Archived: true},
				{ID: "c5", Title: "Ephemeral tmp run", CWD: "/tmp/test-run", UpdatedAtMs: 7000},
			},
		},
		msgsByKey: map[string]*MessagesResult{
			"claude::/a": {
				Messages: map[string][]Message{
					"c1": {
						{Seq: 0, Role: "user", Text: "Please inspect handleAuthRefresh in internal/auth/token.go for exact phrase testing"},
						{Seq: 1, Role: "assistant", Text: "I found a race condition inside the kubernetes scheduler loop and fixed token rotation."},
					},
					"c2": {
						{Seq: 0, Role: "user", Text: "We need better indexing for 日本語 queries and exact phrase fallback tests."},
						{Seq: 1, Role: "assistant", Text: "Consider adding a partial B-tree index on updated_at_ms."},
					},
					"c3": {
						{Seq: 0, Role: "user", Text: "How does C++ placement new interact with foo:bar templates?"},
						{Seq: 1, Role: "assistant", Text: "In C++ you must align memory manually before calling placement new."},
					},
					"c4": {
						{Seq: 0, Role: "user", Text: "Archived kubernetes secret token."},
					},
					"c5": {
						{Seq: 0, Role: "user", Text: "Temporary kubernetes test cluster."},
					},
				},
			},
		},
	}

	codexAd := &mockAdapter{
		agent: "codex",
		sources: []DiscoveredSource{
			{SourceKey: "codex::/b", Agent: "codex", Path: "/b", Kind: SourceFile, SizeBytes: 10, MtimeNs: 200},
		},
		metaByKey: map[string][]Conversation{
			"codex::/b": {
				{ID: "x1", Title: "Exact phrase mismatch order", CWD: "/repo/auth", UpdatedAtMs: 5500},
				{ID: "x2", Title: "Single term OR match", CWD: "/repo/other", UpdatedAtMs: 4500},
			},
		},
		msgsByKey: map[string]*MessagesResult{
			"codex::/b": {
				Messages: map[string][]Message{
					"x1": {
						{Seq: 0, Role: "user", Text: "Testing phrase separation where exact appears far from phrase words."},
						{Seq: 1, Role: "assistant", Text: "The kubernetes pod autoscaler looks healthy."},
					},
					"x2": {
						{Seq: 0, Role: "user", Text: "Only contains unicornword without the second term."},
					},
				},
			},
		},
	}

	if _, err := st.SyncMetadata(ctx, nil, []Adapter{claudeAd, codexAd}); err != nil {
		t.Fatalf("SyncMetadata: %v", err)
	}
	if _, err := st.SyncMessages(ctx, nil, []Adapter{claudeAd, codexAd}, 0); err != nil {
		t.Fatalf("SyncMessages: %v", err)
	}
	return st
}

func TestSearchExactPhraseRanksAboveANDAndORFallback(t *testing.T) {
	ctx := context.Background()
	st := seedSearchStore(t)

	// "exact phrase" appears contiguously in c1 and c2, non-contiguously in x1.
	hits, err := st.Search(ctx, SearchOptions{Query: "exact phrase"})
	if err != nil {
		t.Fatalf("Search exact phrase: %v", err)
	}
	if len(hits) < 3 {
		t.Fatalf("expected >= 3 hits, got %d", len(hits))
	}
	// Exact phrase matches (c1, c2) must outrank non-contiguous AND match (x1).
	top2 := map[string]bool{hits[0].Conversation.ID: true, hits[1].Conversation.ID: true}
	if !top2["c1"] || !top2["c2"] {
		t.Fatalf("expected c1 and c2 in top 2 for exact phrase, got %q and %q", hits[0].Conversation.ID, hits[1].Conversation.ID)
	}
	if hits[2].Conversation.ID != "x1" {
		t.Fatalf("expected x1 as 3rd hit (AND prefix), got %q", hits[2].Conversation.ID)
	}

	// OR fallback: "unicornword nonexistentxyz" has 0 AND hits (< 5), so OR fallback returns x2!
	orHits, err := st.Search(ctx, SearchOptions{Query: "unicornword nonexistentxyz"})
	if err != nil {
		t.Fatalf("Search OR fallback: %v", err)
	}
	if len(orHits) != 1 || orHits[0].Conversation.ID != "x2" {
		t.Fatalf("expected OR fallback to match x2, got %+v", orHits)
	}
}

func TestSearchInlineAgentFilterAndScopes(t *testing.T) {
	ctx := context.Background()
	st := seedSearchStore(t)

	// Inline @codex filter restricts "kubernetes" to codex's x1 (not claude's c1).
	hits, err := st.Search(ctx, SearchOptions{Query: "@codex kubernetes"})
	if err != nil {
		t.Fatalf("Search @codex: %v", err)
	}
	if len(hits) != 1 || hits[0].Conversation.ID != "x1" || hits[0].Conversation.Agent != "codex" {
		t.Fatalf("expected only codex x1, got %+v", hits)
	}

	// Alias @claude-code resolves to canonical claude.
	hits, err = st.Search(ctx, SearchOptions{Query: "@claude-code kubernetes"})
	if err != nil {
		t.Fatalf("Search @claude-code: %v", err)
	}
	if len(hits) != 1 || hits[0].Conversation.ID != "c1" {
		t.Fatalf("expected only claude c1, got %+v", hits)
	}

	// Scope "user": "kubernetes" only appears in assistant messages for c1 and x1, so user scope returns 0!
	userHits, err := st.Search(ctx, SearchOptions{Query: "kubernetes", Scope: "user"})
	if err != nil {
		t.Fatalf("Search user scope: %v", err)
	}
	if len(userHits) != 0 {
		t.Fatalf("expected 0 hits in user scope for assistant-only term, got %+v", userHits)
	}

	// Scope "user" DOES match user message (including camelCase sub-token "refresh" from handleAuthRefresh).
	userHits, err = st.Search(ctx, SearchOptions{Query: "handleAuthRefresh", Scope: "user"})
	if err != nil {
		t.Fatalf("Search user scope identifier: %v", err)
	}
	if len(userHits) != 1 || userHits[0].Conversation.ID != "c1" || userHits[0].MatchRole != "user" {
		t.Fatalf("expected c1 user match, got %+v", userHits)
	}

	// Scope "titles": matches conversation metadata only, NOT body-only terms like "kubernetes".
	titleHits, err := st.Search(ctx, SearchOptions{Query: "kubernetes", Scope: "titles"})
	if err != nil {
		t.Fatalf("Search titles scope miss: %v", err)
	}
	if len(titleHits) != 0 {
		t.Fatalf("expected 0 hits in titles scope for body-only term, got %+v", titleHits)
	}

	titleHits, err = st.Search(ctx, SearchOptions{Query: "OAuth pipeline", Scope: "titles"})
	if err != nil {
		t.Fatalf("Search titles scope hit: %v", err)
	}
	if len(titleHits) != 1 || titleHits[0].Conversation.ID != "c1" || titleHits[0].MatchKind != "metadata" {
		t.Fatalf("expected c1 metadata match in titles scope, got %+v", titleHits)
	}

	// Archived and /tmp exclusion vs inclusion.
	allKube, err := st.Search(ctx, SearchOptions{Query: "kubernetes", IncludeArchived: true, IncludeTmp: true})
	if err != nil {
		t.Fatalf("Search include archived+tmp: %v", err)
	}
	if len(allKube) != 4 {
		t.Fatalf("expected 4 kubernetes hits with archived+tmp included, got %d", len(allKube))
	}
}

func TestSearchPunctuationAndFTS5InjectionSafety(t *testing.T) {
	ctx := context.Background()
	st := seedSearchStore(t)

	trickyQueries := []string{
		`C++`,
		`foo:bar`,
		`""`,
		`"""`,
		`OR`,
		`NOT`,
		`AND`,
		`*`,
		`***`,
		`NEAR(a b)`,
		`col:^"unterminated`,
		`((( )))`,
		`@claude`,
		`   `,
		`--- +++`,
	}
	for _, q := range trickyQueries {
		hits, err := st.Search(ctx, SearchOptions{Query: q})
		if err != nil {
			t.Fatalf("Search(%q) returned FTS5 error: %v", q, err)
		}
		if q == "C++" && len(hits) == 0 {
			t.Fatalf("Search(%q) expected at least 1 hit for C++", q)
		}
		if q == "foo:bar" && len(hits) == 0 {
			t.Fatalf("Search(%q) expected hit for foo:bar", q)
		}
	}
}

func TestSearchSnippetHighlightRuneOffsets(t *testing.T) {
	ctx := context.Background()
	st := seedSearchStore(t)

	// Search multibyte UTF-8 term "日本語" in c2 user message and verify rune spans slice cleanly.
	hits, err := st.Search(ctx, SearchOptions{Query: "日本語", Scope: "user"})
	if err != nil {
		t.Fatalf("Search unicode: %v", err)
	}
	if len(hits) != 1 || hits[0].Conversation.ID != "c2" {
		t.Fatalf("expected c2 hit for 日本語, got %+v", hits)
	}
	hit := hits[0]
	if strings.Contains(hit.Snippet, "\uE000") || strings.Contains(hit.Snippet, "\uE001") {
		t.Fatalf("raw highlight markers leaked into snippet: %q", hit.Snippet)
	}
	if len(hit.SnippetHighlights) == 0 {
		t.Fatalf("expected non-empty SnippetHighlights for snippet %q", hit.Snippet)
	}
	runes := []rune(hit.Snippet)
	foundTerm := false
	for _, sp := range hit.SnippetHighlights {
		if sp.Start < 0 || sp.End > len(runes) || sp.Start >= sp.End {
			t.Fatalf("invalid span %+v for rune len %d (%q)", sp, len(runes), hit.Snippet)
		}
		if string(runes[sp.Start:sp.End]) == "日本語" {
			foundTerm = true
		}
	}
	if !foundTerm {
		t.Fatalf("highlight spans %+v did not mark 日本語 in snippet %q", hit.SnippetHighlights, hit.Snippet)
	}
}

func TestParseSnippetHighlightsUnit(t *testing.T) {
	raw := "…prefix \uE000hello\uE001 and \uE000世界\uE001!"
	clean, spans := parseSnippetHighlights(raw)
	if clean != "…prefix hello and 世界!" {
		t.Fatalf("clean = %q", clean)
	}
	if len(spans) != 2 {
		t.Fatalf("spans = %+v, want 2", spans)
	}
	runes := []rune(clean)
	if got := string(runes[spans[0].Start:spans[0].End]); got != "hello" {
		t.Fatalf("span[0] = %q, want hello", got)
	}
	if got := string(runes[spans[1].Start:spans[1].End]); got != "世界" {
		t.Fatalf("span[1] = %q, want 世界", got)
	}
}

func TestSearchDeduplicatesBestMessageAndCWDFilter(t *testing.T) {
	ctx := context.Background()
	st := seedSearchStore(t)

	// "placement" appears in both user (seq 0) and assistant (seq 1) messages of c3.
	// Search must deduplicate to 1 hit for c3 and prefer user message (BM25 weight 2.5 vs 1.0).
	hits, err := st.Search(ctx, SearchOptions{Query: "placement"})
	if err != nil {
		t.Fatalf("Search placement: %v", err)
	}
	if len(hits) != 1 || hits[0].Conversation.ID != "c3" || hits[0].MatchRole != "user" {
		t.Fatalf("expected deduplicated user hit for c3, got %+v", hits)
	}

	// CWD filter restricts "kubernetes" to /repo/auth (c1 and x1).
	cwdHits, err := st.Search(ctx, SearchOptions{Query: "kubernetes", CWD: "/repo/auth"})
	if err != nil {
		t.Fatalf("Search CWD: %v", err)
	}
	if len(cwdHits) != 2 {
		t.Fatalf("expected 2 hits under /repo/auth, got %d (%+v)", len(cwdHits), cwdHits)
	}
}
