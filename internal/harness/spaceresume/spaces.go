package spaceresume

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// RunningSessionNamesResult returns running session names and whether
// list-sessions succeeded.
func RunningSessionNamesResult() ([]string, bool) {
	stdout, code, err := ZellijFunc([]string{"list-sessions", "-n"}, 5*time.Second)
	if err != nil || code != 0 {
		return nil, false
	}
	var names []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "EXITED") {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			names = append(names, fields[0])
		}
	}
	return names, true
}

// RunningSessionNames returns the running session names.
func RunningSessionNames() []string {
	names, _ := RunningSessionNamesResult()
	if names == nil {
		return []string{}
	}
	return names
}

// SessionPanesResult returns a session's panes and whether list-panes succeeded.
func SessionPanesResult(session string) ([]map[string]any, bool) {
	stdout, code, err := ZellijFunc([]string{
		"--session", session, "action", "list-panes", "--json", "--all",
	}, 5*time.Second)
	if err != nil || code != 0 {
		return nil, false
	}
	var panes []map[string]any
	if err := json.Unmarshal([]byte(stdout), &panes); err != nil || panes == nil {
		return nil, false
	}
	return panes, true
}

// SessionPanes returns a session's panes.
func SessionPanes(session string) []map[string]any {
	panes, _ := SessionPanesResult(session)
	if panes == nil {
		return []map[string]any{}
	}
	return panes
}

// PaneApp resolves the agent app label for one pane record.
func PaneApp(pane map[string]any) string {
	if attention, ok := pane["attention"].(map[string]any); ok {
		if detail, ok := attention["detail"].(map[string]any); ok {
			if app, ok := detail["app"].(string); ok && strings.TrimSpace(app) != "" {
				return DisplayApp(app)
			}
		}
	}
	if records, ok := pane["program_status"].([]any); ok {
		for i := len(records) - 1; i >= 0; i-- {
			if rec, ok := records[i].(map[string]any); ok {
				if app, ok := rec["app"].(string); ok && strings.TrimSpace(app) != "" {
					return DisplayApp(app)
				}
			}
		}
	}
	for _, key := range []string{"terminal_command", "pane_command"} {
		if command, ok := pane[key].(string); ok && strings.TrimSpace(command) != "" {
			if agent := CommandAgent(command); agent != "" {
				return DisplayApp(agent)
			}
		}
	}
	return ""
}

var spaceAgentsSkipped = map[string]bool{
	"terminal": true, "shell": true, "zj": true,
	"sh": true, "bash": true, "zsh": true, "fish": true,
}

// SpaceAgents reports the agent app for each live pane, across all running
// spaces. Peer's session manifests omit program status and terminal
// commands, so this live list-panes pass keeps idle agents distinguishable
// from plain terminals.
func SpaceAgents() int {
	payload := map[string]map[string]string{}
	for _, session := range RunningSessionNames() {
		apps := map[string]string{}
		for _, pane := range SessionPanes(session) {
			if isPlugin(pane) {
				continue
			}
			app := PaneApp(pane)
			if app == "" || spaceAgentsSkipped[app] {
				continue
			}
			id, ok := toInt(pane["id"])
			if !ok {
				continue
			}
			apps[itoa(id)] = app
		}
		if len(apps) > 0 {
			payload[session] = apps
		}
	}
	out(marshalValue(anyMap(payload), false, true))
	return 0
}

func anyMap(m map[string]map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		inner := make(map[string]any, len(v))
		for k2, v2 := range v {
			inner[k2] = v2
		}
		out[k] = inner
	}
	return out
}

// Snapshot caps.
const (
	SnapshotMaxPanes = 64
	SnapshotMaxLines = 200
	SnapshotMaxBytes = 32768
	SnapshotDeadline = 8.0
)

// SnapshotShellLabels are apps treated as plain shells (not snapshotted).
var SnapshotShellLabels = map[string]bool{
	"terminal": true, "shell": true, "zj": true,
	"sh": true, "bash": true, "zsh": true, "fish": true,
}

// TrimScreenText trims padding and keeps the bottom of an over-long viewport.
func TrimScreenText(text string) string {
	lines := strings.Split(text, "\n")
	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed = append(trimmed, strings.TrimRight(line, " \t\r"))
	}
	for len(trimmed) > 0 && strings.TrimSpace(trimmed[len(trimmed)-1]) == "" {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if len(trimmed) > SnapshotMaxLines {
		trimmed = trimmed[len(trimmed)-SnapshotMaxLines:]
	}
	rendered := strings.Join(trimmed, "\n")
	if len(rendered) <= SnapshotMaxBytes {
		return rendered
	}
	tail := []byte(rendered)[len(rendered)-SnapshotMaxBytes:]
	// Keep the tail on a UTF-8 boundary.
	for len(tail) > 0 && tail[0]>>6 == 0x02 {
		tail = tail[1:]
	}
	return string(tail)
}

