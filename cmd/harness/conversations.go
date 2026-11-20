package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/convstore/adapters"
)

func openConvStore() (*convstore.Store, string, error) {
	paths := harnessDefaultPaths()
	dbPath := convstore.DefaultDBPath(paths)
	st, err := convstore.Open(dbPath)
	if err != nil {
		return nil, dbPath, err
	}
	return st, dbPath, nil
}

func spacesHelperArgv() []string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		if abs, err := filepath.Abs(exe); err == nil {
			exe = abs
		}
		if filepath.Base(exe) == "spaces" {
			return []string{exe}
		}
		return []string{exe, "spaces"}
	}
	return []string{"harness", "spaces"}
}

// recordConversationOverlay is wired to spaceresume.RecordConversationOverlay.
// It performs a fast, best-effort overlay upsert into conversations.db.
func recordConversationOverlay(entry map[string]any) {
	if len(entry) == 0 {
		return
	}
	agent, _ := entry["agent"].(string)
	sid, _ := entry["session_id"].(string)
	if strings.TrimSpace(agent) == "" || strings.TrimSpace(sid) == "" {
		return
	}
	st, _, err := openConvStore()
	if err != nil {
		return
	}
	defer st.Close()

	rec := convstore.OverlayRecord{
		Agent:        convstore.CanonicalAgent(agent),
		SessionID:    strings.TrimSpace(sid),
		Profile:      strings.TrimSpace(stringField(entry, "profile")),
		Space:        strings.TrimSpace(stringField(entry, "space")),
		CWD:          strings.TrimSpace(stringField(entry, "cwd")),
		Title:        strings.TrimSpace(stringField(entry, "title")),
		Model:        strings.TrimSpace(stringField(entry, "model")),
		LastOutcome:  strings.TrimSpace(stringField(entry, "last_outcome")),
		LastOpenedMs: int64Field(entry, "last_opened_ms"),
		UpdatedAtMs:  int64Field(entry, "updated_at_ms"),
	}
	if rec.UpdatedAtMs <= 0 {
		rec.UpdatedAtMs = time.Now().UnixMilli()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = st.UpsertOverlay(ctx, rec)
}

func stringField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func int64Field(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// runSpaceConversations implements `spaces __space-conversations`:
//
//	--cached-only [--agent A] [--cwd D] [--include-archived] [--include-tmp] [--limit N]
//	--refresh [--agent A] [--cwd D] [--include-archived] [--include-tmp] [--limit N]
//	--index-messages [--max-sources N]
//	--search <query> [--agent A] [--cwd D] [--scope all|user|titles] [--include-archived] [--include-tmp] [--limit N]
//	--record <json>
func runSpaceConversations(args []string) int {
	fs := flag.NewFlagSet("spaces __space-conversations", flag.ContinueOnError)
	cachedOnly := fs.Bool("cached-only", false, "return cached conversations without syncing")
	refresh := fs.Bool("refresh", false, "run Stage 1 metadata sync and kick off background message indexing")
	indexMsgs := fs.Bool("index-messages", false, "run Stage 2 full-message body indexing under flock")
	searchQuery := fs.String("search", "", "run FTS5 full-message and metadata search")
	recordJSON := fs.String("record", "", "upsert one OverlayRecord JSON into overlay table")
	agent := fs.String("agent", "", "filter by canonical agent")
	cwd := fs.String("cwd", "", "filter by workspace/CWD")
	scope := fs.String("scope", "all", "search scope: all|user|titles")
	includeArchived := fs.Bool("include-archived", false, "include archived conversations")
	includeTmp := fs.Bool("include-tmp", false, "include ephemeral /tmp conversations")
	limit := fs.Int("limit", 0, "maximum results")
	maxSources := fs.Int("max-sources", 0, "maximum sources to index in --index-messages")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	paths := harnessDefaultPaths()
	ctx := context.Background()

	if strings.TrimSpace(*recordJSON) != "" {
		var rec convstore.OverlayRecord
		if err := json.Unmarshal([]byte(*recordJSON), &rec); err != nil {
			writeJSONLine(map[string]any{"ok": false, "error": err.Error()})
			return 0
		}
		st, _, err := openConvStore()
		if err != nil {
			writeJSONLine(map[string]any{"ok": false, "error": err.Error()})
			return 0
		}
		defer st.Close()
		if err := st.UpsertOverlay(ctx, rec); err != nil {
			writeJSONLine(map[string]any{"ok": false, "error": err.Error()})
			return 0
		}
		writeJSONLine(map[string]any{"ok": true})
		return 0
	}

	if *indexMsgs {
		lockPath := convstore.DefaultIndexLockPath(paths)
		release, acquired, err := convstore.AcquireIndexLock(lockPath)
		if err != nil || !acquired {
			writeJSONLine(map[string]any{"ok": true, "skipped": true})
			return 0
		}
		defer release()

		st, _, err := openConvStore()
		if err != nil {
			writeJSONLine(map[string]any{"ok": false, "error": err.Error()})
			return 0
		}
		defer st.Close()

		env := convstore.NewScanEnv(paths)
		allAdapters := adapters.All()
		if status, serr := st.Status(ctx); serr == nil && status.Sources == 0 {
			_, _ = st.SyncMetadata(ctx, env, allAdapters)
		}
		stats, err := st.SyncMessages(ctx, env, allAdapters, *maxSources)
		if err != nil {
			writeJSONLine(map[string]any{"ok": false, "error": err.Error()})
			return 0
		}
		writeJSONLine(map[string]any{"ok": true, "stats": stats})
		return 0
	}

	st, _, err := openConvStore()
	if err != nil {
		writeJSONLine(convstore.ListResponse{OK: false, Conversations: []convstore.Conversation{}})
		return 0
	}
	defer st.Close()

	if strings.TrimSpace(*searchQuery) != "" {
		hits, err := st.Search(ctx, convstore.SearchOptions{
			Query:           *searchQuery,
			Agent:           *agent,
			CWD:             *cwd,
			Scope:           *scope,
			IncludeArchived: *includeArchived,
			IncludeTmp:      *includeTmp,
			Limit:           *limit,
		})
		if err != nil {
			writeJSONLine(convstore.SearchResponse{OK: false, Query: *searchQuery, Scope: *scope, Hits: []convstore.SearchHit{}})
			return 0
		}
		if hits == nil {
			hits = []convstore.SearchHit{}
		}
		writeJSONLine(convstore.SearchResponse{
			OK:    true,
			Query: *searchQuery,
			Scope: *scope,
			Hits:  hits,
		})
		return 0
	}

	if *refresh && !*cachedOnly {
		env := convstore.NewScanEnv(paths)
		_, _ = st.SyncMetadata(ctx, env, adapters.All())
		if status, serr := st.Status(ctx); serr == nil && status.PendingSources > 0 {
			_ = convstore.EnsureBackgroundMessageIndex(spacesHelperArgv(), convstore.DefaultIndexLockPath(paths))
		}
	}

	convs, err := st.List(ctx, convstore.ListOptions{
		Agent:           *agent,
		CWD:             *cwd,
		IncludeArchived: *includeArchived,
		IncludeTmp:      *includeTmp,
		Limit:           *limit,
	})
	if err != nil {
		writeJSONLine(convstore.ListResponse{OK: false, Conversations: []convstore.Conversation{}})
		return 0
	}
	if convs == nil {
		convs = []convstore.Conversation{}
	}
	status, _ := st.Status(ctx)
	writeJSONLine(convstore.ListResponse{
		OK:              true,
		Conversations:   convs,
		SyncedMs:        status.LastSyncedMs,
		IndexedMessages: status.IndexedMessages,
		PendingSources:  status.PendingSources,
	})
	return 0
}

func writeJSONLine(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func splitFlagsAndPositionals(args []string, valueFlags map[string]bool) (flags, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return flags, pos
}

// runConversationsCLI implements user-facing `harness conversations [list|search|sync|status]`.
func runConversationsCLI(args []string) int {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "list", "search", "sync", "status", "help":
			sub = args[0]
			rest = args[1:]
		default:
			return failUsage("unknown conversations subcommand: %s", args[0])
		}
	}
	if sub == "help" {
		return topicHelp("conversations")
	}

	paths := harnessDefaultPaths()
	ctx := context.Background()

	switch sub {
	case "status":
		fs := flag.NewFlagSet("harness conversations status", flag.ContinueOnError)
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		st, _, err := openConvStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		defer st.Close()
		status, err := st.Status(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(status)
			return 0
		}
		fmt.Printf("db:               %s\n", status.DBPath)
		fmt.Printf("conversations:    %d\n", status.Conversations)
		fmt.Printf("sources:          %d\n", status.Sources)
		fmt.Printf("indexed_messages: %d\n", status.IndexedMessages)
		fmt.Printf("pending_sources:  %d\n", status.PendingSources)
		if status.LastSyncedMs > 0 {
			fmt.Printf("last_synced:      %s\n", time.UnixMilli(status.LastSyncedMs).Format(time.RFC3339))
		} else {
			fmt.Printf("last_synced:      never\n")
		}
		return 0

	case "sync":
		fs := flag.NewFlagSet("harness conversations sync", flag.ContinueOnError)
		metadataOnly := fs.Bool("metadata-only", false, "only run Stage 1 metadata sync")
		maxSources := fs.Int("max-sources", 0, "maximum sources for Stage 2 message indexing")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		st, _, err := openConvStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		defer st.Close()
		env := convstore.NewScanEnv(paths)
		allAdapters := adapters.All()
		metaStats, err := st.SyncMetadata(ctx, env, allAdapters)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: metadata sync: %v\n", progName, err)
			return 1
		}
		var msgStats convstore.SyncStats
		if !*metadataOnly {
			msgStats, err = st.SyncMessages(ctx, env, allAdapters, *maxSources)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: message sync: %v\n", progName, err)
				return 1
			}
		}
		status, _ := st.Status(ctx)
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{
				"metadata": metaStats,
				"messages": msgStats,
				"status":   status,
			})
			return 0
		}
		fmt.Printf("metadata: %d sources (%d updated, %d skipped, %d pruned) in %dms\n",
			metaStats.SourcesDiscovered, metaStats.SourcesUpdated, metaStats.SourcesSkipped, metaStats.SourcesPruned, metaStats.DurationMs)
		if !*metadataOnly {
			fmt.Printf("messages: %d messages indexed across %d sources in %dms\n",
				msgStats.MessagesIndexed, msgStats.SourcesUpdated, msgStats.DurationMs)
		}
		fmt.Printf("totals:   %d conversations, %d indexed messages, %d pending sources\n",
			status.Conversations, status.IndexedMessages, status.PendingSources)
		return 0

	case "search":
		fs := flag.NewFlagSet("harness conversations search", flag.ContinueOnError)
		queryFlag := fs.String("query", "", "search query")
		agent := fs.String("agent", "", "filter by agent")
		cwd := fs.String("cwd", "", "filter by CWD")
		scope := fs.String("scope", "all", "search scope: all|user|titles")
		limit := fs.Int("limit", 30, "maximum hits")
		includeArchived := fs.Bool("include-archived", false, "include archived conversations")
		includeTmp := fs.Bool("include-tmp", false, "include /tmp conversations")
		jsonOut := fs.Bool("json", false, "output JSON")
		// Allow flags both before and after positional query words
		// (e.g. `harness conversations search "alacritty" --limit 5`).
		flagArgs, posArgs := splitFlagsAndPositionals(rest, map[string]bool{
			"query": true, "agent": true, "cwd": true, "scope": true, "limit": true,
		})
		if err := fs.Parse(flagArgs); err != nil {
			return 2
		}
		query := strings.TrimSpace(*queryFlag)
		if query == "" && len(posArgs) > 0 {
			query = strings.TrimSpace(strings.Join(posArgs, " "))
		}
		if query == "" {
			return failUsage("conversations search requires a query")
		}
		st, _, err := openConvStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		defer st.Close()
		hits, err := st.Search(ctx, convstore.SearchOptions{
			Query:           query,
			Agent:           *agent,
			CWD:             *cwd,
			Scope:           *scope,
			IncludeArchived: *includeArchived,
			IncludeTmp:      *includeTmp,
			Limit:           *limit,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: search: %v\n", progName, err)
			return 1
		}
		if hits == nil {
			hits = []convstore.SearchHit{}
		}
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(convstore.SearchResponse{OK: true, Query: query, Scope: *scope, Hits: hits})
			return 0
		}
		for _, h := range hits {
			c := h.Conversation
			label := c.App
			if c.Profile != "" {
				label += " — " + c.Profile
			}
			title := c.Title
			if title == "" {
				title = c.ID
			}
			proj := filepath.Base(c.CWD)
			if proj == "" || proj == "." || proj == "/" {
				proj = "-"
			}
			fmt.Printf("%-20s %-18s %s\n", label, proj, title)
			if h.Snippet != "" {
				role := h.MatchRole
				if role == "" {
					role = h.MatchKind
				}
				flat := strings.Join(strings.Fields(h.Snippet), " ")
				fmt.Printf("  ↳ %s: %s\n", role, flat)
			}
		}
		return 0

	default: // list
		fs := flag.NewFlagSet("harness conversations list", flag.ContinueOnError)
		agent := fs.String("agent", "", "filter by agent")
		cwd := fs.String("cwd", "", "filter by CWD")
		query := fs.String("query", "", "optional search query")
		scope := fs.String("scope", "all", "search scope when --query is set")
		limit := fs.Int("limit", 50, "maximum conversations")
		includeArchived := fs.Bool("include-archived", false, "include archived conversations")
		includeTmp := fs.Bool("include-tmp", false, "include /tmp conversations")
		refresh := fs.Bool("refresh", false, "sync metadata before listing")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if strings.TrimSpace(*query) != "" {
			searchArgs := []string{"--query", *query, "--scope", *scope, "--limit", fmt.Sprintf("%d", *limit)}
			if *agent != "" {
				searchArgs = append(searchArgs, "--agent", *agent)
			}
			if *cwd != "" {
				searchArgs = append(searchArgs, "--cwd", *cwd)
			}
			if *includeArchived {
				searchArgs = append(searchArgs, "--include-archived")
			}
			if *includeTmp {
				searchArgs = append(searchArgs, "--include-tmp")
			}
			if *jsonOut {
				searchArgs = append(searchArgs, "--json")
			}
			return runConversationsCLI(append([]string{"search"}, searchArgs...))
		}
		st, _, err := openConvStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		defer st.Close()
		if *refresh {
			_, _ = st.SyncMetadata(ctx, convstore.NewScanEnv(paths), adapters.All())
		}
		convs, err := st.List(ctx, convstore.ListOptions{
			Agent:           *agent,
			CWD:             *cwd,
			IncludeArchived: *includeArchived,
			IncludeTmp:      *includeTmp,
			Limit:           *limit,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: list: %v\n", progName, err)
			return 1
		}
		if convs == nil {
			convs = []convstore.Conversation{}
		}
		if *jsonOut {
			status, _ := st.Status(ctx)
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(convstore.ListResponse{
				OK:              true,
				Conversations:   convs,
				SyncedMs:        status.LastSyncedMs,
				IndexedMessages: status.IndexedMessages,
				PendingSources:  status.PendingSources,
			})
			return 0
		}
		for _, c := range convs {
			label := c.App
			if c.Profile != "" {
				label += " — " + c.Profile
			}
			title := c.Title
			if title == "" {
				title = c.ID
			}
			proj := filepath.Base(c.CWD)
			if proj == "" || proj == "." || proj == "/" {
				proj = "-"
			}
			when := "-"
			if c.UpdatedAtMs > 0 {
				when = time.UnixMilli(c.UpdatedAtMs).Format("2006-01-02 15:04")
			}
			fmt.Printf("%-20s %-18s %-16s %s\n", label, proj, when, title)
		}
		return 0
	}
}
