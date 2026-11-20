package adapters

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type codexAdapter struct{}

// NewCodexAdapter returns the native conversation store adapter for Codex CLI.
func NewCodexAdapter() convstore.Adapter {
	return &codexAdapter{}
}

func (a *codexAdapter) Agent() string { return "codex" }

func (a *codexAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}

	type rootEntry struct {
		dir     string
		profile string
	}
	var roots []rootEntry

	defaultDir := filepath.Join(env.Paths.Home, ".codex")
	if ch := strings.TrimSpace(os.Getenv("CODEX_HOME")); ch != "" && sqliteAdapterIsOSHome(env.Paths.Home) {
		defaultDir = ch
	}
	roots = append(roots, rootEntry{dir: defaultDir, profile: ""})

	for _, ph := range env.ProfileShadowHomes("codex") {
		roots = append(roots,
			rootEntry{dir: filepath.Join(ph.ShadowHome, ".codex"), profile: ph.Profile},
			rootEntry{dir: ph.ShadowHome, profile: ph.Profile},
		)
	}

	var sources []convstore.DiscoveredSource
	for _, root := range roots {
		if ctx.Err() != nil {
			return sources, ctx.Err()
		}
		if fi, err := os.Stat(root.dir); err != nil || !fi.IsDir() {
			continue
		}

		trackedRollouts := make(map[string]bool)
		if dbPath := codexFindStateSQLite(root.dir); dbPath != "" {
			if size, mtime, walSize, walMtime, ok := StatSQLite(dbPath); ok && env.MarkRealPath(dbPath) {
				sources = append(sources, convstore.DiscoveredSource{
					SourceKey:  convstore.MakeSourceKey("codex", root.profile, dbPath),
					Agent:      "codex",
					Profile:    root.profile,
					Path:       filepath.Clean(dbPath),
					Kind:       convstore.SourceSQLite,
					SizeBytes:  size,
					MtimeNs:    mtime,
					WALSize:    walSize,
					WALMtimeNs: walMtime,
				})
				codexCollectTrackedRollouts(ctx, dbPath, trackedRollouts)
			}
		}

		// Discover rollout JSONL files as fallback when no state_*.sqlite exists or for rollouts not in DB.
		for _, sub := range []string{"sessions", "archived_sessions"} {
			subDir := filepath.Join(root.dir, sub)
			if fi, err := os.Stat(subDir); err != nil || !fi.IsDir() {
				continue
			}
			_ = filepath.WalkDir(subDir, func(path string, d os.DirEntry, err error) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil || d.IsDir() {
					return nil
				}
				name := d.Name()
				if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
					return nil
				}
				cleanPath := filepath.Clean(path)
				realPath := cleanPath
				if resolved, rerr := filepath.EvalSymlinks(cleanPath); rerr == nil && resolved != "" {
					realPath = filepath.Clean(resolved)
				}
				if trackedRollouts[cleanPath] || trackedRollouts[realPath] {
					return nil
				}
				if !env.MarkRealPath(cleanPath) {
					return nil
				}
				size, mtime, ok := convstore.StatFile(cleanPath)
				if !ok {
					return nil
				}
				sources = append(sources, convstore.DiscoveredSource{
					SourceKey: convstore.MakeSourceKey("codex", root.profile, cleanPath),
					Agent:     "codex",
					Profile:   root.profile,
					Path:      cleanPath,
					Kind:      convstore.SourceFile,
					SizeBytes: size,
					MtimeNs:   mtime,
				})
				return nil
			})
		}
	}
	return sources, nil
}

func codexFindStateSQLite(rootDir string) string {
	matches, err := filepath.Glob(filepath.Join(rootDir, "state_*.sqlite"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	type cand struct {
		path string
		num  int
	}
	cands := make([]cand, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		stem := strings.TrimSuffix(strings.TrimPrefix(base, "state_"), ".sqlite")
		n, err := strconv.Atoi(stem)
		if err != nil {
			n = -1
		}
		cands = append(cands, cand{path: m, num: n})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].num != cands[j].num {
			return cands[i].num > cands[j].num
		}
		return cands[i].path > cands[j].path
	})
	return cands[0].path
}

