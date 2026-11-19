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

const maxDiscoveredTitleRunes = 200

// CleanDiscoveredTitle normalizes a title or initial prompt extracted from a
// local harness session store into a clean single-line title. Returns "" when
// the text is empty, a placeholder ("New session"), or pure XML/system tags.
func CleanDiscoveredTitle(s string) string {
	return cleanDiscoveredTitle(s)
}

// ExtractTextFromContent extracts the first clean plain-text string from a
// polymorphic JSON message content field.
func ExtractTextFromContent(content any) string {
	return extractTextFromContent(content)
}

// cleanDiscoveredTitle normalizes a title or initial prompt extracted from a
// local harness session store into a clean single-line title. Returns "" when
// the text is empty, a placeholder ("New session"), or pure XML/system tags.
func cleanDiscoveredTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Unwrap <user_input ...>...</user_input> or <USER_REQUEST>...</USER_REQUEST>
	for _, tag := range []string{"user_input", "USER_REQUEST"} {
		openIdx := strings.Index(s, "<"+tag)
		if openIdx >= 0 {
			if gt := strings.IndexByte(s[openIdx:], '>'); gt >= 0 {
				inner := s[openIdx+gt+1:]
				if closeIdx := strings.Index(inner, "</"+tag+">"); closeIdx >= 0 {
					s = strings.TrimSpace(inner[:closeIdx])
				}
			}
		}
	}
	// Ignore synthetic system/command blocks.
	for _, prefix := range []string{
		"<local-command-", "<command-name>", "<environment_context>",
		"<session_context>", "<system-reminder>", "<t3_code_instructions>",
		"<permissions", "<instructions>", "<ide_", "<context>",
		"# AGENTS.md instructions for ",
	} {
		if strings.HasPrefix(s, prefix) {
			return ""
		}
	}
	// Flatten whitespace.
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if lower == "new session" || strings.HasPrefix(lower, "new session - ") || lower == "untitled" {
		return ""
	}
	// Strip leading slash commands like "/plan " when followed by prompt text,
	// or ignore bare slash commands like "/model" or "/clear".
	if strings.HasPrefix(s, "/") {
		fields := strings.Fields(s)
		if len(fields) <= 1 {
			return ""
		}
		switch strings.ToLower(fields[0]) {
		case "/model", "/login", "/logout", "/clear", "/compact", "/help", "/status", "/config":
			return ""
		default:
			s = strings.Join(fields[1:], " ")
		}
	}
	r := []rune(s)
	if len(r) > maxDiscoveredTitleRunes {
		return string(r[:maxDiscoveredTitleRunes-1]) + "…"
	}
	return s
}

func extractTextFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return cleanDiscoveredTitle(v)
	case []any:
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				if t := cleanDiscoveredTitle(elem); t != "" {
					return t
				}
			case map[string]any:
				if typ, _ := elem["type"].(string); typ == "thinking" || typ == "tool_use" || typ == "tool_result" {
					continue
				}
				for _, k := range []string{"text", "content", "input_text"} {
					if s, ok := elem[k].(string); ok {
						if t := cleanDiscoveredTitle(s); t != "" {
							return t
						}
					}
				}
			}
		}
	case map[string]any:
		for _, k := range []string{"text", "content"} {
			if raw, ok := v[k]; ok {
				if t := extractTextFromContent(raw); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// DiscoverTitleClaude finds the conversation title for a Claude session by
// sessionID or newest session in cwd.
func DiscoverTitleClaude(cwd, sessionID string) string {
	projects := filepath.Join(claudeHome(), "projects")
	var candidates []string
	if sessionID != "" {
		if cwd != "" {
			direct := filepath.Join(projects, EncodedPath(cwd), sessionID+".jsonl")
			if _, err := os.Stat(direct); err == nil {
				candidates = append(candidates, direct)
			}
		}
		if len(candidates) == 0 {
			candidates = globDir(projects, filepath.Join("*", sessionID+".jsonl"))
		}
	}
	if len(candidates) == 0 && cwd != "" {
		dir := filepath.Join(projects, EncodedPath(cwd))
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			candidates, _ = filepath.Glob(filepath.Join(dir, "*.jsonl"))
		}
		if len(candidates) == 0 {
			for _, path := range globDir(projects, filepath.Join("*", "*.jsonl")) {
				if fileMatchesCWD(path, cwd) {
					candidates = append(candidates, path)
				}
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return fileMtime(candidates[i]) > fileMtime(candidates[j])
	})
	for _, path := range candidates {
		if sessionID == "" && cwd != "" && !fileMatchesCWD(path, cwd) {
			continue
		}
		if title := claudeTitleFromFile(path); title != "" {
			return title
		}
	}
	return ""
}

func fileMatchesCWD(path, cwd string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := strings.SplitN(string(content), "\n", 10)
	for _, line := range lines {
		if line == "" || !strings.Contains(line, `"cwd"`) {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record["cwd"] == cwd {
			return true
		}
	}
	return false
}

func claudeTitleFromFile(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var aiTitle, customTitle, summary, firstPrompt, lastPrompt string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		typ, _ := rec["type"].(string)
		switch typ {
		case "custom-title":
			if s, _ := rec["customTitle"].(string); cleanDiscoveredTitle(s) != "" {
				customTitle = cleanDiscoveredTitle(s)
			} else if s, _ := rec["title"].(string); cleanDiscoveredTitle(s) != "" {
				customTitle = cleanDiscoveredTitle(s)
			}
		case "ai-title":
			if s, _ := rec["aiTitle"].(string); cleanDiscoveredTitle(s) != "" {
				aiTitle = cleanDiscoveredTitle(s)
			} else if s, _ := rec["title"].(string); cleanDiscoveredTitle(s) != "" {
				aiTitle = cleanDiscoveredTitle(s)
			}
		case "summary":
			if s, _ := rec["summary"].(string); cleanDiscoveredTitle(s) != "" {
				summary = cleanDiscoveredTitle(s)
			}
		case "last-prompt":
			if s, _ := rec["lastPrompt"].(string); cleanDiscoveredTitle(s) != "" {
				lastPrompt = cleanDiscoveredTitle(s)
			}
		case "user":
			if firstPrompt == "" {
				if msg, ok := rec["message"].(map[string]any); ok {
					firstPrompt = extractTextFromContent(msg["content"])
				} else {
					firstPrompt = extractTextFromContent(rec["content"])
				}
			}
		}
	}
	for _, cand := range []string{customTitle, aiTitle, summary, firstPrompt, lastPrompt} {
		if cand != "" {
			return cand
		}
	}
	return ""
}

// DiscoverTitleCodex finds the conversation title/prompt for a Codex session.
func DiscoverTitleCodex(cwd, sessionID string) string {
	sessions := filepath.Join(codexHome(), "sessions")
	if fi, err := os.Stat(sessions); err != nil || !fi.IsDir() {
		return ""
	}
	var matches []string
	_ = filepath.Walk(sessions, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(info.Name(), "rollout-") && strings.HasSuffix(info.Name(), ".jsonl") {
			if sessionID == "" || strings.Contains(info.Name(), sessionID) {
				matches = append(matches, path)
			}
		}
		return nil
	})
	sort.SliceStable(matches, func(i, j int) bool { return fileMtime(matches[i]) > fileMtime(matches[j]) })
	for _, path := range matches {
		if t, ok := codexTitleFromFile(path, cwd, sessionID); ok && t != "" {
			return t
		}
	}
	return ""
}

func codexTitleFromFile(path, cwd, sessionID string) (string, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) == 0 {
		return "", false
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first["type"] != "session_meta" {
		return "", false
	}
	payload, _ := first["payload"].(map[string]any)
	if payload == nil {
		return "", false
	}
	if sessionID != "" {
		sid, _ := payload["session_id"].(string)
		if sid == "" {
			sid, _ = payload["id"].(string)
		}
		if sid != sessionID {
			return "", false
		}
	} else if cwd != "" && payload["cwd"] != cwd {
		return "", false
	}
	for _, k := range []string{"title", "name", "summary", "thread_name"} {
		if s, ok := payload[k].(string); ok {
			if t := cleanDiscoveredTitle(s); t != "" {
				return t, true
			}
		}
	}
	for i := 1; i < len(lines) && i < 60; i++ {
		if lines[i] == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			continue
		}
		p, _ := rec["payload"].(map[string]any)
		if p == nil {
			continue
		}
		if p["role"] == "user" {
			if t := extractTextFromContent(p["content"]); t != "" {
				return t, true
			}
		}
		if p["type"] == "user_message" {
			if t := extractTextFromContent(p["message"]); t != "" {
				return t, true
			}
			if t := extractTextFromContent(p["text"]); t != "" {
				return t, true
			}
		}
	}
	return "", true
}

