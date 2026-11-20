package adapters

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type antigravityAdapter struct{}

// NewAntigravityAdapter returns the native conversation store adapter for Antigravity CLI.
func NewAntigravityAdapter() convstore.Adapter {
	return &antigravityAdapter{}
}

func (a *antigravityAdapter) Agent() string { return "antigravity" }

func (a *antigravityAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}

	type candidate struct {
		path    string
		profile string
	}
	cands := []candidate{
		{
			path:    filepath.Join(env.Paths.Home, ".gemini", "antigravity-cli", "conversation_summaries.db"),
			profile: "",
		},
	}
	for _, ph := range env.ProfileShadowHomes("antigravity") {
		cands = append(cands, candidate{
			path:    filepath.Join(ph.ShadowHome, ".gemini", "antigravity-cli", "conversation_summaries.db"),
			profile: ph.Profile,
		})
	}

	var sources []convstore.DiscoveredSource
	for _, c := range cands {
		if ctx.Err() != nil {
			return sources, ctx.Err()
		}
		size, mtime, walSize, walMtime, ok := StatSQLite(c.path)
		if !ok {
			continue
		}
		if !env.MarkRealPath(c.path) {
			continue
		}
		sources = append(sources, convstore.DiscoveredSource{
			SourceKey:  convstore.MakeSourceKey("antigravity", c.profile, c.path),
			Agent:      "antigravity",
			Profile:    c.profile,
			Path:       filepath.Clean(c.path),
			Kind:       convstore.SourceSQLite,
			SizeBytes:  size,
			MtimeNs:    mtime,
			WALSize:    walSize,
			WALMtimeNs: walMtime,
		})
	}
	return sources, nil
}

