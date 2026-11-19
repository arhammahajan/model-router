package spaceresume

import (
	"fmt"
	"os"
	"strings"
)

// IsNoninteractive reports whether args make an agent run one-shot/headless
// instead of interactive, so headless invocations are never resumed.
func IsNoninteractive(agent string, args []string) bool {
	if agent == "cline" {
		for _, arg := range args {
			if arg == "-i" || arg == "--tui" {
				return false
			}
		}
		return true
	}
	if agent == "codex" || agent == "opencode" || agent == "crush" ||
		agent == "mcode" || agent == "muse" {
		leading := []string{}
		for _, arg := range args {
			if len(leading) >= 2 {
				break
			}
			if strings.HasPrefix(arg, "-") {
				continue
			}
			leading = append(leading, arg)
		}
		for _, arg := range leading {
			if arg == "exec" || arg == "run" {
				return true
			}
		}
		return false
	}
	if agent == "fx" || agent == "fx-router" {
		leading := []string{}
		for _, arg := range args {
			if len(leading) >= 2 {
				break
			}
			if strings.HasPrefix(arg, "-") {
				continue
			}
			leading = append(leading, arg)
		}
		for _, arg := range leading {
			if arg == "ask" || arg == "session" || arg == "sessions" {
				return true
			}
		}
		return false
	}
	printFlags := map[string][]string{
		"claude": {"-p", "--print"}, "pi": {"-p", "--print"},
		"qwen": {"-p", "--prompt"}, "copilot": {"-p", "--prompt"},
		"gemini": {"-p", "--prompt"}, "vibe": {"-p", "--prompt"},
		"grok": {"-p", "--single"}, "omp": {"-p", "--print"},
		"kimi": {"-p", "--prompt"}, "kimi-cli": {"-p", "--prompt"},
		"antigravity": {"-p", "--prompt"}, "deepseek": {"-p", "--prompt"},
		"dsh-tui": {"-p", "--prompt"},
	}
	for _, flag := range printFlags[agent] {
		for _, arg := range args {
			if arg == flag {
				return true
			}
		}
	}
	return false
}

// ResumeArgs returns the resume switch for an agent and session id,
// or the "continue latest" switch when the id is unknown.
func ResumeArgs(agent string, sessionID string) []string {
	switch agent {
	case "claude":
		if sessionID != "" {
			return []string{"--resume", sessionID}
		}
		return []string{"--continue"}
	case "codex":
		if sessionID != "" {
			return []string{"resume", sessionID}
		}
		return []string{"resume", "--last"}
	case "opencode", "pi", "kimi", "kimi-cli", "mcode", "kilo", "crush":
		if sessionID != "" {
			return []string{"--session", sessionID}
		}
		return []string{"--continue"}
	case "copilot":
		if sessionID != "" {
			return []string{"--resume=" + sessionID}
		}
		return []string{"--continue"}
	case "qwen", "grok", "vibe":
		if sessionID != "" {
			return []string{"--resume", sessionID}
		}
		return []string{"--continue"}
	case "muse":
		if sessionID != "" {
			return []string{"resume", sessionID}
		}
		return []string{"resume", "--last"}
	case "gemini":
		if sessionID != "" {
			if looksLikeSessionFilePath(sessionID) {
				return []string{"--session-file", sessionID}
			}
			if path := GeminiSessionFile(sessionID); path != "" {
				return []string{"--session-file", path}
			}
			return []string{"--resume", sessionID}
		}
		return []string{"--resume", "latest"}
	case "cline":
		if sessionID != "" {
			return []string{"--id", sessionID}
		}
		return []string{}
	case "cursor-agent":
		if sessionID != "" {
			return []string{"--resume", sessionID}
		}
		return []string{"--continue"}
	case "omp":
		if sessionID != "" {
			return []string{"--resume=" + sessionID}
		}
		return []string{"--continue"}
	case "antigravity":
		if sessionID != "" {
			return []string{"--conversation", sessionID}
		}
		return []string{"--continue"}
	case "deepseek", "dsh-tui":
		if sessionID != "" {
			return []string{"--resume", sessionID}
		}
		return []string{"--continue"}
	case "fx", "fx-router":
		return []string{"--continue"}
	default:
		return []string{}
	}
}

