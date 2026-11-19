package spaceresume

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SessionIDFromJSONFiles returns the newest session id for cwd from JSON-lines
// files. The first lines win per file; when no inline id exists the newest
// file's stem is the id (Claude/Qwen style stores).
func SessionIDFromJSONFiles(paths []string, cwd string) string {
	if len(paths) == 0 {
		return ""
	}
	ordered := append([]string(nil), paths...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return fileMtime(ordered[i]) > fileMtime(ordered[j])
	})
	for _, path := range ordered {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(content), "\n")
		for i := 0; i < len(lines) && i < 5; i++ {
			line := lines[i]
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				continue
			}
			if record == nil {
				continue
			}
			if cw, present := record["cwd"]; present && cw != nil {
				s, ok := cw.(string)
				if !ok || s != cwd {
					continue
				}
			}
			for _, key := range []string{"sessionId", "session_id", "id"} {
				if v, ok := record[key].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	if len(ordered) > 0 {
		return strings.TrimSuffix(filepath.Base(ordered[0]), filepath.Ext(ordered[0]))
	}
	return ""
}

func fileMtime(path string) float64 {
	if fi, err := os.Stat(path); err == nil {
		return float64(fi.ModTime().UnixNano()) / 1e9
	}
	return 0
}

// EncodedPath maps a cwd to the flat directory name Claude/Qwen use.
func EncodedPath(cwd string) string {
	var sb strings.Builder
	for _, r := range cwd {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	return sb.String()
}

func globDir(root, pattern string) []string {
	matches, _ := filepath.Glob(filepath.Join(root, pattern))
	return matches
}

// DiscoverClaude finds the newest Claude session for cwd.
func DiscoverClaude(cwd string) string {
	projects := filepath.Join(claudeHome(), "projects")
	dir := filepath.Join(projects, EncodedPath(cwd))
	var candidates []string
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		candidates, _ = filepath.Glob(filepath.Join(dir, "*.jsonl"))
	}
	if len(candidates) > 0 {
		return SessionIDFromJSONFiles(candidates, cwd)
	}
	// Fall back to scanning projects when Claude used a different encoding.
	var found []string
	for _, path := range globDir(projects, filepath.Join("*", "*.jsonl")) {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.SplitN(string(content), "\n", 6)
		for _, line := range lines[:minInt(5, len(lines))] {
			if line == "" {
				break
			}
			if !strings.Contains(line, `"cwd"`) {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				break
			}
			if record["cwd"] == cwd {
				found = append(found, path)
			}
			break
		}
	}
	return SessionIDFromJSONFiles(found, cwd)
}

