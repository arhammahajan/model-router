package spaceresume

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Normalize canonicalizes an executable or command name to a known agent,
// or returns "" when it is not one.
func Normalize(name string) string {
	name = strings.ToLower(Basename(name))
	for _, ext := range []string{".js", ".mjs", ".cjs", ".py", ".exe", ".cmd", ".bat"} {
		if strings.HasSuffix(name, ext) {
			name = strings.TrimSuffix(name, ext)
			break
		}
	}
	if canon, ok := Aliases[name]; ok {
		name = canon
	}
	if Agents[name] {
		return name
	}
	return ""
}

// InferAgent maps an executable path or command name to a known agent.
func InferAgent(command string) string {
	name := Basename(command)
	if normalized := Normalize(name); normalized != "" {
		return normalized
	}
	lowered := strings.ToLower(command)
	hints := [][2]string{
		{"claude", "claude"},
		{"codex", "codex"},
		{"opencode", "opencode"},
		{"kilo", "kilo"},
		{"gemini", "gemini"},
		{"qwen", "qwen"},
		{"copilot", "copilot"},
		{"kimi-cli", "kimi-cli"},
		{"kimi-code", "kimi"},
		{"kimi", "kimi"},
		{"minimax", "mcode"},
		{"mcode", "mcode"},
		{"muse", "muse"},
		{"mistral-vibe", "vibe"},
		{"vibe", "vibe"},
		{"cline", "cline"},
		{"cursor-agent", "cursor-agent"},
		{"grok", "grok"},
		{"crush", "crush"},
		{"antigravity", "antigravity"},
		{"deepseek", "deepseek"},
		{"dsh-tui", "dsh-tui"},
		{"dsh", "deepseek"},
		{"fx-router", "fx"},
		{"/fx", "fx"},
		{"omp", "omp"},
		{"/pi", "pi"},
	}
	for _, h := range hints {
		if strings.Contains(lowered, h[0]) {
			return h[1]
		}
	}
	return ""
}

// DetectShellScript recognizes an agent launched through a shell wrapper.
func DetectShellScript(argv []string) string {
	index := 1
	for index < len(argv) {
		token := argv[index]
		if token == "-c" || token == "-lc" || token == "-ic" || token == "-cl" {
			return ""
		}
		if strings.HasPrefix(token, "-") {
			index++
			continue
		}
		break
	}
	if index >= len(argv) {
		return ""
	}
	script := argv[index]
	name := Basename(script)
	if name == "zj" && index+1 < len(argv) && argv[index+1] == "__shim" {
		hold := index+2 < len(argv) && argv[index+2] == "--hold"
		binIndex := index + 4
		if hold {
			// binIndex already accounts for hold above; kept explicit.
		} else {
			binIndex = index + 3
		}
		if binIndex < len(argv) {
			return InferAgent(argv[binIndex])
		}
		if index+2 < len(argv) {
			return InferAgent(argv[index+2])
		}
		return InferAgent(script)
	}
	if name == "harness" && index+1 < len(argv) {
		if argv[index+1] == "profile" && index+2 < len(argv) {
			return ProfileAgent(argv[index+2])
		}
		return Normalize(argv[index+1])
	}
	return InferAgent(script)
}