func (a *antigravityAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	db, err := OpenReadOnly(src.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if !TableExists(ctx, db, "conversation_summaries") {
		return &convstore.MetadataResult{}, nil
	}

	colOrNull := func(col string) string {
		if ColumnExists(ctx, db, "conversation_summaries", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}

	query := `SELECT ` +
		colOrNull("conversation_id") + `, ` +
		colOrNull("title") + `, ` +
		colOrNull("preview") + `, ` +
		colOrNull("workspace_uris") + `, ` +
		colOrNull("created_time") + `, ` +
		colOrNull("last_modified_time") + `, ` +
		colOrNull("step_count") + `, ` +
		colOrNull("parent_conversation_id") + `, ` +
		colOrNull("parent_id") + `, ` +
		colOrNull("is_subagent") + `, ` +
		colOrNull("nesting_depth") + `, ` +
		colOrNull("agent_name") +
		` FROM conversation_summaries`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convs []convstore.Conversation
	for rows.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var (
			idRaw, titleRaw, previewRaw, urisRaw any
			createdRaw, modifiedRaw, stepsRaw    any
			parentConvRaw, parentIDRaw, isSubRaw any
			nestingRaw, agentNameRaw             any
		)
		if err := rows.Scan(
			&idRaw, &titleRaw, &previewRaw, &urisRaw,
			&createdRaw, &modifiedRaw, &stepsRaw,
			&parentConvRaw, &parentIDRaw, &isSubRaw,
			&nestingRaw, &agentNameRaw,
		); err != nil {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" {
			continue
		}
		if strings.TrimSpace(sqliteAdapterString(parentConvRaw)) != "" ||
			strings.TrimSpace(sqliteAdapterString(parentIDRaw)) != "" ||
			sqliteAdapterBool(isSubRaw) ||
			sqliteAdapterInt(nestingRaw) > 0 ||
			strings.TrimSpace(sqliteAdapterString(agentNameRaw)) != "" {
			continue
		}

		rawTitle := sqliteAdapterString(titleRaw)
		rawPreview := sqliteAdapterString(previewRaw)
		title := convstore.CleanTitle(rawTitle)
		if title == "" {
			title = convstore.CleanTitle(rawPreview)
		}
		preview := convstore.CleanPreview(sqliteAdapterFirstNonEmpty(rawPreview, rawTitle))
		steps := int(sqliteAdapterInt(stepsRaw))
		if steps < 0 {
			steps = 0
		}
		if title == "" && preview == "" && steps == 0 {
			continue
		}
		cwd := antigravityParseWorkspaceURI(sqliteAdapterString(urisRaw))

		createdAtMs := sqliteAdapterTimestampMs(createdRaw)
		updatedAtMs := sqliteAdapterTimestampMs(modifiedRaw)
		if updatedAtMs == 0 {
			updatedAtMs = createdAtMs
		}
		if updatedAtMs == 0 && src.MtimeNs > 0 {
			updatedAtMs = src.MtimeNs / 1e6
		}

		convs = append(convs, convstore.Conversation{
			Agent:        "antigravity",
			ID:           id,
			App:          convstore.DisplayApp("antigravity"),
			Profile:      src.Profile,
			Title:        title,
			Preview:      preview,
			CWD:          cwd,
			CreatedAtMs:  createdAtMs,
			UpdatedAtMs:  updatedAtMs,
			MessageCount: steps,
			SourceKey:    src.SourceKey,
			SourcePath:   src.Path,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &convstore.MetadataResult{Conversations: convs}, nil
}

func (a *antigravityAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	db, err := OpenReadOnly(src.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	res := &convstore.MessagesResult{
		Messages: make(map[string][]convstore.Message),
	}
	if !TableExists(ctx, db, "conversations_fts_content") {
		return res, nil
	}

	// Load valid (non-subagent) conversations and their timestamps from conversation_summaries.
	hasSummaries := TableExists(ctx, db, "conversation_summaries")
	validUpdatedMs := make(map[string]int64)
	if hasSummaries {
		colOrNull := func(col string) string {
			if ColumnExists(ctx, db, "conversation_summaries", col) {
				return `"` + col + `"`
			}
			return "NULL"
		}
		sQuery := `SELECT ` +
			colOrNull("conversation_id") + `, ` +
			colOrNull("last_modified_time") + `, ` +
			colOrNull("parent_conversation_id") + `, ` +
			colOrNull("parent_id") + `, ` +
			colOrNull("is_subagent") +
			` FROM conversation_summaries`
		srows, err := db.QueryContext(ctx, sQuery)
		if err == nil {
			for srows.Next() {
				var idRaw, modRaw, parentConvRaw, parentIDRaw, isSubRaw any
				if err := srows.Scan(&idRaw, &modRaw, &parentConvRaw, &parentIDRaw, &isSubRaw); err != nil {
					continue
				}
				id := strings.TrimSpace(sqliteAdapterString(idRaw))
				if id == "" {
					continue
				}
				if strings.TrimSpace(sqliteAdapterString(parentConvRaw)) != "" ||
					strings.TrimSpace(sqliteAdapterString(parentIDRaw)) != "" ||
					sqliteAdapterBool(isSubRaw) {
					continue
				}
				validUpdatedMs[id] = sqliteAdapterTimestampMs(modRaw)
			}
			srows.Close()
		}
	}

	idCol, userCol, agentCol := antigravityFTSColumns(ctx, db)
	if idCol == "" || (userCol == "" && agentCol == "") {
		return res, nil
	}

	selectExpr := func(col string) string {
		if col == "" {
			return "NULL"
		}
		return `"` + strings.ReplaceAll(col, `"`, `""`) + `"`
	}

	query := `SELECT ` + selectExpr(idCol) + `, ` + selectExpr(userCol) + `, ` + selectExpr(agentCol) + ` FROM conversations_fts_content`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var idRaw, userRaw, agentRaw any
		if err := rows.Scan(&idRaw, &userRaw, &agentRaw); err != nil {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" {
			continue
		}

		var updatedAtMs int64
		if hasSummaries {
			ts, ok := validUpdatedMs[id]
			if !ok {
				// Skip subagents or orphaned FTS rows not present in valid summaries.
				continue
			}
			updatedAtMs = ts
		}
		if !sqliteAdapterIsUpdated(prev, id, updatedAtMs) {
			continue
		}

		var msgs []convstore.Message
		appendBody := func(role, body string) {
			for _, chunk := range sqliteAdapterSplitChunks(body, convstore.MaxMessageTextBytes) {
				cleaned, truncated := convstore.CleanMessageText(chunk)
				if cleaned == "" {
					continue
				}
				msgs = append(msgs, convstore.Message{
					Seq:       len(msgs),
					Role:      role,
					Text:      cleaned,
					TsMs:      updatedAtMs,
					Truncated: truncated,
				})
			}
		}

		appendBody("user", sqliteAdapterString(userRaw))
		appendBody("assistant", sqliteAdapterString(agentRaw))
		if len(msgs) > 0 {
			res.Messages[id] = msgs
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

func antigravityFTSColumns(ctx context.Context, db *sql.DB) (idCol, userCol, agentCol string) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info("conversations_fts_content")`)
	if err != nil {
		return "", "", ""
	}
	defer rows.Close()

	cols := make(map[string]string) // lowercase -> actual name
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
			cols[strings.ToLower(name)] = name
		}
	}

	pick := func(cands ...string) string {
		for _, c := range cands {
			if actual, ok := cols[c]; ok {
				return actual
			}
		}
		return ""
	}

	idCol = pick("conversation_id", "c0")
	userCol = pick("user_body", "c13")
	agentCol = pick("agent_body", "c14")
	return idCol, userCol, agentCol
}

func antigravityParseWorkspaceURI(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var uris []string
	if err := json.Unmarshal([]byte(raw), &uris); err != nil {
		uris = []string{raw}
	}
	for _, u := range uris {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if strings.HasPrefix(u, "file://") {
			if parsed, err := url.Parse(u); err == nil && parsed.Path != "" {
				return filepath.Clean(parsed.Path)
			}
			return filepath.Clean(strings.TrimPrefix(u, "file://"))
		}
		return filepath.Clean(u)
	}
	return ""
}
