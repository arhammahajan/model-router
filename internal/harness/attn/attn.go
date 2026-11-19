// Package attn reports launcher attention state over OSC 7501, so
// every agent pane appears in the Zellij Inbox and the Spaces
// navigator with real state — no `zj __shim` layout wrapper and no
// per-agent hook config required.
//
// The emitter matches the zj helper byte for byte: records go to
// /dev/tty when writable, else stdout. Prime and Finish mirror the
// shim's launch/exit semantics, and Wrap runs the agent as a child
// process that owns the tty. Enabled gates the whole feature on the
// Zellij environment, a writable record target and HARNESS_ATTN.
package attn

import "os"

// Enabled reports whether attention state should be reported from
// this pane: inside a Zellij session (ZELLIJ_SESSION_NAME non-empty)
// or inside a tmux pane (TMUX and TMUX_PANE non-empty), with a writable
// record target (see TtyWriter), and not opted out through HARNESS_ATTN —
// "0"/"false" disable, a present-but-empty value means enabled. T3
// invocations (HARNESS_AGENT set) still report: their panes are real.
func Enabled() bool {
	inZellij := os.Getenv("ZELLIJ_SESSION_NAME") != ""
	inTmux := os.Getenv("TMUX") != "" && os.Getenv("TMUX_PANE") != ""
	if !inZellij && !inTmux {
		return false
	}
	switch os.Getenv("HARNESS_ATTN") {
	case "0", "false":
		return false
	}
	return ttyWritable()
}
