// Package zjresume is a Go port of zj-resume.py from the zellij-spaces
// helper tree: session discovery and resume-argument assembly for every
// supported coding agent, the hook client (stdin JSON, OSC 7501 emission,
// publish-or-spool transport), the __space-agents/__space-snapshot views,
// KDL layout resurrection, and the hooks/hook-server command entries.
//
// The hook-core backend (port of zj_hook_core.py) is built in parallel, so
// this package consumes it through package-level seam variables that
// default to the Python "hook_core is None" degraded behavior. The wiring
// layer binds the real implementations at startup.
package spaceresume

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Seams over zjcore, wired at startup by cmd/harness. Each defaults to the
// Python hook_core-is-None degraded behavior; all call sites are nil-safe.
var (
	// HookDisabled reports whether the hook listener is disabled.
	HookDisabled func() bool
	// NormalizeEvent builds the normalized hook envelope.
	NormalizeEvent func(agent, event string, payload map[string]any, env map[string]string) map[string]any
	// ResolvePaneEvent resolves a normalized event against the pane's stored
	// state (e.g. ATT-5 active background work) before emitting tmux/OSC status.
	ResolvePaneEvent func(session string, pane any, event map[string]any) map[string]any
	// Publish posts an envelope to the hook listener. False means spool it.
	Publish func(envelope map[string]any) bool
	// AppendSpool appends an entry to the hook spool file.
	AppendSpool func(entry map[string]any) error
	// EnsureServer lazily starts the hook listener.
	EnsureServer func(helperPath string) error
	// SpaceHooksSummary returns the spaces summary view.
	SpaceHooksSummary func() map[string]any
	// SpaceHooksDetail returns the per-pane spaces detail view.
	SpaceHooksDetail func(session string, pane any) map[string]any
	// RecordOutcome persists one recent completion/failure entry into the hook store.
	RecordOutcome func(entry map[string]any) error
	// RemoveOutcome deletes matching recent outcome entries from the hook store.
	RemoveOutcome func(match map[string]any) error
	// PaneState returns the current non-stale state for (session, pane), or "".
	PaneState func(session string, pane any) string
	// HooksStatus implements `zj hooks status`.
	HooksStatus func() int
	// HooksStop implements `zj hooks stop`.
	HooksStop func() int
	// HooksStart implements `zj hooks start|restart`.
	HooksStart func(helperPath string) int
	// Daemonize implements `zj hook-server --daemon`.
	Daemonize func() int
	// Serve runs the hook listener in the foreground.
	Serve func(foreground bool, tcpPort *int) int
	// RecordConversationOverlay upserts tmux/hook enrichment into the unified
	// conversation store overlay table. Nil-safe; wired by cmd/harness.
	RecordConversationOverlay func(entry map[string]any)
	// SpaceConversationsFunc implements `spaces __space-conversations`.
	// Nil-safe; wired by cmd/harness.
	SpaceConversationsFunc func(args []string) int
)

// Injectable seams for execution, environment observation, and output.
// Tests override these; production defaults shell out or read the OS.
var (
	// ExecFn replaces the current process with argv (Python os.execvp),
	// falling back to `sh -c original` on failure.
	ExecFn func(argv []string, original string)
	// RunBinFunc runs an arbitrary helper binary (kimi, fx, harness).
	RunBinFunc func(name string, args []string, cwd string, timeout time.Duration) (string, int, error)
	// ZellijFunc runs the zellij binary and captures stdout.
	ZellijFunc func(args []string, timeout time.Duration) (string, int, error)
	// LookPathFunc resolves a binary on PATH.
	LookPathFunc func(file string) (string, error)
	// AncestorsFunc returns process ancestor command lines, nearest first.
	AncestorsFunc func() [][]string
	// PaneCandidatesFunc returns candidate pane command lines for this pane.
	PaneCandidatesFunc func() [][]string
	// TtyPathsFunc returns writable tty paths for OSC emission, in order.
	TtyPathsFunc func() []string
	// StdinValue overrides os.Stdin for hook payload reads (nil = os.Stdin).
	StdinValue interface {
		Read([]byte) (int, error)
	}
	// Stdout receives data payloads (JSON). Defaults to os.Stdout.
	Stdout *os.File
	// Stderr receives `zj: ` status lines. Defaults to os.Stderr.
	Stderr *os.File
	// NowFunc reports the current time (snapshot deadline).
	NowFunc func() time.Time
)