// ShortResumeFlags lists short resume switches per agent.
var ShortResumeFlags = map[string][]string{
	"claude": {"-c", "-r"}, "opencode": {"-c", "-s"},
	"pi": {"-c", "-r"}, "qwen": {"-c", "-r"},
	"copilot": {"-r"}, "kimi": {"-c", "-S"},
	"kimi-cli": {"-c", "-r"}, "mcode": {"-c"},
	"vibe": {"-c"}, "gemini": {"-r"},
	"grok": {"-c", "-r"}, "crush": {"-C", "-s"},
	"omp": {"-c", "-r"}, "kilo": {"-c", "-s"},
	"antigravity": {"-c"}, "deepseek": {"-c"},
	"dsh-tui": {"-c"},
}

// StripResumeArgs removes a previous resume target so the hook's current id wins.
func StripResumeArgs(agent string, args []string) []string {
	valueFlags := map[string]bool{
		"--resume": true, "--session": true, "--conversation": true,
		"--id": true, "--session-file": true,
	}
	shortValueFlags := map[string]map[string]bool{
		"claude": {"-r": true}, "opencode": {"-s": true},
		"pi": {"-r": true}, "qwen": {"-r": true},
		"copilot": {"-r": true}, "kimi": {"-S": true},
		"kimi-cli": {"-r": true}, "gemini": {"-r": true},
		"grok": {"-r": true}, "crush": {"-s": true},
		"omp": {"-r": true}, "kilo": {"-s": true},
	}
	shortToggleFlags := map[string]map[string]bool{
		"claude": {"-c": true}, "opencode": {"-c": true},
		"pi": {"-c": true}, "qwen": {"-c": true},
		"kimi": {"-c": true}, "kimi-cli": {"-c": true},
		"mcode": {"-c": true}, "vibe": {"-c": true},
		"grok": {"-c": true}, "crush": {"-C": true},
		"omp": {"-c": true}, "kilo": {"-c": true},
		"antigravity": {"-c": true}, "deepseek": {"-c": true},
		"dsh-tui": {"-c": true},
	}
	isResumeSubcommand := agent == "codex" || agent == "muse"
	cleaned := []string{}
	index := 0
	for index < len(args) {
		token := args[index]
		if valueFlags[token] || shortValueFlags[agent][token] {
			index += 2
			continue
		}
		hasEqPrefix := false
		for flag := range valueFlags {
			if strings.HasPrefix(token, flag+"=") {
				hasEqPrefix = true
				break
			}
		}
		if hasEqPrefix {
			index++
			continue
		}
		if token == "--continue" || shortToggleFlags[agent][token] {
			index++
			continue
		}
		if isResumeSubcommand && token == "resume" {
			index++
			if index < len(args) && !strings.HasPrefix(args[index], "-") {
				index++
			} else if index < len(args) && args[index] == "--last" {
				index++
			}
			continue
		}
		if (agent == "fx" || agent == "fx-router") && token == "session" {
			if index+1 < len(args) && args[index+1] == "resume" {
				index += 2
				if index < len(args) && !strings.HasPrefix(args[index], "-") {
					index++
				}
				continue
			}
		}
		cleaned = append(cleaned, token)
		index++
	}
	return cleaned
}

// AlreadyResuming reports whether args already carry a resume switch.
func AlreadyResuming(agent string, args []string) bool {
	if agent == "codex" || agent == "muse" {
		for _, a := range args {
			if a == "resume" {
				return true
			}
		}
	}
	for _, flag := range []string{"--resume", "--continue", "--session", "--conversation", "--id", "--session-file"} {
		for _, a := range args {
			if a == flag || strings.HasPrefix(a, flag+"=") {
				return true
			}
		}
	}
	for _, short := range ShortResumeFlags[agent] {
		for _, a := range args {
			if a == short {
				return true
			}
		}
	}
	return false
}

