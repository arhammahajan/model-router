//go:build linux

package model

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procIsLive reports whether /proc appears mounted and rootPID is an existing
// process in /proc. When tests pass synthetic PIDs (like 100) that don't exist,
// procIsLive returns false so tests don't falsely conclude a synthetic PID has
// no children unless they stub DetectDescendantAgentFn.
func procIsLive(rootPID int) bool {
	if rootPID <= 1 {
		return false
	}
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(rootPID)))
	return err == nil
}

// detectDescendantAgent walks direct and descendant child processes of rootPID
// via /proc/<pid>/task/*/children (up to 4 levels deep) and returns the first
// known coding-agent name found in a child's comm or cmdline argv.
func detectDescendantAgent(rootPID int) string {
	if rootPID <= 1 {
		return ""
	}
	visited := map[int]bool{rootPID: true}
	frontier := []int{rootPID}
	for depth := 0; depth < 4 && len(frontier) > 0; depth++ {
		var next []int
		for _, pid := range frontier {
			children := procChildren(pid)
			for _, child := range children {
				if visited[child] {
					continue
				}
				visited[child] = true
				if agent := agentFromProc(child); agent != "" {
					return agent
				}
				next = append(next, child)
			}
		}
		frontier = next
	}
	return ""
}

func procChildren(pid int) []int {
	pattern := filepath.Join("/proc", strconv.Itoa(pid), "task", "*", "children")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil
	}
	var out []int
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(raw)) {
			if child, err := strconv.Atoi(field); err == nil && child > 1 {
				out = append(out, child)
			}
		}
	}
	return out
}

func agentFromProc(pid int) string {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	if comm, err := os.ReadFile(filepath.Join(base, "comm")); err == nil {
		if n := NormalizeAgentCommand(strings.TrimSpace(string(comm))); knownAgents[n] {
			return n
		}
	}
	raw, err := os.ReadFile(filepath.Join(base, "cmdline"))
	if err != nil || len(raw) == 0 {
		return ""
	}
	parts := bytes.Split(raw, []byte{0})
	// Check argv[0], then script/entrypoint arguments (e.g. `node /path/to/claude`,
	// `harness muse`, `python -m kimi_cli`). Skip flags starting with '-'.
	checked := 0
	for _, part := range parts {
		arg := strings.TrimSpace(string(part))
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		if n := NormalizeAgentCommand(arg); knownAgents[n] {
			return n
		}
		// Also check parent directory segments for versioned binaries like
		// `~/.local/share/claude/versions/2.1.76`.
		if strings.Contains(arg, "/") {
			for _, seg := range strings.Split(arg, "/") {
				if seg == "" {
					continue
				}
				if n := NormalizeAgentCommand(seg); knownAgents[n] {
					return n
				}
			}
		}
		checked++
		if checked >= 4 {
			break
		}
	}
	return ""
}