// DiscoverTitleOpencode finds the title for an OpenCode session in opencode.db.
func DiscoverTitleOpencode(cwd, sessionID string) string {
	database := filepath.Join(xdgData(), "opencode", "opencode.db")
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	for _, table := range []string{"session_v2", "session"} {
		filter := map[string]string{}
		if sessionID != "" {
			filter["id"] = sessionID
		} else if cwd != "" {
			filter["directory"] = cwd
		}
		rows, err := sqliteRows(database, table, filter, "time_updated", 5)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if t := cleanDiscoveredTitle(sqliteText(row["title"])); t != "" {
				return t
			}
		}
	}
	return ""
}

// DiscoverTitleKilo finds the title for a Kilo session in kilo.db.
func DiscoverTitleKilo(cwd, sessionID string) string {
	database := filepath.Join(xdgData(), "kilo", "kilo.db")
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	for _, table := range []string{"session", "session_v2"} {
		filter := map[string]string{}
		if sessionID != "" {
			filter["id"] = sessionID
		} else if cwd != "" {
			filter["directory"] = cwd
		}
		rows, err := sqliteRows(database, table, filter, "time_updated", 5)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if t := cleanDiscoveredTitle(sqliteText(row["title"])); t != "" {
				return t
			}
		}
	}
	return ""
}

// DiscoverTitleMuse finds the title for a Muse session in session-index.db.
func DiscoverTitleMuse(cwd, sessionID string) string {
	database := filepath.Join(xdgData(), "muse", "session-index.db")
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	filter := map[string]string{}
	if sessionID != "" {
		filter["session_id"] = sessionID
	} else if cwd != "" {
		filter["workspace_root"] = cwd
	}
	rows, err := sqliteRows(database, "sessions", filter, "updated_at_us", 5)
	if err != nil {
		return ""
	}
	for _, row := range rows {
		for _, col := range []string{"session_name", "effective_long_title", "title", "msp_title", "first_user_prompt", "msp_first_user_prompt"} {
			if t := cleanDiscoveredTitle(sqliteText(row[col])); t != "" {
				return t
			}
		}
	}
	return ""
}

// DiscoverTitleMcode finds the title for an mcode session in runtime-state.sqlite.
func DiscoverTitleMcode(cwd, sessionID string) string {
	database := filepath.Join(homeDir(), ".minimax", "v2", "sqlite", "runtime-state.sqlite")
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	filter := map[string]string{}
	if sessionID != "" {
		filter["session_id"] = sessionID
	} else if cwd != "" {
		filter["workspace_dir"] = cwd
	}
	rows, err := sqliteRows(database, "local_runtime_sessions", filter, "updated_at_ms", 10)
	if err != nil {
		return ""
	}
	for _, row := range rows {
		if sessionID == "" {
			if kind := sqliteText(row["session_kind"]); kind == "task" {
				continue
			}
		}
		for _, col := range []string{"title", "purpose"} {
			if t := cleanDiscoveredTitle(sqliteText(row[col])); t != "" {
				return t
			}
		}
		if raw := sqliteText(row["record_json"]); raw != "" {
			var rec map[string]any
			if json.Unmarshal([]byte(raw), &rec) == nil {
				for _, k := range []string{"title", "name", "summary", "purpose"} {
					if s, ok := rec[k].(string); ok {
						if t := cleanDiscoveredTitle(s); t != "" {
							return t
						}
					}
				}
			}
		}
	}
	return ""
}