// DetectAgent finds the agent a command line belongs to, or "" for normal commands.
func DetectAgent(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	exe := Basename(argv[0])
	if Shells[exe] {
		return DetectShellScript(argv)
	}
	if exe == "harness" {
		if len(argv) >= 2 {
			if argv[1] == "spaces" && len(argv) >= 4 && argv[2] == "__shim-profile" {
				if agent, _, ok := strings.Cut(argv[3], "/"); ok {
					return Normalize(agent)
				}
				return ProfileAgent(argv[3])
			}
			if argv[1] == "profile" {
				if len(argv) >= 3 && !strings.HasPrefix(argv[2], "-") {
					return ProfileAgent(argv[2])
				}
				return ""
			}
			return Normalize(argv[1])
		}
		return ""
	}
	if exe == "zj" || exe == "spaces" {
		if len(argv) >= 3 && argv[1] == "__shim-profile" {
			if agent, _, ok := strings.Cut(argv[2], "/"); ok {
				return Normalize(agent)
			}
			return ProfileAgent(argv[2])
		}
		if len(argv) >= 3 && argv[1] == "__shim" {
			hold := argv[2] == "--hold"
			offset := 2
			if hold {
				offset = 3
			}
			if len(argv) > offset+1 {
				return InferAgent(argv[offset+1])
			}
			if len(argv) > offset {
				return InferAgent(argv[offset])
			}
		}
		return ""
	}
	if exe == "env" || exe == "command" || exe == "exec" || exe == "nohup" || exe == "time" || exe == "sudo" {
		index := 1
		for index < len(argv) {
			token := argv[index]
			if strings.HasPrefix(token, "-") || (exe == "env" && strings.Contains(token, "=")) {
				index++
				continue
			}
			break
		}
		if index < len(argv) {
			return DetectAgent(argv[index:])
		}
		return ""
	}
	if exe == "node" || exe == "bun" || exe == "deno" {
		if len(argv) >= 2 {
			return InferAgent(argv[1])
		}
		return ""
	}
	if direct := InferAgent(argv[0]); direct != "" {
		return direct
	}
	// Accept a path to an agent launched by an interpreter or generic
	// wrapper (for example `python3 /path/to/agent`), but do not scan free
	// text such as `echo claude` and turn it into an agent launch.
	for _, token := range argv[1:] {
		if strings.HasPrefix(token, "-") || !strings.Contains(token, "/") {
			continue
		}
		if _, err := os.Stat(token); err != nil {
			continue
		}
		if candidate := InferAgent(token); candidate != "" {
			return candidate
		}
	}
	return ""
}

// ProfileAgent resolves a harness profile instance to its agent.
func ProfileAgent(instance string) string {
	harness, err := LookPathFunc("harness")
	if err != nil || harness == "" {
		return ""
	}
	stdout, code, err := RunBinFunc(harness,
		[]string{"profile", "--describe", instance}, homeDir(), 8*time.Second)
	if err != nil || code != 0 {
		return ""
	}
	described, err := shlexSplit(strings.TrimSpace(stdout))
	if err != nil || len(described) == 0 {
		return ""
	}
	return DetectAgent(described)
}

// CommandAgent detects an agent from a pane's stringified command line.
func CommandAgent(command string) string {
	argv, err := shlexSplit(command)
	if err != nil {
		return ""
	}
	if len(argv) >= 3 && Basename(argv[0]) == "zj" && argv[1] == "__resume-wrap" {
		inner, err := b64Decode(argv[2])
		if err != nil {
			return ""
		}
		argv, err = shlexSplit(inner)
		if err != nil {
			return ""
		}
	}
	return DetectAgent(argv)
}

// IsWrapperCommand reports whether argv runs through the zj/harness wrapper.
func IsWrapperCommand(argv []string) bool {
	end := 4
	if len(argv) < end {
		end = len(argv)
	}
	for _, token := range argv[:end] {
		if base := Basename(token); base == "zj" || base == "harness" {
			return true
		}
	}
	return false
}

// processAncestorCommands returns ancestor command lines, nearest first.
func processAncestorCommands() [][]string {
	commands := [][]string{}
	pid := os.Getpid()
	seen := map[int]bool{}
	for pid > 1 && !seen[pid] {
		seen[pid] = true
		fields, err := procStatFields(pid)
		if err != nil || len(fields) < 2 {
			break
		}
		var ppid int
		if _, err := splitSScan(fields[1], &ppid); err != nil {
			break
		}
		if raw, err := os.ReadFile(filepath.Join("/proc", itoa(ppid), "cmdline")); err == nil {
			var argv []string
			for _, part := range strings.Split(string(raw), "\x00") {
				if part != "" {
					argv = append(argv, part)
				}
			}
			if len(argv) > 0 {
				commands = append(commands, argv)
			}
		}
		pid = ppid
	}
	return commands
}

// ProcessAncestorCommands is the exported ancestor-walk used by seams/tests.
func ProcessAncestorCommands() [][]string { return processAncestorCommands() }

