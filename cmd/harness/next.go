package main

// Wiring for the attention-aware launch path and the usage, models, and
// doctor subcommands. The packages behind these are internal/harness/attn,
// internal/harness/usage, internal/harness/modelpick, and
// internal/harness/doctor; this file only adapts them to the CLI.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/harness/agents"
	"github.com/arhammahajan/model-router/internal/harness/attn"
	"github.com/arhammahajan/model-router/internal/harness/doctor"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
	"github.com/arhammahajan/model-router/internal/harness/modelpick"
	"github.com/arhammahajan/model-router/internal/harness/spacecore"
	"github.com/arhammahajan/model-router/internal/harness/spaceresume"
	usageagg "github.com/arhammahajan/model-router/internal/harness/usage"
)

// defaultUsagePath is the conventional accounting log location; the
// router's [accounting] path should point here so `harness usage` finds it
// without arguments.
func defaultUsagePath(paths harnessenv.Paths) string {
	return filepath.Join(paths.StateHome, "model-router", "accounting.jsonl")
}

// launchAgent runs the resolved agent. Inside a Zellij or tmux pane with
// attention reporting enabled it supervises the agent as a child process so
// the launcher can emit working/done records and track the active harness
// profile; otherwise it exec-replaces like before.
func launchAgent(resolved agents.Resolved, agent, profile string) int {
	profile = strings.TrimSpace(profile)
	resolved.Env = withProfileEnv(resolved.Env, profile)
	if profile == "" {
		resolved.UnsetEnv = append(resolved.UnsetEnv, "HARNESS_PROFILE")
	}
	if attn.Enabled() {
		// The child-run model cannot express unsets via env appends, so
		// apply them to this process's environment first; nothing after
		// this point reads them.
		for _, name := range resolved.UnsetEnv {
			_ = os.Unsetenv(name)
		}
		envAppend := append([]string{}, resolved.Env...)
		if len(resolved.PathPrepend) > 0 {
			joined := strings.Join(append(append([]string{}, resolved.PathPrepend...), os.Getenv("PATH")), ":")
			envAppend = append(envAppend, "PATH="+joined)
		}
		if strings.EqualFold(strings.TrimSpace(os.Getenv("SPACES_PRIOR_OUTCOME")), "failed") {
			spaceresume.SetTmuxPaneOption("", "@spaces_prior_outcome", "failed")
		}
		code := attn.WrapWithProfile(attn.CanonicalApp(agent), profile, resolved.Binary, resolved.Args, envAppend)
		recordLaunchOutcome(agent, profile, resolved.Args, code)
		return code
	}
	attn.SetTmuxProfile(profile)
	if err := agents.Launch(resolved); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	return 0
}

// recordLaunchOutcome persists a recent outcome entry when a supervised agent
// exits inside a tmux/Zellij pane, so harnesses without Stop hooks (such as
// Antigravity) still show up in the Attention Inbox's Recent Outcomes list.
func recordLaunchOutcome(agent, profile string, args []string, exitCode int) {
	if spacecore.Disabled() {
		return
	}
	canonical := spaceresume.Normalize(agent)
	if canonical == "" {
		canonical = strings.ToLower(strings.TrimSpace(agent))
	}
	if spaceresume.IsNoninteractive(canonical, args) {
		return
	}
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "--version" || a == "-V" {
			return
		}
	}
	session, pane, ok := spaceresume.HookTarget()
	if !ok || session == "" {
		return
	}
	cwd := spaceresume.CurrentDirectory()
	sid := spaceresume.GetTmuxPaneOption("", "@spaces_resume_session")
	if sid == "" {
		sid = spaceresume.ExtractSessionIDFromArgs(canonical, args)
	}
	if sid == "" && canonical == "antigravity" && profile != "" {
		sid = spaceresume.DiscoverAntigravityForProfile(cwd, profile)
	}
	if sid == "" {
		sid = spaceresume.Discover(canonical, cwd)
	}
	title := spaceresume.DiscoverTitle(canonical, cwd, sid)
	if title == "" {
		title = spaceresume.GetTmuxPaneOption("", "@spaces_title")
	}

	outcome := "done"
	text := "finished"
	if exitCode != 0 {
		outcome = "failed"
		text = fmt.Sprintf("exited with status %d", exitCode)
	} else if spaceresume.GetTmuxPaneOption("", "@spaces_prior_outcome") == "failed" ||
		spacecore.PaneState(session, pane) == "failed" {
		// Closing a conversation whose last outcome was failed (either reopened
		// while idle or failed during this session) returns it to FAILED.
		outcome = "failed"
		text = "failed"
	}
	spaceresume.UnsetTmuxPaneOption("", "@spaces_prior_outcome")
	_ = spacecore.RecordOutcome(map[string]any{
		"session":    session,
		"pane":       fmt.Sprintf("%d", pane),
		"agent":      canonical,
		"app":        spaceresume.DisplayApp(canonical),
		"profile":    profile,
		"outcome":    outcome,
		"text":       text,
		"title":      title,
		"session_id": sid,
		"cwd":        cwd,
		"ts_ms":      time.Now().UnixMilli(),
	})
	_ = spacecore.ClearPane(session, pane)
}

func withProfileEnv(env []string, profile string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == "HARNESS_PROFILE" {
			continue
		}
		out = append(out, kv)
	}
	if profile != "" {
		out = append(out, "HARNESS_PROFILE="+profile)
	}
	return out
}

// runUsage is `harness usage [--days N] [--by day|model] [--json] [PATH]`.
func runUsage(args []string) int {
	fs := flag.NewFlagSet("harness usage", flag.ContinueOnError)
	days := fs.Int("days", 0, "limit to the last N days (0 = all)")
	by := fs.String("by", "day", "grouping: day or model")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *by != "day" && *by != "model" {
		return failUsage("unknown --by grouping: %s", *by)
	}
	path := fs.Arg(0)
	if path == "" {
		path = defaultUsagePath(harnessenv.DefaultPaths())
	}
	entries, err := usageagg.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	summary := usageagg.Aggregate(entries, time.Now(), *days)
	if *jsonOut {
		fmt.Println(usageagg.RenderJSON(summary))
		return 0
	}
	fmt.Print(usageagg.RenderTable(summary, *by))
	return 0
}

// runModels is `harness models`: an interactive picker over the router's
// model list on a terminal, a plain table when headless.
func runModels(args []string) int {
	for _, a := range args {
		return failUsage("unknown models flag: %s", a)
	}
	opts, _, _ := routerOptions()
	models, err := modelpick.Fetch(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if len(models) == 0 {
		fmt.Fprintf(os.Stderr, "%s: the router serves no models\n", progName)
		return 1
	}
	if picked, ok := modelpick.Pick(models); ok {
		fmt.Println(picked.ID)
		return 0
	}
	// A terminal picker that cancelled prints nothing; a headless run
	// prints the table so the command still answers.
	fmt.Print(modelpick.Render(models))
	return 0
}

// runDoctor is `harness doctor [--json]`.
func runDoctor(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a != "--json" {
			return failUsage("unknown doctor flag: %s", a)
		}
		jsonOut = true
	}
	opts, paths, _ := routerOptions()
	checks := doctor.Run(doctor.CheckOptions{
		Context: context.Background(),
		Paths:   paths,
		Router:  opts,
	})
	if jsonOut {
		out, err := doctor.RenderJSON(checks)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		fmt.Print(out)
		return 0
	}
	fmt.Print(doctor.Render(checks))
	return 0
}