// DiscoverTitleAntigravity finds the title for an Antigravity conversation,
// checking both the default home and any profile homes under ~/.model-router.
func DiscoverTitleAntigravity(cwd, sessionID string) string {
	home := homeDir()
	dbs := []string{filepath.Join(home, ".gemini", "antigravity-cli", "conversation_summaries.db")}
	if profDBs, err := filepath.Glob(filepath.Join(home, ".model-router", "profiles", "antigravity", "*", "home", ".gemini", "antigravity-cli", "conversation_summaries.db")); err == nil {
		dbs = append(dbs, profDBs...)
	}
	type candidate struct {
		modified string
		title    string
	}
	var best candidate
	wanted := "file://" + strings.TrimRight(cwd, "/")
	for _, database := range dbs {
		if _, err := os.Stat(database); err != nil {
			continue
		}
		filter := map[string]string{}
		if sessionID != "" {
			filter["conversation_id"] = sessionID
		}
		rows, err := sqliteRows(database, "conversation_summaries", filter, "last_modified_time", 200)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if sqliteText(row["conversation_id"]) == "" {
				continue
			}
			if sessionID == "" && cwd != "" {
				raw := sqliteText(row["workspace_uris"])
				var uris []string
				if err := json.Unmarshal([]byte(raw), &uris); err != nil {
					uris = []string{raw}
				}
				matched := false
				for _, uri := range uris {
					if strings.TrimRight(uri, "/") == wanted {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			t := cleanDiscoveredTitle(sqliteText(row["title"]))
			if t == "" {
				t = cleanDiscoveredTitle(sqliteText(row["preview"]))
			}
			if t == "" {
				continue
			}
			if sessionID != "" {
				return t
			}
			mod := sqliteText(row["last_modified_time"])
			if best.title == "" || mod > best.modified {
				best = candidate{modified: mod, title: t}
			}
			break
		}
	}
	return best.title
}

// DiscoverTitleCrush finds the title for a Crush session in crush.db.
func DiscoverTitleCrush(cwd, sessionID string) string {
	database := filepath.Join(cwd, ".crush", "crush.db")
	registry := filepath.Join(xdgData(), "crush", "projects.json")
	if _, err := os.Stat(database); err != nil {
		if content, rerr := os.ReadFile(registry); rerr == nil {
			var reg map[string]any
			if json.Unmarshal(content, &reg) == nil {
				projects, _ := reg["projects"].([]any)
				bestLen := -1
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
							database = filepath.Join(dataDir, "crush.db")
						}
					}
				}
			}
		}
	}
	if _, err := os.Stat(database); err != nil {
		return ""
	}
	filter := map[string]string{}
	if sessionID != "" {
		filter["id"] = sessionID
	}
	rows, err := sqliteRows(database, "sessions", filter, "updated_at", 5)
	if err != nil {
		return ""
	}
	for _, row := range rows {
		for _, col := range []string{"title", "name", "summary"} {
			if t := cleanDiscoveredTitle(sqliteText(row[col])); t != "" {
				return t
			}
		}
	}
	return ""
}

// DiscoverTitlePi finds the title or first user prompt for a Pi session.
func DiscoverTitlePi(cwd, sessionID string) string {
	return discoverTitlePiOrOmp(filepath.Join(homeDir(), ".pi", "agent", "sessions"), cwd, sessionID)
}

// DiscoverTitleOmp finds the title or first user prompt for an Omp session.
func DiscoverTitleOmp(cwd, sessionID string) string {
	return discoverTitlePiOrOmp(filepath.Join(homeDir(), ".omp", "agent", "sessions"), cwd, sessionID)
}