// IsolateAgent gives OpenCode a private server so panes do not share OSC
// 7501 status.
func IsolateAgent(agent string, argv []string) []string {
	if agent != "opencode" || len(argv) == 0 {
		return argv
	}
	for _, arg := range argv[1:] {
		if arg == "--standalone" || arg == "--server" || strings.HasPrefix(arg, "--server=") {
			return argv
		}
	}
	if Basename(argv[0]) == "zj" && len(argv) > 1 && argv[1] == "__shim" {
		// The shim adds the flag to the child command it launches.
		return argv
	}
	for i, token := range argv {
		if strings.ToLower(Basename(token)) == "opencode" {
			out := append([]string(nil), argv[:i+1]...)
			out = append(out, "--standalone")
			return append(out, argv[i+1:]...)
		}
	}
	return argv
}

// ExtractSessionIDFromArgs returns an explicit conversation/session ID carried
// in an agent's CLI arguments (e.g. --conversation, --resume, --session,
// codex/muse `resume <id>`), or "" when absent.
func ExtractSessionIDFromArgs(agent string, args []string) string {
	for i := 0; i < len(args); i++ {
		tok := args[i]
		for _, flag := range []string{"--conversation", "--resume", "--session", "--id"} {
			if tok == flag && i+1 < len(args) {
				v := strings.TrimSpace(args[i+1])
				if v != "" && !strings.HasPrefix(v, "-") && v != "latest" {
					return v
				}
			}
			if strings.HasPrefix(tok, flag+"=") {
				v := strings.TrimSpace(strings.TrimPrefix(tok, flag+"="))
				if v != "" && v != "latest" {
					return v
				}
			}
		}
		if (agent == "codex" || agent == "muse") && tok == "resume" && i+1 < len(args) {
			v := strings.TrimSpace(args[i+1])
			if v != "" && !strings.HasPrefix(v, "-") {
				return v
			}
		}
	}
	return ""
}

// Wrap runs the encoded resurrect command, adding the agent's resume switch.
// explicitSessionID wins when non-empty; otherwise the newest local session
// is discovered. Agent commands that already resume, non-interactive runs,
// and non-agent commands exec unchanged (with the opencode isolation rule).
func Wrap(encoded string, explicitSessionID string) int {
	original, err := b64Decode(encoded)
	if err != nil {
		errf("zj: could not decode resurrect command: %v\n", err)
		return 2
	}
	argv, err := shlexSplit(original)
	if err != nil {
		ExecFn([]string{"/bin/sh", "-c", original}, original)
		return 0
	}
	agent := DetectAgent(argv)
	if agent == "" {
		ExecFn(argv, original)
		return 0
	}
	var rest []string
	if len(argv) > 1 {
		rest = argv[1:]
	}
	if IsNoninteractive(agent, rest) {
		ExecFn(argv, original)
		return 0
	}
	// A restored pane should come back idle, not working, and should get a
	// remembered idle status so it is visible in the Inbox. Set the title
	// first so the Inbox labels it with the agent rather than the wrapper.
	EmitPaneTitle(DisplayApp(agent))
	PrimeAgentStatus(agent)
	if explicitSessionID != "" {
		if inTmuxPane() {
			recordTmuxResumeTarget(agent, explicitSessionID)
		}
		if RecordConversationOverlay != nil {
			nowMs := NowFunc().UnixMilli()
			RecordConversationOverlay(map[string]any{
				"agent":          agent,
				"session_id":     explicitSessionID,
				"profile":        harnessProfileFromArgv(argv),
				"cwd":            CurrentDirectory(),
				"last_opened_ms": nowMs,
				"updated_at_ms":  nowMs,
			})
		}
		argv = append([]string{argv[0]}, StripResumeArgs(agent, argv[1:])...)
		if extra := ResumeArgs(agent, explicitSessionID); len(extra) > 0 {
			argv = append(argv, extra...)
		}
		ExecFn(IsolateAgent(agent, argv), original)
		return 0
	}
	if AlreadyResuming(agent, rest) {
		ExecFn(IsolateAgent(agent, argv), original)
		return 0
	}
	sessionID := ""
	if agent == "antigravity" {
		if prof := harnessProfileFromArgv(argv); prof != "" {
			sessionID = DiscoverAntigravityForProfile(CurrentDirectory(), prof)
		}
	}
	if sessionID == "" {
		sessionID = Discover(agent, CurrentDirectory())
	}
	if extra := ResumeArgs(agent, sessionID); len(extra) > 0 {
		argv = append(argv, extra...)
	}
	ExecFn(IsolateAgent(agent, argv), original)
	return 0
}