// DumpPaneScreen captures a pane's viewport text.
func DumpPaneScreen(session string, paneID int, timeoutSecs float64) (bool, string) {
	if timeoutSecs < 0.2 {
		timeoutSecs = 0.2
	}
	stdout, code, err := ZellijFunc([]string{
		"--session", session, "action", "dump-screen",
		"--pane-id", "terminal_" + itoa(paneID),
	}, time.Duration(timeoutSecs*float64(time.Second)))
	if err != nil || code != 0 {
		return false, ""
	}
	return true, TrimScreenText(stdout)
}

// SpaceSnapshot reports viewport text for agent panes across every running
// space. Failures are explicit: `ok` says list-sessions succeeded, each
// session carries `ok` for list-panes, and each pane carries `dump_ok`.
func SpaceSnapshot() int {
	sessions, sessionsOK := RunningSessionNamesResult()
	sessObjs := map[string]string{}
	remaining := SnapshotMaxPanes
	deadline := NowFunc().Add(time.Duration(SnapshotDeadline * float64(time.Second)))
	for _, session := range sessions {
		panes, panesOK := SessionPanesResult(session)
		paneObjs := map[string]string{}
		if panesOK {
			for _, pane := range panes {
				if isPlugin(pane) {
					continue
				}
				if truthy(pane["exited"]) || truthy(pane["is_held"]) {
					continue
				}
				app := PaneApp(pane)
				if app == "" || SnapshotShellLabels[app] {
					continue
				}
				paneID, ok := toInt(pane["id"])
				if !ok {
					continue
				}
				screen := ""
				dumpOK := false
				if remaining > 0 && NowFunc().Before(deadline) {
					remaining--
					dumpOK, screen = DumpPaneScreen(session, paneID, time.Until(deadline).Seconds())
				}
				title, _ := pane["title"].(string)
				paneObjs[itoa(paneID)] = marshalObject([]jsonField{
					{"app", app}, {"dump_ok", dumpOK},
					{"screen", screen}, {"title", title},
				}, true, false)
			}
		}
		sessObjs[session] = marshalObject([]jsonField{
			{"ok", panesOK},
			{"panes", json.RawMessage(joinObjects(paneObjs))},
		}, true, false)
	}
	out(marshalObject([]jsonField{
		{"ok", sessionsOK},
		{"sessions", json.RawMessage(joinObjects(sessObjs))},
	}, true, false))
	return 0
}

// joinObjects renders a string-keyed map of pre-rendered JSON objects with
// sorted keys.
func joinObjects(objs map[string]string) string {
	keys := make([]string, 0, len(objs))
	for k := range objs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(marshalValue(k, true, false))
		sb.WriteByte(':')
		sb.WriteString(objs[k])
	}
	sb.WriteByte('}')
	return sb.String()
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case nil:
		return false
	default:
		return true
	}
}

// SpaceHooks implements `zj __space-hooks [--detail SESSION PANE]`.
func SpaceHooks(args []string) int {
	if SpaceHooksSummary == nil {
		out(marshalObject([]jsonField{
			{"ok", false}, {"version", 0},
			{"panes", map[string]any{}}, {"sessions", map[string]any{}},
			{"outcomes", []any{}},
		}, false, true))
		return 0
	}
	if len(args) > 0 && args[0] == "--titles" {
		return SpaceHookTitles(args[1:])
	}
	if len(args) > 0 && args[0] == "--record-outcome" {
		return SpaceHookRecordOutcome(args[1:])
	}
	if len(args) > 0 && args[0] == "--remove-outcome" {
		return SpaceHookRemoveOutcome(args[1:])
	}
	if len(args) > 0 && args[0] == "--detail" {
		if len(args) < 3 {
			errf("usage: zj __space-hooks --detail SESSION PANE\n")
			return 2
		}
		var pane any = args[2]
		if detailInt, ok := paneInt(args[2]); ok {
			pane = detailInt
		}
		var detail map[string]any
		if SpaceHooksDetail != nil {
			detail = SpaceHooksDetail(args[1], pane)
		}
		if detail == nil {
			detail = map[string]any{}
		}
		out(marshalPython(detail, false))
		return 0
	}
	out(marshalPython(SpaceHooksSummary(), false))
	return 0
}