func discoverTitlePiOrOmp(root, cwd, sessionID string) string {
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	var matches []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".jsonl") {
			if sessionID == "" || strings.Contains(info.Name(), sessionID) {
				matches = append(matches, path)
			}
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
		matchedSession := false
		var explicitTitle, firstPrompt string
		for i := 0; i < len(lines) && i < 30; i++ {
			if lines[i] == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
				continue
			}
			switch rec["type"] {
			case "title":
				if s, _ := rec["title"].(string); cleanDiscoveredTitle(s) != "" {
					explicitTitle = cleanDiscoveredTitle(s)
				}
			case "session":
				sid, _ := rec["id"].(string)
				if sessionID != "" {
					if sid != sessionID {
						break
					}
					matchedSession = true
				} else if cwd != "" {
					if rec["cwd"] != cwd {
						break
					}
					matchedSession = true
				} else {
					matchedSession = true
				}
				if s, _ := rec["title"].(string); cleanDiscoveredTitle(s) != "" {
					explicitTitle = cleanDiscoveredTitle(s)
				}
			case "message":
				if firstPrompt == "" {
					if msg, ok := rec["message"].(map[string]any); ok && msg["role"] == "user" {
						firstPrompt = extractTextFromContent(msg["content"])
					}
				}
			}
		}
		if !matchedSession {
			continue
		}
		if explicitTitle != "" {
			return explicitTitle
		}
		if firstPrompt != "" {
			return firstPrompt
		}
	}
	return ""
}

// DiscoverTitleQwen finds the title or first user prompt for a Qwen session.
func DiscoverTitleQwen(cwd, sessionID string) string {
	projects := filepath.Join(homeDir(), ".qwen", "projects")
	var candidates []string
	if sessionID != "" {
		candidates = globDir(projects, filepath.Join("*", "chats", sessionID+".jsonl"))
	}
	if len(candidates) == 0 && cwd != "" {
		dir := filepath.Join(projects, EncodedPath(cwd), "chats")
		for _, p := range globDir(dir, "*.jsonl") {
			if !strings.HasSuffix(p, ".runtime.json") {
				candidates = append(candidates, p)
			}
		}
		if len(candidates) == 0 {
			for _, p := range globDir(projects, filepath.Join("*", "chats", "*.jsonl")) {
				if !strings.HasSuffix(p, ".runtime.json") && fileMatchesCWD(p, cwd) {
					candidates = append(candidates, p)
				}
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return fileMtime(candidates[i]) > fileMtime(candidates[j]) })
	for _, path := range candidates {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if s, _ := rec["title"].(string); cleanDiscoveredTitle(s) != "" {
				return cleanDiscoveredTitle(s)
			}
			if rec["type"] == "user" {
				if msg, ok := rec["message"].(map[string]any); ok {
					if t := extractTextFromContent(msg["parts"]); t != "" {
						return t
					}
					if t := extractTextFromContent(msg["content"]); t != "" {
						return t
					}
				}
			}
		}
	}
	return ""
}

// DiscoverTitleCopilot finds the name/title for a Copilot session.
func DiscoverTitleCopilot(cwd, sessionID string) string {
	root := filepath.Join(homeDir(), ".copilot", "session-state")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	var files []string
	if sessionID != "" {
		p := filepath.Join(root, sessionID, "workspace.yaml")
		if _, err := os.Stat(p); err == nil {
			files = []string{p}
		}
	}
	if len(files) == 0 {
		files = globDir(root, filepath.Join("*", "workspace.yaml"))
		sort.SliceStable(files, func(i, j int) bool { return fileMtime(files[i]) > fileMtime(files[j]) })
	}
	for _, ws := range files {
		content, err := os.ReadFile(ws)
		if err != nil {
			continue
		}
		var sid, scwd, name string
		for _, line := range strings.Split(string(content), "\n") {
			if !strings.Contains(line, ":") {
				continue
			}
			k, v, _ := strings.Cut(line, ":")
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `'"`)
			switch k {
			case "id":
				sid = v
			case "cwd":
				scwd = v
			case "name", "title", "summary":
				if name == "" && v != "|-" && v != "|" && v != ">-" && v != ">" {
					name = v
				}
			}
		}
		if sessionID != "" && sid != sessionID {
			continue
		}
		if sessionID == "" && cwd != "" && scwd != cwd {
			continue
		}
		if t := cleanDiscoveredTitle(name); t != "" {
			return t
		}
	}
	return ""
}