// paneCommandCandidates lists candidate command lines for this pane from
// zellij list-panes output.
func paneCommandCandidates() [][]string {
	paneID := os.Getenv("ZELLIJ_PANE_ID")
	if paneID == "" {
		return nil
	}
	digits := regexp.MustCompile(`\D`).ReplaceAllString(paneID, "")
	if digits == "" {
		return nil
	}
	var number int
	if _, err := splitSScan(digits, &number); err != nil {
		return nil
	}
	stdout, code, err := ZellijFunc([]string{"action", "list-panes", "--json", "--all"}, 5*time.Second)
	if err != nil || code != 0 {
		return nil
	}
	var panes []map[string]any
	if err := jsonUnmarshal([]byte(stdout), &panes); err != nil {
		return nil
	}
	var matches []map[string]any
	for _, pane := range panes {
		if id, ok := toInt(pane["id"]); !ok || id != number {
			continue
		}
		if isPlugin(pane) {
			continue
		}
		matches = append(matches, pane)
	}
	if len(matches) == 0 {
		return nil
	}
	pane := matches[0]
	for _, m := range matches {
		if focused, _ := m["is_focused"].(bool); focused {
			pane = m
			break
		}
	}
	var candidates []string
	for _, key := range []string{"pane_command", "terminal_command"} {
		if v, ok := pane[key].(string); ok && v != "" {
			candidates = append(candidates, v)
		}
	}
	if cs, ok := pane["command_state"].(map[string]any); ok {
		for _, section := range []string{"running", "last"} {
			sec, _ := cs[section].(map[string]any)
			cmd, _ := sec["command"].(map[string]any)
			if v, _ := cmd["text"].(string); v != "" {
				candidates = append(candidates, v)
			}
		}
	}
	var commands [][]string
	for _, candidate := range candidates {
		argv, err := shlexSplit(candidate)
		if err != nil || len(argv) == 0 {
			continue
		}
		dup := false
		for _, c := range commands {
			if equalStrings(c, argv) {
				dup = true
				break
			}
		}
		if !dup {
			commands = append(commands, argv)
		}
	}
	return commands
}

// PaneCommandCandidates is the exported candidate-listing used by seams/tests.
func PaneCommandCandidates() [][]string { return paneCommandCandidates() }

// ChooseBaseCommand picks the pane's base agent command from candidates,
// stripping any stale resume switch.
func ChooseBaseCommand(agent string, candidates [][]string) []string {
	var usable [][]string
	for _, argv := range candidates {
		if containsString(argv, "__resume-wrap") || containsString(argv, "session-hook") {
			continue
		}
		if DetectAgent(argv) != agent {
			continue
		}
		var rest []string
		if len(argv) > 1 {
			rest = argv[1:]
		}
		if IsNoninteractive(agent, rest) {
			continue
		}
		usable = append(usable, argv)
	}
	var base []string
	var wrappers [][]string
	for _, argv := range usable {
		if IsWrapperCommand(argv) {
			wrappers = append(wrappers, argv)
		}
	}
	switch {
	case len(wrappers) > 0:
		base = wrappers[0]
	case len(usable) > 0:
		base = usable[0]
	default:
		base = []string{agent}
	}
	return append([]string{base[0]}, StripResumeArgs(agent, base[1:])...)
}

// AgentIsNoninteractive reports whether the nearest agent ancestor is a
// non-interactive run. It uses the nearest agent command, not every
// ancestor: a `claude -p` nested inside an interactive Claude session must
// still be treated as headless.
func AgentIsNoninteractive(agent string) bool {
	candidates := AncestorsFunc()
	var agentCommands [][]string
	for _, argv := range candidates {
		if DetectAgent(argv) == agent {
			agentCommands = append(agentCommands, argv)
		}
	}
	if len(agentCommands) == 0 {
		return false
	}
	var rest []string
	if len(agentCommands[0]) > 1 {
		rest = agentCommands[0][1:]
	}
	return IsNoninteractive(agent, rest)
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

func isPlugin(pane map[string]any) bool {
	if b, ok := pane["is_plugin"].(bool); ok {
		return b
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
