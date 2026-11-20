package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type museAdapter struct{}

// NewMuseAdapter returns the native conversation store adapter for Meta Muse.
func NewMuseAdapter() convstore.Adapter {
	return &museAdapter{}
}

func (a *museAdapter) Agent() string { return "muse" }

func (a *museAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}

	type candidate struct {
		path    string
		profile string
	}
	dataHome := sqliteAdapterDataHome(env)
	cands := []candidate{
		{path: filepath.Join(dataHome, "muse", "session-index.db"), profile: ""},
	}
	for _, ph := range env.ProfileShadowHomes("muse") {
		cands = append(cands,
			candidate{path: filepath.Join(ph.ShadowHome, ".local", "share", "muse", "session-index.db"), profile: ph.Profile},
			candidate{path: filepath.Join(ph.ShadowHome, "muse", "session-index.db"), profile: ph.Profile},
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
			SourceKey:  convstore.MakeSourceKey("muse", c.profile, c.path),
			Agent:      "muse",
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

func (a *museAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	db, err := OpenReadOnly(src.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if !TableExists(ctx, db, "sessions") {
		return &convstore.MetadataResult{}, nil
	}

	colOrNull := func(col string) string {
		if ColumnExists(ctx, db, "sessions", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}

	query := `SELECT ` +
		colOrNull("session_id") + `, ` +
		colOrNull("workspace_root") + `, ` +
		colOrNull("git_branch") + `, ` +
		colOrNull("session_name") + `, ` +
		colOrNull("effective_long_title") + `, ` +
		colOrNull("title") + `, ` +
		colOrNull("msp_title") + `, ` +
		colOrNull("first_user_prompt") + `, ` +
		colOrNull("msp_first_user_prompt") + `, ` +
		colOrNull("created_at_us") + `, ` +
		colOrNull("updated_at_us") + `, ` +
		colOrNull("session_log_path") + `, ` +
		colOrNull("message_count") + `, ` +
		colOrNull("msp_parent_session_id") + `, ` +
		colOrNull("parent_session_id") + `, ` +
		colOrNull("archived") + `, ` +
		colOrNull("model") +
		` FROM sessions`

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
			idRaw, wsRaw, branchRaw                              any
			sessNameRaw, longTitleRaw, titleRaw, mspTitleRaw     any
			firstPromptRaw, mspFirstPromptRaw                    any
			createdRaw, updatedRaw, logPathRaw, msgCountRaw      any
			mspParentRaw, parentRaw, archivedRaw, modelRaw       any
		)
		if err := rows.Scan(
			&idRaw, &wsRaw, &branchRaw,
			&sessNameRaw, &longTitleRaw, &titleRaw, &mspTitleRaw,
			&firstPromptRaw, &mspFirstPromptRaw,
			&createdRaw, &updatedRaw, &logPathRaw, &msgCountRaw,
			&mspParentRaw, &parentRaw, &archivedRaw, &modelRaw,
		); err != nil {
			continue
		}

		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" {
			continue
		}
		if strings.TrimSpace(sqliteAdapterString(mspParentRaw)) != "" ||
			strings.TrimSpace(sqliteAdapterString(parentRaw)) != "" {
			continue
		}

		var title string
		for _, cand := range []any{sessNameRaw, longTitleRaw, titleRaw, mspTitleRaw, firstPromptRaw, mspFirstPromptRaw} {
			if t := convstore.CleanTitle(sqliteAdapterString(cand)); t != "" {
				title = t
				break
			}
		}
		preview := convstore.CleanPreview(sqliteAdapterFirstNonEmpty(
			sqliteAdapterString(firstPromptRaw),
			sqliteAdapterString(mspFirstPromptRaw),
			title,
		))

		createdAtMs := sqliteAdapterTimestampMs(createdRaw)
		updatedAtMs := sqliteAdapterTimestampMs(updatedRaw)
		if updatedAtMs == 0 {
			updatedAtMs = createdAtMs
		}
		if updatedAtMs == 0 && src.MtimeNs > 0 {
			updatedAtMs = src.MtimeNs / 1e6
		}

		sourcePath := src.Path
		if lp := strings.TrimSpace(sqliteAdapterString(logPathRaw)); lp != "" {
			sourcePath = filepath.Clean(lp)
		}
		msgCount := int(sqliteAdapterInt(msgCountRaw))
		if msgCount < 0 {
			msgCount = 0
		}

		convs = append(convs, convstore.Conversation{
			Agent:        "muse",
			ID:           id,
			App:          convstore.DisplayApp("muse"),
			Profile:      src.Profile,
			Title:        title,
			Preview:      preview,
			CWD:          strings.TrimSpace(sqliteAdapterString(wsRaw)),
			Branch:       strings.TrimSpace(sqliteAdapterString(branchRaw)),
			Model:        strings.TrimSpace(sqliteAdapterString(modelRaw)),
			CreatedAtMs:  createdAtMs,
			UpdatedAtMs:  updatedAtMs,
			MessageCount: msgCount,
			Archived:     sqliteAdapterBool(archivedRaw),
			SourceKey:    src.SourceKey,
			SourcePath:   sourcePath,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &convstore.MetadataResult{Conversations: convs}, nil
}

func (a *museAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
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
	if !TableExists(ctx, db, "sessions") {
		return res, nil
	}

	colOrNull := func(col string) string {
		if ColumnExists(ctx, db, "sessions", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}

	query := `SELECT ` +
		colOrNull("session_id") + `, ` +
		colOrNull("updated_at_us") + `, ` +
		colOrNull("session_log_path") + `, ` +
		colOrNull("search_text") + `, ` +
		colOrNull("first_user_prompt") + `, ` +
		colOrNull("msp_first_user_prompt") + `, ` +
		colOrNull("msp_parent_session_id") + `, ` +
		colOrNull("parent_session_id") +
		` FROM sessions`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type target struct {
		id          string
		updatedAtMs int64
		logPath     string
		searchText  string
		firstPrompt string
	}
	var targets []target
	for rows.Next() {
		var (
			idRaw, updatedRaw, logPathRaw, searchRaw   any
			firstPromptRaw, mspFirstRaw                any
			mspParentRaw, parentRaw                    any
		)
		if err := rows.Scan(
			&idRaw, &updatedRaw, &logPathRaw, &searchRaw,
			&firstPromptRaw, &mspFirstRaw,
			&mspParentRaw, &parentRaw,
		); err != nil {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" {
			continue
		}
		if strings.TrimSpace(sqliteAdapterString(mspParentRaw)) != "" ||
			strings.TrimSpace(sqliteAdapterString(parentRaw)) != "" {
			continue
		}
		updatedAtMs := sqliteAdapterTimestampMs(updatedRaw)
		if !sqliteAdapterIsUpdated(prev, id, updatedAtMs) {
			continue
		}
		targets = append(targets, target{
			id:          id,
			updatedAtMs: updatedAtMs,
			logPath:     strings.TrimSpace(sqliteAdapterString(logPathRaw)),
			searchText:  sqliteAdapterString(searchRaw),
			firstPrompt: sqliteAdapterFirstNonEmpty(sqliteAdapterString(firstPromptRaw), sqliteAdapterString(mspFirstRaw)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, t := range targets {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var msgs []convstore.Message
		if t.logPath != "" {
			if fi, err := os.Stat(t.logPath); err == nil && !fi.IsDir() {
				msgs = museReadLogJSONL(ctx, t.logPath)
			}
		}
		if len(msgs) == 0 {
			fallback := sqliteAdapterFirstNonEmpty(t.searchText, t.firstPrompt)
			for _, chunk := range sqliteAdapterSplitChunks(fallback, convstore.MaxMessageTextBytes) {
				cleaned, truncated := convstore.CleanMessageText(chunk)
				if cleaned == "" {
					continue
				}
				msgs = append(msgs, convstore.Message{
					Seq:       len(msgs),
					Role:      "user",
					Text:      cleaned,
					TsMs:      t.updatedAtMs,
					Truncated: truncated,
				})
			}
		}
		if len(msgs) > 0 {
			res.Messages[t.id] = msgs
		}
	}
	return res, nil
}

func museReadLogJSONL(ctx context.Context, path string) []convstore.Message {
	var msgs []convstore.Message
	_, _, _, _ = convstore.ReadIncrementalJSONL(
		path,
		0,
		"",
		convstore.DefaultMaxJSONLLineBytes,
		func(line []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil || rec == nil {
				return nil
			}
			role := sqliteAdapterNormalizeRole(sqliteAdapterString(rec["role"]))
			if role == "" {
				role = sqliteAdapterNormalizeRole(sqliteAdapterString(rec["type"]))
			}
			var content any
			if msg, ok := rec["message"].(map[string]any); ok {
				if role == "" {
					role = sqliteAdapterNormalizeRole(sqliteAdapterString(msg["role"]))
				}
				if c, exists := msg["content"]; exists {
					content = c
				} else if p, exists := msg["parts"]; exists {
					content = p
				}
			}
			if content == nil {
				if payload, ok := rec["payload"].(map[string]any); ok {
					if role == "" {
						role = sqliteAdapterNormalizeRole(sqliteAdapterString(payload["role"]))
					}
					if c, exists := payload["content"]; exists {
						content = c
					} else if t, exists := payload["text"]; exists {
						content = t
					}
				}
			}
			if content == nil {
				for _, k := range []string{"content", "text", "parts"} {
					if c, exists := rec[k]; exists {
						content = c
						break
					}
				}
			}
			if role == "" || content == nil {
				return nil
			}
			rawText := convstore.ExtractPlainText(content)
			cleaned, truncated := convstore.CleanMessageText(rawText)
			if cleaned == "" {
				return nil
			}
			tsMs := sqliteAdapterTimestampMs(rec["timestamp"])
			if tsMs == 0 {
				tsMs = sqliteAdapterTimestampMs(rec["ts"])
			}
			if tsMs == 0 {
				tsMs = sqliteAdapterTimestampMs(rec["created_at"])
			}
			msgs = append(msgs, convstore.Message{
				Seq:       len(msgs),
				Role:      role,
				Text:      cleaned,
				TsMs:      tsMs,
				Truncated: truncated,
			})
			return nil
		},
	)
	return msgs
}