// TitleQuery is one pane query for `spaces __space-hooks --titles`.
type TitleQuery struct {
	Key       string `json:"key"`
	Agent     string `json:"agent"`
	CWD       string `json:"cwd,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// SpaceHookTitles resolves conversation titles from local harness session stores
// for a JSON array of TitleQuery objects passed as the first argument.
func SpaceHookTitles(args []string) int {
	titles := map[string]string{}
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		var queries []TitleQuery
		if err := json.Unmarshal([]byte(args[0]), &queries); err == nil {
			type lookupKey struct {
				agent, cwd, sid string
			}
			cache := map[lookupKey]string{}
			for _, q := range queries {
				if q.Key == "" || q.Agent == "" {
					continue
				}
				lk := lookupKey{agent: q.Agent, cwd: q.CWD, sid: q.SessionID}
				t, seen := cache[lk]
				if !seen {
					t = DiscoverTitle(q.Agent, q.CWD, q.SessionID)
					cache[lk] = t
				}
				if t != "" {
					titles[q.Key] = t
				}
			}
		}
	}
	out(marshalPython(map[string]any{"ok": true, "titles": titles}, false))
	return 0
}

// SpaceHookRecordOutcome records one recent completion or failure outcome JSON
// object into the hook store, enriching missing session/pane/session_id/title
// from the tmux environment and local harness session stores.
func SpaceHookRecordOutcome(args []string) int {
	if RecordOutcome == nil || len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		out(marshalPython(map[string]any{"ok": false}, false))
		return 0
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(args[0]), &entry); err != nil || entry == nil {
		out(marshalPython(map[string]any{"ok": false}, false))
		return 0
	}
	session, _ := entry["session"].(string)
	paneStr := ""
	switch pv := entry["pane"].(type) {
	case string:
		paneStr = strings.TrimSpace(pv)
	case float64:
		paneStr = itoa(int(pv))
	}
	profile, _ := entry["profile"].(string)
	cwd, _ := entry["cwd"].(string)
	sid, _ := entry["session_id"].(string)
	title, _ := entry["title"].(string)

	if paneStr == "" {
		if s, p, ok := hookTarget(); ok {
			if session == "" {
				session = s
				entry["session"] = s
			}
			paneStr = itoa(p)
			entry["pane"] = paneStr
		}
	}
	// Query tmux pane metadata whenever any context field is missing so
	// outcomes recorded via OSC 7501 or __shim still capture @spaces_profile,
	// @spaces_resume_session, @spaces_title, @spaces_prior_outcome, session, and pane CWD.
	priorOutcome := ""
	if paneStr != "" && TmuxFunc != nil {
		paneRef := paneStr
		if !strings.HasPrefix(paneRef, "%") {
			paneRef = "%" + paneRef
		}
		if stdout, code, err := TmuxFunc([]string{
			"display-message", "-p", "-t", paneRef,
			"#{session_name}\x1f#{pane_current_path}\x1f#{@spaces_profile}\x1f#{@spaces_resume_session}\x1f#{@spaces_title}\x1f#{@spaces_prior_outcome}",
		}, 2*time.Second); err == nil && code == 0 {
			parts := strings.Split(strings.TrimSuffix(stdout, "\n"), "\x1f")
			getPart := func(i int) string {
				if i < len(parts) {
					v := strings.TrimSpace(parts[i])
					if v != "" && v != "-" {
						return v
					}
				}
				return ""
			}
			if session == "" {
				if v := getPart(0); v != "" {
					session = v
					entry["session"] = session
				}
			}
			if strings.TrimSpace(cwd) == "" {
				if v := getPart(1); v != "" {
					cwd = v
					entry["cwd"] = cwd
				}
			}
			if strings.TrimSpace(profile) == "" {
				if v := getPart(2); v != "" {
					profile = v
					entry["profile"] = profile
				}
			}
			if strings.TrimSpace(sid) == "" {
				if v := getPart(3); v != "" {
					sid = v
					entry["session_id"] = sid
				}
			}
			if strings.TrimSpace(title) == "" {
				if v := getPart(4); v != "" {
					title = v
					entry["title"] = title
				}
			}
			priorOutcome = getPart(5)
		}
	}
	if session == "" {
		if s, p, ok := hookTarget(); ok {
			session = s
			entry["session"] = s
			if paneStr == "" {
				paneStr = itoa(p)
				entry["pane"] = paneStr
			}
		}
	}
	if paneStr != "" {
		entry["pane"] = strings.TrimPrefix(paneStr, "%")
	}

	agent, _ := entry["agent"].(string)
	app, _ := entry["app"].(string)
	if agent == "" && app != "" {
		if norm := Normalize(app); norm != "" {
			agent = norm
		} else {
			agent = strings.ToLower(strings.TrimSpace(app))
		}
		entry["agent"] = agent
	}
	if app == "" && agent != "" {
		app = DisplayApp(agent)
		entry["app"] = app
	}
	if strings.TrimSpace(profile) == "" {
		if envProf := strings.TrimSpace(os.Getenv("HARNESS_PROFILE")); envProf != "" {
			profile = envProf
			entry["profile"] = profile
		}
	}
	if strings.TrimSpace(cwd) == "" {
		cwd = CurrentDirectory()
		entry["cwd"] = cwd
	}
	if strings.TrimSpace(sid) == "" && agent != "" && cwd != "" {
		if agent == "antigravity" && strings.TrimSpace(profile) != "" {
			sid = DiscoverAntigravityForProfile(cwd, strings.TrimSpace(profile))
		}
		if sid == "" {
			sid = Discover(agent, cwd)
		}
		if sid != "" {
			entry["session_id"] = sid
		}
	}
	if strings.TrimSpace(profile) == "" && agent != "" && strings.TrimSpace(sid) != "" {
		if inferred := DiscoverProfile(agent, sid); inferred != "" {
			profile = inferred
			entry["profile"] = profile
		}
	}
	if strings.TrimSpace(title) == "" && agent != "" && (sid != "" || cwd != "") {
		if t := DiscoverTitle(agent, cwd, sid); t != "" {
			entry["title"] = t
		}
	}

	outcomeStr, _ := entry["outcome"].(string)
	textStr, _ := entry["text"].(string)
	if strings.EqualFold(strings.TrimSpace(outcomeStr), "done") {
		paneFailed := PaneState != nil && session != "" && paneStr != "" &&
			PaneState(session, strings.TrimPrefix(paneStr, "%")) == "failed"
		if strings.EqualFold(strings.TrimSpace(textStr), "finished") && (priorOutcome == "failed" || paneFailed) {
			// Closing a failed conversation (reopened while idle or failed
			// during this session) keeps it failed instead of flipping to done.
			entry["outcome"] = "failed"
		} else if !strings.EqualFold(strings.TrimSpace(textStr), "finished") && paneStr != "" && TmuxFunc != nil {
			// A completed turn clears any prior failure marker on the pane.
			paneRef := paneStr
			if !strings.HasPrefix(paneRef, "%") {
				paneRef = "%" + paneRef
			}
			_, _, _ = TmuxFunc([]string{"set-option", "-p", "-u", "-t", paneRef, "@spaces_prior_outcome"}, 2*time.Second)
		}
	}

	if err := RecordOutcome(entry); err != nil {
		out(marshalPython(map[string]any{"ok": false}, false))
		return 0
	}
	if RecordConversationOverlay != nil && strings.TrimSpace(agent) != "" && strings.TrimSpace(sid) != "" {
		titleVal, _ := entry["title"].(string)
		outcomeVal, _ := entry["outcome"].(string)
		modelVal, _ := entry["model"].(string)
		RecordConversationOverlay(map[string]any{
			"agent":         strings.TrimSpace(agent),
			"session_id":    strings.TrimSpace(sid),
			"profile":       strings.TrimSpace(profile),
			"space":         strings.TrimSpace(session),
			"cwd":           strings.TrimSpace(cwd),
			"title":         strings.TrimSpace(titleVal),
			"model":         strings.TrimSpace(modelVal),
			"last_outcome":  strings.TrimSpace(outcomeVal),
			"updated_at_ms": NowFunc().UnixMilli(),
		})
	}
	out(marshalPython(map[string]any{"ok": true}, false))
	return 0
}

// SpaceConversations implements `spaces __space-conversations` by delegating to
// SpaceConversationsFunc, or returning a degraded empty JSON payload when unset.
func SpaceConversations(args []string) int {
	if SpaceConversationsFunc != nil {
		return SpaceConversationsFunc(args)
	}
	out(marshalObject([]jsonField{
		{"ok", false},
		{"conversations", []any{}},
	}, true, false))
	return 0
}

// SpaceHookRemoveOutcome removes a matching outcome entry from hooks.store.json
// when a user reopens that conversation into a live tab.
func SpaceHookRemoveOutcome(args []string) int {
	if RemoveOutcome == nil || len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		out(marshalPython(map[string]any{"ok": false}, false))
		return 0
	}
	var match map[string]any
	if err := json.Unmarshal([]byte(args[0]), &match); err != nil || match == nil {
		out(marshalPython(map[string]any{"ok": false}, false))
		return 0
	}
	if err := RemoveOutcome(match); err != nil {
		out(marshalPython(map[string]any{"ok": false}, false))
		return 0
	}
	out(marshalPython(map[string]any{"ok": true}, false))
	return 0
}