// DiscoverTitleGrok finds the session_summary for a Grok session.
func DiscoverTitleGrok(cwd, sessionID string) string {
	root := filepath.Join(grokHome(), "sessions")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	summaries := globDir(root, filepath.Join("*", "*", "summary.json"))
	sort.SliceStable(summaries, func(i, j int) bool { return fileMtime(summaries[i]) > fileMtime(summaries[j]) })
	for _, summary := range summaries {
		content, err := os.ReadFile(summary)
		if err != nil {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(content, &rec); err != nil {
			continue
		}
		info, _ := rec["info"].(map[string]any)
		if info == nil {
			continue
		}
		if sessionID != "" && info["id"] != sessionID {
			continue
		}
		if sessionID == "" && cwd != "" && info["cwd"] != cwd {
			continue
		}
		for _, k := range []string{"session_summary", "title", "summary"} {
			if s, ok := rec[k].(string); ok {
				if t := cleanDiscoveredTitle(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// DiscoverTitleCline finds the title or initial prompt for a Cline session.
func DiscoverTitleCline(cwd, sessionID string) string {
	root := filepath.Join(homeDir(), ".cline", "data", "sessions")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	files := globDir(root, filepath.Join("*", "*.json"))
	sort.SliceStable(files, func(i, j int) bool { return fileMtime(files[i]) > fileMtime(files[j]) })
	for _, path := range files {
		if strings.HasSuffix(path, ".messages.json") {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(content, &rec); err != nil {
			continue
		}
		if sessionID != "" && rec["session_id"] != sessionID {
			continue
		}
		if sessionID == "" && cwd != "" && rec["cwd"] != cwd {
			continue
		}
		for _, k := range []string{"title", "prompt"} {
			if s, ok := rec[k].(string); ok {
				if t := cleanDiscoveredTitle(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// DiscoverTitleVibe finds the title for a Vibe session.
func DiscoverTitleVibe(cwd, sessionID string) string {
	root := filepath.Join(homeDir(), ".vibe", "logs", "session", "unified")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	metas := globDir(root, filepath.Join("*", "meta.json"))
	sort.SliceStable(metas, func(i, j int) bool { return fileMtime(metas[i]) > fileMtime(metas[j]) })
	for _, meta := range metas {
		content, err := os.ReadFile(meta)
		if err != nil {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(content, &rec); err != nil {
			continue
		}
		if sessionID != "" && rec["session_id"] != sessionID {
			continue
		}
		if sessionID == "" && cwd != "" {
			working := ""
			if env, _ := rec["environment"].(map[string]any); env != nil {
				working, _ = env["working_directory"].(string)
			}
			if working == "" {
				working, _ = rec["origin_directory"].(string)
			}
			if working != cwd {
				continue
			}
		}
		if s, ok := rec["title"].(string); ok {
			if t := cleanDiscoveredTitle(s); t != "" {
				return t
			}
		}
	}
	return ""
}

// DiscoverTitleGemini finds the first user prompt or title for a Gemini session.
func DiscoverTitleGemini(cwd, sessionID string) string {
	var files []string
	if sessionID != "" {
		if f := GeminiSessionFile(sessionID); f != "" {
			files = []string{f}
		}
	}
	if len(files) == 0 {
		sum := sha256.Sum256([]byte(cwd))
		wanted := hex.EncodeToString(sum[:])
		all := GeminiSessionFiles()
		sort.SliceStable(all, func(i, j int) bool { return fileMtime(all[i]) > fileMtime(all[j]) })
		for _, p := range all {
			if cwd == "" || GeminiSessionHeader(p)["projectHash"] == wanted {
				files = append(files, p)
			}
		}
	}
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if s, ok := rec["title"].(string); ok {
				if t := cleanDiscoveredTitle(s); t != "" {
					return t
				}
			}
			if rec["type"] == "user" || rec["role"] == "user" {
				if t := extractTextFromContent(rec["content"]); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// DiscoverTitleKimi finds the custom_title or first user prompt for a Kimi session.
func DiscoverTitleKimi(cwd, sessionID string) string {
	root := filepath.Join(homeDir(), ".kimi", "sessions")
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		var dirs []string
		if sessionID != "" {
			dirs = globDir(root, filepath.Join("*", sessionID))
		} else {
			dirs = globDir(root, filepath.Join("*", "*"))
			sort.SliceStable(dirs, func(i, j int) bool { return fileMtime(dirs[i]) > fileMtime(dirs[j]) })
		}
		for _, d := range dirs {
			if data, err := os.ReadFile(filepath.Join(d, "state.json")); err == nil {
				var st map[string]any
				if json.Unmarshal(data, &st) == nil {
					if s, ok := st["custom_title"].(string); ok {
						if t := cleanDiscoveredTitle(s); t != "" {
							return t
						}
					}
					if s, ok := st["title"].(string); ok {
						if t := cleanDiscoveredTitle(s); t != "" {
							return t
						}
					}
				}
			}
			if data, err := os.ReadFile(filepath.Join(d, "wire.jsonl")); err == nil {
				for _, line := range strings.SplitN(string(data), "\n", 15) {
					if line == "" {
						continue
					}
					var rec map[string]any
					if json.Unmarshal([]byte(line), &rec) != nil {
						continue
					}
					msg, _ := rec["message"].(map[string]any)
					if msg != nil && msg["type"] == "TurnBegin" {
						if p, _ := msg["payload"].(map[string]any); p != nil {
							if t := extractTextFromContent(p["user_input"]); t != "" {
								return t
							}
						}
					}
				}
			}
		}
	}
	// Fallback to kimi CLI if available.
	kimi, err := LookPathFunc("kimi")
	if err != nil || kimi == "" {
		return ""
	}
	stdout, _, err := RunBinFunc(kimi,
		[]string{"session", "list", "--json", "--cwd", cwd, "--limit", "5"},
		cwd, 3*time.Second)
	if err != nil {
		return ""
	}
	var payload []map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return ""
	}
	for _, item := range payload {
		if sessionID != "" && item["id"] != sessionID {
			continue
		}
		for _, k := range []string{"title", "custom_title", "name", "summary"} {
			if s, ok := item[k].(string); ok {
				if t := cleanDiscoveredTitle(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// TitleDiscovery maps canonical agents to their conversation title finder.
var TitleDiscovery = map[string]func(cwd, sessionID string) string{
	"claude":      DiscoverTitleClaude,
	"codex":       DiscoverTitleCodex,
	"opencode":    DiscoverTitleOpencode,
	"pi":          DiscoverTitlePi,
	"qwen":        DiscoverTitleQwen,
	"copilot":     DiscoverTitleCopilot,
	"gemini":      DiscoverTitleGemini,
	"kimi":        DiscoverTitleKimi,
	"kimi-cli":    DiscoverTitleKimi,
	"mcode":       DiscoverTitleMcode,
	"muse":        DiscoverTitleMuse,
	"vibe":        DiscoverTitleVibe,
	"cline":       DiscoverTitleCline,
	"grok":        DiscoverTitleGrok,
	"crush":       DiscoverTitleCrush,
	"omp":         DiscoverTitleOmp,
	"kilo":        DiscoverTitleKilo,
	"antigravity": DiscoverTitleAntigravity,
}

// DiscoverTitle finds the conversation title for an agent by sessionID or
// newest session in cwd.
func DiscoverTitle(agent, cwd, sessionID string) string {
	norm := Normalize(agent)
	if norm == "" {
		norm = strings.ToLower(strings.TrimSpace(agent))
	}
	if norm == "claude-code" {
		norm = "claude"
	}
	fn, ok := TitleDiscovery[norm]
	if !ok {
		return ""
	}
	defer func() { _ = recover() }()
	return fn(cwd, sessionID)
}
