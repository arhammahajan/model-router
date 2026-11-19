package attn

import "fmt"

// Prime registers the pane in the Inbox without showing a fake turn:
// a short working record followed by idle, the shim's launch
// semantics.
func Prime(app string) {
	Emit(app, "working", "", "")
	Emit(app, "idle", "", "")
}

// Finish reports the terminal state for an exit code: 0 → done with
// message "finished"; any other code → error with
// "exited with status N". The message travels base64 in the msg
// field.
func Finish(app string, exitCode int) {
	if exitCode == 0 {
		Emit(app, "done", "finished", "")
		return
	}
	Emit(app, "error", fmt.Sprintf("exited with status %d", exitCode), "")
}