// DiscoverCodex finds the newest Codex session for cwd.
func DiscoverCodex(cwd string) string {
	sessions := filepath.Join(codexHome(), "sessions")
	if fi, err := os.Stat(sessions); err != nil || !fi.IsDir() {
		return ""
	}
	var matches []string
	_ = filepath.Walk(sessions, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(info.Name(), "rollout-") && strings.HasSuffix(info.Name(), ".jsonl") {
			matches = append(matches, path)
		}
		return nil
	})
	sort.SliceStable(matches, func(i, j int) bool { return fileMtime(matches[i]) > fileMtime(matches[j]) })
	for _, path := range matches {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		first := strings.SplitN(string(content), "\n", 2)[0]
		var record map[string]any
		if err := json.Unmarshal([]byte(first), &record); err != nil {
			continue
		}
		if record["type"] != "session_meta" {
			continue
		}
		payload, _ := record["payload"].(map[string]any)
		if payload == nil {
			continue
		}
		if payload["cwd"] != cwd {
			continue
		}
		if v, ok := payload["session_id"].(string); ok && v != "" {
			return v
		}
		if v, ok := payload["id"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// DiscoverOpencode finds the newest opencode session for cwd.
func DiscoverOpencode(cwd string) string {
	database := filepath.Join(xdgData(), "opencode", "opencode.db")
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	for _, table := range []string{"session_v2", "session"} {
		rows, err := sqliteRows(database, table, map[string]string{"directory": cwd}, "time_updated", 1)
		if err != nil || len(rows) == 0 {
			continue
		}
		if id := sqliteText(rows[0]["id"]); id != "" {
			return id
		}
	}
	return ""
}

// DiscoverPi finds the newest pi session for cwd.
func DiscoverPi(cwd string) string {
	sessions := filepath.Join(homeDir(), ".pi", "agent", "sessions")
	if fi, err := os.Stat(sessions); err != nil || !fi.IsDir() {
		return ""
	}
	var matches []string
	_ = filepath.Walk(sessions, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".jsonl") {
			matches = append(matches, path)
		}
		return nil
	})
	sort.SliceStable(matches, func(i, j int) bool { return fileMtime(matches[i]) > fileMtime(matches[j]) })
	for _, path := range matches {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		first := strings.SplitN(string(content), "\n", 2)[0]
		var record map[string]any
		if err := json.Unmarshal([]byte(first), &record); err != nil {
			continue
		}
		if record["type"] != "session" || record["cwd"] != cwd {
			continue
		}
		if v, ok := record["id"].(string); ok && v != "" {
			return v
		}
		return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return ""
}

// DiscoverQwen finds the newest Qwen session for cwd.
func DiscoverQwen(cwd string) string {
	projects := filepath.Join(homeDir(), ".qwen", "projects")
	dir := filepath.Join(projects, EncodedPath(cwd), "chats")
	var candidates []string
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		for _, p := range globDir(dir, "*.jsonl") {
			if !strings.HasSuffix(p, ".runtime.json") {
				candidates = append(candidates, p)
			}
		}
	}
	if len(candidates) > 0 {
		return SessionIDFromJSONFiles(candidates, cwd)
	}
	var found []string
	for _, path := range globDir(projects, filepath.Join("*", "chats", "*.jsonl")) {
		if strings.HasSuffix(path, ".runtime.json") {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.SplitN(string(content), "\n", 6)
		for _, line := range lines[:minInt(5, len(lines))] {
			if line == "" {
				break
			}
			if !strings.Contains(line, `"cwd"`) {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				break
			}
			if record["cwd"] == cwd {
				found = append(found, path)
			}
			break
		}
	}
	return SessionIDFromJSONFiles(found, cwd)
}

// DiscoverCopilot finds the newest Copilot session for cwd.
func DiscoverCopilot(cwd string) string {
	root := filepath.Join(homeDir(), ".copilot", "session-state")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	type found struct {
		modified float64
		updated  string
		id       string
	}
	var items []found
	for _, ws := range globDir(root, filepath.Join("*", "workspace.yaml")) {
		content, err := os.ReadFile(ws)
		if err != nil {
			continue
		}
		var sessionID, sessionCwd, updated string
		for _, line := range strings.Split(string(content), "\n") {
			if !strings.Contains(line, ":") {
				continue
			}
			key, value, _ := strings.Cut(line, ":")
			value = strings.TrimSpace(value)
			switch strings.TrimSpace(key) {
			case "id":
				sessionID = value
			case "cwd":
				sessionCwd = value
			case "updated_at":
				updated = value
			}
		}
		if sessionID == "" || sessionCwd != cwd {
			continue
		}
		items = append(items, found{fileMtime(ws), updated, sessionID})
	}
	if len(items) == 0 {
		return ""
	}
	best := items[0]
	for _, it := range items[1:] {
		if it.modified > best.modified ||
			(it.modified == best.modified && (it.updated > best.updated ||
				(it.updated == best.updated && it.id > best.id))) {
			best = it
		}
	}
	return best.id
}

// DiscoverKimi finds the newest Kimi session for cwd via the kimi CLI.
func DiscoverKimi(cwd string) string {
	kimi, err := LookPathFunc("kimi")
	if err != nil || kimi == "" {
		return ""
	}
	stdout, _, err := RunBinFunc(kimi,
		[]string{"session", "list", "--json", "--cwd", cwd, "--limit", "1"},
		cwd, 10*time.Second)
	if err != nil {
		return ""
	}
	var payload []map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil || len(payload) == 0 {
		return ""
	}
	if v, ok := payload[0]["id"].(string); ok && v != "" {
		return v
	}
	return ""
}

// DiscoverMcode finds the newest mcode session for cwd.
func DiscoverMcode(cwd string) string {
	database := filepath.Join(homeDir(), ".minimax", "v2", "sqlite", "runtime-state.sqlite")
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	if id, ok := sqliteLatestID(database, "local_runtime_sessions", "workspace_dir", cwd, "session_id", "updated_at_ms", true); ok {
		return id
	}
	return ""
}

// DiscoverMuse finds the newest muse session for cwd.
func DiscoverMuse(cwd string) string {
	database := filepath.Join(xdgData(), "muse", "session-index.db")
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	if id, ok := sqliteLatestID(database, "sessions", "workspace_root", cwd, "session_id", "updated_at_us", false); ok {
		return id
	}
	return ""
}

// DiscoverVibe finds the newest vibe session for cwd.
func DiscoverVibe(cwd string) string {
	root := filepath.Join(homeDir(), ".vibe", "logs", "session", "unified")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	var found [][2]string
	for _, meta := range globDir(root, filepath.Join("*", "meta.json")) {
		content, err := os.ReadFile(meta)
		if err != nil {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(content, &record); err != nil {
			continue
		}
		env, _ := record["environment"].(map[string]any)
		working := ""
		if env != nil {
			working, _ = env["working_directory"].(string)
		}
		if working == "" {
			working, _ = record["origin_directory"].(string)
		}
		if working != cwd {
			continue
		}
		sessionID, _ := record["session_id"].(string)
		if sessionID == "" {
			continue
		}
		timestamp := ""
		if v, ok := record["end_time"].(string); ok && v != "" {
			timestamp = v
		} else if v, ok := record["start_time"].(string); ok {
			timestamp = v
		}
		found = append(found, [2]string{timestamp, sessionID})
	}
	if len(found) == 0 {
		return ""
	}
	best := found[0]
	for _, f := range found[1:] {
		if f[0] > best[0] || (f[0] == best[0] && f[1] > best[1]) {
			best = f
		}
	}
	return best[1]
}

// DiscoverCline finds the newest cline session for cwd.
func DiscoverCline(cwd string) string {
	root := filepath.Join(homeDir(), ".cline", "data", "sessions")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	type found struct {
		modified  float64
		timestamp string
		id        string
	}
	var items []found
	for _, path := range globDir(root, filepath.Join("*", "*.json")) {
		if strings.HasSuffix(path, ".messages.json") {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(content, &record); err != nil {
			continue
		}
		if record["cwd"] != cwd {
			continue
		}
		sessionID, _ := record["session_id"].(string)
		if sessionID == "" {
			continue
		}
		ts, _ := record["started_at"].(string)
		items = append(items, found{fileMtime(path), ts, sessionID})
	}
	if len(items) == 0 {
		return ""
	}
	best := items[0]
	for _, it := range items[1:] {
		if it.modified > best.modified ||
			(it.modified == best.modified && (it.timestamp > best.timestamp ||
				(it.timestamp == best.timestamp && it.id > best.id))) {
			best = it
		}
	}
	return best.id
}

// DiscoverGrok finds the newest grok session for cwd.
func DiscoverGrok(cwd string) string {
	root := filepath.Join(grokHome(), "sessions")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	var found [][2]string
	for _, summary := range globDir(root, filepath.Join("*", "*", "summary.json")) {
		content, err := os.ReadFile(summary)
		if err != nil {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(content, &record); err != nil {
			continue
		}
		info, _ := record["info"].(map[string]any)
		if info == nil || info["cwd"] != cwd {
			continue
		}
		sessionID, _ := info["id"].(string)
		if sessionID == "" {
			continue
		}
		timestamp := ""
		if v, ok := record["last_active_at"].(string); ok && v != "" {
			timestamp = v
		} else if v, ok := record["updated_at"].(string); ok {
			timestamp = v
		}
		found = append(found, [2]string{timestamp, sessionID})
	}
	if len(found) == 0 {
		return ""
	}
	best := found[0]
	for _, f := range found[1:] {
		if f[0] > best[0] || (f[0] == best[0] && f[1] > best[1]) {
			best = f
		}
	}
	return best[1]
}

// DiscoverCrush finds the newest crush session for cwd.
func DiscoverCrush(cwd string) string {
	database := filepath.Join(cwd, ".crush", "crush.db")
	registry := filepath.Join(xdgData(), "crush", "projects.json")
	if _, err := os.Stat(database); err != nil {
		if _, rerr := os.Stat(registry); rerr != nil {
			return ""
		}
		content, err := os.ReadFile(registry)
		if err != nil {
			return ""
		}
		var reg map[string]any
		if err := json.Unmarshal(content, &reg); err != nil {
			return ""
		}
		projects, _ := reg["projects"].([]any)
		bestLen := -1
		bestDB := ""
		for _, p := range projects {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			projectPath, _ := pm["path"].(string)
			dataDir, _ := pm["data_dir"].(string)
			if projectPath == "" || dataDir == "" {
				continue
			}
			if cwd == projectPath || strings.HasPrefix(cwd, strings.TrimRight(projectPath, "/")+"/") {
				if len(projectPath) > bestLen {
					bestLen = len(projectPath)
					bestDB = filepath.Join(dataDir, "crush.db")
				}
			}
		}
		if bestDB == "" {
			return ""
		}
		database = bestDB
	}
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	rows, err := sqliteRows(database, "sessions", nil, "updated_at", 1)
	if err != nil || len(rows) == 0 {
		return ""
	}
	return sqliteText(rows[0]["id"])
}

// DiscoverOmp finds the newest omp session for cwd.
func DiscoverOmp(cwd string) string {
	root := filepath.Join(homeDir(), ".omp", "agent", "sessions")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	var matches []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".jsonl") {
			matches = append(matches, path)
		}
		return nil
	})
	sort.SliceStable(matches, func(i, j int) bool { return fileMtime(matches[i]) > fileMtime(matches[j]) })
	for _, path := range matches {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(content), "\n")
	fileLines:
		for i := 0; i < len(lines) && i < 5; i++ {
			if lines[i] == "" {
				break fileLines
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(lines[i]), &record); err != nil {
				break fileLines
			}
			if record["type"] != "session" {
				continue
			}
			if record["cwd"] != cwd {
				break fileLines
			}
			if v, ok := record["id"].(string); ok && v != "" {
				return v
			}
			return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
	}
	return ""
}

// DiscoverKilo finds the newest kilo session for cwd.
func DiscoverKilo(cwd string) string {
	database := filepath.Join(xdgData(), "kilo", "kilo.db")
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	for _, table := range []string{"session", "session_v2"} {
		rows, err := sqliteRows(database, table, map[string]string{"directory": cwd}, "time_updated", 1)
		if err != nil || len(rows) == 0 {
			continue
		}
		if id := sqliteText(rows[0]["id"]); id != "" {
			return id
		}
	}
	return ""
}

// DiscoverAntigravity finds the newest antigravity conversation for cwd,
// checking both the default home and any antigravity profile homes.
func DiscoverAntigravity(cwd string) string {
	return DiscoverAntigravityWithOptions(cwd, "", "", nil)
}

// DiscoverAntigravityForProfile finds the newest antigravity conversation for
// cwd inside a specific profile's shadow home, falling back to DiscoverAntigravity.
func DiscoverAntigravityForProfile(cwd, profile string) string {
	return DiscoverAntigravityWithOptions(cwd, profile, "", nil)
}

// DiscoverProfile infers a harness profile name for an existing conversation ID
// when the recorded outcome omitted it. Currently supports Antigravity profile
// shadow homes under ~/.model-router/profiles/antigravity/<profile>/home.
func DiscoverProfile(agent, sessionID string) string {
	if Normalize(agent) == "antigravity" {
		return DiscoverAntigravityProfile(sessionID)
	}
	return ""
}

// DiscoverAntigravityProfile returns the Antigravity profile name whose shadow
// home owns sessionID, or "" when it lives in the default home or is unknown.
func DiscoverAntigravityProfile(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	home := homeDir()
	// If already executing inside a profile shadow home, extract its profile name.
	marker := filepath.Join(".model-router", "profiles", "antigravity") + string(filepath.Separator)
	if idx := strings.Index(home, marker); idx >= 0 {
		rest := home[idx+len(marker):]
		if prof, _, ok := strings.Cut(rest, string(filepath.Separator)); ok && prof != "" {
			brainDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", sessionID)
			if fi, err := os.Stat(brainDir); err == nil && fi.IsDir() {
				return prof
			}
		}
	}
	profilesRoot := filepath.Join(home, ".model-router", "profiles", "antigravity")
	entries, err := os.ReadDir(profilesRoot)
	if err != nil {
		return ""
	}
	// Fast path: check brain/<sessionID> directory in each profile home.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		brainDir := filepath.Join(profilesRoot, e.Name(), "home", ".gemini", "antigravity-cli", "brain", sessionID)
		if fi, err := os.Stat(brainDir); err == nil && fi.IsDir() {
			return e.Name()
		}
	}
	// Fallback: check conversation_summaries.db in each profile home.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		db := filepath.Join(profilesRoot, e.Name(), "home", ".gemini", "antigravity-cli", "conversation_summaries.db")
		if _, err := os.Stat(db); err != nil {
			continue
		}
		rows, err := sqliteRows(db, "conversation_summaries", map[string]string{"conversation_id": sessionID}, "", 1)
		if err == nil && len(rows) > 0 {
			return e.Name()
		}
	}
	return ""
}

