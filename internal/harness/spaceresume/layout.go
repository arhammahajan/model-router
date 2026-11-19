package spaceresume

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/arhammahajan/model-router/internal/harness/spacesqlite"
)

// KdlUnescape decodes KDL string escape sequences.
func KdlUnescape(value string) string {
	replacements := map[byte]string{
		'\\': "\\", '"': "\"", '/': "/",
		'b': "\b", 'f': "\f", 'n': "\n",
		'r': "\r", 't': "\t",
	}
	var sb strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) && value[i+1] != '\n' {
			if rep, ok := replacements[value[i+1]]; ok {
				sb.WriteString(rep)
			} else {
				sb.WriteByte(value[i+1])
			}
			i++
			continue
		}
		sb.WriteByte(value[i])
	}
	return sb.String()
}

var kdlPropRe = regexp.MustCompile(`([A-Za-z_][\w-]*)=("(?:[^"\\]|\\.)*"|[^\s]+)`)

// ParseKDLProperties parses `key=value` pairs from a KDL node header
// (everything before the opening brace).
func ParseKDLProperties(header string) map[string]string {
	body := header
	if i := strings.IndexByte(header, '{'); i >= 0 {
		body = header[:i]
	}
	properties := map[string]string{}
	for _, match := range kdlPropRe.FindAllStringSubmatch(body, -1) {
		key, value := match[1], match[2]
		if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			value = KdlUnescape(value[1 : len(value)-1])
		}
		properties[key] = value
	}
	return properties
}

// ParseKDLStrings tokenizes KDL string/identifier arguments.
func ParseKDLStrings(text string) []string {
	var values []string
	i, n := 0, len(text)
	for i < n {
		if text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r' {
			i++
			continue
		}
		if text[i] != '"' {
			start := i
			for i < n && text[i] != ' ' && text[i] != '\t' && text[i] != '\n' && text[i] != '\r' {
				i++
			}
			values = append(values, text[start:i])
			continue
		}
		i++
		var buf strings.Builder
		for i < n {
			c := text[i]
			if c == '\\' && i+1 < n {
				following := text[i+1]
				switch following {
				case 'b':
					buf.WriteByte('\b')
				case 'f':
					buf.WriteByte('\f')
				case 'n':
					buf.WriteByte('\n')
				case 'r':
					buf.WriteByte('\r')
				case 't':
					buf.WriteByte('\t')
				case '"':
					buf.WriteByte('"')
				case '\\':
					buf.WriteByte('\\')
				case '/':
					buf.WriteByte('/')
				default:
					buf.WriteByte(following)
				}
				i += 2
				continue
			}
			if c == '"' {
				i++
				break
			}
			buf.WriteByte(c)
			i++
		}
		values = append(values, buf.String())
	}
	return values
}

// BraceDelta counts the net brace depth change of a line, ignoring braces
// inside quoted strings.
func BraceDelta(line string) int {
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inString {
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
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return depth
}

var (
	paneLineRe        = regexp.MustCompile(`^[ \t]*pane\b`)
	argsLineRe        = regexp.MustCompile(`^([ \t]*)args\s+(.*?)\s*$`)
	startSuspendedRe  = regexp.MustCompile(`^[ \t]*start_suspended\b`)
	headerSuspendedRe = regexp.MustCompile(`[ \t]+start_suspended=(?:true|false)`)
	commandRe         = regexp.MustCompile(`command=("(?:[^"\\]|\\.)*")`)
)

// RewriteLayout makes saved commands auto-run and routes agents through the
// resume wrapper: it strips start_suspended and rewrites agent pane commands
// to `zj __resume-wrap <base64>`.
func RewriteLayout(text string) string {
	lines := strings.SplitAfter(text, "\n")
	// SplitAfter leaves a trailing empty element when text ends with \n;
	// drop it so reassembly is exact.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var output []string
	index := 0
	for index < len(lines) {
		line := lines[index]
		if !paneLineRe.MatchString(line) || !strings.Contains(line, "command=") {
			output = append(output, line)
			index++
			continue
		}
		depth := BraceDelta(line)
		end := index + 1
		for depth > 0 && end < len(lines) {
			depth += BraceDelta(lines[end])
			end++
		}
		block := append([]string(nil), lines[index:end]...)
		header := block[0]
		properties := ParseKDLProperties(header)
		command, hasCommand := properties["command"]
		var args []string
		argsIndex := -1
		for offset := 1; offset < len(block); offset++ {
			if m := argsLineRe.FindStringSubmatch(strings.TrimRight(block[offset], "\n")); m != nil {
				args = ParseKDLStrings(m[2])
				argsIndex = offset
				break
			}
		}
		if hasCommand && command != "" {
			argv := append([]string{command}, args...)
			agent := DetectAgent(argv)
			if agent != "" && !IsNoninteractive(agent, args) {
				original := shlexJoin(argv)
				encoded := b64Encode(original)
				header = replaceFirst(commandRe, header, `command="zj"`)
				indent := strings.Repeat(" ", len(header)-len(strings.TrimLeft(header, " \t"))+4)
				newArgs := indent + `args "__resume-wrap" "` + encoded + "\"\n"
				if argsIndex >= 0 {
					block[argsIndex] = newArgs
				} else {
					block = append(block[:1], append([]string{newArgs}, block[1:]...)...)
				}
				block[0] = header
			}
		}
		var kept []string
		for _, bl := range block {
			if !startSuspendedRe.MatchString(bl) {
				kept = append(kept, bl)
			}
		}
		block = kept
		block[0] = headerSuspendedRe.ReplaceAllString(block[0], "")
		output = append(output, block...)
		index = end
	}
	return strings.Join(output, "")
}

// replaceFirst replaces the first regex match in s with repl.
func replaceFirst(re *regexp.Regexp, s, repl string) string {
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return s
	}
	var sb strings.Builder
	sb.WriteString(s[:loc[0]])
	// Expand $ references like ReplaceAllString does.
	sb.Write(re.ExpandString(nil, repl, s, loc))
	sb.WriteString(s[loc[1]:])
	return sb.String()
}

