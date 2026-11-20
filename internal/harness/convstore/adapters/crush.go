package adapters

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/arhammahajan/model-router/internal/harness/convstore"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
)

type crushAdapter struct {
	mu          sync.RWMutex
	dbPathToCWD map[string]string
}

// NewCrushAdapter returns the native conversation store adapter for Crush.
func NewCrushAdapter() convstore.Adapter {
	return &crushAdapter{
		dbPathToCWD: make(map[string]string),
	}
}

func (a *crushAdapter) Agent() string { return "crush" }

type crushProjectsRegistry struct {
	Projects []struct {
		Path    string `json:"path"`
		DataDir string `json:"data_dir"`
	} `json:"projects"`
}

func (a *crushAdapter) Discover(ctx context.Context, env *convstore.ScanEnv) ([]convstore.DiscoveredSource, error) {
	if env == nil {
		return nil, nil
	}

	type regCandidate struct {
		path    string
		profile string
	}
	dataHome := sqliteAdapterDataHome(env)
	regs := []regCandidate{
		{path: filepath.Join(dataHome, "crush", "projects.json"), profile: ""},
	}
	for _, ph := range env.ProfileShadowHomes("crush") {
		regs = append(regs,
			regCandidate{path: filepath.Join(ph.ShadowHome, ".local", "share", "crush", "projects.json"), profile: ph.Profile},
			regCandidate{path: filepath.Join(ph.ShadowHome, "crush", "projects.json"), profile: ph.Profile},
		)
	}

	var sources []convstore.DiscoveredSource
	for _, rc := range regs {
		if ctx.Err() != nil {
			return sources, ctx.Err()
		}
		raw, err := os.ReadFile(rc.path)
		if err != nil {
			continue
		}
		var reg crushProjectsRegistry
		if err := json.Unmarshal(raw, &reg); err != nil {
			continue
		}
		for _, proj := range reg.Projects {
			if ctx.Err() != nil {
				return sources, ctx.Err()
			}
			var dbPath string
			if strings.TrimSpace(proj.DataDir) != "" {
				dbPath = filepath.Join(proj.DataDir, "crush.db")
			} else if strings.TrimSpace(proj.Path) != "" {
				dbPath = filepath.Join(proj.Path, ".crush", "crush.db")
			}
			if dbPath == "" {
				continue
			}
			size, mtime, walSize, walMtime, ok := StatSQLite(dbPath)
			if !ok {
				continue
			}
			if !env.MarkRealPath(dbPath) {
				continue
			}
			cleanDB := filepath.Clean(dbPath)
			if strings.TrimSpace(proj.Path) != "" {
				a.rememberCWD(cleanDB, filepath.Clean(proj.Path))
			}
			sources = append(sources, convstore.DiscoveredSource{
				SourceKey:  convstore.MakeSourceKey("crush", rc.profile, cleanDB),
				Agent:      "crush",
				Profile:    rc.profile,
				Path:       cleanDB,
				Kind:       convstore.SourceSQLite,
				SizeBytes:  size,
				MtimeNs:    mtime,
				WALSize:    walSize,
				WALMtimeNs: walMtime,
			})
		}
	}
	return sources, nil
}

func (a *crushAdapter) rememberCWD(dbPath, cwd string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dbPathToCWD == nil {
		a.dbPathToCWD = make(map[string]string)
	}
	a.dbPathToCWD[dbPath] = cwd
	if resolved, err := filepath.EvalSymlinks(dbPath); err == nil && resolved != "" {
		a.dbPathToCWD[filepath.Clean(resolved)] = cwd
	}
}

func (a *crushAdapter) resolveCWD(dbPath string) string {
	clean := filepath.Clean(dbPath)
	a.mu.RLock()
	if cwd, ok := a.dbPathToCWD[clean]; ok && cwd != "" {
		a.mu.RUnlock()
		return cwd
	}
	a.mu.RUnlock()

	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != "" {
		a.mu.RLock()
		if cwd, ok := a.dbPathToCWD[filepath.Clean(resolved)]; ok && cwd != "" {
			a.mu.RUnlock()
			return cwd
		}
		a.mu.RUnlock()
	}

	// Fallback 1: look up in default XDG_DATA_HOME/crush/projects.json.
	paths := harnessenv.DefaultPaths()
	regPath := filepath.Join(paths.DataHome, "crush", "projects.json")
	if raw, err := os.ReadFile(regPath); err == nil {
		var reg crushProjectsRegistry
		if json.Unmarshal(raw, &reg) == nil {
			dbDir := filepath.Dir(clean)
			for _, p := range reg.Projects {
				if p.DataDir != "" && filepath.Clean(p.DataDir) == dbDir && p.Path != "" {
					cwd := filepath.Clean(p.Path)
					a.rememberCWD(clean, cwd)
					return cwd
				}
			}
		}
	}

	// Fallback 2: <project>/.crush/crush.db
	parent := filepath.Dir(clean)
	if filepath.Base(parent) == ".crush" {
		return filepath.Dir(parent)
	}
	return ""
}