// DiscoverAntigravityWithOptions finds an antigravity conversation for cwd,
// optionally scoped to profile, preferring a title/preview match when title is
// non-empty, and skipping any conversation_id in exclude.
func DiscoverAntigravityWithOptions(cwd, profile, title string, exclude map[string]bool) string {
	home := homeDir()
	var dbs []string
	if profile != "" {
		profDB := filepath.Join(home, ".model-router", "profiles", "antigravity", profile, "home", ".gemini", "antigravity-cli", "conversation_summaries.db")
		if _, err := os.Stat(profDB); err == nil {
			dbs = []string{profDB}
		}
	}
	if len(dbs) == 0 {
		dbs = []string{filepath.Join(home, ".gemini", "antigravity-cli", "conversation_summaries.db")}
		if profDBs, err := filepath.Glob(filepath.Join(home, ".model-router", "profiles", "antigravity", "*", "home", ".gemini", "antigravity-cli", "conversation_summaries.db")); err == nil {
			dbs = append(dbs, profDBs...)
		}
	}

	normTitle := normalizeTitleMatch(title)
	bestID := ""
	var bestMod sqliteValue
	bestMatchedTitle := false

	for _, database := range dbs {
		id, mod, matchedTitle := discoverAntigravityInDB(database, cwd, normTitle, exclude)
		if id == "" {
			continue
		}
		if matchedTitle && !bestMatchedTitle {
			bestID = id
			bestMod = mod
			bestMatchedTitle = true
			continue
		}
		if matchedTitle == bestMatchedTitle && (bestID == "" || sqliteLess(bestMod, mod)) {
			bestID = id
			bestMod = mod
		}
	}
	return bestID
}

