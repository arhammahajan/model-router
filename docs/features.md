# Feature tracker

Tracks shipped and planned work for tmux-spaces, orchestration, the harness CLI, and the
router. Check the code for current behavior.

| Status | Meaning |
| --- | --- |
| Shipped | Available in the current code |
| Partial | Some of the work is available |
| Proposed | Planned work with a design below |
| Deferred | Set aside for later |

Herdr informed the socket API, status resolution, and managed agent starts. T3 Code
informed ACP, MCP, delegation, event replay, and composer workflows (follow-up queues,
steering, attachments, capability negotiation, and offline drafts). Orca informed skills,
mailboxes, task dependencies, worktree tasks, and the hybrid mobile chat-over-CLI
architecture (native transcript tailing, ordered input delivery, and terminal WebView
fallback). Orca is the primary reference for scheduled CLI
agent runs. T3 Code is the secondary reference for task controls, schedule edits,
and run recovery.
Herdr's CLI informed agent launch and prompt delivery. Its scheduling plugins
informed scheduler diagnostics and visible runs.
The pane-watch design draws on zellij-agent-tools.
workmux informed the git/PR/CI badges and automatic worktree lifecycle, tracking,
monitoring, and management. TUIOS informed Inbox triage, pane status, prompt delivery,
event replay, worktree review, and config provenance. The zellij-pane-tracker link
records a project reviewed for comparison.
[Herdr](https://herdr.dev) · [T3 Code](https://github.com/pingdotgg/t3code) ·
[Orca](https://orca.dev) · [workmux](https://github.com/raine/workmux) ·
[zellij-agent-tools](https://github.com/thisisryanswift/zellij-agent-tools) ·
[zellij-pane-tracker](https://github.com/theslyprofessor/zellij-pane-tracker) ·
[TUIOS](https://github.com/Gaurav-Gosain/tuios)

## Where things live

| Area | Path or section |
| --- | --- |
| Spaces architecture | `plugins/tmux-spaces` and `internal/harness/{spacecore,spacelaunch,spaceresume}` |
| Feature plans | §1 through §11 |
| Router and engine behavior | Code and `conformance/`, which replace the retired M4 through M6 contracts |
| Hook store, listener, and spool | `internal/harness/spacecore` |
| tmux pane identity and `@spaces_*` options | `internal/harness/spaceresume` |
| Agent launch resolution | `internal/harness/spacelaunch` |
| Navigator and its CLI | `plugins/tmux-spaces` |
| Proposed orchestration daemon and drivers | `internal/harness/orch*`, §3.11 |
| Proposed skills code | `internal/harness/skills`, §3.8 |
| Proposed Docker sandboxes | `internal/harness/sandbox`, §7 |
| Proposed automation runner and scheduler adapters | `internal/harness/automations`, §9 |
| Proposed remote mobile gateway & structured transcript tailing | `internal/harness/{mobilegw,convstore}`, §10 |
| Proposed terminal companion contract and adapters | `plugins/{kitty,wezterm,iterm2}-spaces`, `plugins/tmux-spaces/internal/osc`, `internal/harness/attn`, §11 |

## Tracker

| ID | Area | Item | Status | Section |
| --- | --- | --- | --- | --- |
| SPC-1 | spaces | Navigator popup, agent picker, status-bar badge | Shipped | §0 |
| SPC-2 | spaces | `next`, `prompt`, `read`, `wait`, `agent`, `doctor`, `inbox`, and `resurrect` | Shipped | §0 |
| SPC-3 | attention | Hook store, `@spaces_*` pane options, OSC watcher, screen fallback | Shipped | §0 |
| SPC-4 | attention | Attention Inbox popup | Shipped | §0 |
| SPC-5 | launch | Profiled launches through the `harness` CLI | Shipped | `internal/harness/{agents,profiles,routerctl}` |
| CONV-1 | navigation | Unified Conversation Store (SQLite WAL + FTS5 full-message search, modular per-agent adapters, `prefix + y` popup) | Shipped | `internal/harness/convstore`, `plugins/tmux-spaces` |
| ATT-1 | attention | One owner for resolved pane status, with Inbox acknowledgment and snooze | Partial | §1.1 |
| ATT-2 | attention | Named agents that survive pane moves and layout restores | Proposed | §1.2 |
| ATT-3 | attention | Prompt queue, steering, and stall detection | Proposed | §1.2 |
| ATT-4 | attention | Exact resume for profiled agents across all launch paths | Partial | §1.3 |
| ATT-5 | attention | Accurate completion with background work | Shipped | §1.4 |
| ATT-6 | attention | One-line attention strip with exact-pane jumps | Partial | §1.5 |
| ATT-7 | attention | Recent outcomes across spaces | Shipped | §1.6 |
| ATT-8 | navigation | Searchable conversation titles, Git branches, diff stats, and PR/CI badges | Partial | §1.7 |
| ATT-9 | attention | Inline subagent and background job status | Shipped | §1.8 |
| ATT-10 | diagnostics | Evidence behind a pane's status | Proposed | §1.9 |
| ATT-11 | navigation | Numbered pane shortcuts and notification jumps | Proposed | §1.10 |
| ATT-12 | attention | Answer the current approval from the Inbox | Proposed | §1.11 |
| NAV-1 | navigation | Search across panes, conversations, and worktrees | Proposed | §1.12 |
| DET-1 | attention | Explainable harness screen rules | Proposed | §1.13 |
| TSK-1 | tasks | Worktree-backed tasks (New task flow) | Proposed | §2.1 |
| TSK-2 | tasks | Usage and quota by agent, profile, task | Proposed | §2.2 |
| TSK-3 | review | Open a task diff from the Inbox, then add review feedback | Proposed | §2.3 |
| HOF-1 | handoffs | Portable handoffs and checkpoints | Deferred | §2.4 |
| SSH-1 | remote | Agent navigator across SSH hosts | Deferred | §2.5 |
| WT-1 | worktrees | Automatic worktree handling, tracking, monitoring, and management | Proposed | §2.6 |
| API-1 | events | Local event API shared by inbox, scripts, notifications, and waits | Proposed | §4 |
| API-2 | CLI | Common pane targets and resolved JSON status | Partial | §4.1 |
| ORC-1 | orchestration | Runs, tasks, mailboxes, dispatch instructions, and depth limits | Proposed | §3.2 |
| ORC-2 | orchestration | Terminal driver on tmux | Proposed | §3.3 |
| ORC-3 | orchestration | ACP driver | Proposed | §3.3 |
| ORC-4 | orchestration | `orchd` API, event log, tokens, and waits | Proposed | §3.4 |
| ORC-5 | orchestration | `spaces task`, `watch`, and `skills` | Proposed | §3.5 |
| ORC-6 | orchestration | Pane watch grants (consent) | Proposed | §3.6 |
| ORC-7 | orchestration | TUI task board | Proposed | §3.7 |
| ORC-8 | orchestration | MCP toolkits and skills | Proposed | §3.8 |
| ORC-9 | orchestration | Recovery and task reconciliation | Proposed | §3.9 |
| OPS-1 | diagnostics | End-to-end status checks and an isolated demo | Partial | §6 |
| SBX-1 | sandbox | Ephemeral and persistent Docker sandbox lifecycle | Proposed | §7.2 |
| SBX-2 | sandbox | Host binary mounting and bundle delivery for agent CLIs | Proposed | §7.3 |
| SBX-3 | sandbox | Profile credential projection and Unix socket router bridge | Proposed | §7.4 |
| SBX-4 | sandbox | Base image builder, doctor checks, and attention supervision | Proposed | §7.5 |
| RTR-1 | router | Explain model routing and effective capabilities | Proposed | §8.1 |
| MUX-1 | integration | Router-backed profiles with native status in TUIOS | Proposed | §8.2 |
| AUTO-1 | automations | Popup for system-level cron and heartbeat schedules | Proposed | §9.1 |
| AUTO-2 | automations | Profiled CLI agent runs, history, and Inbox outcomes | Proposed | §9.2 |
| AUTO-3 | automations | Worktree runs, session reuse, and additional triggers | Deferred | §9.3 |
| MOB-1 | mobile | Structured transcript tailing and live pane-to-transcript binding in `convstore` | Proposed | §10.2 |
| MOB-2 | mobile | State-gated tmux input floor, follow-up queue, attachments, and approval cards | Proposed | §10.3 |
| MOB-3 | mobile | Remote WebSocket gateway, capability negotiation, and live terminal escape hatch | Proposed | §10.4 |
| MOB-4 | mobile | Mobile-first PWA prototype and shared client runtime over Tailscale | Proposed | §10.5 |
| MOB-5 | mobile | Native mobile app (Expo RN), Attention Inbox push alerts, and optional E2EE relay | Deferred | §10.6 |
| TERM-1 | terminal | Shared terminal contract with extended pipe-pane OSC parsing, bare-terminal user vars, and presence reporting | Proposed | §11.2 |
| TERM-2 | terminal | Kitty starter plugin with OS-focus watcher, clickable OSC 99 pane jumps, panel bar, and profile cues | Proposed | §11.3 |
| TERM-3 | terminal | Kitty overlay palettes, clipboard image paste, agent hints, and inline graphics previews | Proposed | §11.4 |
| TERM-4 | terminal | WezTerm Lua plugin with focus watcher, InputSelector palettes, tab formatting, and toast alerts | Proposed | §11.5 |
| TERM-5 | terminal | iTerm2 Python plugin and escape-sequence baseline for Ghostty and Foot | Deferred | §11.6 |

## 0. Shipped baseline

The Spaces port from Zellij to tmux is complete. §3.10 maps the old design to the
current implementation.

- The Bubble Tea navigator runs in a tmux popup. It lists spaces, windows, and agents,
  with live attention, search, and pane jumps. tmux-spaces also provides an agent
  picker, a status-bar badge, and the commands listed under SPC-2.
- `spacecore` stores hook records from supported harnesses. The hook client writes
  per-pane `@spaces_*` options. The fallbacks are OSC 7501 via `pipe-pane` and
  `__osc-watch` , and a screen detector derived from Herdr.
- Resurrect restores layouts and can resume native conversations.
- The `harness` CLI provides profiles, model selection, usage reports, agent launches
  with attention tracking, and `doctor` .
- The router accepts and emits multiple API dialects. It supports versioned tool
  boundaries, optional Responses-to-Chat search replacement for streaming and
  non-streaming requests, keyless bearer compatibility, and metadata-only usage
  accounting. The conformance suite and `internal/server` regression tests cover this
  behavior.

## 1. Attention & state

### 1.1 Attention inbox and consistent pane state (ATT-1)

**Partial.** The Inbox, badge, `next`, and `wait` share a resolver. Each invocation can
read tmux options, hooks, and the screen. The resolver can also write inferred state
back to tmux and record outcomes.

Make the existing hook service the single writer of resolved pane status. Feed it
observations from hooks, OSC 7501, and bounded screen reads. Let the popup and CLI read
one snapshot, and use `@spaces_*` options as a projection for tmux formats. Keep the
finite-work rules from ATT-5 and the expandable child rows from ATT-9.

Give each blocker or completion a stable Inbox item ID. Keep approvals, questions,
failures, and finished turns in a persistent Inbox. Put blockers first and unreviewed
completions next. Show what the agent needs and how long it has waited. Enter opens
the exact pane when it is live. Closed outcomes use the resume path in §1.3.

Store seen, unread, and snoozed separately from the agent's state. Wake a snoozed item
at its deadline or when the blocker changes. Close it if the underlying condition
clears while it is snoozed. Let users opt into alerts for new blockers or completions.
[shared resolver](../plugins/tmux-spaces/cmd/tmux-spaces/main.go#L429) ·
[wait](../plugins/tmux-spaces/internal/cli/wait.go) ·
[TUIOS Inbox](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/AGENT_STATE.md#the-inbox) ·
[TUIOS snoozing](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/AGENT_STATE.md#snoozing-undo-and-unread) ·
[Orca inbox](https://orca.dev/docs/notifications) ·
[Herdr agent states](https://herdr.dev/tui/docs/concepts/)

### 1.2 Named agents and reliable prompt delivery (ATT-2, ATT-3)

**Proposed.**

Give each agent a name that survives pane moves and layout restores. Keep immediate
`prompt` and add `prompt --when-idle` with an editable, ordered queue. Bind each entry
to an agent instance so a replacement process in the same pane cannot receive it.
Wait for a stable idle state, give an unsent human draft priority, and send at most
one queued prompt per idle period. Offer **Steer now** when the adapter supports it.

After pressing Enter, look for evidence that the agent took the prompt. If receipt
is unclear, mark the entry stalled in the Inbox and hold the remaining queue for
manual resolution. Use the same readiness and delivery checks in the terminal
driver (§3.3).
[prompt](../plugins/tmux-spaces/internal/cli/prompt.go) ·
[TUIOS queued messages](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/AGENT_STATE.md#queued-messages) ·
[Herdr agent automation](https://herdr.dev/tui/docs/agent-automation/) ·
[T3 queue and steering](https://github.com/pingdotgg/t3code/blob/main/docs/user/composer.md)

### 1.3 Exact resume for profiled agents (ATT-4)

**Partial.** The restore snapshot saves the profile and native conversation ID.
`BuildPaneCommand` already passes the saved ID through the profile launch wrapper
when the ID is present.

Use one typed resume target with the agent, profile, native conversation ID, and
working directory. Send it through the same launch path from Resurrect,
Conversations, and closed Inbox outcomes. Verify that the original profile and
exact conversation reopen together across agents that support native resume.
Offer saved conversations in the Inbox after restart and put resume failures there.
If the exact ID is unavailable, offer the conversation picker as a separate action.
[snapshot fields](../plugins/tmux-spaces/internal/resurrect/store.go) ·
[restore command builder](../plugins/tmux-spaces/internal/resurrect/restore.go) ·
[TUIOS resume](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/AGENT_STATE.md#resuming-after-a-restart) ·
[Herdr session restore](https://herdr.dev/tui/docs/session-state/)

### 1.4 Accurate completion with background work (ATT-5)

**Shipped.**

`spacecore` classifies background work from `Stop`/`SubagentStop` payloads (`background_tasks`,
`session_crons`), `SubagentStart`/`SubagentStop`, `TaskCreated`/`TaskCompleted`, and background tool
calls (`Bash` with `run_in_background`, `Monitor`, `CronCreate`/`ScheduleWakeup`,
`TaskStop`/`KillShell`) into **finite** work (subagents, tests, builds, scripts) versus long-running
**services**, **watchers**, and **scheduled wakeups**. When `Stop` (or an `idle_prompt` notification)
arrives while finite work is still running, `spacecore.ResolvePaneEvent` and `MergeEvent` keep the
pane `working` and report what it is waiting for (`waiting_on`), suppressing premature `done`
outcomes. When only services, watchers, or scheduled wakeups remain—or when a later `Stop` reports
no finite work—the turn finishes as `done` while preserving the long-running background details.
`spaceresume.HookClient` resolves the event through `ResolvePaneEvent` before emitting OSC 7501 and
`@spaces_*` tmux options so the hook store and tmux status always make the same decision.
[background classification & resolution](../internal/harness/spacecore/background.go) ·
[hook event merge](../internal/harness/spacecore/events.go) ·
[hook client](../internal/harness/spaceresume/commands.go) ·
[Falcode's `Stop` hook](https://github.com/victor-falcon/falcode-zellij/blob/972df9c2b7c3741c9cea88dbae50b4547b344ca2/claude-extension/falcode-hook.sh#L145-L166)
·
[zj-radar's background task rules](https://github.com/marktoda/zj-radar/blob/65b52cc165c4df93c4b2f497facac257088e6d44/crates/cli/src/agents/claude/background.rs#L49-L96)

### 1.5 One-line attention strip (ATT-6)

**Partial.** tmux-spaces already has a status-bar badge.

Add an optional line that shows the highest-priority blocked agent, why it is blocked,
its wait time, and the number of working agents. Clicking the blocker opens its pane.
Refresh from ATT-1's resolved state so the status bar can update without scanning pane
contents.
[zj-radar's live view](https://github.com/marktoda/zj-radar/blob/65b52cc165c4df93c4b2f497facac257088e6d44/docs/using.md)
·
[Zellaude's tab bar](https://github.com/ishefi/zellaude/blob/1d1d56c6d29155d59ed9bd1386f970168f781bb8/README.md)

### 1.6 Recent outcomes (ATT-7)

**Shipped.**

`spacecore` records a bounded, 24-hour history of completions (`done`) and failures
(`failed`) across spaces in `hooks.store.json` (from hook transitions, supervised
`harness`/`__shim` exits, OSC 7501 terminal states, and screen-reader turn completions,
deduplicated per conversation with workspace, agent, profile, outcome, title/summary,
conversation ID, CWD, and timestamp) and exposes them through `__space-hooks`. Entries
stay in the Attention Inbox's existing `DONE` and `FAILED` sections after a pane closes.
Clicking or pressing `Enter` on a closed entry reopens that exact agent conversation
under its original `harness` profile in a new tmux tab (dropping to an interactive shell
when closed so the tab stays open), moves it to `IDLE` while open (following the normal
`WORKING` → `DONE`/`FAILED` cycle), and returns it to `DONE`—or `FAILED` if it had
previously failed without a subsequent completion—when closed again without duplicating
entries.
[hook store outcomes](../internal/harness/spacecore/store.go) ·
[Inbox recent outcomes](../plugins/tmux-spaces/internal/tui/inbox.go) ·
[zj-radar's completion history](https://github.com/marktoda/zj-radar/blob/65b52cc165c4df93c4b2f497facac257088e6d44/docs/using.md)

### 1.7 Conversation titles and Git branches (ATT-8)

**Partial.** Conversation titles, Git branches, diff stats, and PR/CI badges are shown
and searchable in both the Spaces and Inbox popups (`internal/gitinfo`). Manual pane
names and stable agent identity from ATT-2 are pending.

Keep manual pane names and the stable agent identity from ATT-2. Share the branch and
worktree metadata with worktree tasks (§2.1) and automatic worktree management (§2.6).
[Falcode's pane labels](https://github.com/victor-falcon/falcode-zellij/blob/972df9c2b7c3741c9cea88dbae50b4547b344ca2/src/main.rs#L1236)
·
[zaag's branch lookup](https://github.com/IsmailHatim/zaag/blob/c37ee056d1c7dc91c0e306b5122938db29f72b73/bin/zaag#L366)
·
[workmux git/PR status](https://github.com/raine/workmux)

### 1.8 Inline background work (ATT-9)

**Shipped.**

`spacecore`'s `__space-hooks` summary and detail views expose structured `children` and
`background_tasks` records (with ATT-5 categories, states, outcomes, schedules, and start/end
timestamps). In both the Spaces navigator and Attention Inbox popups, agent rows with background
work display an expandable badge distinguishing subagents/finite jobs from services (`▸ +N subs · +N bg · N svc`).
Pressing `e` or `Space` (plus `→`/`l` and `←`/`h` in the Inbox) expands or collapses inline tree
rows beneath the agent showing each subagent, background command, service, watcher, and scheduled
wakeup with live or frozen elapsed time and outcome (`running`, `service`, `watcher`, `scheduled`,
`done`, `failed`, `stopped`). Pressing `H` opens the hook detail overlay grouped into `waiting on`,
`subagents & finite jobs`, and `services, watchers & schedules`, and popup search matches
background task descriptions and commands.
[inline background rendering](../plugins/tmux-spaces/internal/tui/background.go) ·
[hook summary & detail views](../internal/harness/spacecore/views.go) ·
[zj-radar's background task rows](https://github.com/marktoda/zj-radar/blob/65b52cc165c4df93c4b2f497facac257088e6d44/docs/using.md)

### 1.9 Status evidence in the hook detail overlay (ATT-10)

**Proposed.**

Show the winning source, its age, and why it won in the `H` detail overlay and
`tmux-spaces panes --explain`. Identify the hook, OSC 7501 report, or matched screen
rule. Flag missing or stale evidence.

Prefer direct reports. Allow a screen rule that sees a blocker to override a stale
`working` report after fresh pane output. Reevaluate the claim when the blocker
disappears. Keep the source visible while the override holds.
[TUIOS source precedence](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/AGENT_STATE.md#sources-and-precedence) ·
[TUIOS visible blocker override](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/AGENT_STATE.md#the-one-exception-a-visible-blocker) ·
[Agent Beacon's diagnostics](https://github.com/anton-fuji/zellij-agent-beacon/blob/e6b2d719fb777df5084731626ac88b2757614dd2/src/main.rs#L285)

### 1.10 Pane shortcuts and notification jumps (ATT-11)

**Proposed.**

Let `1` through `9` open numbered panes in the agent-only view. Include the conversation
title and blocker type in desktop alerts. If the notification backend supports clicks,
open the originating pane, including when it is in another session.
[Falcode's navigation and notifications](https://github.com/victor-falcon/falcode-zellij/blob/972df9c2b7c3741c9cea88dbae50b4547b344ca2/README.md)
·
[Zellaude's pane focus](https://github.com/ishefi/zellaude/blob/1d1d56c6d29155d59ed9bd1386f970168f781bb8/src/render.rs#L396)

### 1.11 Answer the current approval from the Inbox (ATT-12)

**Proposed.**

Show the exact approval and the actions the adapter recognizes. When the user picks
an action, require the prompt ID from the preview. Recheck the agent instance,
prompt ID, and available action under a per-pane lock before sending keys. If the
prompt changed or another client answered it, refresh the preview and send no keys.
Open the pane when the adapter cannot identify a supported prompt.
[TUIOS prompt recheck](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/AGENT_STATE.md#what-the-daemon-checks-before-it-presses-anything)

### 1.12 Search across panes, conversations, and worktrees (NAV-1)

**Proposed.**

Extend the existing navigator search with results for live panes, saved
conversations, and tracked worktrees. Show what Enter will do for the selected
result and display the shortcut. Enter focuses a live pane, opens the resume action
for a saved conversation, or opens the worktree's tracked tmux window.
[TUIOS navigation](https://github.com/Gaurav-Gosain/tuios#session-management) ·
[Spaces search](../plugins/tmux-spaces/internal/model/search.go) ·
[Conversation search](../internal/harness/convstore/search.go)

### 1.13 Explainable harness screen rules (DET-1)

**Proposed.**

Keep per-agent screen rules in manifests and expose the matched rule through ATT-10.
Compare `tuios.dev/harness` with the current detector using one agent's fixtures.
Check working, idle, blocked, and prompt-box states before deciding whether to use
the library or adapt its manifest and explanation design. The reviewed library
requires Go 1.25. The Spaces module declares Go 1.24.
[TUIOS harness library](https://github.com/Gaurav-Gosain/tuios/tree/main/harness) ·
[Spaces screen detector](../plugins/tmux-spaces/internal/screen/screen.go) ·
[Spaces Go module](../plugins/tmux-spaces/go.mod)

## 2. Workspaces & tasks

### 2.1 Worktree-backed tasks (TSK-1)

**Proposed.**

In **New task**, pick a repository, branch, base ref, and harness profile. Create
the Git worktree in the background and open a tracked tmux window while the current
pane keeps focus. Show setup progress, branch, uncommitted changes, and the linked
issue or pull request. If configured setup fails, keep the output available, show
a retry action in the Inbox, and wait to launch the agent until setup succeeds.

Keep the base ref for diffs and add **Review diff** to the Inbox row (§2.3).
Add a user-configured verification command when tasks need it. Track the end of an
agent turn separately from the verification result. Deliver this workflow before
the task board and orchestration daemon. Use the same launch flow for orchestration
tasks that request isolation (§3). See §2.6 for repository cleanup hooks and
branch retention.
[TUIOS worktree setup](https://github.com/Gaurav-Gosain/tuios/blob/main/skills/tuios/fleet.md#setup-before-the-agent) ·
[TUIOS task verification](https://github.com/Gaurav-Gosain/tuios/blob/main/skills/tuios/tasks.md#when-you-are-done-verify-and-review) ·
[Orca worktrees](https://orca.dev/docs/model/worktrees) ·
[T3 thread workflows](https://github.com/pingdotgg/t3code/blob/main/docs/user/thread-sidebar.md)

### 2.2 Usage and quota by agent (TSK-2)

**Proposed.**

Show the model, token totals, estimated API spend, and subscription quota reset times in
the agent detail view. Group usage by profile or task. The router already records model,
provider, token counts, response status, and duration. Use a correlation ID to connect
router requests to the agent, profile, and task. Show native quota data when the
provider reports it and mark it unavailable otherwise.
[router accounting](../internal/accounting/accounting.go) ·
[T3 usage and limits](https://github.com/pingdotgg/t3code/blob/main/docs/user/usage.md)
· [Orca usage tracking](https://orca.dev/docs/agents/usage-tracking)

### 2.3 Diff review and feedback (TSK-3)

**Proposed.**

Open the task's diff against its saved base ref from the Inbox. Start with a
read-only popup or a jump to `lazygit` in the worktree.

Defer review comments and feedback delivery. When added, keep file and line
references on comments and send the feedback to the agent in one batch. Use the
same flow for selected pull request comments and failed CI checks.
[TUIOS diff review](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/AGENT_STATE.md#reviewing-in-the-client) ·
[Orca diff annotations](https://orca.dev/docs/review/annotate-ai-diff) ·
[T3 source control](https://github.com/pingdotgg/t3code/blob/main/docs/user/source-control.md)

### 2.4 Portable handoffs and checkpoints (HOF-1)

**Deferred.**

Continue a task in another harness with the original request, selected conversation
history, worktree, and relevant command results. Start with Git checkpoints for each
task. Add turn-level diffs, rollback, and conversation forks once an adapter can report
turn boundaries and perform those operations.
[T3 portable handoffs](https://github.com/pingdotgg/t3code/blob/main/docs/user/portable-handoffs.md)
·
[T3 checkpoint lifecycle](https://github.com/pingdotgg/t3code/blob/main/docs/orchestration-v2/feature-lifecycles.md#checkpointing)

### 2.5 Agent navigator across SSH hosts (SSH-1)

**Deferred.**

List local and remote agents in the same navigator. Group remote spaces by SSH host and
show whether each host is online, stale, or disconnected. Identify a remote agent by its
host and the tmux target from API-2. Open its exact session and pane over SSH.
[Herdr connecting machines](https://herdr.dev/tui/docs/connecting-machines/) ·
[zaag's remote machines](https://github.com/IsmailHatim/zaag/blob/c37ee056d1c7dc91c0e306b5122938db29f72b73/README.md#other-machines)

### 2.6 Automatic worktree handling, tracking, monitoring, and management (WT-1)

**Proposed.** Not scheduled for immediate implementation; builds on `internal/gitinfo`
(ATT-8) and shares creation/cleanup primitives with TSK-1 (§2.1) and orchestration
isolation (`--worktree new`, §3.14).

Automate the full Git worktree + tmux lifecycle so parallel agent work needs no manual
worktree setup, tracking, or teardown:

- **Automatic handling & workspace bootstrap:** Create a Git worktree paired with a tmux
  window/space in one step (`add <branch>`, `add --pr <num|url>` to check out a GitHub
  PR directly, and optional `--auto-name` via `model-router` to generate a branch name
  from a task prompt). Support two-level config (global + per-repo `.spaces.toml`) for
  declarative `files.copy` (`.env`, local configs), `files.symlink` (`node_modules`,
  `.turbo`, `.venv`, build caches), multi-pane layouts (`<agent>` + dev server/watchers),
  and lifecycle hooks (`post_create`, `pre_merge`, `pre_remove`).
- **Tracking:** Record per-worktree repo identity (git common dir), base branch for
  diffs/merges (with `set-base` to retarget), linked tmux window/pane, and associated
  PR/issue. Support `open` to jump to or recreate the tmux window for an existing
  worktree whose window was closed.
- **Monitoring:** Add a Worktrees view inside the existing `tmux-spaces` popup (no
  persistent sidebar) listing all worktrees across active repositories with branch,
  committed and uncommitted diff stats, ahead/behind counts, merge-conflict/rebase
  indicators, PR and CI check badges, and active agent status rollup.
- **Management & cleanup:** Provide CLI commands and popup actions to `merge`
  (configurable merge/rebase/squash strategy, runs `pre_merge` hooks, `--keep` to retain
  the worktree), `rebase` onto the base branch, and `remove` finished or abandoned
  worktrees—closing the linked tmux window, deleting the worktree, and pruning the local
  branch only when merged or confirmed.
[workmux](https://github.com/raine/workmux) ·
[workmux workflows](https://github.com/raine/workmux/tree/main/src/workflow)

## 3. Multi-agent orchestration

**Proposed.** The four implementation phases are in §3.12.

Use orchestration to assign a task with a written spec, supervise it, and record the
result. When a user works directly with an agent, they type in its pane, steer the turn
there, and answer permission prompts in the agent's own UI. The Inbox and `next` support
that workflow.

### 3.1 Goal and the human boundary

A run groups tasks. Each task names an agent and working directory, and can depend on
other tasks or name a parent. A user can create a task from the CLI or navigator. Agents
can create tasks through the terminal CLI or through MCP tools.

The ACP driver talks directly to agents that support the Agent Client Protocol. The
terminal driver uses tmux panes. With `auto` , the daemon chooses a driver for the
agent. It records state from driver events, relays questions, and wakes callers waiting
on a result. The `spaces task` CLI calls the daemon API and supports JSON output, stable
exit and error codes, and server-side waits. The skill guides explain the commands to
agents launched by `harness` .

Use an explicit task to run work in parallel, order work by dependencies, record an
assignment and result, or dispatch into a space outside the current view. An assignment
can also give the worker a parent for delegated tasks. Agents can start their own runs.
The navigator opens the dispatch dialog on request.

### 3.2 Model (ORC-1)

A run records its creator, mailbox, and tasks. Creating a task outside a run opens a
run. A task assigns one unit of work to one agent.

```jsonc
{
  "id": "t8f21a0c3d",
  "run_id": "r4d90bb17e",
  "parent": null,                 // parent task id when delegated
  "depth": 0,                     // parent-chain length, capped
  "title": "port screen fallback test",
  "spec": "TASK.md contents verbatim",
  "agent": "codex",               // canonical agents-registry id
  "model": "",                    // optional router model hint
  "cwd": "/home/u/dev/model-router",
  "deps": ["t1c07ee510"],         // DAG, cycle-checked
  "channel": "auto",              // auto | acp | terminal
  "created_by": "human:arham",    // human | task:<id> | mcp:<id> | cli
  "creation_source": "plugin",    // cli | plugin | mcp | api
  "created_ms": 1765000000000,
  "state": "running",             // queued|ready|dispatching|running|
                                  // blocked|done|failed|cancelled|orphaned
  "kind": "",                     // blocked substate: question|permission|auth
  "dispatch": {
    "channel": "terminal",
    "session": "work", "window": "@4", "pane": "%9", "pane_pid": 91823,
    "started_ms": 1765000001000
  },
  "result": {"source": "worker", "text": "…", "exit_hint": "ok"},
  "allow_subtasks": true,
  "max_depth": 3
}
```

Task transitions depend on prerequisites and events from the driver.

```
queued ──deps satisfied──▶ ready ──dispatch──▶ dispatching ──confirmed──▶ running
                                                                  │
        ┌───────────────────────── channel evidence ─────────────┤
        ▼                         ▼                              ▼
     blocked(question|permission|auth)   channel lost      done | failed
        │  answer/cancel                  ▼                     ▲
        └────────▶ running ─────────▶ orphaned ──re-dispatch─▶ ready
```

- Use the hook store's blocked kinds for tasks. The kinds are `question` , `permission`
  , and `auth` .
- If the channel disappears before a terminal report, mark the task `orphaned` .
  `spaces task redispatch <id>` returns it to `ready` .
- If pane attention provides the only completion evidence, set `result.source` to
  `"attention"` and write a short report. Use the worker's `spaces task done` report if
  it arrives first.
- Record state changes in the event log. Rebuild the stored view by replaying the log,
  as in T3 Code.

Each run and task has a mailbox. Message types are `status` , `question` , `answer` ,
`worker_done` , `escalation` , and `system` . `spaces task check --wait` blocks in the
daemon until a message arrives for the caller, following Herdr's `events.wait` . The CLI
takes its identity from the environment. It can address a run with `run:<id>` or a task
with `task:<id>` . Each mailbox keeps 512 messages by default. When it reaches the
limit, remove the oldest messages and record a `system` notice.

Prepend the worker identity and reporting commands to each dispatched spec, following
Orca's approach.

```text
# Task assignment

Your task ID is t8f21a0c3d. The run ID is r4d90bb17e. The run creator is human:arham.

Report progress.

  spaces task send --task t8f21a0c3d --type status --text "…"

Ask a question, then wait for the answer.

  spaces task ask --task t8f21a0c3d "…"
  spaces task check --wait

Submit the report.

  spaces task done --task t8f21a0c3d --report-file REPORT.md [--exit fail]

Work on this assignment. Delegate only if the spec permits it. After you submit the report, stay idle.

## Delegate work

  spaces task create --agent AGENT --spec-file S.md --parent t8f21a0c3d
  spaces task wait t<child>
```

Include the delegation section when `allow_subtasks` is true and `depth < max_depth` .
The default maximum depth is 3. Requests beyond the limit return `orch_depth_exceeded` .

### 3.3 Channels (ORC-2, ORC-3)

Both drivers implement the same Go interface in the root module.

```go
// internal/harness/orchapi
type Caps struct {
    Structured bool // protocol-level prompt/result
    Stream     bool // interim output observable
    Interrupt  bool // cancel mid-turn
    Resume     bool // reattach after restart
    AskRelay   bool // structured question relay
}

type ChannelEvent struct {
    Type string // state | output | question | done | failed | orphaned
    Kind string // question | permission | auth
    Text string
    TsMs int64
}

type Driver interface {
    Name() string
    Probe(agent string) (Caps, bool)
    Dispatch(ctx context.Context, t orchapi.Task, preamble string) (Handle, error)
    Events(h Handle) <-chan ChannelEvent
    Answer(h Handle, questionID, answer string) error
    Cancel(h Handle, reason string) error
    Release(h Handle) error
}
```

Drivers report events. `orchd` resolves task state, following Herdr's arbitration rule.

#### Terminal driver

1. Resolve the agent through `spacelaunch` , as `harness <agent>` does. Use the
   attention shim from the plugin's `agent` command. Start the process with
   `tmux new-window -d -t <space>: -c <cwd> -n <task-title>` . Pass `SPACES_TASK` ,
   `SPACES_RUN` , `SPACES_TOKEN` , and `SPACES_ORCH_SOCKET` to the worker. tmux supplies
   `TMUX` and `TMUX_PANE` for hooks, pane options, and worker CLI calls.
2. Keep the task `dispatching` until `list-panes -F` confirms the pane and the hook
   store records a session start for the expected agent. Then send the spec with
   `tmux send-keys -l -t %<pane>` followed by `send-keys Enter` , and mark the task
   `running` . Share these checks with interactive `prompt` under ATT-3.
3. Read the hook store first, then `@spaces_*` options, then the screen detector over
   `capture-pane -p -S -200` . A prompt indicates `running` . A blocker keeps its kind.
   A stop or done signal can indicate completion. Use ATT-5 when a turn is waiting on
   background work. Treat the pane as gone if `#{pane_dead}` is set or
   `pane_current_command` shows the agent has exited. Without a terminal report, mark
   the task `orphaned` .
4. On cancel, send `C-c` to the pane. If it stays open past the grace period, run
   `kill-pane` .

#### ACP driver

Resolve the agent through `agents` and `spacelaunch` . Start its process, negotiate the
protocol version with `initialize` , and call `session/new` with the orchestration MCP
bridge in `mcpServers` . Send the preamble and spec with `session/prompt` . An idle
`session/update` signals completion. Use the final assistant text as the report. Call
`session/cancel` to interrupt and `session/resume` to recover.

Read ACP registrations from `~/.config/harness/acp.json` and probe `~/.t3/tools/*-acp` .
Adding a registration needs a config entry. The daemon owns the ACP processes. They have
no panes, so the task board shows their progress.

### 3.4 Daemon (`orchd`) (ORC-4)

Start `orchd` on demand, like the hook listener. Use `startstamp` to throttle restarts
and `flock` to keep one instance. The daemon commands are
`spaces orch ensure|start|stop|status|schema` .

Keep daemon files beside the hook files in `SPACES_HOOKS_RUNTIME` . The default is
`${XDG_RUNTIME_DIR}/spaces` , with `/tmp/spaces-<uid>` as the fallback.

| File | Role |
| --- | --- |
| `orch.sock`, `orch.endpoint.json` | NDJSON requests and responses. Check tokens and peer credentials as for `h.sock`. |
| `orch.store.json` | Runs, tasks, mailboxes, and hashed tokens. Write a temporary file with mode 0600 and rename it under `orch.store.lock`. |
| `orch.log.jsonl` | Accepted commands and receipts, state events, and queued effects. |
| `orch-spool.jsonl` | Commands accepted while the daemon is down. Replay on startup. |
| `orch.server.lock`, `orch.startstamp`, `orch.log` | Instance lock, start throttle, and daemon log. |

The method table defines the NDJSON API and its stable error envelope.
`spaces orch schema --json` prints the schema.

`run.new|list|get|close`,
`task.create|get|list|dispatch|cancel|redispatch|answer|done`,
`mail.send|check|list`, `wait.task`, `events.subscribe`,
`watch.request|list|revoke`, `pane.snapshot|pane.events`,
`server.ping|schema|stop`.

General errors are `orch_not_running` , `orch_not_found` , `orch_depth_exceeded` ,
`orch_deps_unsatisfied` , `orch_bad_state` , `orch_agent_unknown` ,
`orch_channel_unavailable` , `orch_timeout` , and `orch_denied` . Pane capture errors
are `orch_watch_pending` , `orch_watch_denied` , `orch_watch_revoked` ,
`orch_pane_identity_mismatch` , `orch_capture_stale` , and `orch_bounds_exceeded` . The
CLI uses exit codes 0, 1, and 2.

A mutating request can include a `command_id` . Record its receipt in the log. A retry
returns the recorded "already applied" result.

`spaces task wait` checks task state and waits for task transitions. Task events use
the cursor and gap rules in §4. The pane methods read or forward the hook service's
resolved state. The hook service owns pane status and pane transitions.

### 3.5 CLI surface (ORC-5)

Install `spaces` in `~/.local/bin` for worker and scripting commands. Use `tmux-spaces`
for interactive views.

```
spaces orch ensure|start|stop|status|schema [--json]

spaces task create --agent A [--spec-file F | --spec-string S]
                   [--cwd D] [--model M] [--deps t1,t2] [--parent tX]
                   [--channel auto|acp|terminal] [--allow-subtasks]
                   [--max-depth N] [--dispatch]
spaces task list [--run R] [--json] | show <id> | dispatch <id>
spaces task cancel <id> [--reason S] | redispatch <id>
spaces task wait <id> [--until done|failed|blocked|terminal] [--timeout S]
spaces task check [--wait] [--timeout S] [--json]
spaces task ask --task T TEXT
spaces task answer --id <msgId> TEXT | allow | deny
spaces task send --to TASK --type status --text S
spaces task done --task T [--report-file F | --report-string S] [--exit ok|fail]

spaces watch request --pane P | list | revoke <grant-id>
spaces pane read P [--lines N] | events P --after-seq N

spaces skills list|get <topic>|install|update|installed|remove [--json]
```

`tmux-spaces` opens `tasks`, `inbox`, `agents`, and `popup`. The orchestration commands call `orch.sock`. Workers inherit `SPACES_TASK` and `SPACES_TOKEN`, so they can call the commands without passing their identity explicitly.

### 3.6 Pane watch grants (ORC-6)

Use zellij-agent-tools' consent model for tmux pane capture.

- A task can watch the pane where it runs. Watching any other pane requires an explicit,
  revocable grant.
- Show the requesting agent when a watch request appears. Approve with `y` or deny with
  `n` . Grant states are `pending` , `approved` , `denied` , `expired` , and `revoked` .
- Bind a grant to `session_id:window_id:pane_id:pane_pid` . Check that identity when
  recovering a task so a reused pane ID cannot inherit a grant.
- `pane.events --after-seq` returns bounded, coalesced changes from `capture-pane` .
  `pane.snapshot --lines N` returns the tail. Both feed `ChannelEvent` output.
- Reject reads when the grant is absent or revoked, or when the capture is stale or the
  pane identity differs. The watch API reads pane output. Dispatch handles keystrokes.
  Authenticate access to pane metadata.

### 3.7 TUI task board (ORC-7)

The Tasks view reads `orch.store.json` and follows `events.subscribe` . It uses the
navigator's Catppuccin theme.

| Key | Action |
| --- | --- |
| `T` | Show runs and their tasks, with state, dependencies, blockers, and age. |
| `D` on an agent | Open the spec editor, then create and dispatch the task. |
| `H` on a task | Show recent mailbox messages and the report. |
| `x` | Cancel the task. |
| `Enter` | Open the terminal task's pane. |
| `u` | Revoke the selected row's watch grant. |

A watch request opens an approval dialog with `y` and `n` . Agent rows show a task badge
alongside the pane's attention state.

### 3.8 MCP and skills (ORC-8)

Pass `spaces __orch-mcp-bridge` to ACP agents in `session/new` . The bridge forwards
stdio requests to `orch.sock` using the task token from the environment. Issue a token
at dispatch, store its SHA-256 hash, and revoke it when the task reaches a terminal
state. Tokens expire after 24 hours.

The MCP tools are `orch_delegate` (async with wake or blocking wait), `orch_task_status`
, `orch_task_cancel` , `orch_ask` , `orch_check` , `orch_done` , `orch_watch_request` ,
and `orch_read_pane` . Terminal workers use the corresponding CLI commands from the
preamble.

Each skill has a short `skills/<topic>/STUB.md` with `name` and `description`
frontmatter, plus a `GUIDE.md` with instructions. Both ship in the binary through
`go:embed` , so `spaces skills get <topic>` returns the guide for that binary's version.
Install stubs under `~/.agents/skills/<name>/SKILL.md` . Use the agent registry to link
them into harness directories and keep local edits on reinstall.

The first guides cover orchestration, tmux-spaces navigation and attention, and router
model selection. Keep review loops, handoffs, and coordinator patterns in the guides.

### 3.9 Recovery (ORC-9)

At startup, replay `orch-spool.jsonl` and rebuild the stored views from `orch.log.jsonl`
. Check every non-terminal task, retry queued effects without applying them twice, and
write `system` messages.

- For a terminal task, check `tmux list-panes -F` and the hook record. Reattach
  supervision if the pane still exists and its compound identity matches. Mark the task
  `orphaned` if its pane is gone.
- For an ACP task, resume a saved session ID with `session/resume` if the adapter
  supports it. Mark other tasks `orphaned` .
- `tmux-spaces resurrect` restores layouts and native conversations with new pane IDs.
  Restore orchestration supervision only when the saved `pane_pid` or start marker
  matches. Otherwise mark the task `orphaned` so it can be dispatched again.

`spaces orch status` and `harness doctor` report daemon, socket, store, and log health. They also check ACP registration and skill installation.

### 3.10 Deltas from the Zellij-era plan

| Zellij-era design | tmux equivalent |
| --- | --- |
| WASM plugin with a popup opener | Bubble Tea via `display-popup`. Shipped. |
| `SessionUpdate` and `GlobalAttentionUpdate` | `list-panes`, `list-windows -F`, and `@spaces_*` options. The hook store supplies authoritative state. |
| Server parses OSC 7501 | `spaceresume` writes `@spaces_*` pane options. `pipe-pane` and `__osc-watch` keep OSC as a fallback. |
| `zj prompt` sends to the focused pane | `send-keys -t %<pane>` targets a pane in any session. |
| `zellij action dump-screen` polling | `capture-pane -p -S -<n>` reads scrollback in attached or detached sessions. |
| Client-socket handoff between spaces | `switch-client -t <session>:<window>.<pane>` |
| KDL layout restore and `session_index.sqlite` | Native snapshots and restore in `internal/resurrect`. |
| `zj wait` polling | `tmux-spaces wait` polls the resolved snapshot. §4 proposes a local event feed for waits. |
| Plugin permissions such as `RunCommands` | Same-user Unix socket checks and task tokens. |
| `zjcli`, `zjresume`, and `zjcore` behind `zj` | `spaces` for helpers and `tmux-spaces` for the navigator and interactive CLI. Shared packages are `spacecore`, `spacelaunch`, and `spaceresume`. |

The orchestration model carries over. It retains runs, tasks, mailboxes, dependencies,
depth limits, the worker preamble, ACP, MCP, skills, the event log, and retryable
commands and effects.

### 3.11 Package layout and ownership

Keep the daemon, drivers, bridge, and skills in the root module. The plugin keeps its
task client and views behind the TUI's `Backend` interface.

```
internal/harness/orchapi/    wire types, method table, error codes, schema
internal/harness/orchcore/   state machine, store/log/spool, daemon, tokens, outbox
internal/harness/orchacp/    ACP client + registry
internal/harness/orchterm/   tmux terminal driver (Runner seam)
internal/harness/orchmcp/    MCP bridge + toolkits
internal/harness/skills/     stubs/guides (go:embed), install fan-out
plugins/tmux-spaces/internal/tasks/  TUI task client + view model
```

| Owner | Packages and responsibilities |
| --- | --- |
| E | `orchapi` and `orchcore`. State machine, store, log, spool, daemon, tokens, waits, method table, and `noop` driver. |
| F | `orchterm`, `orchacp`, and `orchmcp`. Use E's packages, `spacelaunch`, `agents`, and `spaceresume` without editing them. |
| G | `skills`, dispatch for `orch`, `task`, `watch`, and `skills` in `cmd/harness`, and smoke scripts. |
| H | Plugin task client, Tasks view, and watch approvals behind `Backend`. |

Go's module boundary keeps the plugin from importing the root module's internal
packages. Define the task response structs in the plugin as it already does for hook
summaries.

### 3.12 Phasing

1. **M1.** Build `orchapi` and `orchcore` with the v1 method table, state machine,
   store, event log, spool replay, and mailboxes. Wire `spaces orch` , `spaces task` ,
   and `check --wait` . Run every command against fake tasks using the `noop` driver.
2. **M2.** Implement the terminal driver through the tmux runner. Add detached dispatch,
   readiness checks, prompt delivery, hook-based supervision, wait signals, and
   cancellation. Add watch grants, an output cursor, and the Tasks view.
3. **M3.** Add ACP protocol negotiation, session setup, updates, cancellation, and
   resume. Load the ACP registry and pass the MCP bridge to ACP sessions.
4. **M4.** Add task tokens, the `orch_*` tools, async delegation with wake, blocking
   waits, and question relay. Ship the orchestration guide through `spaces skills` .

### 3.13 Testing bar

- Run `go test ./...` with temporary directories and environment overrides. Use fake
  tmux runners or temporary scripts so tests need no real home directory, network, mux,
  or agent.
- Table-test command and event sequences for depth limits, dependency order, cycle
  rejection, stale receipts, and deterministic replay.
- Test the Unix socket in a temporary runtime directory. Use `net.Pipe` for fake peers
  and an injectable clock for long-poll wake. Cover authorization and peer credentials.
- Test terminal dispatch with a fake tmux runner and hook store fixtures. Cover each
  blocker kind, dead panes, orphaned tasks, and mismatched pane identities.
- Use a fake ACP agent for both protocol versions, prompts, updates, question callbacks,
  and cancellation.
- Verify MCP tool calls produce the expected commands and events. Test token issue,
  revocation, and expiry with an injected clock.
- Test skill planning and installation in a fake home. Confirm that reinstalls preserve
  local edits and leave the same symlinks.
- In `scripts/spaces-orch-smoke.sh` , create a task, dispatch it to a fake agent,
  complete it, and receive the result through `check --wait` .

### 3.14 Non-goals and future work

- Run one `orchd` . The ACP agents and MCP bridge account for the extra processes needed
  per task.
- Reach registered agents through ACP or terminal drivers. Add native provider SDK
  drivers later under the same interface if needed.
- Use the JSON store and size-limited JSONL log. If volume calls for SQLite, change the
  store that reads and writes the current view.
- Add `--worktree new` after basic dispatch is ready. Use `cwd` for placement and share
  the launch flow with TSK-1 (§2.1).
- Prove the local workflow before adding remote hosts. Keep the Unix socket method table
  usable by a future relay. See SSH-1 (§2.5).

## 4. Local event API (API-1)

**Proposed.**

Expose a snapshot and subscription from the hook service after it owns resolved
pane status (ATT-1). Publish pane, attention, and prompt delivery transitions.
Give snapshots and events a boot ID and sequence number so a client can load a
snapshot, subscribe from its cursor, and receive changes made between those calls.

Keep a bounded replay buffer and bounded subscriber queues. Send a gap marker
when the cursor predates retained events, the service restarted, or the subscriber
queue overflowed. A client that receives a gap reloads the snapshot. Keep pane
output and transcript data out of the replay buffer.

Use the feed for the Inbox, notifications, scripts, and waits. `tmux-spaces wait`
currently polls every 500 ms. Check the snapshot first, then wait on matching
events until the condition holds or the timeout expires. The orchestration
methods in §3.4 can use the same cursor contract for task transitions.
[TUIOS event replay](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/protocol.md#resuming-a-stream) ·
[Spaces wait](../plugins/tmux-spaces/internal/cli/wait.go) ·
[Herdr event subscriptions](https://herdr.dev/tui/docs/socket-api/#event-subscriptions)

### 4.1 Common pane targets and resolved JSON status (API-2)

**Partial.** `read` , `prompt` , and `wait` accept tmux targets.
`tmux-spaces panes --json` lists panes across spaces.

Include a pane address that all three commands accept. Unfiltered `panes` already
uses the shared resolver. Route `panes --session` through the same snapshot and
filter by session after resolution. Include the winning source and its age in the
JSON output.
[zaag's pane addressing](https://github.com/IsmailHatim/zaag/blob/c37ee056d1c7dc91c0e306b5122938db29f72b73/bin/zaag#L664)
·
[zj-radar's state command](https://github.com/marktoda/zj-radar/blob/65b52cc165c4df93c4b2f497facac257088e6d44/docs/using.md#scripting-zj-radar-state)

## 5. Milestones

| Milestone | What the user can do |
| --- | --- |
| Spaces, first | See why a pane needs input, jump to it, and mark or snooze the Inbox item. The popup and CLI read the same status. |
| Spaces, next | Queue a prompt for a named agent, see delivery stalls, resume the exact conversation, and wait for state changes without polling. |
| Spaces, later | Launch a profiled agent in a new worktree, see setup progress in the Inbox, and review the task diff. |
| Automations, first | Create a weekday cron job or a heartbeat in the popup, test it with Run now, and inspect a scheduled run after closing tmux. |
| Orchestration M2 | Dispatch to a named agent in a worktree from the board and answer the worker's question there. The completion report survives detach and reattach. This path uses event-driven waits. |
| Orchestration M4 | Have an ACP coordinator delegate a review through MCP, receive the child's report, and finish the review in the same thread. This path uses task tokens, question relay, and delegation depth limits. |
| Sandbox Phase 1 | Run a router-backed agent profile in an ephemeral or persistent Docker container using the host CLI binary, with traffic forwarded to the host `model-router` over a Unix socket. |
| Sandbox Phase 2 | Run direct OAuth agent profiles in a sandbox with one-way copied auth files and read-only host bundle mounts, without modifying host profiles. |
| Mobile, prototype | Read structured agent chats, answer approvals, queue prompts, and drop into a live tmux terminal from a mobile-first PWA over Tailscale. |
| Mobile, native | Receive push alerts for Attention Inbox blockers and completions, dictate follow-ups, attach photos and files offline, and review worktree diffs in a native mobile app. |
| Terminal plugins, Kitty | Receive clickable OSC 99 alerts that focus the exact Kitty window and tmux pane, clear unread items only on real OS focus, and launch harness profiles or pick models from Kitty overlays. |
| Terminal plugins, multi | Install the WezTerm Lua plugin against the same spacecore presence and OSC 1337 contract, with native InputSelector pickers and tab status badges. |

## 6. Status diagnostics and an isolated demo (OPS-1)

**Partial.** `tmux-spaces doctor` checks tmux and the hook listener.

Check which detected harnesses have hooks installed, whether the spool drains, and
whether recent events reached the expected pane. Report whether the active mux has its
status view configured. Provide an isolated tmux session with mock panes in working,
blocked, done, and failed states. The demo runs without real agents or installed hooks.
[zj-radar's demo and setup checks](https://github.com/marktoda/zj-radar/blob/65b52cc165c4df93c4b2f497facac257088e6d44/docs/install.md)
·
[Agent Beacon's mock layout](https://github.com/anton-fuji/zellij-agent-beacon/blob/e6b2d719fb777df5084731626ac88b2757614dd2/README.md#running-locally)

## 7. Docker sandboxes

**Proposed.** The implementation phases are in §7.6.

Run coding agents inside isolated Linux Docker containers managed by the `harness` CLI.
The sandbox reuses existing `harness` profiles for credentials and injects agent CLIs
from the host, so container images stay generic and do not need agent packages
pre-installed.

Design boundaries:

- Support both ephemeral one-shot runs and persistent containers.
- Copy credentials one way into the sandbox. Never sync refreshed tokens or edits back to
  the host profile.
- Allow outbound internet access by default over Docker bridge networking.
- Target CLI agents directly. Exclude T3 server daemons and pairing flows.

### 7.1 Container layout and defaults

Every sandbox mounts the same paths inside the container:

```text
/workspace                       rw  Host workspace bind mount, defaults to current directory
/opt/harness/bin/<agent>         ro  Host ELF CLI binary from the staging cache
/opt/harness/bundles/<agent>/    ro  Host package directory for relocatable wrapper and Node CLIs
/run/harness/profile/            rw  One-way copy of profile auth files and optional read-only configs
/run/harness/router.sock         rw  Unix socket bridged to the host model-router
/home/agent                      rw  Container-local writable home directory
```

Container runtime defaults:

- Run with the host UID and GID so files created in `/workspace` keep host ownership.
- Pass `--cap-drop=ALL`, `--security-opt=no-new-privileges`, and `--init`. Support
  optional `--cpus` and `--memory` limits.
- Pass `--security-opt label=disable` on SELinux hosts so workspace mounts work without
  relabeling host repositories.
- Default the agent access level to `full` inside sandboxes and skip the interactive
  access picker in `spacelaunch`. Honor an explicit `--access` flag when the caller wants
  `default`, `auto`, or `read-only`.
- Use Docker bridge networking by default so package managers, Git remotes, and direct
  OAuth endpoints work. Offer `--net none` for router-only sessions that talk solely over
  `/run/harness/router.sock`.
- Tag managed containers with Docker labels `dev.model-router.sandbox=true`,
  `dev.model-router.agent`, `dev.model-router.profile`, and `dev.model-router.workspace`
  so `ls` and `inspect` read live state from Docker.

### 7.2 Lifecycle and CLI (SBX-1)

Add a `sandbox` command group to `cmd/harness/main.go`:

```text
harness sandbox run <agent> [--profile <name>] [--workspace <dir>] [--image <img>]
                    [--access <level>] [--net bridge|none]
                    [--binary-mode auto|host|image] [--keep] [-- <agent-args...>]

harness sandbox create <name> [--workspace <dir>] [--image <img>]
                       [--agent <agent>] [--profile <name>] [--net bridge|none]
harness sandbox exec <name> [<agent>] [--profile <name>] [--access <level>]
                     [--binary-mode auto|host|image] [-- <agent-args...>]
harness sandbox shell <name>
harness sandbox ls [--json]
harness sandbox inspect <name> [--json]
harness sandbox stop <name>
harness sandbox start <name>
harness sandbox rm <name> [--force]

harness sandbox image build [--tag harness-sandbox:base]
```

`harness sandbox run` creates a container, attaches the terminal to the agent, and
removes both the container and `$HOME/.model-router/sandboxes/<id>/` when the agent
exits. Passing `--keep` retains the container for later inspection or reuse.

`harness sandbox create` starts a long-lived container running `sleep infinity` so
installed packages and build caches survive across sessions. `harness sandbox exec` runs
an agent inside that container and can inject a different agent or profile on each call
without rebuilding the image. `harness sandbox shell` opens an interactive bash session
in the container.

### 7.3 Host binary delivery (SBX-2)

Because `harness` injects the agent CLI at runtime, any Linux container image such as
`harness-sandbox:base`, `golang:1.24`, or `node:22` can run supported agents without
custom agent Dockerfiles. `internal/harness/sandbox/binary.go` picks one of three
delivery strategies per agent:

- **Host binary.** Applies to standalone static and portable ELF CLIs: `claude`, `codex`,
  `antigravity`, `opencode`, `copilot`, `kimi`, `grok`, `kilo`, and `fx`. Resolve the
  configured host binary with `filepath.EvalSymlinks`, copy it into
  `$HOME/.model-router/sandbox-cache/bins/` keyed by path, size, and mtime, and mount the
  cached file read-only at `/opt/harness/bin/<agent>`. Set `DISABLE_AUTOUPDATER=1` where
  supported so the agent does not try to update a read-only binary.
- **Host directory.** Applies to relocatable shell and Node packages: `pi`, `mcode`,
  `qwen`, `deepseek`, and `gemini`. Mount the release or package directory read-only
  under `/opt/harness/bundles/<agent>` and run its entrypoint with the container shell or
  `node`.
- **Image binary.** Applies to Python `uv` virtualenvs for `vibe` and `kimi-cli`, the
  `fnm` global tree for `cline`, the self-downloading launcher for `muse`, non-Linux
  hosts, and explicit `--binary-mode image` runs. Run the CLI installed inside the
  container image.

### 7.4 Profile credential projection and router bridge (SBX-3)

Resolve profiles through `profiles.Resolve` and handle credentials in two paths based on
`Profile.Router`.

- **Router profiles.** When `Profile.Router` is true, real upstream provider keys stay on
  the host. Because `model-router` binds to `127.0.0.1:4000` and host firewalls such as
  Fedora `firewalld` block container traffic to `docker0`, `harness` avoids exposing the
  router over TCP. Instead, `harness` runs a host-side Unix socket forwarder at
  `$HOME/.model-router/sandboxes/<id>/router.sock` that connects to the host router
  address and mounts it at `/run/harness/router.sock`. A mounted shim subcommand,
  `harness __sandbox-exec`, starts a local listener on container `127.0.0.1:4000` that
  forwards to `/run/harness/router.sock`, then executes the agent. This works with any
  base image and keeps `model-router` bound to host loopback.
- **Direct and OAuth profiles.** When `Profile.Router` is false, `harness` reads
  `AuthFiles`, `ShadowSubdir`, and `AuthFileOnly` from `profiles.OverlayLayout` and
  copies only real auth files from the host profile's `ShadowHome` into
  `$HOME/.model-router/sandboxes/<id>/profile/` with mode `0700`. `harness` skips host
  symlinks in `ShadowHome` because their host targets do not exist inside the container.
  Mounting the staged directory rather than individual files lets OAuth token refreshes
  call `rename` without failing with `EBUSY`. Refreshed tokens stay inside the sandbox
  directory, and `harness sandbox rm` deletes them with the sandbox. `harness` can also
  copy or mount non-secret configs such as `skills` and `mcp_config.json` read-only into
  the staged layout.
- **Container launch resolution.** Call `agents.Resolve` with container-scoped
  `harnessenv.Paths`, container `ShadowHome` set to `/run/harness/profile`, and container
  `RouterSettings.URL` set to `http://127.0.0.1:4000`. Clear host `Profile.BinaryPath` so
  the container binary path takes effect. Forward `ANTIGRAVITY_CREDENTIAL_SCOPE` computed
  from the host `ShadowHome` path so Antigravity matches its stored credential scope
  inside the container.

### 7.5 Base image, attention, and diagnostics (SBX-4)

- **Base image.** Embed one minimal Dockerfile in `internal/harness/sandbox/baseimage.go`
  for `harness sandbox image build`. Include `git`, `bash`, `curl`, `ca-certificates`,
  `ripgrep`, `libstdc++6`, `node`, and `python3` with a non-root `agent` user.
- **Attention tracking.** Run interactive `sandbox run` and `sandbox exec` commands
  through `attn.WrapWithProfile` in `cmd/harness/next.go` so `tmux-spaces` records
  `working`, `done`, and `failed` states for sandboxed agents.
- **Diagnostics.** Add checks to `harness doctor` for Docker daemon reachability,
  `harness-sandbox:base` image presence, and router bridge health.
- **Git worktrees.** When `/workspace` is a linked Git worktree whose `.git` file points
  to an external common Git directory on the host, mount that common directory so Git
  commands work inside the container.

### 7.6 Package layout, phasing, and testing

Keep the sandbox subsystem in the root module with no new `go.mod` dependencies. Run the
`docker` CLI behind an injectable `DockerRunner` interface, the same way
`internal/harness/routerctl` wraps `systemctl`.

```text
internal/harness/sandbox/
  docker.go      DockerRunner interface and exec implementation
  store.go       Staged state under $HOME/.model-router/sandboxes/<name>/
  binary.go      Host binary, host directory, and image delivery strategies
  credproj.go    One-way profile auth copy from OverlayLayout and ShadowEnvMap
  resolve.go     Container-scoped translation over agents.Resolve
  bridge.go      Host Unix socket forwarder and in-container __sandbox-exec shim
  spec.go        Docker run, create, and exec argument assembly
  lifecycle.go   Run, Create, Exec, Shell, List, Inspect, Stop, Start, and Remove
  baseimage.go   Embedded base Dockerfile and image build helper
```

Phases:

1. **Phase 1.** Build `docker.go`, `store.go`, `binary.go` for host ELF binaries,
   `bridge.go` and `harness __sandbox-exec`, `resolve.go`, `spec.go`, `lifecycle.go`,
   `baseimage.go`, and `cmd/harness` wiring. Support router-backed profiles for both
   ephemeral `run` and persistent `create` and `exec`.
2. **Phase 2.** Add `credproj.go` for `Router: false` profiles, host directory bundles
   for `pi`, `mcode`, `qwen`, `deepseek`, and `gemini`, and optional read-only skills and
   MCP config projection.
3. **Phase 3.** Add `harness doctor` checks, `harness <agent> --sandbox` shorthand, and
   linked Git worktree mounting.

Testing bar:

- Run `go test ./...` with a fake `DockerRunner` and temporary directories so tests need
  no Docker daemon, real home directory, or network.
- Table-test `credproj.go` against temporary shadow homes with real auth files, dangling
  symlinks, `ShadowSubdir`, `AuthFileOnly`, and `ANTIGRAVITY_CREDENTIAL_SCOPE`
  preservation. Verify that host profile files stay untouched after staging and cleanup.
- Test `bridge.go` end-to-end against a local `httptest.Server` over a temporary Unix
  socket.
- Test `binary.go` symlink resolution, staging cache reuse, and strategy selection across
  all agent categories.

## 8. Router diagnostics and TUIOS integration

### 8.1 Explain model routing and effective capabilities (RTR-1)

**Proposed.**

Add `harness models explain <model> --ingress responses`. Show the requested name,
the canonical name if an alias matched, the selected provider and upstream dialect,
and whether the route is native or translated. Show the effective capability values
and the source of each override. If resolution fails, show the reason.

Read the explanation from the pinned router snapshot and use the same resolution
and capability functions as request handling. Expose it through the authenticated
diagnostics interface. Return route metadata without credentials or request data.
[TUIOS config origin](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/CONFIGURATION.md#find-where-a-key-comes-from) ·
[route resolution](../internal/router/snapshot.go) ·
[effective capabilities](../internal/router/reasoning.go) ·
[router diagnostics](configuration.md#diagnostics-endpoint)

### 8.2 Show router-backed profiles in TUIOS (MUX-1)

**Proposed.**

Keep tmux as the backend for tmux-spaces. Test launching a router-backed
`harness` profile inside an existing TUIOS pane. Send OSC 7501 through the pane so
TUIOS can display working, blocked, and completed states in its rail and Inbox.
Verify that the agent uses the selected profile and reaches the router. Keep
profile and launch state in `harness` and pane state in TUIOS.
[TUIOS program status](https://github.com/Gaurav-Gosain/tuios/blob/main/docs/PROGRAM_STATUS.md) ·
[profile launch code](../internal/harness/spacelaunch/launch.go)

## 9. Automations

### 9.1 Create an automation in the popup (AUTO-1)

**Proposed.**

Add a `tmux-spaces automations` popup and a navigator action. The list shows the
automation's name, target, trigger, next run in the host's time zone, and last
outcome. Open a row to edit, pause, resume, run now, view history, or remove the
schedule. Filter by project, agent, or outcome when the list grows.

**New automation** defaults the working directory to the current pane's directory.
Choose a name, a harness profile, a working directory, a prompt, and a trigger.
Show the next few run times before saving. Save paused by default so the prompt
and target can be tested before enabling the schedule.

| Trigger | What the host schedules |
| --- | --- |
| Cron | Run at calendar times. Offer hourly, daily, weekdays, and weekly presets, plus a five-field cron expression. Show the effective time zone and reject expressions the selected backend cannot represent. |
| Heartbeat | Wake at a fixed interval. If configured, run a short local precheck with a timeout. An exit code of zero starts the agent. A nonzero exit records a skip. Report a launch error or timeout from the precheck in run history. |

For example, schedule a weekday report at 09:00, or check every 15 minutes for
new items in a local report directory and start the agent only when items exist.
Let a heartbeat run within an optional time window, such as weekdays from
09:00 to 18:00. Use the schedule's time zone to check the window before the
precheck and show only eligible ticks in the preview.
Offer **Check now** to test the heartbeat precheck on its own. **Run now**
starts an immediate run through the precheck, even outside the scheduled
window. Offer **Run without precheck** as an explicit override.
[Orca scheduled automations](https://orca.dev/docs/cli/automations) ·
[T3 Code scheduled task controls](https://github.com/pingdotgg/t3code/blob/main/docs/user/project-settings.md#scheduled-tasks-on-mobile) ·
[Herdr Shepherd heartbeat settings](https://github.com/mikedclarke/herdr-shepherd#defaults)

Use OS scheduling so jobs run with the popup, tmux, and `orchd` closed. Start
with systemd user timers and one-shot services on Linux. Add a user crontab
backend for hosts without a usable systemd user manager. Show which backend
owns the schedule and whether it can run after logout. For systemd, report
whether the user manager is configured to stay active after logout. For cron,
preserve unrelated entries and report any cadence or time-zone limits before
saving. Use a cron backend for expressions that need cron semantics when it
is available.

The popup calls the same `harness automations` commands as scripts. Keep one
saved definition and one active scheduler registration per automation. Generate
the timer, service, or marked crontab entry from its stable ID. The OS entry
invokes the runner by ID and schedule generation. Keep prompts and profile
credentials out of scheduler entries. If registration fails, report the error
and retain the last applied schedule. The runner checks the applied schedule
generation and enabled state when it claims a run. Pause prevents new
scheduled claims. A run already claimed keeps its own record.

Use an expected definition revision when saving an edit. If another client
edited or deleted the automation, ask the user to reload the form. Compare
schedules by their parsed meaning. A prompt edit, rename, or equivalent
schedule expression keeps the next occurrence and the current OS registration.
Change the schedule generation when timing, backend, or enabled state changes.
[T3 Code schedule comparison](https://github.com/pingdotgg/t3code/blob/main/apps/server/src/scheduledTasks/Schedule.ts) ·
[T3 Code stale edit and schedule tests](https://github.com/pingdotgg/t3code/blob/main/apps/server/src/scheduledTasks/ScheduledTaskService.schedule.test.ts)

Expose the run lifecycle through `harness automations run <id> --json` and
`harness automations runs --id <id> --json`. Return a run ID and current state
when the runner accepts an invocation. Offer `run --wait` for callers that
want the settled outcome. Use the same records and reason codes in the popup,
CLI, and Inbox.

Add `harness automations doctor` and show the same registration status in the
popup. Compare the saved definition with what the host scheduler has installed.
Distinguish a missing registration, an edited registration, a leftover
registration for a deleted automation, and a scheduler the tool cannot query.
Offer repair or removal for entries the tool owns. Identify those entries by
their ownership marker as well as their stable ID.
[Existing systemd user commands](../internal/harness/routerctl/routerctl.go) ·
[herdr-cron OS registration and status](https://github.com/huketo/herdr-cron#how-it-runs-your-jobs) ·
[systemd timers](https://www.freedesktop.org/software/systemd/man/latest/systemd.timer.html) ·
[systemd user manager lifetime](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html) ·
[User crontab format](https://manpages.debian.org/bookworm/cron/crontab.5.en.html)

### 9.2 Run a profile and inspect the result (AUTO-2)

**Proposed.**

Start with a fresh CLI agent run in an existing directory. Resolve the profile
through the normal harness launch path, including the router and model settings.
Use an agent's one-shot mode when it supports the saved profile and prompt.
Show a clear launch error if the selected profile needs an interactive setup.
Reuse the profile's normal permission settings and apply a run timeout.

Give each invocation a run ID. Store automation definitions and run records
separately in the local SQLite store. Claim a scheduled occurrence by automation
ID and scheduled instant before launching. In the same transaction, check that
the definition still exists, is enabled, and matches the schedule generation.
Use that definition for the run and record its revision. A completion updates
that run by ID, even if a user has since edited, paused, or removed the
automation.

Allow one active run per automation. **Run now** uses the same run lock and
leaves the next scheduled time in place. Default to skipping missed runs when
the host was offline. Record a skip when an invocation reaches the runner
while a previous run is active. Keep repeated heartbeat skips in history so a
successful check can be distinguished from a timer that stopped firing.

On the next invocation, reconcile earlier runs left in progress. Keep any
verified live process or pane attached to its original run. If the owner is
gone and no result was recorded, mark the run interrupted. Leave the next
scheduled occurrence eligible and preserve the original record for inspection.
Require an explicit retry when the earlier launch might have happened.
[T3 Code dispatch and recovery](https://github.com/pingdotgg/t3code/blob/main/apps/server/src/scheduledTasks/ScheduledTaskService.ts) ·
[T3 Code recovery tests](https://github.com/pingdotgg/t3code/blob/main/apps/server/src/scheduledTasks/ScheduledTaskService.test.ts)

Use the one-shot result to settle a headless run. For the pane-backed mode
in §9.3, follow Herdr's start, prompt, and wait sequence. Bind the run to the
exact pane and agent instance at launch. Wait for the expected CLI to hold
the foreground and be ready for input. A startup blocker creates an Inbox
item. If the agent is busy, use the idle queue from ATT-3.

Capture the event cursor before submitting the prompt. Use it to catch fast
transitions that happen during submission. Require evidence that this
submission took effect before waiting for completion. Match completion to the
same agent instance and submitted turn. If the pane changes occupant, end the
wait as interrupted. If input may have been sent without a receipt, mark the
run stalled and keep the output available for inspection before a manual retry.

History shows when the run was due, when it started and finished, whether it
was scheduled or manual, and its outcome. Record launch failures, precheck
skips, timeouts, and interrupted runs. Link each run to its output and to its
conversation or pane when available. Bound how much history and output the
runner keeps. Show the last heartbeat check separately from the last agent run.
Put failures and requests for input in the existing Inbox. Let users opt into
completion alerts.
[Orca run history and manual runs](https://orca.dev/docs/cli/automations#run-on-demand) ·
[Herdr agent launch](https://herdr.dev/tui/docs/agent-automation/#agent-identity-and-launch) ·
[Herdr agent readiness and completion](https://herdr.dev/tui/docs/agent-automation/#choose-the-control-surface) ·
[Profile resolution](../internal/harness/agents/resolve.go) ·
[Attention Inbox](#11-attention-inbox-and-consistent-pane-state-att-1)

Exercise the boundaries with a controllable clock and a scheduler adapter.
Cover a prompt edit just before a due tick, a pause or delete racing dispatch,
duplicate OS callbacks, and an interrupted launch followed by the next tick.
Check calendar previews across daylight-saving changes against the selected
backend.

### 9.3 Add workspace and trigger options (AUTO-3)

**Deferred.**

Use the worktree flow in §2.1 to create a fresh checkout per run, with optional
repository setup and a link to review the resulting diff. For an existing
worktree, offer a fresh conversation or exact resume of the automation's
previous conversation through ATT-4. Show the selected profile and model in
the preview. When resume is unavailable, report it in the run history.

Offer an optional visible run in a detached tmux window under a dedicated
`scheduled` session. Name the window after the automation and its start time.
Keep the current focus where it is. Link the Inbox blocker and run history to
the exact pane so you can answer a prompt or inspect the output later.
[Herdr Shepherd run windows](https://github.com/mikedclarke/herdr-shepherd#semantics-worth-knowing)

Add an optional missed-run grace window on backends that can honor it. Run
at most one catch-up occurrence. Consider webhooks as a separate trigger
after cron and heartbeats ship. T3 Code's signed deliveries and delivery
history are the reference for that work.
[Orca worktree and session options](https://orca.dev/docs/cli/automations#choose-where-runs-happen) ·
[Orca missed-run grace](https://orca.dev/docs/cli/automations#missed-run-grace) ·
[T3 Code webhook automations](https://github.com/pingdotgg/t3code/blob/main/docs/user/project-settings.md#webhook-automations)

## 10. Remote mobile interface & chat composer over tmux

**Proposed (long-term goal).** Not scheduled for immediate implementation; builds on
`API-1` (§4), `ATT-1` through `ATT-3` (§1.1–1.2), `ATT-12` (§1.11), `convstore`, and
`spacecore`. The implementation phases are in §10.7.

Provide a mobile companion interface that remotely monitors and drives coding agents
running in tmux while presenting a structured chat timeline and rich composer over the
CLI—similar to Orca's native chat over PTYs and T3 Code's composer—without giving up
interactive desktop tmux sessions.

### 10.1 Architectural model and boundaries

Two reference designs define the trade-off space:

- **Structured protocol (T3 Code).** The server owns headless provider processes over
  structured RPC (ACP, Codex app-server, Claude SDK, Pi RPC, OpenCode API) and persists
  normalized events in an event-sourced SQLite journal. This yields rich chat, structured
  approvals, checkpoints, and steering, at the cost of losing interactive `tmux attach`
  desktop workflows.
- **Hybrid PTY + native transcript tail (Orca).** Agents remain real CLI processes in
  terminals. Structured chat views read the agent's **on-disk native transcript** rather
  than scraping ANSI terminal output, writes go through an ordered PTY input-delivery
  layer, an overlay journal holds client-only state (queues, optimistic sends, attachment
  claims), and a live `xterm.js` WebView remains available as an escape hatch.

Because tmux is the primary desktop runtime here, start with **Orca's hybrid model** over
tmux and expose it behind a unified `ThreadDriver` abstraction so future headless
`ACPDriver` (`ORC-3`) sessions render in the exact same mobile UI.

```text
Phone (PWA v0 / Expo RN)                Host Daemon (Go: spacesd / orchd gateway)
┌─────────────────────────────────┐     ┌───────────────────────────────────────────┐
│ Inbox · Spaces · Conversations  │     │ WS/HTTP Gateway · Auth · Upload Store     │
│ Chat Timeline + Rich Composer   │◄───►│ Unified Event Bus (monotonic seq)         │
│ xterm.js WebView (Escape Hatch) │ WSS │  ├── convstore Live Transcript Tailer     │
│ Shared TS client-runtime        │     │  ├── Input Floor & Follow-up Queue (ATT-3)│
│  (reconnect + offline queue)    │     │  ├── tmux Control Mode (-C) / pipe-pane   │
└─────────────────────────────────┘     │  ├── spacecore Hooks + Screen Fallback    │
         Tailscale / E2EE               │  └── Future: ACPDriver (ORC-3)            │
                                        └───────────────────────────────────────────┘
```

### 10.2 Structured transcript tailing and pane binding (MOB-1)

**Proposed.**

Never scrape `tmux capture-pane` ANSI output to synthesize chat bubbles; spinners,
narrow-width line wrapping, alt-screen redraws, and collapsed tool output make terminal
scraping brittle. Instead, extend `internal/harness/convstore`—which already discovers
native session stores across default and isolated profile shadow homes and maintains
incremental `MsgByteOffset` / `MsgPrefixHash` watermarks—to serve live structured chat
timelines:

- **Structured timeline blocks.** Add a `StructuredTimeline` extraction path to core
  adapters first (`claude`, `codex`, `antigravity`, `opencode`), emitting normalized
  blocks: `user_message`, `assistant_markdown`, `thinking`, `tool_call`, `tool_result`,
  `file_diff`, and `subagent_ref` (linked to `ATT-9` inline background work).
- **Incremental tailing and chunked history.** Watch only actively subscribed transcripts
  with `fsnotify` (with stat/WAL polling fallback for SQLite stores such as OpenCode).
  Read backwards by byte chunk (`beforeOffset`) for history pagination and forwards from
  the last complete newline/offset for live streaming.
- **Pane-to-transcript correlation.** Use `spacecore`'s `session_id`, `profile`, and
  `cwd` hook metadata as the authoritative link between a live tmux pane and its
  transcript file, updating cleanly across `/clear` and `/resume`. For hookless agents or
  brand-new panes before turn 1, match by `(agent, shadow_home, cwd, newest_file >= pane_start_ts)`
  and upgrade the binding once the file appears.
- **Overlay journal.** Keep mobile- and daemon-owned state that does not exist in native
  transcripts (queued follow-ups, optimistic sends, attachment claims, stop marks,
  acknowledgments) in a lightweight host overlay table merged at read time.

[convstore adapter interface](../internal/harness/convstore/adapter.go) ·
[convstore schema](../internal/harness/convstore/schema.go) ·
[spacecore views](../internal/harness/spacecore/views.go)

### 10.3 State-gated input floor, queue, attachments, and approvals (MOB-2)

**Proposed.**

Driving a live CLI in tmux from a mobile composer requires stronger guarantees than raw
`tmux send-keys`:

- **Multiline-safe delivery.** Use `tmux load-buffer` + `tmux paste-buffer -p` (bracketed
  paste) followed by an agent-appropriate submit keystroke (`Enter`), avoiding premature
  newline submission or mangled escape sequences. Isolate per-agent input quirks behind
  the agent adapter.
- **Input floor gating.** Before injecting input into `%pane`, verify that the compound
  pane identity (`session_id:window_id:pane_id:pane_pid`, matching `ORC-6`) still matches,
  that the pane is not stuck in tmux copy-mode (`#{pane_in_mode}`), and that `spacecore`'s
  resolved state allows prompt entry.
- **Host-owned follow-up queue and steering.** Build on `ATT-3` so messages submitted
  while an agent is `working` are persisted in an editable, reorderable host-side queue
  and flushed automatically when `spacecore` settles to `done`/`idle` (respecting `ATT-5`
  finite background work). Expose explicit **Interrupt** (`Esc` / `C-c`) and **Steer**
  controls only when the backing agent supports them.
- **Idempotency and ordering.** Require a client-generated `command_id` (recorded in the
  daemon's receipt log) and per-pane sequence ordering so reconnects and mobile network
  retries never double-send prompts.
- **Attachments and large pastes.** Upload mobile images and files to a host-side staging
  directory outside the repository with TTL cleanup, then inject the staged path (or
  agent-specific paste-file reference) into the prompt. Automatically convert large text
  pastes (>32 KiB) into staged file attachments so they do not overwhelm PTY buffers.
- **Epoch-guarded approval and question cards.** Reuse `ATT-12` and `spacecore`'s
  `blocked` (`permission | question | auth`) classification to render inline Action Cards
  above the composer (`Approve`, `Deny`, `Open Terminal`). Guard every tap with a
  `state_epoch` / `event_seq` so a stale mobile response after desktop resolution is
  rejected instead of typing into an active shell.

[prompt CLI](../plugins/tmux-spaces/internal/cli/prompt.go) ·
[T3 Code composer](https://github.com/pingdotgg/t3code/blob/main/docs/user/composer.md)

### 10.4 Host gateway, live terminal escape hatch, and protocol (MOB-3)

**Proposed.**

Expose a versioned HTTP + WebSocket gateway from the Go host daemon, extending `API-1`'s
snapshot and cursor subscription contract (`boot_id`, monotonic `seq`, bounded replay
buffer, and gap-reload markers) over the network:

- **Capability negotiation.** Return a host capability manifest during handshake
  (`structuredChat`, `steer`, `attachments`, `terminalStream`, `diffReview`, plus
  `protocolVersion` / `minClientVersion`) so independently upgraded mobile clients and
  host daemons degrade gracefully rather than failing silently.
- **Unified `ThreadDriver` surface.** Expose one RPC contract (`threads.list`,
  `thread.subscribe`, `thread.send`, `thread.queue.*`, `thread.respond`, `thread.diff`,
  `terminal.subscribe`, `terminal.send`) backed by either `tmux-hybrid` or the future
  `ACPDriver` (`ORC-3`), letting capability flags control which UI affordances light up.
- **Live terminal escape hatch.** Always offer a 1-tap raw terminal view (`xterm.js`
  WebView with a soft-key accessory bar for `Esc`, `Tab`, `Ctrl-C`, arrows, and `Enter`)
  for OAuth logins, interactive TUIs, `vim`, raw shells, or agents without a structured
  transcript decoder yet. Stream terminal output via **`tmux -C` (control mode)** or
  per-pane `pipe-pane` into a bounded host ring/VT snapshot—never by polling
  `capture-pane`.
- **Avoiding desktop resize conflicts.** Do not attach mobile clients in a way that
  shrinks an active 220-column desktop tmux window down to phone width. Default to
  structured chat (which reflows Markdown natively to phone width without touching PTY
  dimensions) and keep the raw terminal view as a read-only byte viewer + `send-keys`
  writer while a desktop client is attached.

### 10.5 Remote transport, shared client runtime, and PWA prototype (MOB-4)

**Proposed.**

Validate the remote experience before taking on native app-store build overhead:

- **Zero-infra transport first.** Serve the gateway over **Tailscale / WireGuard**
  (using Tailscale Serve or TLS with a paired device token and pinned host public key via
  QR code). Keep cloud brokers and tunnels out of the critical path until multi-user
  access is needed.
- **Shared TypeScript `client-runtime`.** Isolate connection lifecycle, stream replay
  (`after_seq`), optimistic sends, offline draft/queue persistence, and state reducers in
  a platform-neutral TypeScript package so a mobile-first web/PWA client and a later
  native app share identical protocol logic.
- **Navigation mapped to `tmux-spaces`.** Mirror the existing tmux mental model on small
  screens:
  1. **Inbox** — hottest blockers, active working agents, and recent outcomes (`prefix + i`).
  2. **Spaces** — sessions, windows, panes, git branch, and diff stats (`prefix + s`).
  3. **Conversations** — `convstore` FTS5 search with tap-to-resume on host (`prefix + y`).
  4. **Thread** — Chat timeline + composer (with `/` commands, `@` file mentions,
     Queue/Steer toggle, Interrupt), plus swipe/tab toggles to **Terminal** and **Diff**
     (reusing `TSK-3` base-ref diffs).
- **Mobile-first PWA prototype.** Ship the first client as a responsive PWA over Tailscale
  to test transcript decoding, bracketed-paste reliability, approval cards, and `xterm.js`
  ergonomics on a real phone before investing in native builds.

### 10.6 Native mobile app, push notifications, and optional E2EE relay (MOB-5)

**Deferred.**

Once the protocol and PWA UX stabilize, wrap the shared `client-runtime` in a native
mobile client (Expo / React Native):

- **Attention Inbox push notifications.** Trigger alerts from `spacecore` transitions into
  `blocked` (`permission | question | auth`), `failed`, and opt-in `done`. Debounce rapid
  transitions, suppress alerts when a desktop tmux client had recent activity
  (`#{client_activity}`), and deep-link taps directly to the originating `space/pane`.
  Start with a lightweight bridge (`ntfy` / Pushover) before operating an APNs/FCM push
  proxy.
- **Native mobile ergonomics.** Add on-device voice dictation into the composer, OS
  share-sheet file/screenshot uploads, local SQLite/MMKV persistence for offline drafts
  and queued attachments across app restarts, and QR-code host pairing.
- **Optional stateless E2EE relay.** If remote access is ever needed outside Tailscale/LAN,
  add an opt-in stateless relay with end-to-end encryption (`X25519` + `ChaCha20-Poly1305`)
  so the relay never sees prompts, transcripts, diffs, or credentials.

### 10.7 Phasing and testing bar

Phases (sequenced so early milestones also improve the desktop workflow):

1. **Phase 1 — Host event & input foundations.** Ship `API-1`, `ATT-1`, `ATT-2`, `ATT-3`,
   and `ATT-12` over the local Unix socket: single status owner, `after_seq` replay,
   bracketed-paste input floor, host follow-up queue, and epoch-guarded approvals.
2. **Phase 2 — Live structured transcripts.** Add structured block decoding and `fsnotify`
   tailing to `convstore` for `claude`, `codex`, `antigravity`, and `opencode`, with
   verified pane-to-transcript binding.
3. **Phase 3 — Remote gateway + PWA prototype.** Expose the daemon over Tailscale
   WebSocket with capability negotiation, `pipe-pane`/`tmux -C` terminal streaming,
   attachment uploads, and a mobile-first PWA built on a shared TS `client-runtime`.
4. **Phase 4 — Native mobile app & push alerts.** Wrap `client-runtime` in Expo RN with
   Attention Inbox push notifications, voice dictation, share-sheet uploads, and offline
   draft/queue persistence.
5. **Phase 5 — Diff review & multi-engine convergence.** Add worktree diff review
   (`TSK-3`) and plug `ACPDriver` (`ORC-3`) behind the same `ThreadDriver` interface.

Testing bar:

- Table-test `convstore` structured decoders and backwards/forwards chunk tailing against
  synthetic JSONL and SQLite WAL fixtures (including partial trailing lines, truncation,
  `/clear` file switches, and oversized records).
- Test the input floor with a fake tmux runner: verify compound pane identity checks,
  copy-mode refusal, bracketed-paste framing, `command_id` deduplication, queue flushing
  after `ATT-5` finite work clears, and rejection of stale `state_epoch` approval taps.
- Provide a headless WebSocket streaming repro script (no phone required) that subscribes
  to a mock pane, verifies initial snapshot + live transcript/terminal deltas + reconnect
  `after_seq` replay, and isolates host gateway bugs from mobile UI rendering bugs.

## 11. Terminal companion plugins

**Proposed.** Builds on `ATT-1` (§1.1), `ATT-6` (§1.5), `ATT-11` (§1.10), `API-1` (§4),
and the existing JSON commands in `cmd/harness/main.go`. Start with Kitty as the first
adapter, then add plugins for other scriptable terminals against the same contract. The
implementation phases are in §11.7.

Define one terminal-agnostic contract across `tmux-spaces`, `spacecore`, and `harness`.
Keep terminal state extraction inside tmux and `spacecore`, and let each terminal plugin
handle OS window focus, desktop notifications, persistent panels, inline graphics, tab
styling, and single-chord overlays in that terminal's native API.

### 11.1 Architectural boundaries and terminal capabilities

When tmux runs inside a terminal emulator, the terminal receives only the active attached
client viewport. Detached tmux sessions, background windows, and unfocused panes send no
bytes to the outer terminal. Scraping panes from a terminal plugin would miss every
background agent.

Each layer owns distinct work:

- **`tmux-spaces` and `spacecore` own state extraction.** Hooks, `tmux pipe-pane -O`,
  `/proc` inspection, `convstore` transcripts, and `capture-pane` run inside or beside
  tmux. They work across detached sessions, SSH clients, and every terminal emulator.
- **`harness` stays terminal-agnostic.** The CLI exposes `--json` output on `agents`,
  `profile list`, `profile show`, `router status`, `router models`, `conversations`,
  `usage`, and `doctor`. Its only terminal additions are standard escape sequences
  understood by multiple terminals (`OSC 1337;SetUserVar`, `OSC 9;4`, and `OSC 8`).
- **`plugins/<terminal>-spaces` owns thin per-terminal adapters.** Each plugin translates
  native focus events into `spacecore` presence updates, renders `API-1` attention state
  into native notifications or tab bars, and launches `harness` or `tmux-spaces` actions
  from native overlays and shortcuts.

| Terminal | Plugin runtime | Capabilities supported by its adapter |
| --- | --- | --- |
| Kitty | Python watchers, kittens, `kitten panel`, `tab_bar.py` | OS focus watcher, stateful clickable OSC 99 alerts, Wayland and macOS edge panel, overlays, clipboard image paste, agent hints, Kitty graphics, `OSC 1337` user vars. |
| WezTerm | Lua plugin via `wezterm.plugin.require` | Cross-platform focus and user-var events, `update-status`, `format-tab-title`, built-in fuzzy `InputSelector` pickers, toast alerts, `OSC 1337` user vars. |
| iTerm2 | Python API (`FocusMonitor`, `VariableMonitor`) | macOS OS focus tracking, native status bar component, per-pane badges and tab colors from `OSC 1337`, clickable notifications. |
| Ghostty, Foot, and others | Escape sequences only | No plugin needed for baseline support. Receive `TERM-1` `pipe-pane` OSC parsing inside tmux, `OSC 9;4` progress bars, `OSC 8` hyperlinks, and `OSC 9` or `OSC 777` alerts. Add a dedicated adapter once Ghostty ships a plugin API. |

### 11.2 Shared Go contract and extended OSC parsing (TERM-1)

**Proposed.**

Build the shared contract once in Go so every terminal plugin stays small and free of
duplicated state logic:

- **Extended escape parsing in `tmux-spaces`.** `plugins/tmux-spaces/internal/osc/osc.go`
  parses only OSC 7501 program-status records. Extend `internal/osc` and
  `internal/cli/oscwatch.go` to parse OSC 9, OSC 99, and OSC 777 desktop notifications,
  OSC `9;4` progress bars, OSC 133 shell prompt and exit markers, and OSC 7 working
  directory updates. Because `tmux pipe-pane -O` reads raw pane output before tmux
  filters escape sequences, this works for unfocused panes and detached sessions on any
  terminal. Route parsed records into `spacecore` and `@spaces_*` pane options through
  the existing coalescing worker.
- **Standard outbound escape sequences in `harness`.** `internal/harness/attn/attn.go`
  disables attention reporting outside Zellij and tmux. Extend `attn.Enabled` to detect
  capable bare-terminal sessions via `KITTY_WINDOW_ID`, `WEZTERM_PANE`,
  `ITERM_SESSION_ID`, `GHOSTTY_RESOURCES_DIR`, or `HARNESS_ATTN=1`. Emit
  `OSC 1337;SetUserVar` for `spaces_state`, `spaces_app`, `spaces_profile`, and
  `spaces_access` from `internal/harness/attn/emit.go`, which Kitty, WezTerm, and iTerm2
  all parse natively with zero subprocess calls. Also emit `OSC 9;4` progress sequences
  and `OSC 8` clickable file hyperlinks in `harness doctor`, `harness profile path`,
  `harness profile show`, and `harness router logs`.
- **Inbound presence reporting in `spacecore`.** Add a terminal-agnostic presence command
  and socket method (`spaces presence --terminal <name> --window <id> --tmux-client <tty> --focused true|false`).
  `spacecore` uses reported OS focus so `ATT-1` auto-clears `done` and unread items only
  when both the tmux pane is active and its outer terminal window holds OS focus.
- **Shared JSON and event consumption.** Terminal plugins subscribe to `API-1` snapshots
  and event streams for alerts and status bars, and invoke `harness --json` and
  `tmux-spaces` commands for pickers and pane jumps.
[OSC parser](../plugins/tmux-spaces/internal/osc/osc.go) ·
[OSC watcher](../plugins/tmux-spaces/internal/cli/oscwatch.go) ·
[Attention gate](../internal/harness/attn/attn.go) ·
[Attention emitter](../internal/harness/attn/emit.go) ·
[Harness CLI](../cmd/harness/main.go)

### 11.3 Kitty starter plugin for focus, alerts, panel bar, and profile cues (TERM-2)

**Proposed.**

Ship Kitty as the first terminal adapter in `plugins/kitty-spaces`:

- **OS focus watcher and client mapping.** Hook `on_focus_change`, `on_load`, and
  `on_close` in `watcher.py` without polling. Correlate each Kitty OS window and tab to
  its attached tmux client TTY via `$KITTY_WINDOW_ID` and `kitty @ ls`, then report focus
  changes through `spaces presence`.
- **Stateful clickable OSC 99 notifications.** Suppress desktop alerts when the user is
  viewing that pane in a focused Kitty window. When Kitty is unfocused or the agent sits
  in another tmux session or pane, emit a Kitty OSC 99 notification keyed by pane ID.
  Update the notification in place as the pane moves from `working` to `blocked` or
  `done`, and close it when the user focuses the pane. Clicking a notification runs
  `jump.py`, which focuses the matching Kitty OS window and tab and executes
  `tmux switch-client` and `select-pane` (`ATT-11`).
- **Always-visible `kitten panel` bar and tab badges.** Run a slim edge bar on Wayland
  and macOS backed by `tmux-spaces status-bar --json` and `spacecore` so the highest
  priority blocked agent, wait time, and active counts stay visible over browsers and
  editors (`ATT-6`). Clicking the bar triggers `jump.py`. Provide an optional
  `tab_bar.py` helper that displays attention counts and `model-router` health in Kitty's
  tab bar and OS window title.
- **Bare Kitty attention, profile accents, and clickable paths.** Handle
  `on_set_user_var` in `watcher.py` to display `spaces_state` and `spaces_profile` on
  bare Kitty tabs, fire OSC 99 alerts when unfocused, and register bare Kitty windows
  with `spacecore` when configured. Apply distinct Kitty tab or border colors for
  `spaces_access` and `spaces_profile` so `full-access` runs look different from
  `read-only` runs and router-backed profiles look different from direct profiles. Ship
  `open-actions.conf` rules so clicking `harness` OSC 8 links opens config files, profile
  directories, and logs in `$EDITOR`.

### 11.4 Kitty overlays, agent input kittens, and graphics previews (TERM-3)

**Proposed.**

Add Kitty kittens launched via `kitty @ launch --type=overlay` on single-chord Kitty
shortcuts. Overlays work without tmux prefix keys, work inside nested tmux or SSH
sessions, and close on `Esc` without changing the pane layout:

- **Quick-launch palette.** Read `harness agents --json` and `harness profile list --json`,
  let the user pick an agent, profile, and access level, and open the session in a new
  tmux window or Kitty split.
- **Model picker with paste-back.** Open `harness models` in a Kitty overlay and paste the
  chosen canonical model ID into the underlying window or pane.
- **Ops drawer.** Toggle `harness router status`, `harness router logs --follow`,
  `harness usage --by model`, `harness conversations`, or `harness doctor` in a floating
  overlay without leaving the active agent pane.
- **Clipboard image paste kitten.** Read an image from the system clipboard, save it to a
  temporary staging directory outside the repository, and paste the file path into the
  active agent pane or attach it to a queued prompt in `ATT-3` and `MOB-2`.
- **Agent hints kitten.** Extend `kitten hints` to match file and line references, Git
  commit SHAs, test names, and pull request URLs in agent output. Let the user open a
  match in `$EDITOR`, copy it, or send it as a prompt to another agent pane.
- **Inline graphics previews.** Render screenshots, Mermaid diagrams, visual diffs from
  `TSK-3`, and `harness usage --json` charts from `TSK-2` inside Kitty overlay kittens
  using the Kitty graphics protocol, which plain tmux popups strip or mangle.

### 11.5 WezTerm Lua plugin (TERM-4)

**Proposed.**

Add a cross-platform Lua plugin in `plugins/wezterm-spaces` installable with
`wezterm.plugin.require` on Linux, macOS, and Windows:

- **Focus and user-var hooks.** Subscribe to `window-focus-changed` to map WezTerm
  windows to attached tmux client TTYs and report focus through `spaces presence`.
  Subscribe to `user-var-changed` to read `OSC 1337;SetUserVar` emitted by `harness` in
  bare WezTerm panes.
- **Tab formatting and status bar.** Use `format-tab-title` and `update-status` to render
  `tmux-spaces` attention counts, bare-pane agent states, `spaces_access` and
  `spaces_profile` color accents, and `model-router` health.
- **Native `InputSelector` palettes.** Build `harness` profile quick-launch, `harness models`
  paste-back, and `harness` ops actions using WezTerm's built-in fuzzy `InputSelector`
  over `harness --json` output without maintaining a separate TUI.
- **Focus-gated toast alerts.** Fire `window:toast_notification` when `API-1` reports a
  blocker or completion in an unfocused WezTerm window or inactive tmux pane.

### 11.6 iTerm2 Python plugin and escape-only terminals (TERM-5)

**Deferred.**

- **iTerm2 Python plugin.** Ship `plugins/iterm2-spaces` on the official iTerm2 Python
  API for macOS. Use `FocusMonitor` to report OS window focus to `spaces presence`,
  `VariableMonitor` to read `OSC 1337;SetUserVar` from `harness`, a native Status Bar
  component for `tmux-spaces` attention counts and router health, per-pane badges and tab
  colors for `spaces_profile` and `spaces_access`, and macOS notification click handlers
  that focus the matching iTerm2 session and tmux pane.
- **Ghostty, Foot, and escape-only terminals.** These terminals require no plugin for
  baseline behavior. They inherit `TERM-1` extended `pipe-pane` parsing inside tmux,
  `OSC 9;4` progress bars, `OSC 8` hyperlinks, and `OSC 9` or `OSC 777` alerts. Add a
  dedicated adapter once Ghostty ships a plugin or scripting API.

### 11.7 Package layout, phasing, and testing bar

Keep terminal plugins isolated under `plugins/` and keep Go changes limited to the shared
contract in `internal/osc`, `internal/harness/attn`, and `spacecore`:

```text
plugins/kitty-spaces/
  watcher.py           on_focus_change, on_set_user_var, window-to-tmux mapping, OSC 99 alerts
  jump.py              notification and panel click handler for Kitty window and tmux pane focus
  palette.py           overlay quick-launcher, model picker paste-back, and harness ops drawer
  paste_image.py       clipboard image staging and path paste kitten
  agent_hints.py       file:line, commit SHA, test name, and PR URL hints kitten
  preview.py           Kitty graphics protocol preview overlay for images and usage charts
  tab_bar.py           optional Kitty tab bar formatter for attention badges and router health
  panel.sh             kitten panel entrypoint for the persistent desktop attention bar
  open-actions.conf    OSC 8 click rules for harness config, profile, and log paths

plugins/wezterm-spaces/
  plugin/init.lua      wezterm.plugin.require entrypoint, focus/user-var hooks, tab/status bar
  plugin/palette.lua   InputSelector pickers for harness profiles, models, and ops commands

plugins/iterm2-spaces/
  spaces_daemon.py     FocusMonitor, VariableMonitor, status bar component, and badge updater
```

Phases:

1. **Phase 1.** Ship `TERM-1` with extended OSC parsing in
   `plugins/tmux-spaces/internal/osc`, multi-terminal `OSC 1337` and `OSC 9;4` emission
   in `internal/harness/attn`, OSC 8 file hyperlinks in `harness`, and `spaces presence`
   in `spacecore`.
2. **Phase 2.** Ship `TERM-2` for Kitty with `watcher.py`, `jump.py`, clickable OSC 99
   notifications, bare Kitty user-var badges, visual profile accents, `open-actions.conf`,
   `kitten panel`, and `tab_bar.py`.
3. **Phase 3.** Ship `TERM-3` interactive Kitty kittens in `palette.py`, `paste_image.py`,
   `agent_hints.py`, and `preview.py` for `TSK-2` and `TSK-3`.
4. **Phase 4.** Ship `TERM-4` WezTerm Lua plugin against the same `TERM-1` contract.
   Follow with deferred `TERM-5` adapters for iTerm2 and future scriptable terminals.

Testing bar:

- Table-test `plugins/tmux-spaces/internal/osc` against mixed chunk boundaries for OSC 9,
  OSC 99, OSC 777, OSC `9;4`, OSC 133, OSC 7, and tmux DCS passthrough sequences.
- Unit-test `internal/harness/attn` with injected `TtyWriter` buffers and environment
  overrides for `KITTY_WINDOW_ID`, `WEZTERM_PANE`, `ITERM_SESSION_ID`, and
  `GHOSTTY_RESOURCES_DIR` to verify base64 `OSC 1337;SetUserVar` output and zero
  subprocess calls outside tmux.
- Unit-test `spacecore` presence gating so `ATT-1` clears unread and `done` items only
  when both tmux pane activity and outer terminal focus match.
- Unit-test `plugins/kitty-spaces` in Python with fake Kitty `boss` and `window` objects
  and `plugins/wezterm-spaces` in Lua with a fake `wezterm` table and `harness` JSON
  fixtures so all plugin tests run headless.