func (a *crushAdapter) ParseMetadata(ctx context.Context, src convstore.DiscoveredSource, _ *convstore.SourceCursor) (*convstore.MetadataResult, error) {
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
		colOrNull("id") + `, ` +
		colOrNull("title") + `, ` +
		colOrNull("created_at") + `, ` +
		colOrNull("updated_at") + `, ` +
		colOrNull("message_count") + `, ` +
		colOrNull("parent_session_id") +
		` FROM sessions`
	if ColumnExists(ctx, db, "sessions", "parent_session_id") {
		query += ` WHERE parent_session_id IS NULL OR parent_session_id = ''`
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cwd := a.resolveCWD(src.Path)
	var convs []convstore.Conversation
	for rows.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var idRaw, titleRaw, createdRaw, updatedRaw, msgCountRaw, parentRaw any
		if err := rows.Scan(&idRaw, &titleRaw, &createdRaw, &updatedRaw, &msgCountRaw, &parentRaw); err != nil {
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
		msgCount := int(sqliteAdapterInt(msgCountRaw))
		if msgCount < 0 {
			msgCount = 0
		}

		convs = append(convs, convstore.Conversation{
			Agent:        "crush",
			ID:           id,
			App:          convstore.DisplayApp("crush"),
			Profile:      src.Profile,
			Title:        title,
			CWD:          cwd,
			CreatedAtMs:  createdAtMs,
			UpdatedAtMs:  updatedAtMs,
			MessageCount: msgCount,
			SourceKey:    src.SourceKey,
			SourcePath:   src.Path,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &convstore.MetadataResult{Conversations: convs}, nil
}

func (a *crushAdapter) ParseMessages(ctx context.Context, src convstore.DiscoveredSource, prev *convstore.SourceCursor) (*convstore.MessagesResult, error) {
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
	if !TableExists(ctx, db, "sessions") || !TableExists(ctx, db, "messages") {
		return res, nil
	}

	targetSessions := make(map[string]bool)
	updExpr := "NULL"
	if ColumnExists(ctx, db, "sessions", "updated_at") {
		updExpr = `"updated_at"`
	}
	sQuery := `SELECT id, ` + updExpr + ` FROM sessions`
	if ColumnExists(ctx, db, "sessions", "parent_session_id") {
		sQuery += ` WHERE parent_session_id IS NULL OR parent_session_id = ''`
	}
	srows, err := db.QueryContext(ctx, sQuery)
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

	colOrNull := func(col string) string {
		if ColumnExists(ctx, db, "messages", col) {
			return `"` + col + `"`
		}
		return "NULL"
	}
	orderBy := "rowid ASC"
	if ColumnExists(ctx, db, "messages", "created_at") {
		orderBy = "created_at ASC, rowid ASC"
	}

	mQuery := `SELECT ` +
		colOrNull("session_id") + `, ` +
		colOrNull("role") + `, ` +
		colOrNull("parts") + `, ` +
		colOrNull("content") + `, ` +
		colOrNull("created_at") +
		` FROM messages ORDER BY ` + orderBy

	mrows, err := db.QueryContext(ctx, mQuery)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()

	for mrows.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var sidRaw, roleRaw, partsRaw, contentRaw, tsRaw any
		if err := mrows.Scan(&sidRaw, &roleRaw, &partsRaw, &contentRaw, &tsRaw); err != nil {
			continue
		}
		sid := strings.TrimSpace(sqliteAdapterString(sidRaw))
		if !targetSessions[sid] {
			continue
		}
		role := sqliteAdapterNormalizeRole(sqliteAdapterString(roleRaw))
		if role == "" {
			continue
		}
		rawPayload := sqliteAdapterFirstNonEmpty(
			strings.TrimSpace(sqliteAdapterString(partsRaw)),
			strings.TrimSpace(sqliteAdapterString(contentRaw)),
		)
		if rawPayload == "" {
			continue
		}

		var rawText string
		var parsed any
		if err := json.Unmarshal([]byte(rawPayload), &parsed); err == nil {
			rawText = crushExtractPartsText(parsed)
		} else {
			rawText = rawPayload
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

func crushExtractPartsText(parsed any) string {
	switch v := parsed.(type) {
	case []any:
		var out []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				typ := strings.ToLower(strings.TrimSpace(sqliteAdapterString(m["type"])))
				switch typ {
				case "tool_call", "tool_result", "tool_use", "tool-call", "tool-result", "reasoning", "thinking", "finish", "binary":
					continue
				}
				if data, ok := m["data"]; ok {
					if s := convstore.ExtractPlainText(data); s != "" {
						out = append(out, s)
						continue
					}
				}
			}
			if s := convstore.ExtractPlainText(item); s != "" {
				out = append(out, s)
			}
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	case map[string]any:
		if data, ok := v["data"]; ok {
			if s := convstore.ExtractPlainText(data); s != "" {
				return s
			}
		}
		return convstore.ExtractPlainText(v)
	default:
		return convstore.ExtractPlainText(v)
	}
}
