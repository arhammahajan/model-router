package adapters

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
)

type opencodeAdapter struct{}

// NewOpencodeAdapter returns the native conversation store adapter for OpenCode.
func NewOpencodeAdapter() convstore.Adapter {
	return &opencodeAdapter{}
}

func (a *opencodeAdapter) Agent() string { return "opencode" }

func (a *opencodeAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	return opencodeKiloDiscover(ctx, env, "opencode", "opencode.db")
}

func (a *opencodeAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
	return opencodeKiloParseMetadata(ctx, src, "opencode", []string{"session_v2", "session"})
}

func (a *opencodeAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
	return opencodeKiloParseMessages(ctx, src, prev, []string{"session_v2", "session"})
}

func opencodeKiloDiscover(ctx context.Context, env *convstore.ScanEnv, agent, dbName string) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}
	type candidate struct {
		path    string
		profile string
	}
	dataHome := sqliteAdapterDataHome(env)
	cands := []candidate{
		{path: filepath.Join(dataHome, agent, dbName), profile: ""},
	}
	for _, ph := range env.ProfileShadowHomes(agent) {
		cands = append(cands,
			candidate{path: filepath.Join(ph.ShadowHome, ".local", "share", agent, dbName), profile: ph.Profile},
			candidate{path: filepath.Join(ph.ShadowHome, agent, dbName), profile: ph.Profile},
			candidate{path: filepath.Join(ph.ShadowHome, dbName), profile: ph.Profile},
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
			SourceKey:  convstore.MakeSourceKey(agent, c.profile, c.path),
			Agent:      agent,
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

func opencodeKiloPickSessionTable(ctx context.Context, db *sql.DB, tableOrder []string) string {
	var fallback string
	for _, tbl := range tableOrder {
		if !TableExists(ctx, db, tbl) {
			continue
		}
		if fallback == "" {
			fallback = tbl
		}
		var one int
		if err := db.QueryRowContext(ctx, `SELECT 1 FROM "`+tbl+`" LIMIT 1`).Scan(&one); err == nil {
			return tbl
		}
	}
	return fallback
}

func opencodeKiloParseMetadata(ctx context.Context, src convstore.DiscoveredSource, agent string, tableOrder []string) (*convstore.MetadataResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	db, err := OpenReadOnly(src.Path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	table := opencodeKiloPickSessionTable(ctx, db, tableOrder)
	if table == "" {
		return &convstore.MetadataResult{}, nil
	}

	colOrNull := func(col string) string {
		if ColumnExists(ctx, db, table, col) {
			return `"` + col + `"`
		}
		return "NULL"
	}

	query := `SELECT ` +
		colOrNull("id") + `, ` +
		colOrNull("title") + `, ` +
		colOrNull("directory") + `, ` +
		colOrNull("time_created") + `, ` +
		colOrNull("time_updated") + `, ` +
		colOrNull("time_archived") + `, ` +
		colOrNull("parent_id") +
		` FROM "` + table + `"`
	if ColumnExists(ctx, db, table, "parent_id") {
		query += ` WHERE parent_id IS NULL OR parent_id = ''`
	}

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
		var idRaw, titleRaw, dirRaw, createdRaw, updatedRaw, archivedRaw, parentRaw any
		if err := rows.Scan(&idRaw, &titleRaw, &dirRaw, &createdRaw, &updatedRaw, &archivedRaw, &parentRaw); err != nil {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" || strings.TrimSpace(sqliteAdapterString(parentRaw)) != "" {
			continue
		}

		title := convstore.CleanTitle(sqliteAdapterString(titleRaw))
		createdAtMs := sqliteAdapterTimestampMs(createdRaw)
		updatedAtMs := sqliteAdapterTimestampMs(updatedRaw)
		if updatedAtMs == 0 {
			updatedAtMs = createdAtMs
		}
		if updatedAtMs == 0 && src.MtimeNs > 0 {
			updatedAtMs = src.MtimeNs / 1e6
		}
		archived := sqliteAdapterInt(archivedRaw) > 0

		convs = append(convs, convstore.Conversation{
			Agent:       agent,
			ID:          id,
			App:         convstore.DisplayApp(agent),
			Profile:     src.Profile,
			Title:       title,
			CWD:         strings.TrimSpace(sqliteAdapterString(dirRaw)),
			CreatedAtMs: createdAtMs,
			UpdatedAtMs: updatedAtMs,
			Archived:    archived,
			SourceKey:   src.SourceKey,
			SourcePath:  src.Path,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &convstore.MetadataResult{Conversations: convs}, nil
}

func opencodeKiloParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor, tableOrder []string) (*convstore.MessagesResult, error) {
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

	table := opencodeKiloPickSessionTable(ctx, db, tableOrder)
	if table == "" {
		return res, nil
	}

	targetSessions := make(map[string]bool)
	updatedExpr := "NULL"
	if ColumnExists(ctx, db, table, "time_updated") {
		updatedExpr = `"time_updated"`
	}
	sessQuery := `SELECT id, ` + updatedExpr + ` FROM "` + table + `"`
	if ColumnExists(ctx, db, table, "parent_id") {
		sessQuery += ` WHERE parent_id IS NULL OR parent_id = ''`
	}
	srows, err := db.QueryContext(ctx, sessQuery)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var idRaw, updRaw any
		if err := srows.Scan(&idRaw, &updRaw); err != nil {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		if id == "" {
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

	if TableExists(ctx, db, "session_message") {
		if err := opencodeKiloReadSessionMessageTable(ctx, db, targetSessions, res); err != nil {
			return nil, err
		}
		if len(res.Messages) > 0 {
			return res, nil
		}
	}

	if TableExists(ctx, db, "message") {
		if err := opencodeKiloReadMessagePartTables(ctx, db, targetSessions, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func opencodeKiloReadSessionMessageTable(ctx context.Context, db *sql.DB, targetSessions map[string]bool, res *convstore.MessagesResult) error {
	colOrNull := func(col string) string {
		if ColumnExists(ctx, db, "session_message", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}
	orderBy := "rowid ASC"
	if ColumnExists(ctx, db, "session_message", "time_created") {
		orderBy = "time_created ASC, rowid ASC"
	} else if ColumnExists(ctx, db, "session_message", "created_at") {
		orderBy = "created_at ASC, rowid ASC"
	}
	timeCol := colOrNull("time_created")
	if timeCol == "NULL" {
		timeCol = colOrNull("created_at")
	}

	query := `SELECT ` +
		colOrNull("session_id") + `, ` +
		colOrNull("role") + `, ` +
		colOrNull("data") + `, ` +
		colOrNull("content") + `, ` +
		colOrNull("parts") + `, ` +
		timeCol +
		` FROM session_message ORDER BY ` + orderBy

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var sidRaw, roleRaw, dataRaw, contentRaw, partsRaw, tsRaw any
		if err := rows.Scan(&sidRaw, &roleRaw, &dataRaw, &contentRaw, &partsRaw, &tsRaw); err != nil {
			continue
		}
		sid := strings.TrimSpace(sqliteAdapterString(sidRaw))
		if !targetSessions[sid] {
			continue
		}
		role := sqliteAdapterNormalizeRole(sqliteAdapterString(roleRaw))
		var rawText string

		if dataStr := strings.TrimSpace(sqliteAdapterString(dataRaw)); dataStr != "" {
			r, t := opencodeKiloExtractFromJSON(dataStr)
			if role == "" {
				role = r
			}
			rawText = t
		}
		if rawText == "" {
			if partsStr := strings.TrimSpace(sqliteAdapterString(partsRaw)); partsStr != "" {
				r, t := opencodeKiloExtractFromJSON(partsStr)
				if role == "" {
					role = r
				}
				rawText = t
			}
		}
		if rawText == "" {
			if contentStr := strings.TrimSpace(sqliteAdapterString(contentRaw)); contentStr != "" {
				r, t := opencodeKiloExtractFromJSON(contentStr)
				if role == "" {
					role = r
				}
				rawText = t
			}
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
	return rows.Err()
}

func opencodeKiloReadMessagePartTables(ctx context.Context, db *sql.DB, targetSessions map[string]bool, res *convstore.MessagesResult) error {
	colOrNull := func(tbl, col string) string {
		if ColumnExists(ctx, db, tbl, col) {
			return `"` + col + `"`
		}
		return "NULL"
	}

	type msgMeta struct {
		id           string
		sessionID    string
		role         string
		tsMs         int64
		fallbackText string
	}

	orderBy := "rowid ASC"
	if ColumnExists(ctx, db, "message", "time_created") {
		orderBy = "time_created ASC, rowid ASC"
	}
	mQuery := `SELECT ` +
		colOrNull("message", "id") + `, ` +
		colOrNull("message", "session_id") + `, ` +
		colOrNull("message", "role") + `, ` +
		colOrNull("message", "data") + `, ` +
		colOrNull("message", "content") + `, ` +
		colOrNull("message", "time_created") +
		` FROM message ORDER BY ` + orderBy

	mrows, err := db.QueryContext(ctx, mQuery)
	if err != nil {
		return err
	}
	var ordered []msgMeta
	wantedMsg := make(map[string]bool)
	for mrows.Next() {
		var idRaw, sidRaw, roleRaw, dataRaw, contentRaw, tsRaw any
		if err := mrows.Scan(&idRaw, &sidRaw, &roleRaw, &dataRaw, &contentRaw, &tsRaw); err != nil {
			continue
		}
		sid := strings.TrimSpace(sqliteAdapterString(sidRaw))
		if !targetSessions[sid] {
			continue
		}
		id := strings.TrimSpace(sqliteAdapterString(idRaw))
		role := sqliteAdapterNormalizeRole(sqliteAdapterString(roleRaw))
		var fallbackText string
		if ds := strings.TrimSpace(sqliteAdapterString(dataRaw)); ds != "" {
			r, t := opencodeKiloExtractFromJSON(ds)
			if role == "" {
				role = r
			}
			fallbackText = t
		}
		if fallbackText == "" {
			if cs := strings.TrimSpace(sqliteAdapterString(contentRaw)); cs != "" {
				r, t := opencodeKiloExtractFromJSON(cs)
				if role == "" {
					role = r
				}
				fallbackText = t
			}
		}
		if role == "" {
			continue
		}
		ordered = append(ordered, msgMeta{
			id:           id,
			sessionID:    sid,
			role:         role,
			tsMs:         sqliteAdapterTimestampMs(tsRaw),
			fallbackText: fallbackText,
		})
		if id != "" {
			wantedMsg[id] = true
		}
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return err
	}

	partsByMsg := make(map[string][]string)
	if len(wantedMsg) > 0 && TableExists(ctx, db, "part") {
		pOrder := "rowid ASC"
		if ColumnExists(ctx, db, "part", "time_created") {
			pOrder = "time_created ASC, rowid ASC"
		}
		pQuery := `SELECT ` +
			colOrNull("part", "message_id") + `, ` +
			colOrNull("part", "data") + `, ` +
			colOrNull("part", "content") +
			` FROM part ORDER BY ` + pOrder
		prows, err := db.QueryContext(ctx, pQuery)
		if err == nil {
			for prows.Next() {
				var midRaw, dataRaw, contentRaw any
				if err := prows.Scan(&midRaw, &dataRaw, &contentRaw); err != nil {
					continue
				}
				mid := strings.TrimSpace(sqliteAdapterString(midRaw))
				if !wantedMsg[mid] {
					continue
				}
				raw := sqliteAdapterFirstNonEmpty(
					strings.TrimSpace(sqliteAdapterString(dataRaw)),
					strings.TrimSpace(sqliteAdapterString(contentRaw)),
				)
				if raw == "" {
					continue
				}
				if txt := opencodeKiloExtractPartText(raw); txt != "" {
					partsByMsg[mid] = append(partsByMsg[mid], txt)
				}
			}
			prows.Close()
		}
	}

	for _, m := range ordered {
		var rawText string
		if ps := partsByMsg[m.id]; len(ps) > 0 {
			rawText = strings.Join(ps, "\n")
		} else {
			rawText = m.fallbackText
		}
		cleaned, truncated := convstore.CleanMessageText(rawText)
		if cleaned == "" {
			continue
		}
		cur := res.Messages[m.sessionID]
		cur = append(cur, convstore.Message{
			Seq:       len(cur),
			Role:      m.role,
			Text:      cleaned,
			TsMs:      m.tsMs,
			Truncated: truncated,
		})
		res.Messages[m.sessionID] = cur
	}
	return nil
}

func opencodeKiloExtractFromJSON(raw string) (role string, text string) {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", raw
	}
	if obj, ok := parsed.(map[string]any); ok {
		role = sqliteAdapterNormalizeRole(sqliteAdapterString(obj["role"]))
		if typ, _ := obj["type"].(string); typ == "tool" || typ == "step-start" || typ == "step-finish" || typ == "patch" {
			return role, ""
		}
	}
	return role, convstore.ExtractPlainText(parsed)
}

func opencodeKiloExtractPartText(raw string) string {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return raw
	}
	if obj, ok := parsed.(map[string]any); ok {
		switch sqliteAdapterString(obj["type"]) {
		case "tool", "tool-invocation", "tool_use", "tool_result", "reasoning", "thinking", "step-start", "step-finish", "patch", "snapshot", "file":
			return ""
		}
	}
	return convstore.ExtractPlainText(parsed)
}

// --- Shared unexported helpers for SQLite & major adapters (prefixed sqliteAdapter* to avoid merge collisions) ---

func sqliteAdapterDataHome(env *convstore.ScanEnv) string {
	if env != nil && strings.TrimSpace(env.Paths.DataHome) != "" {
		return env.Paths.DataHome
	}
	if env != nil && strings.TrimSpace(env.Paths.Home) != "" {
		return filepath.Join(env.Paths.Home, ".local", "share")
	}
	return ""
}

func sqliteAdapterIsOSHome(home string) bool {
	if home == "" {
		return false
	}
	osHome := os.Getenv("HOME")
	if osHome == "" {
		if h, err := os.UserHomeDir(); err == nil {
			osHome = h
		}
	}
	if osHome == "" {
		return false
	}
	return filepath.Clean(home) == filepath.Clean(osHome)
}

func sqliteAdapterIsUpdated(prev *convstore.SourceCursor, id string, updatedAtMs int64) bool {
	if prev == nil || prev.ExistingUpdatedMs == nil {
		return true
	}
	oldTs, exists := prev.ExistingUpdatedMs[id]
	if !exists {
		return true
	}
	return updatedAtMs > oldTs
}

func sqliteAdapterNormalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "human":
		return "user"
	case "assistant", "model", "agent", "ai":
		return "assistant"
	default:
		return ""
	}
}

func sqliteAdapterFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func sqliteAdapterString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case time.Time:
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(t)
	}
}

func sqliteAdapterInt(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case []byte:
		if i, err := strconv.ParseInt(strings.TrimSpace(string(t)), 10, 64); err == nil {
			return i
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i
		}
	}
	return 0
}

func sqliteAdapterBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	case []byte:
		return sqliteAdapterBool(string(t))
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes"
	default:
		return false
	}
}

func sqliteAdapterTimestampMs(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case time.Time:
		if t.IsZero() {
			return 0
		}
		return t.UnixMilli()
	case []byte:
		return convstore.ParseTimestampMs(string(t))
	default:
		return convstore.ParseTimestampMs(v)
	}
}

// sqliteAdapterSplitChunks splits long plain text into chunks of at most maxBytes
// along paragraph, line, whitespace, or UTF-8 rune boundaries so no searchable
// text is lost when CleanMessageText enforces MaxMessageTextBytes.
func sqliteAdapterSplitChunks(s string, maxBytes int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = convstore.MaxMessageTextBytes
	}
	if len(s) <= maxBytes {
		return []string{s}
	}

	var chunks []string
	for len(s) > 0 {
		s = strings.TrimSpace(s)
		if s == "" {
			break
		}
		if len(s) <= maxBytes {
			chunks = append(chunks, s)
			break
		}
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			_, size := utf8.DecodeRuneInString(s)
			cut = size
		}

		// Try to break cleanly at paragraph, newline, or space in the latter half.
		minCut := cut / 2
		splitAt := -1
		if idx := strings.LastIndex(s[:cut], "\n\n"); idx >= minCut {
			splitAt = idx
		} else if idx := strings.LastIndexByte(s[:cut], '\n'); idx >= minCut {
			splitAt = idx
		} else if idx := strings.LastIndexByte(s[:cut], ' '); idx >= minCut {
			splitAt = idx
		}
		if splitAt > 0 {
			cut = splitAt
		}

		part := strings.TrimSpace(s[:cut])
		if part != "" {
			chunks = append(chunks, part)
		}
		s = s[cut:]
	}
	return chunks
}