func looksLikeSessionFilePath(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.Contains(s, string(os.PathSeparator)) {
		if strings.HasSuffix(s, ".jsonl") || strings.HasSuffix(s, ".json") {
			return true
		}
		if fi, err := os.Stat(s); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

func harnessProfileFromArgv(argv []string) string {
	if len(argv) < 3 {
		return ""
	}
	base := Basename(argv[0])
	if base == "spaces" || base == "zj" {
		if argv[1] == "__shim-profile" && !strings.HasPrefix(argv[2], "-") {
			if _, prof, ok := strings.Cut(argv[2], "/"); ok {
				return prof
			}
			return argv[2]
		}
		return ""
	}
	if base != "harness" {
		return ""
	}
	switch {
	case argv[1] == "profile" && !strings.HasPrefix(argv[2], "-"):
		return argv[2]
	case len(argv) >= 4 && argv[2] == "profile" && !strings.HasPrefix(argv[3], "-"):
		return argv[3]
	case len(argv) >= 4 && argv[1] == "spaces" && argv[2] == "__shim-profile":
		if _, prof, ok := strings.Cut(argv[3], "/"); ok {
			return prof
		}
		return argv[3]
	}
	return ""
}

// HookResurrect implements the no-agent `hook` path: Zellij's
// post_command_discovery_hook. When Zellij serializes a pane running an
// interactive coding agent, the command is wrapped so the restored pane
// reopens the agent instead of a bare shell.
func HookResurrect() int {
	original := strings.TrimSpace(os.Getenv("RESURRECT_COMMAND"))
	if original == "" {
		return 0
	}
	if strings.Contains(original, "__resume-wrap") {
		out(original)
		return 0
	}
	argv, err := shlexSplit(original)
	if err != nil {
		out(original)
		return 0
	}
	agent := DetectAgent(argv)
	if agent == "" {
		out(original)
		return 0
	}
	var rest []string
	if len(argv) > 1 {
		rest = argv[1:]
	}
	if IsNoninteractive(agent, rest) {
		out(original)
		return 0
	}
	encoded := b64Encode(original)
	out(fmt.Sprintf("zj __resume-wrap %s", encoded))
	return 0
}

// ResumeCommand prints the resume plan for an agent in a directory:
// {"agent","cwd","session_id","args"}.
// Optional flags: --profile <name>, --title <title>, --exclude <id1,id2,...>.
func ResumeCommand(args []string) int {
	if len(args) < 1 {
		errf("usage: zj resume-command <agent> [cwd] [--profile P] [--title T] [--exclude id1,id2]\n")
		return 2
	}
	var positional []string
	var profile, title string
	exclude := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile":
			if i+1 < len(args) {
				profile = args[i+1]
				i++
			}
		case "--title":
			if i+1 < len(args) {
				title = args[i+1]
				i++
			}
		case "--exclude":
			if i+1 < len(args) {
				for _, id := range strings.Split(args[i+1], ",") {
					id = strings.TrimSpace(id)
					if id != "" {
						exclude[id] = true
					}
				}
				i++
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 1 {
		errf("usage: zj resume-command <agent> [cwd] [--profile P] [--title T] [--exclude id1,id2]\n")
		return 2
	}
	cwd := CurrentDirectory()
	if len(positional) >= 2 && positional[1] != "" {
		cwd = positional[1]
	}
	normalized := Normalize(positional[0])
	if normalized == "" {
		normalized = positional[0]
	}
	var sessionID string
	if normalized == "antigravity" && (profile != "" || title != "" || len(exclude) > 0) {
		sessionID = DiscoverAntigravityWithOptions(cwd, profile, title, exclude)
	} else {
		sessionID = Discover(normalized, cwd)
	}
	extra := ResumeArgs(normalized, sessionID)
	if extra == nil {
		extra = []string{}
	}
	var sid any
	if sessionID != "" {
		sid = sessionID
	}
	out(marshalObject([]jsonField{
		{"agent", normalized}, {"cwd", cwd},
		{"session_id", sid}, {"args", extra},
	}, false, true))
	return 0
}