func normalizeTitleMatch(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimSuffix(s, "…")
	s = strings.TrimSuffix(s, "...")
	s = strings.Join(strings.Fields(s), " ")
	switch s {
	case "", "antigravity", "agy", "tmux-spaces", "zsh", "bash", "sh", "fish":
		return ""
	}
	return s
}

func antigravityRowMatchesTitle(row map[string]sqliteValue, normTitle string) bool {
	if normTitle == "" {
		return false
	}
	for _, col := range []string{"title", "preview"} {
		cand := strings.Join(strings.Fields(strings.ToLower(sqliteText(row[col]))), " ")
		if cand == "" {
			continue
		}
		if cand == normTitle || strings.HasPrefix(cand, normTitle) || (len(normTitle) >= 8 && strings.Contains(cand, normTitle)) {
			return true
		}
	}
	return false
}

func discoverAntigravityInDB(database, cwd, normTitle string, exclude map[string]bool) (string, sqliteValue, bool) {
	if _, err := os.Stat(database); err != nil {
		return "", nil, false
	}
	rows, err := sqliteRows(database, "conversation_summaries", nil, "last_modified_time", 200)
	if err != nil {
		return "", nil, false
	}
	wanted := "file://" + strings.TrimRight(cwd, "/")
	var firstID string
	var firstMod sqliteValue
	for _, row := range rows {
		conversationID := sqliteText(row["conversation_id"])
		if conversationID == "" || exclude[conversationID] {
			continue
		}
		raw := sqliteText(row["workspace_uris"])
		var uris []string
		if err := json.Unmarshal([]byte(raw), &uris); err != nil {
			uris = []string{raw}
		}
		cwdMatched := false
		for _, uri := range uris {
			if strings.TrimRight(uri, "/") == wanted {
				cwdMatched = true
				break
			}
		}
		if !cwdMatched {
			continue
		}
		if firstID == "" {
			firstID = conversationID
			firstMod = row["last_modified_time"]
		}
		if normTitle != "" && antigravityRowMatchesTitle(row, normTitle) {
			return conversationID, row["last_modified_time"], true
		}
	}
	return firstID, firstMod, false
}

