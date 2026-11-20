package adapters

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type mcodeAdapter struct{}

// NewMcodeAdapter returns the native conversation store adapter for mcode.
func NewMcodeAdapter() convstore.Adapter {
	return &mcodeAdapter{}
}

func (a *mcodeAdapter) Agent() string { return "mcode" }

func (a *mcodeAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}

	type candidate struct {
		path    string
		profile string
	}
	cands := []candidate{
		{
			path:    filepath.Join(env.Paths.Home, ".minimax", "v2", "sqlite", "runtime-state.sqlite"),
			profile: "",
		},
	}
	for _, ph := range env.ProfileShadowHomes("mcode") {
		cands = append(cands,
			candidate{path: filepath.Join(ph.ShadowHome, ".minimax", "v2", "sqlite", "runtime-state.sqlite"), profile: ph.Profile},
			candidate{path: filepath.Join(ph.ShadowHome, "v2", "sqlite", "runtime-state.sqlite"), profile: ph.Profile},
		)
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
			SourceKey:  convstore.MakeSourceKey("mcode", c.profile, c.path),
			Agent:      "mcode",
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

func (a *mcodeAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	db, err := OpenReadOnly(src.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if !TableExists(ctx, db, "local_runtime_sessions") {
		return &convstore.MetadataResult{}, nil
	}

	colOrNull := func(col string) string {
		if ColumnExists(ctx, db, "local_runtime_sessions", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}

	query := `SELECT ` +
		colOrNull("session_id") + `, ` +
		colOrNull("workspace_dir") + `, ` +
		colOrNull("session_kind") + `, ` +
		colOrNull("title") + `, ` +
		colOrNull("purpose") + `, ` +
		colOrNull("record_json") + `, ` +
		colOrNull("created_at_ms") + `, ` +
		colOrNull("updated_at_ms") + `, ` +
		colOrNull("parent_session_id") + `, ` +
		colOrNull("hidden") + `, ` +
		colOrNull("archived") + `, ` +
		colOrNull("model") + `, ` +
		colOrNull("git_branch") + `, ` +
		colOrNull("message_count") +
		` FROM local_runtime_sessions`

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
			idRaw, wsRaw, kindRaw, titleRaw, purposeRaw, recRaw any
			createdRaw, updatedRaw, parentRaw, hiddenRaw        any
			archivedRaw, modelRaw, branchRaw, msgCountRaw       any
		)
		if err := rows.Scan(
			&idRaw, &wsRaw, &kindRaw, &titleRaw, &purposeRaw, &recRaw,
			&createdRaw, &updatedRaw, &parentRaw, &hiddenRaw,
			&archivedRaw, &modelRaw, &branchRaw, &msgCountRaw,
		); err != nil {
			continue
		}

		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(sqliteAdapterString(kindRaw)), "task") {
			continue
		}
		if strings.TrimSpace(sqliteAdapterString(parentRaw)) != "" || sqliteAdapterBool(hiddenRaw) {
			continue
		}

		var recMap map[string]any
		if rawJSON := strings.TrimSpace(sqliteAdapterString(recRaw)); rawJSON != "" {
			_ = json.Unmarshal([]byte(rawJSON), &recMap)
		}

		var title string
		for _, cand := range []string{sqliteAdapterString(titleRaw), sqliteAdapterString(purposeRaw)} {
			if t := convstore.CleanTitle(cand); t != "" {
				title = t
				break
			}
		}
		if title == "" && recMap != nil {
			for _, k := range []string{"title", "name", "summary", "purpose"} {
				if t := convstore.CleanTitle(sqliteAdapterString(recMap[k])); t != "" {
					title = t
					break
				}
			}
		}

		preview := convstore.CleanPreview(sqliteAdapterFirstNonEmpty(
			sqliteAdapterString(purposeRaw),
			title,
		))
		model := strings.TrimSpace(sqliteAdapterString(modelRaw))
		if model == "" && recMap != nil {
			model = strings.TrimSpace(sqliteAdapterString(recMap["model"]))
		}

		createdAtMs := sqliteAdapterTimestampMs(createdRaw)
		updatedAtMs := sqliteAdapterTimestampMs(updatedRaw)
		if updatedAtMs == 0 {
			updatedAtMs = createdAtMs
		}
		if updatedAtMs == 0 && src.MtimeNs > 0 {
			updatedAtMs = src.MtimeNs / 1e6
		}
		msgCount := int(sqliteAdapterInt(msgCountRaw))
		if msgCount < 0 {
			msgCount = 0
		}

		convs = append(convs, convstore.Conversation{
			Agent:        "mcode",
			ID:           id,
			App:          convstore.DisplayApp("mcode"),
			Profile:      src.Profile,
			Title:        title,
			Preview:      preview,
			CWD:          strings.TrimSpace(sqliteAdapterString(wsRaw)),
			Branch:       strings.TrimSpace(sqliteAdapterString(branchRaw)),
			Model:        model,
			CreatedAtMs:  createdAtMs,
			UpdatedAtMs:  updatedAtMs,
			MessageCount: msgCount,
			Archived:     sqliteAdapterBool(archivedRaw),
			SourceKey:    src.SourceKey,
			SourcePath:   src.Path,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &convstore.MetadataResult{Conversations: convs}, nil
}

func (a *mcodeAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
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
	if !TableExists(ctx, db, "local_runtime_sessions") || !TableExists(ctx, db, "local_runtime_message_rows") {
		return res, nil
	}

	sessColOrNull := func(col string) string {
		if ColumnExists(ctx, db, "local_runtime_sessions", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}

	targetSessions := make(map[string]bool)
	sQuery := `SELECT ` +
		sessColOrNull("session_id") + `, ` +
		sessColOrNull("session_kind") + `, ` +
		sessColOrNull("updated_at_ms") + `, ` +
		sessColOrNull("parent_session_id") + `, ` +
		sessColOrNull("hidden") +
		` FROM local_runtime_sessions`
	srows, err := db.QueryContext(ctx, sQuery)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var idRaw, kindRaw, updRaw, parentRaw, hiddenRaw any
		if err := srows.Scan(&idRaw, &kindRaw, &updRaw, &parentRaw, &hiddenRaw); err != nil {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(sqliteAdapterString(kindRaw)), "task") ||
			strings.TrimSpace(sqliteAdapterString(parentRaw)) != "" ||
			sqliteAdapterBool(hiddenRaw) {
			continue
		}
		if sqliteAdapterIsUpdated(prev, id, sqliteAdapterTimestampMs(updRaw)) {
			targetSessions[id] = true
		}
	}
	srows.Close()
	if len(targetSessions) == 0 {
		return res, nil
	}

	msgColOrNull := func(col string) string {
		if ColumnExists(ctx, db, "local_runtime_message_rows", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}
	orderBy := "rowid ASC"
	if ColumnExists(ctx, db, "local_runtime_message_rows", "created_at_ms") {
		orderBy = "created_at_ms ASC, rowid ASC"
	}

	mQuery := `SELECT ` +
		msgColOrNull("session_id") + `, ` +
		msgColOrNull("role") + `, ` +
		msgColOrNull("data_json") + `, ` +
		msgColOrNull("content") + `, ` +
		msgColOrNull("created_at_ms") +
		` FROM local_runtime_message_rows ORDER BY ` + orderBy

	mrows, err := db.QueryContext(ctx, mQuery)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()

	for mrows.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var sidRaw, roleRaw, dataRaw, contentRaw, tsRaw any
		if err := mrows.Scan(&sidRaw, &roleRaw, &dataRaw, &contentRaw, &tsRaw); err != nil {
			continue
		}
		sid := strings.TrimSpace(sqliteAdapterString(sidRaw))
		if !targetSessions[sid] {
			continue
		}
		role := sqliteAdapterNormalizeRole(sqliteAdapterString(roleRaw))
		rawPayload := sqliteAdapterFirstNonEmpty(
			strings.TrimSpace(sqliteAdapterString(dataRaw)),
			strings.TrimSpace(sqliteAdapterString(contentRaw)),
		)
		if rawPayload == "" {
			continue
		}

		var rawText string
		var parsed any
		if err := json.Unmarshal([]byte(rawPayload), &parsed); err == nil {
			if obj, ok := parsed.(map[string]any); ok && role == "" {
				role = sqliteAdapterNormalizeRole(sqliteAdapterString(obj["role"]))
			}
			rawText = convstore.ExtractPlainText(parsed)
		} else {
			rawText = rawPayload
		}
		if role == "" || rawText == "" {
			continue
		}

		cleaned, truncated := convstore.CleanMessageText(rawText)
		if cleaned == "" {
			continue
		}
		cur := res.Messages[sid]
		cur = append(cur, convstore.Message{
			Seq:       len(cur),
			Role:      role,
			Text:      cleaned,
			TsMs:      sqliteAdapterTimestampMs(tsRaw),
			Truncated: truncated,
		})
		res.Messages[sid] = cur
	}
	if err := mrows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}