func init() {
	ExecFn = realExec
	RunBinFunc = realRunBin
	ZellijFunc = realZellij
	LookPathFunc = exec.LookPath
	AncestorsFunc = processAncestorCommands
	PaneCandidatesFunc = paneCommandCandidates
	TtyPathsFunc = reportTTYPaths
	NowFunc = time.Now
}

func realExec(argv []string, original string) {
	if len(argv) == 0 {
		return
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		path = argv[0]
	}
	// syscall.Exec only returns on failure.
	if err := syscallExec(path, argv, os.Environ()); err != nil {
		syscallExec("/bin/sh", []string{"sh", "-c", original}, os.Environ())
	}
}

func realRunBin(name string, args []string, cwd string, timeout time.Duration) (string, int, error) {
	ctx, cancel := contextWithTimeout(timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode(), nil
		}
		return string(out), 0, err
	}
	return string(out), 0, nil
}

func realZellij(args []string, timeout time.Duration) (string, int, error) {
	return RunBinFunc(ZellijBinary(), args, "", timeout)
}

// Aliases maps shorthand agent names to canonical agents.
var Aliases = map[string]string{
	"agy":       "antigravity",
	"dsh":       "deepseek",
	"kimi-code": "kimi",
	"cursor":    "cursor-agent",
}

// AppNames maps canonical agents to the app label they use in OSC records.
var AppNames = map[string]string{
	"claude":   "claude-code",
	"kimi-cli": "kimi",
	"dsh":      "deepseek",
	"dsh-tui":  "deepseek",
	"cursor":   "cursor-agent",
}

// Agents is the set of known canonical agent names.
var Agents = map[string]bool{
	"claude": true, "codex": true, "opencode": true, "pi": true,
	"qwen": true, "copilot": true, "kimi": true, "kimi-cli": true,
	"mcode": true, "muse": true, "vibe": true, "gemini": true,
	"cline": true, "cursor-agent": true, "grok": true, "crush": true,
	"omp": true, "kilo": true, "antigravity": true, "deepseek": true,
	"dsh-tui": true, "fx": true, "fx-router": true,
}

// Shells is the set of shell executable basenames.
var Shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true,
	"nu": true, "ksh": true, "tcsh": true, "csh": true,
}

// Environment locations. The Python helper snapshots these at import; here
// they are functions so tests can override them with t.Setenv.

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

func xdgCache() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".cache")
}

func xdgData() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".local", "share")
}

func claudeHome() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".claude")
}

func codexHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".codex")
}

func grokHome() string {
	if v := os.Getenv("GROK_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".grok")
}

func dshHome() string {
	if v := os.Getenv("DSH_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".dsh")
}

func geminiHome() string {
	if v := os.Getenv("GEMINI_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".gemini")
}

// zellijCacheDir is the session_info cache directory.
func zellijCacheDir() string {
	if v := os.Getenv("ZJ_ZELLIJ_CACHE_DIR"); v != "" {
		return v
	}
	return filepath.Join(xdgCache(), "zellij", "contract_version_1", "session_info")
}

// Basename returns the final path component, trimming trailing slashes.
func Basename(value string) string {
	return filepath.Base(strings.TrimRight(value, "/"))
}

// CurrentDirectory returns the working directory with PWD/HOME fallbacks.
func CurrentDirectory() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if pwd := os.Getenv("PWD"); pwd != "" {
		return pwd
	}
	return homeDir()
}

// ZellijBinary returns the zellij binary path ($ZELLIJ_BIN or "zellij").
func ZellijBinary() string {
	if v := os.Getenv("ZELLIJ_BIN"); v != "" {
		return v
	}
	return "zellij"
}

// DisplayApp returns the canonical app label used in OSC 7501 records.
func DisplayApp(agentName string) string {
	name := strings.ToLower(strings.TrimSpace(Basename(agentName)))
	if normalized := Normalize(name); normalized != "" {
		name = normalized
	}
	if app, ok := AppNames[name]; ok {
		return app
	}
	return name
}

// OSCStates maps normalized hook states to OSC pane statuses.
var OSCStates = map[string]string{
	"working": "working",
	"blocked": "blocked",
	"done":    "done",
	"failed":  "error",
	"idle":    "idle",
}

func stdoutFile() *os.File {
	if Stdout != nil {
		return Stdout
	}
	return os.Stdout
}

func stderrFile() *os.File {
	if Stderr != nil {
		return Stderr
	}
	return os.Stderr
}

func errf(format string, args ...any) {
	fmt.Fprintf(stderrFile(), format, args...)
}

func out(s string) {
	fmt.Fprintln(stdoutFile(), s)
}

// environMap snapshots os.Environ into a map for the normalize seam.
func environMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return env
}

func helperPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		if abs, err := filepath.Abs(exe); err == nil {
			return abs
		}
		return exe
	}
	return "zjresume"
}

