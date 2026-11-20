package tui

import (
	"context"
	"time"

	"github.com/arhammahajan/model-router/plugins/tmux-spaces/internal/model"
)

// Backend abstracts all I/O the TUI needs so it compiles and tests
// standalone without importing tmux/screen/cli packages.
type Backend interface {
	LoadSpaces(ctx context.Context) ([]model.Space, error)
	LoadHookDetail(ctx context.Context, session string, paneNum int) (*model.HookDetail, error)
	LoadHarnesses(ctx context.Context) ([]model.HarnessEntry, error)
	OpenSpace(ctx context.Context, name string) error
	FocusPane(ctx context.Context, session, windowID, paneID string) error
	CreateSpace(ctx context.Context, name, cwd string) error
	RenameSpace(ctx context.Context, oldName, newName string) error
	KillSpace(ctx context.Context, name string) error
	LaunchAgent(ctx context.Context, session, cwd, agent, profile string) error
}

// ResurrectBackend is an optional extension of Backend for listing, restoring,
// and deleting persisted exited space snapshots.
type ResurrectBackend interface {
	LoadExited(ctx context.Context) ([]model.ExitedSpace, error)
	RestoreSpace(ctx context.Context, name string) error
	DeleteExited(ctx context.Context, name string) error
}

// PreviewBackend is an optional extension of Backend for capturing compact
// live pane viewports for the Attention Inbox.
type PreviewBackend interface {
	CapturePreviews(ctx context.Context, paneIDs []string, lines int) map[string]string
}

// WindowRenameBackend is an optional extension of Backend for renaming a
// tmux window (tab) within a space.
type WindowRenameBackend interface {
	RenameWindow(ctx context.Context, windowID, newName string) error
}

// GitBackend is an optional extension of Backend for asynchronously collecting
// git branch/diff stats and GitHub PR/CI status for pane and space CWDs.
type GitBackend interface {
	LoadGitStatus(ctx context.Context, cwds []string, force bool) map[string]*model.GitInfo
	LoadPRStatus(ctx context.Context, gitByCWD map[string]*model.GitInfo, force bool) map[string]*model.GitInfo
}

// OutcomesBackend is an optional extension of Backend for loading recent
// completion/failure outcomes across spaces and opening an outcome's agent
// conversation in a new tab.
type OutcomesBackend interface {
	LoadOutcomes(ctx context.Context) ([]model.OutcomeEntry, error)
	OpenOutcome(ctx context.Context, outcome model.OutcomeEntry) error
}

// ConversationsBackend is an optional extension of Backend for listing,
// searching, and reopening unified coding-agent conversations across harnesses.
type ConversationsBackend interface {
	LoadConversations(ctx context.Context, refresh, includeArchived bool) (model.ConversationSnapshot, error)
	SearchConversations(ctx context.Context, req model.ConversationSearchRequest) ([]model.ConversationSearchHit, error)
	OpenConversation(ctx context.Context, conv model.ConversationEntry, forceNewTab bool) error
}

// Mode selects which screen the TUI shows.
type Mode int

const (
	ModeNavigate Mode = iota
	ModeSearch
	ModeHelp
	ModeNewSpace
	ModeRename
	ModeConfirmKill
	ModeHookDetail
	ModeAgents
	ModeInbox
	ModeConversations
)

// Options configures a TUI run.
type Options struct {
	InitialMode      Mode
	InitialQuery     string
	OpenNew          bool
	OpenRenameSpace  bool
	OpenRenameWindow bool
	Target           string
	HomeDir          string
	RefreshInterval  time.Duration
	Now              func() time.Time
}

// NowOrDefault returns the current time using Options.Now or time.Now.
func (o Options) NowOrDefault() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// RefreshIntervalOrDefault returns the polling interval, defaulting to 1.5s.
func (o Options) RefreshIntervalOrDefault() time.Duration {
	if o.RefreshInterval <= 0 {
		return 1500 * time.Millisecond
	}
	return o.RefreshInterval
}