func codexCollectTrackedRollouts(ctx context.Context, dbPath string, out map[string]bool) {
	db, err := OpenReadOnly(dbPath)
	if err != nil {
		return
	}
	defer db.Close()
	if !TableExists(ctx, db, "threads") || !ColumnExists(ctx, db, "threads", "rollout_path") {
		return
	}
	rows, err := db.QueryContext(ctx, `SELECT rollout_path FROM threads WHERE rollout_path IS NOT NULL AND rollout_path != ''`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var p sql.NullString
		if err := rows.Scan(&p); err == nil && p.Valid && p.String != "" {
			clean := filepath.Clean(p.String)
			out[clean] = true
			if resolved, rerr := filepath.EvalSymlinks(clean); rerr == nil && resolved != "" {
				out[filepath.Clean(resolved)] = true
			}
		}
	}
}

func (a *codexAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if src.Kind == convstore.SourceSQLite {
		return codexParseSQLiteMetadata(ctx, src)
	}
	return codexParseRolloutMetadata(ctx, src)
}

func codexParseSQLiteMetadata(ctx context.Context, src convstore.DiscoveredSource) (*convstore.MetadataResult, error) {
	db, err := OpenReadOnly(src.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if !TableExists(ctx, db, "threads") {
		return &convstore.MetadataResult{}, nil
	}

	threadNames := codexReadSessionIndex(filepath.Join(filepath.Dir(src.Path), "session_index.jsonl"))
	childIDs := codexLoadSpawnedChildren(ctx, db)

	colOrNull := func(col string) string {
		if ColumnExists(ctx, db, "threads", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}

	query := `SELECT ` +
		colOrNull("id") + `, ` +
		colOrNull("rollout_path") + `, ` +
		colOrNull("created_at") + `, ` +
		colOrNull("updated_at") + `, ` +
		colOrNull("cwd") + `, ` +
		colOrNull("title") + `, ` +
		colOrNull("first_user_message") + `, ` +
		colOrNull("archived") + `, ` +
		colOrNull("git_branch") + `, ` +
		colOrNull("model_provider") + `, ` +
		colOrNull("model") + `, ` +
		colOrNull("parent_thread_id") +
		` FROM threads`

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
			idRaw, rolloutRaw, createdRaw, updatedRaw, cwdRaw any
			titleRaw, firstMsgRaw, archivedRaw, branchRaw     any
			providerRaw, modelRaw, parentRaw                  any
		)
		if err := rows.Scan(
			&idRaw, &rolloutRaw, &createdRaw, &updatedRaw, &cwdRaw,
			&titleRaw, &firstMsgRaw, &archivedRaw, &branchRaw,
			&providerRaw, &modelRaw, &parentRaw,
		); err != nil {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" || childIDs[id] {
			continue
		}
		if strings.TrimSpace(sqliteAdapterString(parentRaw)) != "" {
			continue
		}

		rolloutPath := strings.TrimSpace(sqliteAdapterString(rolloutRaw))
		cwd := strings.TrimSpace(sqliteAdapterString(cwdRaw))
		branch := strings.TrimSpace(sqliteAdapterString(branchRaw))
		model := sqliteAdapterFirstNonEmpty(
			strings.TrimSpace(sqliteAdapterString(modelRaw)),
			strings.TrimSpace(sqliteAdapterString(providerRaw)),
		)
		indexedName := convstore.CleanTitle(threadNames[id])
		rawTitle := convstore.CleanTitle(sqliteAdapterString(titleRaw))
		rawFirst := sqliteAdapterString(firstMsgRaw)
		firstTitle := convstore.CleanTitle(rawFirst)

		title := sqliteAdapterFirstNonEmpty(indexedName, rawTitle, firstTitle)
		preview := convstore.CleanPreview(sqliteAdapterFirstNonEmpty(rawFirst, rawTitle, indexedName))

		createdAtMs := sqliteAdapterTimestampMs(createdRaw)
		updatedAtMs := sqliteAdapterTimestampMs(updatedRaw)
		if updatedAtMs == 0 {
			updatedAtMs = createdAtMs
		}
		if updatedAtMs == 0 && src.MtimeNs > 0 {
			updatedAtMs = src.MtimeNs / 1e6
		}

		sourcePath := src.Path
		if rolloutPath != "" {
			sourcePath = filepath.Clean(rolloutPath)
		}

		convs = append(convs, convstore.Conversation{
			Agent:       "codex",
			ID:          id,
			App:         convstore.DisplayApp("codex"),
			Profile:     src.Profile,
			Title:       title,
			Preview:     preview,
			CWD:         cwd,
			Branch:      branch,
			Model:       model,
			CreatedAtMs: createdAtMs,
			UpdatedAtMs: updatedAtMs,
			Archived:    sqliteAdapterBool(archivedRaw),
			SourceKey:   src.SourceKey,
			SourcePath:  sourcePath,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &convstore.MetadataResult{Conversations: convs}, nil
}

func codexLoadSpawnedChildren(ctx context.Context, db *sql.DB) map[string]bool {
	children := make(map[string]bool)
	if !TableExists(ctx, db, "thread_spawn_edges") {
		return children
	}
	var childCol string
	for _, cand := range []string{"child_thread_id", "child_id", "spawned_thread_id", "target_thread_id", "to_thread_id"} {
		if ColumnExists(ctx, db, "thread_spawn_edges", cand) {
			childCol = cand
			break
		}
	}
	if childCol == "" {
		return children
	}
	rows, err := db.QueryContext(ctx, `SELECT "`+childCol+`" FROM thread_spawn_edges WHERE "`+childCol+`" IS NOT NULL AND "`+childCol+`" != ''`)
	if err != nil {
		return children
	}
	defer rows.Close()
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err == nil && id.Valid && id.String != "" {
			children[id.String] = true
		}
	}
	return children
}

func codexReadSessionIndex(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	names := make(map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec == nil {
			continue
		}
		id := sqliteAdapterFirstNonEmpty(
			sqliteAdapterString(rec["id"]),
			sqliteAdapterString(rec["session_id"]),
			sqliteAdapterString(rec["thread_id"]),
		)
		name := sqliteAdapterFirstNonEmpty(
			sqliteAdapterString(rec["thread_name"]),
			sqliteAdapterString(rec["title"]),
			sqliteAdapterString(rec["name"]),
		)
		if id != "" && name != "" {
			names[id] = name
		}
	}
	return names
}

func codexParseRolloutMetadata(ctx context.Context, src convstore.DiscoveredSource) (*convstore.MetadataResult, error) {
	head, tail, err := convstore.ReadHeadTailLines(src.Path, convstore.DefaultHeadBytes, convstore.DefaultTailBytes, 60, 60)
	if err != nil {
		return nil, err
	}
	if len(head) == 0 && len(tail) == 0 {
		return &convstore.MetadataResult{}, nil
	}

	var (
		sessionID     string
		cwd           string
		branch        string
		model         string
		explicitTitle string
		firstPrompt   string
		createdAtMs   int64
		updatedAtMs   int64
		msgCount      int
		isSubagent    bool
	)

	processLine := func(line string) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec == nil {
			return
		}
		if ts := sqliteAdapterTimestampMs(rec["timestamp"]); ts > 0 {
			if createdAtMs == 0 || ts < createdAtMs {
				createdAtMs = ts
			}
			if ts > updatedAtMs {
				updatedAtMs = ts
			}
		}

		payload, _ := rec["payload"].(map[string]any)
		typ, _ := rec["type"].(string)

		switch typ {
		case "session_meta":
			if payload == nil {
				return
			}
			if sqliteAdapterBool(payload["is_subagent"]) ||
				strings.TrimSpace(sqliteAdapterString(payload["parent_thread_id"])) != "" ||
				strings.EqualFold(sqliteAdapterString(payload["source"]), "subagent") {
				isSubagent = true
				return
			}
			if s := sqliteAdapterFirstNonEmpty(sqliteAdapterString(payload["session_id"]), sqliteAdapterString(payload["id"])); s != "" {
				sessionID = s
			}
			if s := sqliteAdapterString(payload["cwd"]); s != "" {
				cwd = s
			}
			if s := sqliteAdapterFirstNonEmpty(sqliteAdapterString(payload["model"]), sqliteAdapterString(payload["model_provider"])); s != "" {
				model = s
			}
			if gitObj, ok := payload["git"].(map[string]any); ok {
				if b := sqliteAdapterString(gitObj["branch"]); b != "" {
					branch = b
				}
			}
			if branch == "" {
				if b := sqliteAdapterString(payload["git_branch"]); b != "" {
					branch = b
				}
			}
			for _, k := range []string{"thread_name", "title", "name", "summary"} {
				if t := convstore.CleanTitle(sqliteAdapterString(payload[k])); t != "" {
					explicitTitle = t
					break
				}
			}
			if ts := sqliteAdapterTimestampMs(payload["timestamp"]); ts > 0 {
				if createdAtMs == 0 || ts < createdAtMs {
					createdAtMs = ts
				}
				if ts > updatedAtMs {
					updatedAtMs = ts
				}
			}
		default:
			if payload == nil {
				return
			}
			role, text := codexExtractMessageFromRecord(typ, payload)
			if role != "" && text != "" {
				msgCount++
				if role == "user" && firstPrompt == "" {
					if t := convstore.CleanTitle(text); t != "" {
						firstPrompt = t
					}
				}
			}
			if m := sqliteAdapterString(payload["model"]); m != "" {
				model = m
			}
		}
	}

	for _, line := range head {
		processLine(line)
	}
	for _, line := range tail {
		processLine(line)
	}

	if isSubagent {
		return &convstore.MetadataResult{}, nil
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	}
	if sessionID == "" {
		return &convstore.MetadataResult{}, nil
	}

	title := sqliteAdapterFirstNonEmpty(explicitTitle, firstPrompt)
	preview := convstore.CleanPreview(sqliteAdapterFirstNonEmpty(firstPrompt, explicitTitle))

	if updatedAtMs == 0 {
		updatedAtMs = createdAtMs
	}
	if updatedAtMs == 0 && src.MtimeNs > 0 {
		updatedAtMs = src.MtimeNs / 1e6
	}
	if updatedAtMs == 0 {
		if _, mtime, ok := convstore.StatFile(src.Path); ok {
			updatedAtMs = mtime / 1e6
		}
	}

	archived := strings.Contains(filepath.ToSlash(src.Path), "/archived_sessions/")

	return &convstore.MetadataResult{
		Conversations: []convstore.Conversation{{
			Agent:        "codex",
			ID:           sessionID,
			App:          convstore.DisplayApp("codex"),
			Profile:      src.Profile,
			Title:        title,
			Preview:      preview,
			CWD:          cwd,
			Branch:       branch,
			Model:        model,
			CreatedAtMs:  createdAtMs,
			UpdatedAtMs:  updatedAtMs,
			MessageCount: msgCount,
			Archived:     archived,
			SourceKey:    src.SourceKey,
			SourcePath:   src.Path,
		}},
	}, nil
}

func (a *codexAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if src.Kind == convstore.SourceSQLite {
		return codexParseSQLiteMessages(ctx, src, prev)
	}
	return codexParseRolloutMessages(ctx, src, prev)
}

func codexParseSQLiteMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	db, err := OpenReadOnly(src.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	res := &convstore.MessagesResult{
		Messages: make(map[string][]convstore.Message),
	}
	if !TableExists(ctx, db, "threads") || !ColumnExists(ctx, db, "threads", "rollout_path") {
		return res, nil
	}

	childIDs := codexLoadSpawnedChildren(ctx, db)
	parentExpr := "NULL"
	if ColumnExists(ctx, db, "threads", "parent_thread_id") {
		parentExpr = `"parent_thread_id"`
	}
	updatedExpr := "NULL"
	if ColumnExists(ctx, db, "threads", "updated_at") {
		updatedExpr = `"updated_at"`
	}

	rows, err := db.QueryContext(ctx, `SELECT id, rollout_path, `+updatedExpr+`, `+parentExpr+` FROM threads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type target struct {
		id          string
		rolloutPath string
	}
	var targets []target
	for rows.Next() {
		var idRaw, rolloutRaw, updatedRaw, parentRaw any
		if err := rows.Scan(&idRaw, &rolloutRaw, &updatedRaw, &parentRaw); err != nil {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		rolloutPath := strings.TrimSpace(sqliteAdapterString(rolloutRaw))
		if id == "" || rolloutPath == "" || childIDs[id] {
			continue
		}
		if strings.TrimSpace(sqliteAdapterString(parentRaw)) != "" {
			continue
		}
		updatedAtMs := sqliteAdapterTimestampMs(updatedRaw)
		if !sqliteAdapterIsUpdated(prev, id, updatedAtMs) {
			continue
		}
		targets = append(targets, target{id: id, rolloutPath: rolloutPath})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, t := range targets {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		msgs, _, _, _, rerr := codexStreamRolloutFile(ctx, t.rolloutPath, 0, "")
		if rerr != nil {
			continue
		}
		if len(msgs) > 0 {
			res.Messages[t.id] = msgs
		}
	}
	return res, nil
}

func codexParseRolloutMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	var prevOff int64
	var prevHash string
	if prev != nil {
		prevOff = prev.MsgByteOffset
		prevHash = prev.MsgPrefixHash
	}

	var (
		sessionID  string
		isSubagent bool
		msgs       []convstore.Message
	)

	newOff, newHash, appended, err := convstore.ReadIncrementalJSONL(
		src.Path,
		prevOff,
		prevHash,
		convstore.DefaultMaxJSONLLineBytes,
		func(line []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil || rec == nil {
				return nil
			}
			typ, _ := rec["type"].(string)
			payload, _ := rec["payload"].(map[string]any)
			if payload == nil {
				return nil
			}
			if typ == "session_meta" {
				if sqliteAdapterBool(payload["is_subagent"]) ||
					strings.TrimSpace(sqliteAdapterString(payload["parent_thread_id"])) != "" ||
					strings.EqualFold(sqliteAdapterString(payload["source"]), "subagent") {
					isSubagent = true
					return nil
				}
				if s := sqliteAdapterFirstNonEmpty(sqliteAdapterString(payload["session_id"]), sqliteAdapterString(payload["id"])); s != "" {
					sessionID = s
				}
				return nil
			}

			role, rawText := codexExtractMessageFromRecord(typ, payload)
			if role == "" || rawText == "" {
				return nil
			}
			cleaned, truncated := convstore.CleanMessageText(rawText)
			if cleaned == "" {
				return nil
			}
			// Deduplicate identical consecutive message emitted by both event_msg and response_item.
			if n := len(msgs); n > 0 && msgs[n-1].Role == role && msgs[n-1].Text == cleaned {
				return nil
			}
			tsMs := sqliteAdapterTimestampMs(rec["timestamp"])
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
	if err != nil {
		return nil, err
	}

	out := &convstore.MessagesResult{
		Messages:   make(map[string][]convstore.Message),
		Append:     appended,
		ByteOffset: newOff,
		PrefixHash: newHash,
	}
	if isSubagent {
		return out, nil
	}
	if sessionID == "" && prev != nil && len(prev.ExistingUpdatedMs) == 1 {
		for id := range prev.ExistingUpdatedMs {
			sessionID = id
		}
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	}
	if sessionID != "" && len(msgs) > 0 {
		out.Messages[sessionID] = msgs
	}
	return out, nil
}

func codexStreamRolloutFile(ctx context.Context, path string, prevOff int64, prevHash string) ([]convstore.Message, int64, string, bool, error) {
	var msgs []convstore.Message
	newOff, newHash, appended, err := convstore.ReadIncrementalJSONL(
		path,
		prevOff,
		prevHash,
		convstore.DefaultMaxJSONLLineBytes,
		func(line []byte) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil || rec == nil {
				return nil
			}
			typ, _ := rec["type"].(string)
			payload, _ := rec["payload"].(map[string]any)
			if payload == nil {
				return nil
			}
			role, rawText := codexExtractMessageFromRecord(typ, payload)
			if role == "" || rawText == "" {
				return nil
			}
			cleaned, truncated := convstore.CleanMessageText(rawText)
			if cleaned == "" {
				return nil
			}
			if n := len(msgs); n > 0 && msgs[n-1].Role == role && msgs[n-1].Text == cleaned {
				return nil
			}
			tsMs := sqliteAdapterTimestampMs(rec["timestamp"])
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
	return msgs, newOff, newHash, appended, err
}

func codexExtractMessageFromRecord(recType string, payload map[string]any) (string, string) {
	if payload == nil {
		return "", ""
	}
	if ptyp, _ := payload["type"].(string); ptyp == "user_message" {
		if raw, ok := payload["message"]; ok {
			if txt := convstore.ExtractPlainText(raw); txt != "" {
				return "user", txt
			}
		}
		if raw, ok := payload["text"]; ok {
			if txt := convstore.ExtractPlainText(raw); txt != "" {
				return "user", txt
			}
		}
		return "", ""
	}
	if recType == "response_item" {
		role := sqliteAdapterNormalizeRole(sqliteAdapterString(payload["role"]))
		if role == "" {
			return "", ""
		}
		if raw, ok := payload["content"]; ok {
			return role, convstore.ExtractPlainText(raw)
		}
	}
	return "", ""
}