// DiscoverDeepseek finds the newest deepseek session for cwd.
func DiscoverDeepseek(cwd string) string {
	sessions := filepath.Join(dshHome(), "sessions")
	if fi, err := os.Stat(sessions); err != nil || !fi.IsDir() {
		return ""
	}
	encoded := "--" + strings.ReplaceAll(strings.Trim(cwd, "/"), "/", "-") + "--"
	dir := filepath.Join(sessions, encoded)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() {
			candidates = append(candidates, filepath.Join(dir, e.Name()))
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool { return fileMtime(candidates[i]) > fileMtime(candidates[j]) })
	return filepath.Base(candidates[0])
}

// GeminiSessionFiles lists gemini chat session files.
func GeminiSessionFiles() []string {
	root := filepath.Join(geminiHome(), "tmp")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil
	}
	return globDir(root, filepath.Join("*", "chats", "session-*.jsonl"))
}

// GeminiSessionHeader reads the first-line header of a gemini session file.
func GeminiSessionHeader(path string) map[string]any {
	content, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	first := strings.SplitN(string(content), "\n", 2)[0]
	var record map[string]any
	if err := json.Unmarshal([]byte(first), &record); err != nil || record == nil {
		return map[string]any{}
	}
	return record
}

// DiscoverGemini finds the newest gemini session for cwd.
func DiscoverGemini(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	wanted := hex.EncodeToString(sum[:])
	files := GeminiSessionFiles()
	sort.SliceStable(files, func(i, j int) bool { return fileMtime(files[i]) > fileMtime(files[j]) })
	for _, path := range files {
		header := GeminiSessionHeader(path)
		if header["projectHash"] != wanted {
			continue
		}
		if v, ok := header["sessionId"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// GeminiSessionFile locates the session file for a gemini session id.
func GeminiSessionFile(sessionID string) string {
	for _, path := range GeminiSessionFiles() {
		if GeminiSessionHeader(path)["sessionId"] == sessionID {
			return path
		}
	}
	return ""
}

// DiscoverFx finds the newest fx session for cwd via the fx CLI.
func DiscoverFx(cwd string) string {
	binary, err := LookPathFunc("fx")
	if err != nil || binary == "" {
		candidate := filepath.Join(homeDir(), ".local", "libexec", "fx", "fx-router")
		if _, serr := os.Stat(candidate); serr != nil {
			return ""
		}
		binary = candidate
	}
	stdout, _, err := RunBinFunc(binary, []string{"sessions", "--json", "--limit", "1"}, cwd, 10*time.Second)
	if err != nil {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return ""
	}
	sessions, _ := payload["sessions"].([]any)
	if len(sessions) == 0 {
		return ""
	}
	first, _ := sessions[0].(map[string]any)
	if first == nil {
		return ""
	}
	if v, ok := first["id"].(string); ok && v != "" {
		return v
	}
	return ""
}

// Discovery maps canonical agents to their session finder.
var Discovery = map[string]func(string) string{
	"claude": DiscoverClaude, "codex": DiscoverCodex,
	"opencode": DiscoverOpencode, "pi": DiscoverPi,
	"qwen": DiscoverQwen, "copilot": DiscoverCopilot,
	"gemini": DiscoverGemini, "kimi": DiscoverKimi,
	"mcode": DiscoverMcode, "muse": DiscoverMuse,
	"vibe": DiscoverVibe, "cline": DiscoverCline,
	"grok": DiscoverGrok, "crush": DiscoverCrush,
	"omp": DiscoverOmp, "kilo": DiscoverKilo,
	"antigravity": DiscoverAntigravity, "deepseek": DiscoverDeepseek,
	"dsh-tui": DiscoverDeepseek, "fx": DiscoverFx,
	"fx-router": DiscoverFx,
}

// Discover finds the newest session id for an agent in cwd.
func Discover(agent, cwd string) string {
	fn, ok := Discovery[agent]
	if !ok {
		return ""
	}
	defer func() { _ = recover() }()
	return fn(cwd)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