// --- shlex (POSIX) ----------------------------------------------------------

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shlexSplit splits a command line following POSIX shell quoting rules.
func shlexSplit(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	var inSingle, inDouble bool
	hasToken := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(c)
			}
			i++
		case inDouble:
			if c == '"' {
				inDouble = false
				i++
			} else if c == '\\' && i+1 < len(s) {
				n := s[i+1]
				if n == '"' || n == '\\' || n == '$' || n == '`' || n == '\n' {
					cur.WriteByte(n)
					i += 2
				} else {
					cur.WriteByte(c)
					i++
				}
			} else {
				cur.WriteByte(c)
				i++
			}
		case c == '\'':
			inSingle = true
			hasToken = true
			i++
		case c == '"':
			inDouble = true
			hasToken = true
			i++
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New("shlex: no escaped character")
			}
			n := s[i+1]
			if n == '\n' {
				i += 2
			} else {
				cur.WriteByte(n)
				i += 2
				hasToken = true
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if hasToken {
				args = append(args, cur.String())
				cur.Reset()
				hasToken = false
			}
			i++
		default:
			cur.WriteByte(c)
			hasToken = true
			i++
		}
	}
	if inSingle || inDouble {
		return nil, errors.New("shlex: no closing quotation")
	}
	if hasToken {
		args = append(args, cur.String())
	}
	return args, nil
}

// shlexQuote quotes one shell word the way Python's shlex.quote does.
func shlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// shlexJoin joins argv the way Python's shlex.join does.
func shlexJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shlexQuote(a)
	}
	return strings.Join(quoted, " ")
}

// --- Python-compatible JSON -------------------------------------------------

// marshalValue renders v with sorted map keys. Compact selects separators
// (",", ":") like the snapshot/hooks shapes; spaced selects (", ", ": ")
// like the default dumps() used by space-agents and resume-command. When
// ascii is true, non-ASCII is escaped as \uXXXX (ensure_ascii=True) and
// Go's \u0008 \u000c become Python's short \b \f escapes.
func marshalValue(v any, compact, ascii bool) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	s := strings.TrimSuffix(sb.String(), "\n")
	s = strings.ReplaceAll(s, `\u0008`, `\b`)
	s = strings.ReplaceAll(s, `\u000c`, `\f`)
	if !compact {
		s = spacify(s)
	}
	if ascii {
		s = escapeNonASCII(s)
	}
	return s
}

// marshalPython renders v with sorted map keys and compact separators,
// matching json.dumps(v, sort_keys=True, separators=(",", ":")).
// When ascii is true it also escapes non-ASCII as \uXXXX (ensure_ascii=True)
// and uses Python's short \b \f escapes.
func marshalPython(v any, ascii bool) string {
	return marshalValue(v, true, ascii)
}

// jsonField is one ordered object member.
type jsonField struct {
	Key string
	Val any
}

// marshalObject renders ordered members as a JSON object, preserving
// insertion order the way Python dicts do.
func marshalObject(fields []jsonField, compact, ascii bool) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			if compact {
				sb.WriteByte(',')
			} else {
				sb.WriteString(", ")
			}
		}
		sb.WriteString(marshalValue(f.Key, true, false))
		if compact {
			sb.WriteByte(':')
		} else {
			sb.WriteString(": ")
		}
		sb.WriteString(marshalValue(f.Val, compact, ascii))
	}
	sb.WriteByte('}')
	return sb.String()
}

// spacify converts compact JSON separators to Python's default
// (", ", ": ") outside of string literals.
func spacify(s string) string {
	var sb strings.Builder
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			sb.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			sb.WriteByte(c)
		case ':', ',':
			sb.WriteByte(c)
			sb.WriteByte(' ')
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// escapeNonASCII rewrites raw UTF-8 runes as \uXXXX (with surrogate pairs),
// matching Python's ensure_ascii=True output. s must be ASCII-safe JSON
// apart from string contents.
func escapeNonASCII(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r < 0x20 || r == '"' || r == '\\' {
			sb.WriteRune(r)
			continue
		}
		if r < 0x80 {
			sb.WriteRune(r)
			continue
		}
		if r <= 0xFFFF {
			fmt.Fprintf(&sb, `\u%04x`, r)
			continue
		}
		r -= 0x10000
		fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
	}
	return sb.String()
}

func b64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func b64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
