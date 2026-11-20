// Command harness: the unified CLI for launching coding harnesses,
// managing provider profiles, and controlling the model router.
//
// Usage:
//
//	harness <agent> [args...]
//	harness profile <name> [args...] | list | show | create | remove | describe
//	harness router start | stop | restart | status | models | validate | logs
//	harness agents | status | version | help
//
// Exit codes: 0 success, 1 runtime failure, 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/harness/agents"
	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
	"github.com/arhammahajan/model-router/internal/harness/profiles"
	"github.com/arhammahajan/model-router/internal/harness/routerctl"
	"github.com/arhammahajan/model-router/internal/harness/spacecore"
	"github.com/arhammahajan/model-router/internal/harness/spacelaunch"
	"github.com/arhammahajan/model-router/internal/harness/spaceresume"
	"github.com/arhammahajan/model-router/internal/server"
)

const progName = "harness"

func main() {
	os.Exit(run(os.Args[1:]))
}

func usage(w *os.File) {
	fmt.Fprintf(w, `Usage: harness <agent> [args...]
       harness <agent> profile <profile> [args...]
       harness profile <instance> [args...]
       harness profile list [--json] [--agent <agent>]
       harness profile show <name> [--agent <agent>] [--json] [--reveal]
       harness profile get <name> <field> [--agent <agent>] [--reveal]
       harness profile path [<name>] [--agent <agent>]
       harness profile init
       harness profile create <name> --agent <agent> [options]
       harness profile edit <name> [--agent <agent>] [options]
       harness profile enable <name> [--agent <agent>]
       harness profile disable <name> [--agent <agent>]
       harness profile clone <src> <dst> [--agent <agent>] [--no-seed]
       harness profile rename <old> <new> [--agent <agent>]
       harness profile remove <name> [--agent <agent>] [--purge]
       harness profile describe <name> [--agent <agent>] [args...]
       harness router start|stop|restart|status|models|validate|logs [options]
       harness agents [--json]
       harness status [--json]
       harness spaces [command] [args...]
       harness conversations [list|search|sync|status] [options]
       harness models                     pick a model from the router (table when headless)
       harness usage [--days N] [--by day|model] [--json] [PATH]
       harness doctor [--json]           whole-stack health report

Profile routing: each agent starts with a single built-in unmodified
"default" profile. In a single-profile setup (only "default"),
harness <agent> [args...] launches it directly. Once you add one or more
custom profiles for an agent, harness <agent> fails with a usage error
and asks you to choose one via
harness <agent> profile <profile> [args...] (for example "default" or
your custom profile name). Profile names are scoped to their agent.
Use agent/name to select a profile wherever a name is accepted for lookup.

The spaces command is also installed as the "spaces" entry name of this binary and
controls Spaces hooks, session resume, and agent launcher helpers.

Agents:
  claude       codex        grok          cline
  opencode     pi           qwen          copilot
  kimi         kimi-cli     mcode         muse
  antigravity  fx           deepseek      kilo
  vibe         vibe-acp     antigravity-acp  gemini

Aliases: agy = antigravity, dsh = deepseek, kimi-code = kimi
Set HARNESS_AGENT to choose the agent without a positional argument
(T3 Code invokes it this way; those launches skip the access picker).
Interactive launches offer the agent's access levels (full access, auto,
read-only, ...) before starting; preselect with HARNESS_ACCESS=<level>,
or answer "q" at the prompt to cancel.
All remaining arguments are passed unchanged to the agent.
`)
}

func failUsage(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "%s: %s\n", progName, fmt.Sprintf(format, args...))
	usage(os.Stderr)
	return 2
}

