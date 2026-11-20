package convstore

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	snippetHighlightStart = "\uE000"
	snippetHighlightEnd   = "\uE001"
	orFallbackThreshold   = 5
)

type queryRoute struct {
	expr       string
	tierBonus  float64
	isFallback bool
}

// Search executes full-text FTS5 search over indexed messages and/or conversation metadata.
func (s *Store) Search(ctx context.Context, opts SearchOptions) ([]SearchHit, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	inlineAgent, remaining := parseInlineAgentFilter(opts.Query)
	agent := CanonicalAgent(opts.Agent)
	if inlineAgent != "" {
		agent = inlineAgent
	}

	tokens := extractSearchTokens(remaining)
	if len(tokens) == 0 {
		return []SearchHit{}, nil
	}

	scope := strings.ToLower(strings.TrimSpace(opts.Scope))
	if scope != "user" && scope != "titles" {
		scope = "all"
	}

	nowMs := time.Now().UnixMilli()
	bestByConv := make(map[int64]SearchHit, limit)

	mergeHit := func(hit SearchHit) {
		if existing, ok := bestByConv[hit.Conversation.RowID]; ok {
			if hit.Score > existing.Score {
				bestByConv[hit.Conversation.RowID] = hit
			}
			return
		}
		bestByConv[hit.Conversation.RowID] = hit
	}

	if scope == "all" || scope == "user" {
		colPrefix := ""
		if scope == "user" {
			colPrefix = "{user_text identifiers} : "
		}
		msgRoutes := buildFTSRoutes(tokens, colPrefix)
		for _, route := range msgRoutes {
			if route.isFallback && len(bestByConv) >= orFallbackThreshold {
				continue
			}
			hits, err := s.queryMessagesFTS(ctx, route, agent, opts, scope == "user", limit, nowMs)
			if err != nil {
				return nil, err
			}
			for _, h := range hits {
				mergeHit(h)
			}
		}
	}

	if scope == "all" || scope == "titles" {
		metaRoutes := buildFTSRoutes(tokens, "")
		for _, route := range metaRoutes {
			if route.isFallback && len(bestByConv) >= orFallbackThreshold {
				continue
			}
			hits, err := s.queryConversationsFTS(ctx, route, agent, opts, limit, nowMs)
			if err != nil {
				return nil, err
			}
			for _, h := range hits {
				if existing, ok := bestByConv[h.Conversation.RowID]; ok {
					// If already matched by message body, give a small metadata corroboration boost
					// while keeping the message snippet.
					if existing.MatchKind == "message" {
						existing.Score += 1.5
						bestByConv[h.Conversation.RowID] = existing
						continue
					}
				}
				mergeHit(h)
			}
		}
	}

	results := make([]SearchHit, 0, len(bestByConv))
	for _, h := range bestByConv {
		results = append(results, h)
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		ti := maxInt64(results[i].Conversation.UpdatedAtMs, results[i].Conversation.LastOpenedMs)
		tj := maxInt64(results[j].Conversation.UpdatedAtMs, results[j].Conversation.LastOpenedMs)
		if ti != tj {
			return ti > tj
		}
		return results[i].Conversation.RowID > results[j].Conversation.RowID
	})

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (s *Store) queryMessagesFTS(
	ctx context.Context,
	route queryRoute,
	agent string,
	opts SearchOptions,
	userOnly bool,
	limit int,
	nowMs int64,
) ([]SearchHit, error) {
	conds := []string{"messages_fts MATCH ?"}
	args := []any{
		snippetHighlightStart, snippetHighlightEnd,
		snippetHighlightStart, snippetHighlightEnd,
		route.expr,
	}

	if userOnly {
		conds = append(conds, "m.role = 'user'")
	}
	conds, args = appendConversationFilterSQL(conds, args, agent, opts.CWD, opts.IncludeArchived, opts.IncludeTmp)

	fetchLimit := limit * 10
	if fetchLimit < 250 {
		fetchLimit = 250
	}
	if fetchLimit > 2000 {
		fetchLimit = 2000
	}

	query := `
		SELECT
			c.row_id,
			c.agent,
			c.id,
			c.app,
			COALESCE(NULLIF(o.profile, ''), c.profile) AS eff_profile,
			COALESCE(NULLIF(o.title, ''), c.title) AS eff_title,
			c.preview,
			COALESCE(NULLIF(o.cwd, ''), c.cwd) AS eff_cwd,
			c.branch,
			COALESCE(NULLIF(o.model, ''), c.model) AS eff_model,
			c.created_at_ms,
			c.updated_at_ms,
			c.message_count,
			c.archived,
			c.source_key,
			c.source_path,
			c.resume_target,
			COALESCE(o.space, '') AS eff_space,
			COALESCE(o.last_outcome, '') AS eff_outcome,
			COALESCE(o.last_opened_ms, 0) AS eff_last_opened_ms,
			m.role,
			bm25(messages_fts, 2.5, 1.0, 0.5) AS bm25_score,
			CASE WHEN m.role = 'user'
				THEN snippet(messages_fts, 0, ?, ?, '…', 14)
				ELSE snippet(messages_fts, 1, ?, ?, '…', 14)
			END AS raw_snippet
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN conversations c ON c.row_id = m.conv_row_id
		LEFT JOIN overlay o ON o.agent = c.agent AND o.session_id = c.id
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY bm25_score ASC
		LIMIT ?
	`
	args = append(args, fetchLimit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("convstore: search messages: %w", err)
	}
	defer rows.Close()

	bestByConv := make(map[int64]SearchHit)
	for rows.Next() {
		var c Conversation
		var archivedInt int
		var role string
		var bm25Score float64
		var rawSnippet string
		if err := rows.Scan(
			&c.RowID, &c.Agent, &c.ID, &c.App, &c.Profile, &c.Title, &c.Preview,
			&c.CWD, &c.Branch, &c.Model, &c.CreatedAtMs, &c.UpdatedAtMs,
			&c.MessageCount, &archivedInt, &c.SourceKey, &c.SourcePath, &c.ResumeTarget,
			&c.Space, &c.LastOutcome, &c.LastOpenedMs,
			&role, &bm25Score, &rawSnippet,
		); err != nil {
			return nil, fmt.Errorf("convstore: search messages scan: %w", err)
		}
		c.Archived = archivedInt != 0
		if c.App == "" {
			c.App = DisplayApp(c.Agent)
		}
		if !opts.IncludeTmp && IsEphemeralCWD(c.CWD) {
			continue
		}

		cleanSnippet, spans := parseSnippetHighlights(rawSnippet)
		score := route.tierBonus + normalizeBM25(bm25Score) + recencyBoost(nowMs, maxInt64(c.UpdatedAtMs, c.LastOpenedMs))
		hit := SearchHit{
			Conversation:      c,
			MatchKind:         "message",
			MatchRole:         role,
			Snippet:           cleanSnippet,
			SnippetHighlights: spans,
			Score:             score,
		}
		if prev, ok := bestByConv[c.RowID]; !ok || hit.Score > prev.Score {
			bestByConv[c.RowID] = hit
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("convstore: search messages rows: %w", err)
	}

	out := make([]SearchHit, 0, len(bestByConv))
	for _, h := range bestByConv {
		out = append(out, h)
	}
	return out, nil
}

func (s *Store) queryConversationsFTS(
	ctx context.Context,
	route queryRoute,
	agent string,
	opts SearchOptions,
	limit int,
	nowMs int64,
) ([]SearchHit, error) {
	conds := []string{"conversations_fts MATCH ?"}
	args := []any{
		snippetHighlightStart, snippetHighlightEnd,
		route.expr,
	}

	conds, args = appendConversationFilterSQL(conds, args, agent, opts.CWD, opts.IncludeArchived, opts.IncludeTmp)

	fetchLimit := limit * 4
	if fetchLimit < 100 {
		fetchLimit = 100
	}
	if fetchLimit > 1000 {
		fetchLimit = 1000
	}

	query := `
		SELECT
			c.row_id,
			c.agent,
			c.id,
			c.app,
			COALESCE(NULLIF(o.profile, ''), c.profile) AS eff_profile,
			COALESCE(NULLIF(o.title, ''), c.title) AS eff_title,
			c.preview,
			COALESCE(NULLIF(o.cwd, ''), c.cwd) AS eff_cwd,
			c.branch,
			COALESCE(NULLIF(o.model, ''), c.model) AS eff_model,
			c.created_at_ms,
			c.updated_at_ms,
			c.message_count,
			c.archived,
			c.source_key,
			c.source_path,
			c.resume_target,
			COALESCE(o.space, '') AS eff_space,
			COALESCE(o.last_outcome, '') AS eff_outcome,
			COALESCE(o.last_opened_ms, 0) AS eff_last_opened_ms,
			bm25(conversations_fts, 3.0, 1.5, 1.0, 1.0, 0.5, 0.5) AS bm25_score,
			snippet(conversations_fts, -1, ?, ?, '…', 14) AS raw_snippet
		FROM conversations_fts
		JOIN conversations c ON c.row_id = conversations_fts.rowid
		LEFT JOIN overlay o ON o.agent = c.agent AND o.session_id = c.id
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY bm25_score ASC
		LIMIT ?
	`
	args = append(args, fetchLimit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("convstore: search metadata: %w", err)
	}
	defer rows.Close()

	out := make([]SearchHit, 0, 16)
	for rows.Next() {
		var c Conversation
		var archivedInt int
		var bm25Score float64
		var rawSnippet string
		if err := rows.Scan(
			&c.RowID, &c.Agent, &c.ID, &c.App, &c.Profile, &c.Title, &c.Preview,
			&c.CWD, &c.Branch, &c.Model, &c.CreatedAtMs, &c.UpdatedAtMs,
			&c.MessageCount, &archivedInt, &c.SourceKey, &c.SourcePath, &c.ResumeTarget,
			&c.Space, &c.LastOutcome, &c.LastOpenedMs,
			&bm25Score, &rawSnippet,
		); err != nil {
			return nil, fmt.Errorf("convstore: search metadata scan: %w", err)
		}
		c.Archived = archivedInt != 0
		if c.App == "" {
			c.App = DisplayApp(c.Agent)
		}
		if !opts.IncludeTmp && IsEphemeralCWD(c.CWD) {
			continue
		}

		cleanSnippet, spans := parseSnippetHighlights(rawSnippet)
		score := route.tierBonus + normalizeBM25(bm25Score) + recencyBoost(nowMs, maxInt64(c.UpdatedAtMs, c.LastOpenedMs))
		out = append(out, SearchHit{
			Conversation:      c,
			MatchKind:         "metadata",
			Snippet:           cleanSnippet,
			SnippetHighlights: spans,
			Score:             score,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("convstore: search metadata rows: %w", err)
	}
	return out, nil
}

func appendConversationFilterSQL(
	conds []string,
	args []any,
	agent string,
	cwd string,
	includeArchived bool,
	includeTmp bool,
) ([]string, []any) {
	if agent != "" {
		conds = append(conds, "c.agent = ?")
		args = append(args, agent)
	}
	if !includeArchived {
		conds = append(conds, "c.archived = 0")
	}
	effCWDExpr := "COALESCE(NULLIF(o.cwd, ''), c.cwd)"
	if trimmed := strings.TrimSpace(cwd); trimmed != "" {
		cleanCWD := filepath.Clean(trimmed)
		if cleanCWD == "/" {
			conds = append(conds, "substr("+effCWDExpr+", 1, 1) = '/'")
		} else {
			normExpr := "CASE WHEN " + effCWDExpr + " = '/' THEN '/' ELSE RTRIM(" + effCWDExpr + ", '/') END"
			conds = append(conds, "("+normExpr+" = ? OR substr("+normExpr+", 1, ?) = ?)")
			prefix := cleanCWD + "/"
			args = append(args, cleanCWD, len(prefix), prefix)
		}
	}
	if !includeTmp {
		conds = append(conds, ephemeralCWDExclusionSQL(effCWDExpr))
	}
	return conds, args
}

// parseInlineAgentFilter extracts @<agent> filter tokens from query.
func parseInlineAgentFilter(rawQuery string) (agent string, remaining string) {
	fields := strings.Fields(rawQuery)
	if len(fields) == 0 {
		return "", ""
	}
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.HasPrefix(f, "@") && len(f) > 1 && isValidAgentFilterToken(f[1:]) {
			agent = CanonicalAgent(f[1:])
			continue
		}
		kept = append(kept, f)
	}
	return agent, strings.Join(kept, " ")
}

func isValidAgentFilterToken(s string) bool {
	for i, r := range s {
		if i == 0 && !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func isFTSTokenChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) ||
		r == '_' || r == '.' || r == '-' || r == '/' || r == '+'
}

// extractSearchTokens splits user input strictly along FTS5 tokenchar boundaries
// so arbitrary punctuation (quotes, colons, wildcards, operators) can never
// produce invalid FTS5 syntax.
func extractSearchTokens(query string) []string {
	var tokens []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			tok := string(cur)
			// Require at least one alphanumeric rune to avoid pure-punctuation tokens
			// like "---" or "+++" that may produce empty/noisy FTS5 terms.
			hasAlnum := false
			for _, r := range cur {
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					hasAlnum = true
					break
				}
			}
			if hasAlnum {
				tokens = append(tokens, tok)
			}
			cur = cur[:0]
		}
	}
	for _, r := range query {
		if isFTSTokenChar(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

func quoteFTSToken(tok string) string {
	return `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
}

func buildTokenExpr(tok string) string {
	primary := quoteFTSToken(tok) + "*"
	parts := splitIdentifierParts(tok)
	if len(parts) <= 1 {
		return primary
	}
	subTerms := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) >= 2 {
			subTerms = append(subTerms, quoteFTSToken(p)+"*")
		}
	}
	if len(subTerms) <= 1 {
		return primary
	}
	return "(" + primary + " OR (" + strings.Join(subTerms, " AND ") + "))"
}

// buildFTSRoutes constructs the multi-route FTS5 query plan:
//  1. Exact phrase when >= 2 tokens
//  2. AND prefix (with camelCase/snake_case sub-token expansion)
//  3. OR fallback when >= 2 tokens (used if AND returns < 5 hits)
func buildFTSRoutes(tokens []string, colPrefix string) []queryRoute {
	if len(tokens) == 0 {
		return nil
	}
	var routes []queryRoute

	if len(tokens) >= 2 {
		phraseInner := strings.Join(tokens, " ")
		phraseExpr := colPrefix + quoteFTSToken(phraseInner)
		routes = append(routes, queryRoute{
			expr:      phraseExpr,
			tierBonus: 40.0,
		})
	}

	andParts := make([]string, len(tokens))
	for i, tok := range tokens {
		andParts[i] = buildTokenExpr(tok)
	}
	var andExpr string
	if colPrefix != "" {
		andExpr = colPrefix + "(" + strings.Join(andParts, " AND ") + ")"
	} else {
		andExpr = strings.Join(andParts, " AND ")
	}
	routes = append(routes, queryRoute{
		expr:      andExpr,
		tierBonus: 15.0,
	})

	if len(tokens) >= 2 {
		var orExpr string
		if colPrefix != "" {
			orExpr = colPrefix + "(" + strings.Join(andParts, " OR ") + ")"
		} else {
			orExpr = strings.Join(andParts, " OR ")
		}
		routes = append(routes, queryRoute{
			expr:       orExpr,
			tierBonus:  0.0,
			isFallback: true,
		})
	}

	return routes
}

// parseSnippetHighlights strips \uE000 / \uE001 markers from an FTS5 snippet
// and computes exact rune-offset [Start, End) highlight spans.
func parseSnippetHighlights(raw string) (string, []SnippetSpan) {
	if raw == "" {
		return "", nil
	}
	var sb strings.Builder
	sb.Grow(len(raw))

	var spans []SnippetSpan
	runeIdx := 0
	inHighlight := false
	startIdx := 0

	for _, r := range raw {
		switch r {
		case '\uE000':
			if !inHighlight {
				inHighlight = true
				startIdx = runeIdx
			}
		case '\uE001':
			if inHighlight {
				if runeIdx > startIdx {
					spans = append(spans, SnippetSpan{Start: startIdx, End: runeIdx})
				}
				inHighlight = false
			}
		default:
			sb.WriteRune(r)
			runeIdx++
		}
	}
	if inHighlight && runeIdx > startIdx {
		spans = append(spans, SnippetSpan{Start: startIdx, End: runeIdx})
	}
	return sb.String(), spans
}

func normalizeBM25(bm25 float64) float64 {
	// SQLite bm25() returns negative scores where more negative = better match.
	if bm25 < 0 {
		return -bm25
	}
	return 0
}

func recencyBoost(nowMs, updatedMs int64) float64 {
	if updatedMs <= 0 || nowMs <= updatedMs {
		if updatedMs > 0 {
			return 1.0
		}
		return 0
	}
	ageDays := float64(nowMs-updatedMs) / float64(24*time.Hour/time.Millisecond)
	return 1.0 / (1.0 + ageDays/30.0)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