// SessionInfoDirectories lists zellij session_info cache directories.
func SessionInfoDirectories() []string {
	if os.Getenv("ZJ_ZELLIJ_CACHE_DIR") != "" {
		return []string{zellijCacheDir()}
	}
	base := filepath.Dir(filepath.Dir(zellijCacheDir())) // ~/.cache/zellij
	entries, _ := filepath.Glob(filepath.Join(base, "*", "session_info"))
	var dirs []string
	for _, e := range entries {
		if fi, err := os.Stat(e); err == nil && fi.IsDir() {
			dirs = append(dirs, e)
		}
	}
	sort.Strings(dirs)
	found := false
	for _, d := range dirs {
		if d == zellijCacheDir() {
			found = true
			break
		}
	}
	if !found {
		dirs = append(dirs, zellijCacheDir())
	}
	return dirs
}

// SessionIndexDatabases lists zellij session_index.sqlite files.
func SessionIndexDatabases() []string {
	if os.Getenv("ZJ_ZELLIJ_CACHE_DIR") != "" {
		database := filepath.Join(filepath.Dir(zellijCacheDir()), "session_index.sqlite")
		if fi, err := os.Stat(database); err == nil && !fi.IsDir() {
			return []string{database}
		}
		return nil
	}
	base := filepath.Dir(filepath.Dir(zellijCacheDir())) // ~/.cache/zellij
	entries, _ := filepath.Glob(filepath.Join(base, "*", "session_index.sqlite"))
	database := filepath.Join(filepath.Dir(zellijCacheDir()), "session_index.sqlite")
	var out []string
	for _, e := range entries {
		if fi, err := os.Stat(e); err == nil && !fi.IsDir() {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	found := false
	for _, d := range out {
		if d == database {
			found = true
			break
		}
	}
	if !found {
		out = append(out, database)
	}
	var existing []string
	for _, d := range out {
		if fi, err := os.Stat(d); err == nil && !fi.IsDir() {
			existing = append(existing, d)
		}
	}
	return existing
}

// IndexedLayout resolves a saved layout through Zellij's session index.
//
// Zellij writes session-metadata.kdl only on a clean shutdown, so a space
// whose server was killed (or that was saved mid-run) can still be
// resurrectable while lacking metadata. The index always has the
// name-to-directory mapping.
func IndexedLayout(sessionName string) string {
	for _, database := range SessionIndexDatabases() {
		id, ok := spacesqlite.LookupSession(database, sessionName)
		if !ok || id == "" {
			continue
		}
		for _, sessionInfo := range SessionInfoDirectories() {
			layout := filepath.Join(sessionInfo, id, "session-layout.kdl")
			if fi, err := os.Stat(layout); err == nil && fi.Mode().IsRegular() {
				return layout
			}
		}
	}
	return ""
}

var metadataNameRe = regexp.MustCompile(`(?m)^\s*name\s+"((?:[^"\\]|\\.)*)"`)

// FindSavedLayout locates the newest saved layout for a session, preferring
// the session index with a metadata scan fallback.
func FindSavedLayout(sessionName string) string {
	if indexed := IndexedLayout(sessionName); indexed != "" {
		return indexed
	}
	var candidates []string
	for _, sessionInfo := range SessionInfoDirectories() {
		entries, err := os.ReadDir(sessionInfo)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(sessionInfo, e.Name())
			metadata := filepath.Join(dir, "session-metadata.kdl")
			layout := filepath.Join(dir, "session-layout.kdl")
			if fi, err := os.Stat(metadata); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if fi, err := os.Stat(layout); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			text, err := os.ReadFile(metadata)
			if err != nil {
				continue
			}
			m := metadataNameRe.FindStringSubmatch(string(text))
			if m == nil {
				continue
			}
			if KdlUnescape(m[1]) == sessionName {
				candidates = append(candidates, layout)
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	best := candidates[0]
	bestTime := fileMtime(best)
	for _, c := range candidates[1:] {
		if t := fileMtime(c); t > bestTime {
			best, bestTime = c, t
		}
	}
	return best
}

// PrepareRestore strips start_suspended from a saved session layout and
// routes agents through the resume wrapper, atomically.
func PrepareRestore(sessionName string) int {
	layout := FindSavedLayout(sessionName)
	if layout == "" {
		return 0
	}
	text, err := os.ReadFile(layout)
	if err != nil {
		errf("zj: could not read %s: %v\n", layout, err)
		return 1
	}
	updated := RewriteLayout(string(text))
	if updated != string(text) {
		dir, base := filepath.Split(layout)
		temporary := filepath.Join(dir, "."+base+".zj-restore")
		if err := os.WriteFile(temporary, []byte(updated), 0o666); err != nil {
			errf("zj: could not update %s: %v\n", layout, err)
			return 1
		}
		if err := os.Rename(temporary, layout); err != nil {
			errf("zj: could not update %s: %v\n", layout, err)
			return 1
		}
	}
	return 0
}