func run(args []string) int {
	// The harness binary serves several entry names: `zj` routes the whole
	// argument vector to the Zellij helper surface.
	if filepath.Base(os.Args[0]) == "spaces" {
		return runSpaces(args)
	}
	if len(args) == 0 {
		return failUsage("choose an agent or command")
	}
	switch args[0] {
	case "-h", "--help":
		usage(os.Stdout)
		return 0
	case "version", "--version":
		fmt.Println(progName, server.BuildVersion)
		return 0
	case "help":
		if len(args) > 1 {
			return topicHelp(args[1])
		}
		usage(os.Stdout)
		return 0
	case "router":
		return runRouter(args[1:])
	case "profile":
		return runProfile(args[1:])
	case "agents":
		return runAgents(args[1:])
	case "status":
		return runStatus(args[1:])
	case "spaces":
		return runSpaces(args[1:])
	case "conversations", "chats":
		return runConversationsCLI(args[1:])
	case "usage":
		return runUsage(args[1:])
	case "models":
		return runModels(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "hook-server":
		// The hook listener lazy-spawns itself as `<binary> hook-server
		// --daemon` (spacecore.EnsureServer/HooksStart), whatever the binary
		// is named; route that bare form to the zj surface too.
		return runSpaces(args)
	case "__shim", "__layout", "__prepare-restore", "__resume-wrap",
		"__space-harnesses", "__space-launch-agent", "__shim-profile", "__space-conversations":
		// Internal helpers re-exec this binary by its resolved path (see
		// zjcli SelfPath and zjspace), so argv[0] is not `zj` when they
		// run. Route them to the zj surface like `hook-server --daemon`;
		// none of these names can be an agent.
		return runSpaces(args)
	}
	return runAgentLaunch(args)
}

func topicHelp(topic string) int {
	switch topic {
	case "router":
		fmt.Println("harness router start [--config PATH] [--foreground] [--wait DURATION]")
		fmt.Println("harness router stop")
		fmt.Println("harness router restart")
		fmt.Println("harness router status [--json]")
		fmt.Println("harness router models [--json]")
		fmt.Println("harness router validate [--config PATH]")
		fmt.Println("harness router logs [--follow] [-n LINES]")
	case "profile":
		fmt.Println("harness <agent> [args...]")
		fmt.Println("harness <agent> profile <profile> [args...]")
		fmt.Println("harness profile <name> [args...]")
		fmt.Println("harness profile list [--json] [--agent <agent>]")
		fmt.Println("harness profile show <name> [--agent <agent>] [--json] [--reveal]")
		fmt.Println("harness profile get <name> <field> [--agent <agent>] [--reveal]")
		fmt.Println("                    fields: binary, home-path, shadow-home, template-dir, env, env.<KEY>,")
		fmt.Println("                            args, launch-env, launch-args, launch-cmd, router, overlay,")
		fmt.Println("                            agent, enabled, display-name, source, seed-from, auth-store, config-file")
		fmt.Println("harness profile path [<name>] [--agent <agent>]")
		fmt.Println("harness profile init")
		fmt.Println("harness profile create <name> --agent <agent> [--display-name N] [--shadow-home PATH]")
		fmt.Println("                    [--seed-from DIR] [--no-seed] [--env K=V]... [--arg A]... [--router]")
		fmt.Println("                    [--binary PATH] [--home-path PATH] [--overlay]")
		fmt.Println("harness profile edit <name> [--agent <agent>] [--display-name N] [--binary PATH] [--home-path PATH]")
		fmt.Println("                    [--shadow-home PATH] [--no-shadow-home] [--seed-from DIR]")
		fmt.Println("                    [--env K=V]... [--unset-env K]... [--arg A]... [--clear-args]")
		fmt.Println("                    [--router] [--no-router] [--overlay] [--no-overlay]")
		fmt.Println("harness profile enable <name> [--agent <agent>]")
		fmt.Println("harness profile disable <name> [--agent <agent>]")
		fmt.Println("harness profile clone <src> <dst> [--agent <agent>] [--no-seed]")
		fmt.Println("harness profile rename <old> <new> [--agent <agent>]")
		fmt.Println("harness profile remove <name> [--agent <agent>] [--purge]")
		fmt.Println("harness profile describe <name> [--agent <agent>] [args...]")
	case "agents":
		fmt.Println("harness agents [--json]")
	case "conversations", "chats":
		fmt.Println("harness conversations [list] [--agent A] [--cwd D] [--limit N] [--include-archived] [--refresh] [--json]")
		fmt.Println("harness conversations search <query> [--agent A] [--cwd D] [--scope all|user|titles] [--limit N] [--json]")
		fmt.Println("harness conversations sync [--metadata-only] [--max-sources N] [--json]")
		fmt.Println("harness conversations status [--json]")
	default:
		return failUsage("unknown help topic: %s", topic)
	}
	return 0
}

// --- agent launching -------------------------------------------------------

// Seams for unit tests: stub these to avoid exec'ing real binaries or
// touching the real home directory.
var (
	harnessDefaultPaths = harnessenv.DefaultPaths
	harnessForAgent     = profiles.ForAgent
	harnessLaunchAgent  = launchAgent
)

// profileResolutionNames returns sorted profile names for messages.
func profileResolutionNames(res []profiles.Resolution) []string {
	names := make([]string, 0, len(res))
	for _, r := range res {
		names = append(names, r.Name)
	}
	return names
}

// availableAgentProfiles lists resolvable profile names for agent for error
// messages. It returns nil when the lookup fails or finds nothing.
func availableAgentProfiles(canonical string, paths harnessenv.Paths) []string {
	res, err := harnessForAgent(canonical, paths)
	if err != nil || len(res) == 0 {
		return nil
	}
	return profileResolutionNames(res)
}

// formatAvailable renders the "(available for <agent>: a, b)" suffix, or the
// "(no profiles found for <agent>)" fallback.
func formatAvailable(canonical string, paths harnessenv.Paths) string {
	if names := availableAgentProfiles(canonical, paths); len(names) > 0 {
		return fmt.Sprintf("(available for %s: %s)", canonical, strings.Join(names, ", "))
	}
	return fmt.Sprintf("(no profiles found for %s)", canonical)
}

func runAgentLaunch(args []string) int {
	// T3 binary overrides cannot prepend an argument before probes and SDK
	// commands, so HARNESS_AGENT selects the agent when present. Those
	// launches keep the existing bare-agent behavior without profile
	// interception or `profile` subcommand parsing.
	if envAgent := os.Getenv("HARNESS_AGENT"); envAgent != "" {
		canonical, ok := agents.Normalize(envAgent)
		if !ok {
			return failUsage("unknown agent: %s", envAgent)
		}
		paths := harnessDefaultPaths()
		return runBareAgent(canonical, paths, args, "")
	}
	agentName := args[0]
	agentArgs := args[1:]
	canonical, ok := agents.Normalize(agentName)
	if !ok {
		return failUsage("unknown agent: %s", agentName)
	}
	paths := harnessDefaultPaths()
	// Explicit syntax: harness <agent> profile <profile> [args...].
	if len(agentArgs) > 0 && agentArgs[0] == "profile" {
		return runAgentProfileExplicit(canonical, paths, agentArgs)
	}
	res, err := harnessForAgent(canonical, paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	switch len(res) {
	case 0:
		return runBareAgent(canonical, paths, agentArgs, profiles.DefaultProfileName)
	case 1:
		if res[0].Source == profiles.SourceDefault {
			if !res[0].Customized {
				return runBareAgent(canonical, paths, agentArgs, res[0].Name)
			}
			return launchDefaultProfile(res[0], paths, agentArgs, false)
		}
		return launchResolvedProfile(res[0], paths, agentArgs, false)
	default:
		names := profileResolutionNames(res)
		fmt.Fprintf(os.Stderr, "%s: %s has %d profiles (%s); use \"harness %s profile <profile> [args...]\" to choose one\n",
			progName, canonical, len(res), strings.Join(names, ", "), canonical)
		return 2
	}
}

// runBareAgent launches the agent directly without any custom profile overrides.
func runBareAgent(canonical string, paths harnessenv.Paths, agentArgs []string, profile string) int {
	router := harnessenv.RouterEnv(paths)
	spec := harnessenv.LaunchSpec{Agent: canonical}
	if canonical == "antigravity" || canonical == "antigravity-acp" || canonical == "grok" {
		if err := profiles.MaterializeForLaunch(profiles.DefaultResolution(canonical), paths); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
	}
	if err := spacelaunch.ApplyAccessLevel(&spec); err != nil {
		if errors.Is(err, spacelaunch.ErrAccessCancelled) {
			fmt.Fprintln(os.Stderr, "harness: launch cancelled")
			return 0
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 2
	}
	resolved, err := agents.Resolve(spec, paths, router, agentArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	// Attention-aware: inside Zellij or tmux the agent is supervised so the
	// launcher can report working/done around the run.
	return harnessLaunchAgent(resolved, canonical, profile)
}

// runAgentProfileExplicit implements `harness <agent> profile <profile>
// [args...]`: argsAfterAgent[0] is the literal "profile" keyword.
func runAgentProfileExplicit(canonical string, paths harnessenv.Paths, argsAfterAgent []string) int {
	if len(argsAfterAgent) < 2 || argsAfterAgent[1] == "" {
		fmt.Fprintf(os.Stderr, "%s: harness %s profile requires a profile name %s\n",
			progName, canonical, formatAvailable(canonical, paths))
		return 2
	}
	name := argsAfterAgent[1]
	rest := argsAfterAgent[2:]
	ref, err := qualifyProfileName(name, canonical)
	if err != nil {
		return failUsage("%v", err)
	}
	resolution, err := profiles.Resolve(ref, paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: profile %q for agent %s: %v %s\n",
			progName, name, canonical, err, formatAvailable(canonical, paths))
		return 2
	}
	if resolution.Source == profiles.SourceDefault {
		if !resolution.Customized {
			return runBareAgent(canonical, paths, rest, resolution.Name)
		}
		return launchDefaultProfile(resolution, paths, rest, false)
	}
	return launchResolvedProfile(resolution, paths, rest, false)
}

// launchDefaultProfile launches a customized per-agent default profile using
// agents.Resolve (no profile-mode PATH augmentation) while honoring all JSON
// overrides (binaryPath, homePath, shadowHomePath, env, prependArgs, router,
// overlay).
func launchDefaultProfile(resolution profiles.Resolution, paths harnessenv.Paths, rest []string, describe bool) int {
	spec := profiles.LaunchSpecFromPaths(resolution, paths)
	router := harnessenv.RouterEnv(paths)
	if describe {
		if err := spacelaunch.PreselectAccessLevel(&spec); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 2
		}
		resolved, err := agents.Resolve(spec, paths, router, rest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		fmt.Println(agents.Describe(resolved))
		return 0
	}
	if err := profiles.MaterializeForLaunch(resolution, paths); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if err := spacelaunch.ApplyAccessLevel(&spec); err != nil {
		if errors.Is(err, spacelaunch.ErrAccessCancelled) {
			fmt.Fprintln(os.Stderr, "harness: launch cancelled")
			return 0
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 2
	}
	resolved, err := agents.Resolve(spec, paths, router, rest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	return harnessLaunchAgent(resolved, spec.Agent, resolution.Name)
}

// launchResolvedProfile resolves and launches one profile with rest as the
// caller args. It is shared by `harness profile <name>` and the
// agent-scoped `harness <agent> [profile ...]` paths. When describe is true
// it prints the resolved command instead of launching.
func launchResolvedProfile(resolution profiles.Resolution, paths harnessenv.Paths, rest []string, describe bool) int {
	spec := profiles.LaunchSpecFromPaths(resolution, paths)
	router := harnessenv.RouterEnv(paths)
	// HARNESS_AGENT (T3) invocations pass their own flags; a deterministic
	// HARNESS_ACCESS preselect also shows in `describe`.
	if err := spacelaunch.PreselectAccessLevel(&spec); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 2
	}
	var resolved agents.Resolved
	var err error
	if resolution.NativeAntigravity {
		// Native Antigravity uses the regular CLI with an imported private login.
		resolved, err = agents.Resolve(spec, paths, router, rest)
	} else {
		// Store and T3 profiles get profile-mode PATH augmentation.
		resolved, err = agents.ResolveProfile(spec, paths, router, rest)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if describe {
		fmt.Println(agents.Describe(resolved))
		return 0
	}
	if err := profiles.MaterializeForLaunch(resolution, paths); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	// Only real launches offer the interactive picker.
	if err := spacelaunch.PromptAccessLevelForLaunch(&spec); err != nil {
		if errors.Is(err, spacelaunch.ErrAccessCancelled) {
			fmt.Fprintln(os.Stderr, "harness: launch cancelled")
			return 0
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 2
	}
	// The prompt may have layered flags onto the spec; re-resolve.
	if resolution.NativeAntigravity {
		resolved, err = agents.Resolve(spec, paths, router, rest)
	} else {
		resolved, err = agents.ResolveProfile(spec, paths, router, rest)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	// Attention-aware: inside Zellij or tmux the agent is supervised so the
	// launcher can report working/done around the run.
	return harnessLaunchAgent(resolved, spec.Agent, resolution.Name)
}

// --- profiles ---------------------------------------------------------------

func runProfile(args []string) int {
	if len(args) == 0 {
		return failUsage("profile requires an instance or subcommand")
	}
	switch args[0] {
	case "-h", "--help":
		return topicHelp("profile")
	case "list", "--list":
		return runProfileList(args[1:])
	case "create":
		return runProfileCreate(args[1:])
	case "edit":
		return runProfileEdit(args[1:])
	case "enable":
		return runProfileSetEnabled(args[1:], true)
	case "disable":
		return runProfileSetEnabled(args[1:], false)
	case "clone":
		return runProfileClone(args[1:])
	case "rename":
		return runProfileRename(args[1:])
	case "remove":
		return runProfileRemove(args[1:])
	case "show":
		return runProfileShow(args[1:])
	case "get":
		return runProfileGet(args[1:])
	case "path":
		return runProfilePath(args[1:])
	case "init":
		return runProfileInit()
	}
	// `profile --describe NAME` (old spelling) and `profile describe NAME`
	// both resolve; a bare NAME launches.
	describe := false
	if args[0] == "--describe" || args[0] == "describe" {
		describe = true
		args = args[1:]
	}
	agentOpt := ""
	if describe {
		var err error
		agentOpt, args, err = extractAgentFlag(args)
		if err != nil {
			return failUsage("%v", err)
		}
	}
	if len(args) == 0 {
		return failUsage("profile requires an instance name")
	}
	paths := harnessDefaultPaths()
	resolution, code := resolveProfileQuery(args[0], agentOpt, paths)
	if code != 0 {
		return code
	}
	if resolution.Source == profiles.SourceDefault {
		return launchDefaultProfile(resolution, paths, args[1:], describe)
	}
	return launchResolvedProfile(resolution, paths, args[1:], describe)
}

func defaultBinaryLookup(paths harnessenv.Paths) func(string) string {
	return func(agent string) string {
		bin, err := agents.DefaultBinary(agent, paths)
		if err != nil {
			return ""
		}
		return bin
	}
}

func runProfilePath(args []string) int {
	agentOpt, pos, err := extractAgentFlag(args)
	if err != nil {
		return failUsage("%v", err)
	}
	paths := harnessDefaultPaths()
	if len(pos) == 0 {
		if agentOpt != "" {
			canon, ok := agents.Normalize(agentOpt)
			if !ok {
				return failUsage("unknown agent: %s", agentOpt)
			}
			fmt.Println(profiles.ProfileFilePath(paths, canon, profiles.DefaultProfileName))
			return 0
		}
		fmt.Println(profiles.ProfilesDir(paths))
		return 0
	}
	if len(pos) != 1 {
		return failUsage("path accepts at most one profile name")
	}
	if pos[0] == profiles.DefaultProfileName && agentOpt == "" {
		return failUsage("path default requires --agent <agent>")
	}
	res, code := resolveProfileQuery(pos[0], agentOpt, paths)
	if code != 0 {
		return code
	}
	fmt.Println(profiles.ProfileFilePath(paths, res.Profile.Agent, res.Name))
	return 0
}

func runProfileInit() int {
	paths := harnessDefaultPaths()
	store, err := profiles.Open(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if err := store.EnsureDefaults(defaultBinaryLookup(paths)); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Println(profiles.ProfilesDir(paths))
	return 0
}

// extractAgentFlag extracts an optional --agent <val> or --agent=<val> from
// args without consuming flags meant for the target agent.
func extractAgentFlag(args []string) (string, []string, error) {
	var agentVal string
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--agent":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--agent requires a value")
			}
			agentVal = args[i+1]
			i++
		case strings.HasPrefix(a, "--agent="):
			agentVal = strings.TrimPrefix(a, "--agent=")
		default:
			out = append(out, a)
		}
	}
	return agentVal, out, nil
}

func runProfileList(args []string) int {
	jsonOut := false
	agentFilter := ""
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
			i++
		case a == "--agent":
			if i+1 >= len(args) {
				return failUsage("list --agent requires a value")
			}
			agentFilter = args[i+1]
			i += 2
		case len(a) > len("--agent=") && a[:len("--agent=")] == "--agent=":
			agentFilter = a[len("--agent="):]
			i++
		default:
			return failUsage("unknown list flag: %s", a)
		}
	}
	var entries []profiles.ListEntry
	var err error
	if agentFilter != "" {
		canon, ok := agents.Normalize(agentFilter)
		if !ok {
			return failUsage("unknown agent: %s", agentFilter)
		}
		entries, err = profiles.ListForAgent(canon, harnessenv.DefaultPaths())
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
	} else {
		entries, err = profiles.ListAll(harnessenv.DefaultPaths())
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
	}
	if entries == nil {
		entries = []profiles.ListEntry{}
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		return 0
	}
	fmt.Print(profiles.FormatPlain(entries))
	return 0
}

func runProfileCreate(args []string) int {
	fs := flag.NewFlagSet("harness profile create", flag.ContinueOnError)
	agent := fs.String("agent", "", "canonical agent id (required)")
	displayName := fs.String("display-name", "", "human-readable name")
	shadowHome := fs.String("shadow-home", "", "shadow home path (default: preconfigured template)")
	seedFrom := fs.String("seed-from", "", "dotfiles subdirectory to seed the shadow home from")
	noSeed := fs.Bool("no-seed", false, "skip starting template / dotfiles seeding")
	useRouter := fs.Bool("router", false, "launch through the model router instead of the agent's own login/provider")
	noRouter := fs.Bool("no-router", false, "launch with the agent's own provider settings (default)")
	binaryPath := fs.String("binary", "", "explicit agent binary override")
	homePath := fs.String("home-path", "", "shared home for conversation/chat/MCP/skills overlay symlinks")
	useOverlay := fs.Bool("overlay", false, "enable shared MCPs/skills/chats overlay from default agent home (default)")
	noOverlay := fs.Bool("no-overlay", false, "disable shared MCPs/skills/chats overlay")
	var envs, argVals repeatedFlag
	fs.Var(&envs, "env", "extra environment KEY=VALUE (repeatable)")
	fs.Var(&argVals, "arg", "argument prepended to every launch (repeatable)")
	// Accept `create NAME --flag...` and `create --flag... NAME` alike:
	// Go's flag package stops at the first positional, so peel a leading
	// positional name off before parsing.
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if name == "" {
		if fs.NArg() != 1 {
			return failUsage("create requires exactly one profile name")
		}
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return failUsage("create requires exactly one profile name")
	}
	if *agent == "" {
		return failUsage("create requires --agent")
	}
	canonical, ok := agents.Normalize(*agent)
	if !ok {
		return failUsage("unknown agent: %s", *agent)
	}
	if *useRouter && *noRouter {
		return failUsage("create accepts only one of --router and --no-router")
	}
	if *useOverlay && *noOverlay {
		return failUsage("create accepts only one of --overlay and --no-overlay")
	}
	env := map[string]string{}
	for _, kv := range envs {
		key, value, found := strings.Cut(kv, "=")
		if !found || key == "" {
			return failUsage("--env expects KEY=VALUE, got %q", kv)
		}
		env[key] = value
	}
	paths := harnessenv.DefaultPaths()
	store, err := profiles.Open(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	binPath := *binaryPath
	if binPath == "" {
		if defBin, berr := agents.DefaultBinary(canonical, paths); berr == nil {
			binPath = defBin
		}
	}
	profile := profiles.Profile{
		Agent:       canonical,
		DisplayName: *displayName,
		Enabled:     true,
		ShadowHome:  *shadowHome,
		SeedFrom:    *seedFrom,
		Env:         env,
		PrependArgs: argVals,
		Router:      *useRouter,
		BinaryPath:  binPath,
		HomePath:    *homePath,
	}
	if *useOverlay {
		t := true
		profile.Overlay = &t
	} else if *noOverlay {
		f := false
		profile.Overlay = &f
	}
	if err := store.EnsureDefaults(defaultBinaryLookup(paths)); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	// Create saves the store atomically; seeding resolves the agent template
	// directory or dotfiles root (honoring HARNESS_DOTFILES) unless --no-seed.
	if err := store.Create(name, profile, profiles.CreateOptions{NoSeed: *noSeed}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if created, ok := store.Get(canonical + "/" + name); ok && created.Profile.ShadowHome != "" {
		fmt.Printf("created profile %s (agent %s, shadow home %s)\n", name, canonical, created.Profile.ShadowHome)
	} else {
		fmt.Printf("created profile %s (agent %s)\n", name, canonical)
	}
	refreshHarnessesCache()
	return 0
}

// refreshHarnessesCache rebuilds the Spaces plugin inventory after a
// mutation that changes the launchable set. Failures warn but never fail
// the command that triggered them.
func refreshHarnessesCache() {
	if err := spacelaunch.RefreshCache(); err != nil {
		fmt.Fprintf(os.Stderr, "harness: warning: %s\n", err)
	}
}

func runProfileRemove(args []string) int {
	agentOpt, args, err := extractAgentFlag(args)
	if err != nil {
		return failUsage("%v", err)
	}
	purge := false
	var name string
	for _, a := range args {
		switch a {
		case "--purge":
			purge = true
		default:
			if name != "" {
				return failUsage("remove takes exactly one profile name")
			}
			name = a
		}
	}
	if name == "" {
		return failUsage("remove requires a profile name")
	}
	ref, err := qualifyProfileName(name, agentOpt)
	if err != nil {
		return failUsage("%v", err)
	}
	paths := harnessenv.DefaultPaths()
	store, err := profiles.Open(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	// Remove saves the store atomically; --purge deletes the shadow home
	// when it lives under the profiles base directory.
	if err := store.Remove(ref, purge); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Printf("removed profile %s\n", name)
	refreshHarnessesCache()
	return 0
}

type profileLaunchSnapshot struct {
	Binary   string            `json:"binary"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	UnsetEnv []string          `json:"unsetEnv"`
	Command  string            `json:"command"`
}

type profileShowSnapshot struct {
	Name              string                 `json:"name"`
	Source            string                 `json:"source"`
	Agent             string                 `json:"agent"`
	Enabled           bool                   `json:"enabled"`
	Customized        bool                   `json:"customized,omitempty"`
	DisplayName       string                 `json:"displayName"`
	BinaryPath        string                 `json:"binaryPath"`
	EffectiveBinary   string                 `json:"effectiveBinary"`
	HomePath          string                 `json:"homePath"`
	EffectiveHomePath string                 `json:"effectiveHomePath"`
	ShadowHomePath    string                 `json:"shadowHomePath"`
	TemplateDir       string                 `json:"templateDir"`
	SeedFrom          string                 `json:"seedFrom"`
	Overlay           bool                   `json:"overlay"`
	Router            bool                   `json:"router"`
	AuthStorePath     string                 `json:"authStorePath,omitempty"`
	AuthStorePresent  bool                   `json:"authStorePresent,omitempty"`
	Env               map[string]string      `json:"env"`
	PrependArgs       []string               `json:"prependArgs"`
	ConfigFile        string                 `json:"configFile"`
	EffectiveLaunch   *profileLaunchSnapshot `json:"effectiveLaunch,omitempty"`
}

func buildProfileSnapshot(resolution profiles.Resolution, paths harnessenv.Paths, reveal bool) profileShowSnapshot {
	p := resolution.Profile
	effBin := p.BinaryPath
	if effBin == "" {
		if defBin, err := agents.DefaultBinary(p.Agent, paths); err == nil {
			effBin = defBin
		}
	}
	effHome := p.HomePath
	if effHome == "" {
		effHome = profiles.DefaultSharedConfigHome(p.Agent, paths)
	}
	overlay := p.Overlay == nil || *p.Overlay
	envCopy := map[string]string{}
	for k, v := range p.Env {
		if !reveal && isSecretEnvKey(k) {
			v = "***"
		}
		envCopy[k] = v
	}
	argsCopy := append([]string{}, p.PrependArgs...)
	if argsCopy == nil {
		argsCopy = []string{}
	}
	snap := profileShowSnapshot{
		Name:              resolution.Name,
		Source:            resolution.Source,
		Agent:             p.Agent,
		Enabled:           p.Enabled,
		Customized:        resolution.Customized,
		DisplayName:       p.DisplayName,
		BinaryPath:        p.BinaryPath,
		EffectiveBinary:   effBin,
		HomePath:          p.HomePath,
		EffectiveHomePath: effHome,
		ShadowHomePath:    p.ShadowHome,
		TemplateDir:       profiles.DefaultTemplateDir(paths, p.Agent),
		SeedFrom:          p.SeedFrom,
		Overlay:           overlay,
		Router:            p.Router,
		Env:               envCopy,
		PrependArgs:       argsCopy,
		ConfigFile:        profiles.ProfileFilePath(paths, p.Agent, resolution.Name),
	}
	if authPath, present, ok := profiles.AuthStoreStatus(p, paths); ok {
		snap.AuthStorePath = authPath
		snap.AuthStorePresent = present
	}
	spec := profiles.LaunchSpecFromPaths(resolution, paths)
	router := harnessenv.RouterEnv(paths)
	var resolved agents.Resolved
	var rerr error
	if resolution.Source == profiles.SourceDefault || resolution.NativeAntigravity {
		resolved, rerr = agents.Resolve(spec, paths, router, nil)
	} else {
		resolved, rerr = agents.ResolveProfile(spec, paths, router, nil)
	}
	if rerr == nil {
		launchEnv := map[string]string{}
		for _, kv := range resolved.Env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if !reveal && isSecretEnvKey(k) {
				v = "***"
			}
			launchEnv[k] = v
		}
		launchArgs := append([]string{}, resolved.Args...)
		if launchArgs == nil {
			launchArgs = []string{}
		}
		unsetEnv := append([]string{}, resolved.UnsetEnv...)
		if unsetEnv == nil {
			unsetEnv = []string{}
		}
		snap.EffectiveLaunch = &profileLaunchSnapshot{
			Binary:   resolved.Binary,
			Args:     launchArgs,
			Env:      launchEnv,
			UnsetEnv: unsetEnv,
			Command:  agents.Describe(resolved),
		}
	}
	return snap
}

func qualifyProfileName(name, agentOpt string) (string, error) {
	if agent, shortName, qualified := strings.Cut(name, "/"); qualified {
		canon, ok := agents.Normalize(agent)
		if !ok {
			return "", fmt.Errorf("unknown agent: %s", agent)
		}
		if agentOpt != "" {
			requested, ok := agents.Normalize(agentOpt)
			if !ok || requested != canon {
				return "", fmt.Errorf("--agent %q conflicts with profile %q", agentOpt, name)
			}
		}
		return canon + "/" + shortName, nil
	}
	if agentOpt != "" {
		canon, ok := agents.Normalize(agentOpt)
		if !ok {
			return "", fmt.Errorf("unknown agent: %s", agentOpt)
		}
		return canon + "/" + name, nil
	}
	return name, nil
}

func resolveProfileQuery(name, agentOpt string, paths harnessenv.Paths) (profiles.Resolution, int) {
	ref, err := qualifyProfileName(name, agentOpt)
	if err != nil {
		return profiles.Resolution{}, failUsage("%v", err)
	}
	res, err := profiles.Resolve(ref, paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return profiles.Resolution{}, 2
	}
	return res, 0
}

func runProfileShow(args []string) int {
	jsonOut := false
	reveal := false
	agentOpt := ""
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--reveal":
			reveal = true
		case a == "--agent":
			if i+1 >= len(args) {
				return failUsage("show --agent requires a value")
			}
			agentOpt = args[i+1]
			i++
		case strings.HasPrefix(a, "--agent="):
			agentOpt = strings.TrimPrefix(a, "--agent=")
		case strings.HasPrefix(a, "-"):
			return failUsage("unknown show flag: %s", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		return failUsage("show requires exactly one profile name")
	}
	paths := harnessDefaultPaths()
	if pos[0] == profiles.DefaultProfileName && agentOpt == "" {
		fmt.Printf("name:         %s\n", profiles.DefaultProfileName)
		fmt.Printf("source:       %s\n", profiles.SourceDefault)
		fmt.Printf("profiles dir: %s\n", profiles.ProfilesDir(paths))
		fmt.Printf("note:         pass --agent <agent> to inspect $HOME/.model-router/profiles/<agent>/default/profile.json\n")
		return 0
	}
	resolution, code := resolveProfileQuery(pos[0], agentOpt, paths)
	if code != 0 {
		return code
	}
	snap := buildProfileSnapshot(resolution, paths, reveal)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(snap); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		return 0
	}
	fmt.Printf("name:         %s\n", snap.Name)
	fmt.Printf("source:       %s\n", snap.Source)
	fmt.Printf("agent:        %s\n", snap.Agent)
	fmt.Printf("enabled:      %t\n", snap.Enabled)
	if snap.DisplayName != "" {
		fmt.Printf("display name: %s\n", snap.DisplayName)
	}
	if snap.BinaryPath != "" {
		fmt.Printf("binary:       %s\n", snap.BinaryPath)
	} else if snap.EffectiveBinary != "" {
		fmt.Printf("binary:       %s (default)\n", snap.EffectiveBinary)
	}
	if snap.HomePath != "" {
		fmt.Printf("home path:    %s\n", snap.HomePath)
	} else if snap.EffectiveHomePath != "" {
		fmt.Printf("home path:    %s (default)\n", snap.EffectiveHomePath)
	}
	if snap.ShadowHomePath != "" {
		fmt.Printf("shadow home:  %s\n", snap.ShadowHomePath)
	}
	fmt.Printf("template dir: %s\n", snap.TemplateDir)
	if profiles.SupportsOverlay(snap.Agent) {
		fmt.Printf("overlay:      %t\n", snap.Overlay)
		if snap.Overlay && snap.EffectiveHomePath != "" {
			fmt.Printf("shared home:  %s\n", snap.EffectiveHomePath)
		}
	}
	if snap.AuthStorePath != "" {
		state := "not logged in"
		if snap.AuthStorePresent {
			state = "present"
		}
		fmt.Printf("auth store:   %s (%s)\n", snap.AuthStorePath, state)
	}
	fmt.Printf("router:       %t\n", snap.Router)
	if len(snap.PrependArgs) > 0 {
		fmt.Printf("args:         %s\n", strings.Join(snap.PrependArgs, " "))
	}
	if len(snap.Env) > 0 {
		fmt.Printf("env:\n")
		keys := make([]string, 0, len(snap.Env))
		for k := range snap.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s=%s\n", k, snap.Env[k])
		}
	}
	fmt.Printf("config file:  %s\n", snap.ConfigFile)
	if resolution.NativeAntigravity {
		fmt.Printf("credential:    native antigravity\n")
	}
	return 0
}

func runProfileGet(args []string) int {
	reveal := false
	agentOpt := ""
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--reveal":
			reveal = true
		case a == "--agent":
			if i+1 >= len(args) {
				return failUsage("get --agent requires a value")
			}
			agentOpt = args[i+1]
			i++
		case strings.HasPrefix(a, "--agent="):
			agentOpt = strings.TrimPrefix(a, "--agent=")
		case strings.HasPrefix(a, "-"):
			return failUsage("unknown get flag: %s", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return failUsage("get requires <profile> <field> [--agent <agent>]")
	}
	name, field := pos[0], pos[1]
	paths := harnessDefaultPaths()
	if name == profiles.DefaultProfileName && agentOpt == "" {
		return failUsage("get default requires --agent <agent>")
	}
	resolution, code := resolveProfileQuery(name, agentOpt, paths)
	if code != 0 {
		return code
	}
	snap := buildProfileSnapshot(resolution, paths, reveal)
	if strings.HasPrefix(field, "env.") {
		key := strings.TrimPrefix(field, "env.")
		v, ok := snap.Env[key]
		if !ok {
			return 1
		}
		fmt.Println(v)
		return 0
	}
	switch strings.ToLower(field) {
	case "name":
		fmt.Println(snap.Name)
	case "agent":
		fmt.Println(snap.Agent)
	case "source":
		fmt.Println(snap.Source)
	case "enabled":
		fmt.Printf("%t\n", snap.Enabled)
	case "customized":
		fmt.Printf("%t\n", snap.Customized)
	case "display-name", "displayname":
		fmt.Println(snap.DisplayName)
	case "binary", "binary-path", "binarypath":
		fmt.Println(snap.EffectiveBinary)
	case "home", "home-path", "homepath", "central-home":
		fmt.Println(snap.EffectiveHomePath)
	case "shadow", "shadow-home", "shadowhome", "shadowhomepath":
		fmt.Println(snap.ShadowHomePath)
	case "template", "template-dir", "templatedir":
		fmt.Println(snap.TemplateDir)
	case "seed", "seed-from", "seedfrom":
		fmt.Println(snap.SeedFrom)
	case "overlay":
		fmt.Printf("%t\n", snap.Overlay)
	case "router":
		fmt.Printf("%t\n", snap.Router)
	case "auth-store", "authstore":
		fmt.Println(snap.AuthStorePath)
	case "config-file", "configfile", "config":
		fmt.Println(snap.ConfigFile)
	case "args", "prepend-args", "prependargs":
		for _, a := range snap.PrependArgs {
			fmt.Println(a)
		}
	case "env":
		keys := make([]string, 0, len(snap.Env))
		for k := range snap.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s=%s\n", k, snap.Env[k])
		}
	case "launch-args", "launchargs":
		if snap.EffectiveLaunch != nil {
			for _, a := range snap.EffectiveLaunch.Args {
				fmt.Println(a)
			}
		}
	case "launch-env", "launchenv":
		if snap.EffectiveLaunch != nil {
			keys := make([]string, 0, len(snap.EffectiveLaunch.Env))
			for k := range snap.EffectiveLaunch.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("%s=%s\n", k, snap.EffectiveLaunch.Env[k])
			}
		}
	case "launch-cmd", "launchcmd", "command":
		if snap.EffectiveLaunch != nil {
			fmt.Println(snap.EffectiveLaunch.Command)
		}
	default:
		return failUsage("unknown profile field: %s", field)
	}
	return 0
}

// isSecretEnvKey reports whether an env key looks like a credential that
// must be masked in `profile show` output.
func isSecretEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, sub := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "AUTH", "CREDENTIAL"} {
		if strings.Contains(upper, sub) {
			return true
		}
	}
	return false
}

func runProfileEdit(args []string) int {
	fs := flag.NewFlagSet("harness profile edit", flag.ContinueOnError)
	agent := fs.String("agent", "", "agent that owns the profile")
	displayName := fs.String("display-name", "", "human-readable name")
	binaryPath := fs.String("binary", "", "explicit agent binary override")
	homePath := fs.String("home-path", "", "shared home for conversation/chat overlay symlinks")
	shadowHome := fs.String("shadow-home", "", "shadow home path")
	noShadowHome := fs.Bool("no-shadow-home", false, "clear the shadow home")
	seedFrom := fs.String("seed-from", "", "dotfiles subdirectory to seed the shadow home from")
	_ = fs.Bool("router", false, "launch through the model router")
	_ = fs.Bool("no-router", false, "launch with the agent's own provider settings")
	_ = fs.Bool("overlay", false, "enable shared-conversation / isolated-auth overlay")
	_ = fs.Bool("no-overlay", false, "disable shared-conversation / isolated-auth overlay")
	clearArgs := fs.Bool("clear-args", false, "clear prepended args (combined with --arg to replace)")
	var envs, unsetEnvs, argVals repeatedFlag
	fs.Var(&envs, "env", "extra environment KEY=VALUE (repeatable)")
	fs.Var(&unsetEnvs, "unset-env", "remove environment KEY (repeatable)")
	fs.Var(&argVals, "arg", "argument prepended to every launch (repeatable)")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if name == "" {
		if fs.NArg() != 1 {
			return failUsage("edit requires exactly one profile name")
		}
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return failUsage("edit requires exactly one profile name")
	}
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if setFlags["router"] && setFlags["no-router"] {
		return failUsage("edit accepts only one of --router and --no-router")
	}
	if setFlags["overlay"] && setFlags["no-overlay"] {
		return failUsage("edit accepts only one of --overlay and --no-overlay")
	}
	if setFlags["shadow-home"] && *noShadowHome {
		return failUsage("edit accepts only one of --shadow-home and --no-shadow-home")
	}
	var patch profiles.EditPatch
	if setFlags["display-name"] {
		v := *displayName
		patch.DisplayName = &v
	}
	if setFlags["binary"] {
		v := *binaryPath
		patch.BinaryPath = &v
	}
	if setFlags["home-path"] {
		v := *homePath
		patch.HomePath = &v
	}
	if *noShadowHome {
		patch.ClearShadowHome = true
	} else if setFlags["shadow-home"] {
		v := *shadowHome
		patch.ShadowHome = &v
	}
	if setFlags["seed-from"] {
		v := *seedFrom
		patch.SeedFrom = &v
	}
	if len(envs) > 0 {
		m := map[string]string{}
		for _, kv := range envs {
			key, value, found := strings.Cut(kv, "=")
			if !found || key == "" {
				return failUsage("--env expects KEY=VALUE, got %q", kv)
			}
			m[key] = value
		}
		patch.SetEnv = m
	}
	if len(unsetEnvs) > 0 {
		patch.UnsetEnv = append([]string{}, unsetEnvs...)
	}
	if *clearArgs && len(argVals) > 0 {
		patch.SetPrependArgs = true
		patch.PrependArgs = append([]string{}, argVals...)
	} else if *clearArgs {
		patch.ClearPrependArgs = true
	} else if len(argVals) > 0 {
		patch.PrependArgs = append([]string{}, argVals...)
	}
	if setFlags["router"] {
		v := true
		patch.Router = &v
	} else if setFlags["no-router"] {
		v := false
		patch.Router = &v
	}
	if setFlags["overlay"] {
		v := true
		patch.Overlay = &v
	} else if setFlags["no-overlay"] {
		v := false
		patch.Overlay = &v
	}
	ref, err := qualifyProfileName(name, *agent)
	if err != nil {
		return failUsage("%v", err)
	}
	paths := harnessenv.DefaultPaths()
	store, err := profiles.Open(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if err := store.Edit(ref, patch); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Printf("edited profile %s\n", name)
	refreshHarnessesCache()
	return 0
}

func runProfileSetEnabled(args []string, enabled bool) int {
	action := "disable"
	if enabled {
		action = "enable"
	}
	agentOpt, args, err := extractAgentFlag(args)
	if err != nil {
		return failUsage("%v", err)
	}
	if len(args) != 1 {
		return failUsage("%s requires exactly one profile name", action)
	}
	ref, err := qualifyProfileName(args[0], agentOpt)
	if err != nil {
		return failUsage("%v", err)
	}
	store, err := profiles.Open(harnessenv.DefaultPaths())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if err := store.SetEnabled(ref, enabled); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Printf("%sd profile %s\n", action, ref)
	refreshHarnessesCache()
	return 0
}

func runProfileClone(args []string) int {
	agentOpt, args, err := extractAgentFlag(args)
	if err != nil {
		return failUsage("%v", err)
	}
	noSeed := false
	var positional []string
	for _, a := range args {
		switch a {
		case "--no-seed":
			noSeed = true
		case "-h", "--help":
			return topicHelp("profile")
		default:
			if strings.HasPrefix(a, "-") {
				return failUsage("unknown clone flag: %s", a)
			}
			positional = append(positional, a)
		}
	}
	if len(positional) != 2 {
		return failUsage("clone requires exactly two profile names")
	}
	ref, err := qualifyProfileName(positional[0], agentOpt)
	if err != nil {
		return failUsage("%v", err)
	}
	paths := harnessenv.DefaultPaths()
	store, err := profiles.Open(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if err := store.Clone(ref, positional[1], profiles.CreateOptions{NoSeed: noSeed}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Printf("cloned profile %s to %s\n", positional[0], positional[1])
	refreshHarnessesCache()
	return 0
}

func runProfileRename(args []string) int {
	agentOpt, args, err := extractAgentFlag(args)
	if err != nil {
		return failUsage("%v", err)
	}
	if len(args) != 2 {
		return failUsage("rename requires exactly two profile names")
	}
	ref, err := qualifyProfileName(args[0], agentOpt)
	if err != nil {
		return failUsage("%v", err)
	}
	paths := harnessenv.DefaultPaths()
	store, err := profiles.Open(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if err := store.Rename(ref, args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Printf("renamed profile %s to %s\n", args[0], args[1])
	refreshHarnessesCache()
	return 0
}

// --- router ------------------------------------------------------------------

func routerOptions() (routerctl.Options, harnessenv.Paths, harnessenv.RouterSettings) {
	paths := harnessenv.DefaultPaths()
	router := harnessenv.RouterEnv(paths)
	return routerctl.Detect(paths, router), paths, router
}

func runRouter(args []string) int {
	if len(args) == 0 {
		return failUsage("router requires a subcommand")
	}
	ctx := context.Background()
	opts, _, router := routerOptions()
	switch args[0] {
	case "start":
		fs := flag.NewFlagSet("harness router start", flag.ContinueOnError)
		config := fs.String("config", "", "router config path override")
		foreground := fs.Bool("foreground", false, "run in the foreground instead of daemonizing")
		wait := fs.Duration("wait", 30*time.Second, "how long to wait for readiness")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *config != "" {
			opts.RouterConfig = *config
		}
		if *foreground {
			if opts.RouterBin == "" {
				fmt.Fprintf(os.Stderr, "%s: no model-router binary found\n", progName)
				return 1
			}
			argv := []string{opts.RouterBin}
			if opts.RouterConfig != "" {
				argv = append(argv, "--config", opts.RouterConfig)
			}
			fmt.Fprintf(os.Stderr, "%s: foreground start: %s\n", progName, strings.Join(argv, " "))
			// The child inherits stdio; exec replaces this process.
			return execForeground(argv)
		}
		if err := routerctl.Start(ctx, opts, *wait); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		fmt.Printf("router started at %s\n", router.URL)
		return 0
	case "stop":
		if len(args) > 1 {
			return failUsage("stop takes no arguments")
		}
		if err := routerctl.Stop(ctx, opts, 15*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		fmt.Println("router stopped")
		return 0
	case "restart":
		fs := flag.NewFlagSet("harness router restart", flag.ContinueOnError)
		wait := fs.Duration("wait", 30*time.Second, "how long to wait for readiness")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if err := routerctl.Restart(ctx, opts, *wait); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		fmt.Printf("router restarted at %s\n", router.URL)
		return 0
	case "status":
		jsonOut := false
		for _, a := range args[1:] {
			if a != "--json" {
				return failUsage("unknown status flag: %s", a)
			}
			jsonOut = true
		}
		status, err := routerctl.FetchStatus(ctx, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(status); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
				return 1
			}
			return 0
		}
		printRouterStatus(status)
		if !status.Livez {
			return 1
		}
		return 0
	case "models":
		jsonOut := false
		for _, a := range args[1:] {
			if a != "--json" {
				return failUsage("unknown models flag: %s", a)
			}
			jsonOut = true
		}
		models, err := routerctl.ListModels(ctx, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		if jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(models); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
				return 1
			}
			return 0
		}
		for _, m := range models {
			fmt.Println(m.ID)
		}
		return 0
	case "validate":
		fs := flag.NewFlagSet("harness router validate", flag.ContinueOnError)
		config := fs.String("config", "", "router config path (default: detected)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		path := *config
		if path == "" {
			path = opts.RouterConfig
		}
		if path == "" {
			return failUsage("no router config found; pass --config PATH")
		}
		if err := routerctl.ValidateConfig(path); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 2
		}
		fmt.Printf("configuration valid: %s\n", path)
		return 0
	case "logs":
		fs := flag.NewFlagSet("harness router logs", flag.ContinueOnError)
		follow := fs.Bool("follow", false, "keep streaming new log lines")
		lines := fs.Int("n", 50, "number of trailing lines")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if err := routerctl.Logs(ctx, opts, *follow, *lines); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		return 0
	default:
		return failUsage("unknown router subcommand: %s", args[0])
	}
}

func printRouterStatus(status routerctl.Status) {
	fmt.Printf("url:      %s\n", status.URL)
	fmt.Printf("managed:  %s\n", status.Managed)
	fmt.Printf("live:     %t\n", status.Livez)
	fmt.Printf("ready:    %t\n", status.Ready)
	if status.Version != "" {
		fmt.Printf("version:  %s\n", status.Version)
	}
	fmt.Printf("models:   %d\n", status.Models)
	if status.DiagnosticsNote != "" {
		fmt.Printf("diagnostics: %s\n", status.DiagnosticsNote)
	}
}

func execForeground(argv []string) int {
	for i := range argv {
		if argv[i] == "--config" && i+1 < len(argv) {
			// keep as-is; nothing to do beyond preserving stdio
			_ = argv[i+1]
		}
	}
	attr := &os.ProcAttr{Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}}
	process, err := os.StartProcess(argv[0], argv, attr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	state, err := process.Wait()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	return 1
}

// --- agents / status ---------------------------------------------------------

func runAgents(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a != "--json" {
			return failUsage("unknown agents flag: %s", a)
		}
		jsonOut = true
	}
	infos := agents.Infos(harnessenv.DefaultPaths())
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(infos); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		return 0
	}
	for _, info := range infos {
		state := info.BinaryState
		if state == "" {
			state = "?"
		}
		fmt.Printf("%-18s %-8s %s\n", info.Name, state, info.Binary)
	}
	return 0
}

type combinedStatus struct {
	Router   routerctl.Status     `json:"router"`
	Profiles []profiles.ListEntry `json:"profiles"`
	Agents   []agents.AgentInfo   `json:"agents"`
}

func runStatus(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a != "--json" {
			return failUsage("unknown status flag: %s", a)
		}
		jsonOut = true
	}
	ctx := context.Background()
	opts, _, _ := routerOptions()
	status, err := routerctl.FetchStatus(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	entries, err := profiles.ListAll(harnessenv.DefaultPaths())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	infos := agents.Infos(harnessenv.DefaultPaths())
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(combinedStatus{Router: status, Profiles: entries, Agents: infos}); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		return 0
	}
	printRouterStatus(status)
	installed := 0
	for _, info := range infos {
		if info.BinaryState == "ok" {
			installed++
		}
	}
	fmt.Printf("agents:   %d/%d installed\n", installed, len(infos))
	fmt.Printf("profiles: %d\n", len(entries))
	if !status.Livez {
		return 1
	}
	return 0
}

// --- spaces (Spaces hooks, resume, and launcher helper surface) -------------

// runSpaces dispatches the Spaces helper surface: the `spaces` entry name and
// `harness spaces ...` both route here.
func runSpaces(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: spaces <hook|session-hook|hooks|hook-server|resume-command|__space-hooks|__space-harnesses|__shim-profile|__resume-wrap> [args...]\n")
		return 2
	}
	bindSpacesSeams()
	switch args[0] {
	case "__space-harnesses", "__space-launch-agent", "__shim-profile":
		return spacelaunch.Run(args)
	}
	return runSpacesResume(args)
}

// bindSpacesSeams wires the spaceresume seam variables to the real spacecore
// implementations.
func bindSpacesSeams() {
	spaceresume.HookDisabled = spacecore.Disabled
	spaceresume.NormalizeEvent = spacecore.NormalizeEvent
	spaceresume.ResolvePaneEvent = spacecore.ResolvePaneEvent
	spaceresume.Publish = spacecore.Publish
	spaceresume.AppendSpool = spacecore.AppendSpool
	spaceresume.EnsureServer = spacecore.EnsureServer
	spaceresume.SpaceHooksSummary = spacecore.SpaceHooksSummary
	spaceresume.SpaceHooksDetail = spacecore.SpaceHooksDetail
	spaceresume.RecordOutcome = spacecore.RecordOutcome
	spaceresume.RemoveOutcome = spacecore.RemoveOutcome
	spaceresume.PaneState = spacecore.PaneState
	spacecore.SpaceHookTitleResolver = spaceresume.DiscoverTitle
	spacecore.SpaceHookProfileResolver = spaceresume.DiscoverProfile
	spaceresume.HooksStatus = spacecore.HooksStatus
	spaceresume.HooksStop = spacecore.HooksStop
	spaceresume.HooksStart = spacecore.HooksStart
	spaceresume.Daemonize = spacecore.Daemonize
	spaceresume.Serve = func(foreground bool, tcpPort *int) int {
		port := 0
		if tcpPort != nil {
			port = *tcpPort
		}
		return spacecore.Serve(foreground, port)
	}
	spaceresume.RecordConversationOverlay = recordConversationOverlay
	spaceresume.SpaceConversationsFunc = runSpaceConversations
}

// runSpacesResume routes the hook and resume commands to spaceresume.
func runSpacesResume(args []string) int {
	command := args[0]
	rest := args[1:]
	second := func() string {
		if len(rest) > 1 {
			return rest[1]
		}
		return ""
	}
	switch command {
	case "hook":
		if len(rest) == 0 {
			fmt.Fprintf(os.Stderr, "%s: hook needs an agent\n", progName)
			return 1
		}
		return spaceresume.Hook(rest[0], second())
	case "__hook":
		// The post_command_discovery_hook form takes no arguments.
		return spaceresume.Hook("", "")
	case "session-hook":
		if len(rest) == 0 {
			fmt.Fprintf(os.Stderr, "%s: session-hook needs an agent\n", progName)
			return 1
		}
		return spaceresume.SessionHook(rest[0], second())
	case "hooks":
		return spaceresume.Hooks(rest)
	case "hook-server":
		return spaceresume.HookServer(rest)
	case "__space-hooks":
		return spaceresume.SpaceHooks(rest)
	case "__space-conversations":
		return spaceresume.SpaceConversations(rest)
	case "__space-agents":
		return spaceresume.SpaceAgents()
	case "__space-snapshot":
		return spaceresume.SpaceSnapshot()
	case "__resume-wrap":
		if len(rest) == 0 {
			fmt.Fprintf(os.Stderr, "%s: __resume-wrap needs an encoded command\n", progName)
			return 1
		}
		return spaceresume.Wrap(rest[0], second())
	case "__prepare-restore":
		if len(rest) == 0 {
			fmt.Fprintf(os.Stderr, "%s: __prepare-restore needs a session name\n", progName)
			return 1
		}
		return spaceresume.PrepareRestore(rest[0])
	case "resume-command":
		return spaceresume.ResumeCommand(rest)
	}
	fmt.Fprintf(os.Stderr, "%s: unhandled zj command %s\n", progName, command)
	return 1
}

// repeatedFlag collects repeatable string flag values.

type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}
