// Command router: CLI for managing and inspecting the model-router daemon.
//
// Usage:
//
//	router start [--config PATH] [--foreground] [--wait DURATION]
//	router stop
//	router restart [--wait DURATION]
//	router status [--json]
//	router models [--json] [--pick]
//	router validate [--config PATH]
//	router logs [--follow] [-n LINES]
//	router usage [--days N] [--by day|model] [--json] [PATH]
//	router version | help [topic]
//
// Exit codes: 0 success, 1 runtime failure, 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arhammahajan/model-router/internal/harness/harnessenv"
	"github.com/arhammahajan/model-router/internal/harness/modelpick"
	"github.com/arhammahajan/model-router/internal/harness/routerctl"
	usageagg "github.com/arhammahajan/model-router/internal/harness/usage"
	"github.com/arhammahajan/model-router/internal/server"
)

const progName = "router"

func main() {
	os.Exit(run(os.Args[1:]))
}

// Seams for unit tests so tests never touch the real home directory,
// systemd, or live router processes unless explicitly testing them.
var (
	routerDefaultPaths    = harnessenv.DefaultPaths
	routerEnv             = harnessenv.RouterEnv
	routerDetect          = routerctl.Detect
	routerStart           = routerctl.Start
	routerStop            = routerctl.Stop
	routerRestart         = routerctl.Restart
	routerFetchStatus     = routerctl.FetchStatus
	routerListModels      = routerctl.ListModels
	routerValidateConfig  = routerctl.ValidateConfig
	routerLogs            = routerctl.Logs
	routerExecForeground  = execForeground
	routerFetchPickModels = modelpick.Fetch
	routerPickModel       = modelpick.Pick
)

func usage(w *os.File) {
	fmt.Fprintf(w, `Usage: router <command> [options]

Commands:
  start [--config PATH] [--foreground] [--wait DURATION]   start the router daemon
  stop                                                     stop the router daemon
  restart [--wait DURATION]                                restart the router daemon
  status [--json]                                          show daemon and endpoint status
  models [--json] [--pick]                                 list or interactively pick models
  validate [--config PATH]                                 validate router configuration offline
  logs [--follow] [-n LINES]                               show router logs
  usage [--days N] [--by day|model] [--json] [PATH]        summarize accounting log usage
  version                                                  print version and exit
  help [topic]                                             show help for a command

Environment:
  MODEL_ROUTER_URL       router base URL (default: http://127.0.0.1:4000)
  MODEL_ROUTER_API_KEY   router bearer token
  MODEL_ROUTER_BIN       router binary path override (also ROUTER_BIN, HARNESS_ROUTER_BIN)
  MODEL_ROUTER_CONFIG    router config path override (also ROUTER_CONFIG, HARNESS_ROUTER_CONFIG)
`)
}

func failUsage(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "%s: %s\n", progName, fmt.Sprintf(format, args...))
	usage(os.Stderr)
	return 2
}

func run(args []string) int {
	if len(args) == 0 {
		return failUsage("choose a command")
	}
	switch args[0] {
	case "-h", "--help":
		usage(os.Stdout)
		return 0
	case "version", "--version", "-v":
		fmt.Println(progName, server.BuildVersion)
		return 0
	case "help":
		if len(args) > 1 {
			return topicHelp(args[1])
		}
		usage(os.Stdout)
		return 0
	case "start":
		return runStart(args[1:])
	case "stop":
		return runStop(args[1:])
	case "restart":
		return runRestart(args[1:])
	case "status":
		return runStatus(args[1:])
	case "models":
		return runModels(args[1:])
	case "validate":
		return runValidate(args[1:])
	case "logs":
		return runLogs(args[1:])
	case "usage":
		return runUsage(args[1:])
	default:
		return failUsage("unknown command: %s", args[0])
	}
}

func topicHelp(topic string) int {
	switch topic {
	case "start":
		fmt.Println("router start [--config PATH] [--foreground] [--wait DURATION]")
	case "stop":
		fmt.Println("router stop")
	case "restart":
		fmt.Println("router restart [--wait DURATION]")
	case "status":
		fmt.Println("router status [--json]")
	case "models":
		fmt.Println("router models [--json] [--pick]")
	case "validate":
		fmt.Println("router validate [--config PATH]")
	case "logs":
		fmt.Println("router logs [--follow] [-n LINES]")
	case "usage":
		fmt.Println("router usage [--days N] [--by day|model] [--json] [PATH]")
	default:
		return failUsage("unknown help topic: %s", topic)
	}
	return 0
}

func routerOptions() (routerctl.Options, harnessenv.Paths, harnessenv.RouterSettings) {
	paths := routerDefaultPaths()
	router := routerEnv(paths)
	return routerDetect(paths, router), paths, router
}

func runStart(args []string) int {
	fs := flag.NewFlagSet("router start", flag.ContinueOnError)
	config := fs.String("config", "", "router config path override")
	foreground := fs.Bool("foreground", false, "run in the foreground instead of daemonizing")
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for readiness")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("start takes no positional arguments")
	}
	opts, _, router := routerOptions()
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
		return routerExecForeground(argv)
	}
	if err := routerStart(context.Background(), opts, *wait); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Printf("router started at %s\n", router.URL)
	return 0
}

func runStop(args []string) int {
	if len(args) > 0 {
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
			return topicHelp("stop")
		}
		return failUsage("stop takes no arguments")
	}
	opts, _, _ := routerOptions()
	if err := routerStop(context.Background(), opts, 15*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Println("router stopped")
	return 0
}

func runRestart(args []string) int {
	fs := flag.NewFlagSet("router restart", flag.ContinueOnError)
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for readiness")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("restart takes no positional arguments")
	}
	opts, _, router := routerOptions()
	if err := routerRestart(context.Background(), opts, *wait); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	fmt.Printf("router restarted at %s\n", router.URL)
	return 0
}

func runStatus(args []string) int {
	jsonOut := false
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		case "-h", "--help":
			return topicHelp("status")
		default:
			return failUsage("unknown status flag: %s", a)
		}
	}
	opts, _, _ := routerOptions()
	status, err := routerFetchStatus(context.Background(), opts)
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
}

func runModels(args []string) int {
	jsonOut := false
	pick := false
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		case "--pick":
			pick = true
		case "-h", "--help":
			return topicHelp("models")
		default:
			return failUsage("unknown models flag: %s", a)
		}
	}
	if jsonOut && pick {
		return failUsage("--json and --pick are mutually exclusive")
	}
	opts, _, _ := routerOptions()
	if pick {
		models, err := routerFetchPickModels(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		if len(models) == 0 {
			fmt.Fprintf(os.Stderr, "%s: the router serves no models\n", progName)
			return 1
		}
		if picked, ok := routerPickModel(models); ok {
			fmt.Println(picked.ID)
			return 0
		}
		fmt.Print(modelpick.Render(models))
		return 0
	}
	models, err := routerListModels(context.Background(), opts)
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
}

func runValidate(args []string) int {
	fs := flag.NewFlagSet("router validate", flag.ContinueOnError)
	config := fs.String("config", "", "router config path (default: detected)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("validate takes no positional arguments")
	}
	opts, _, _ := routerOptions()
	path := *config
	if path == "" {
		path = opts.RouterConfig
	}
	if path == "" {
		return failUsage("no router config found; pass --config PATH")
	}
	if err := routerValidateConfig(path); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 2
	}
	fmt.Printf("configuration valid: %s\n", path)
	return 0
}

func runLogs(args []string) int {
	fs := flag.NewFlagSet("router logs", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "keep streaming new log lines")
	lines := fs.Int("n", 50, "number of trailing lines")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("logs takes no positional arguments")
	}
	opts, _, _ := routerOptions()
	if err := routerLogs(context.Background(), opts, *follow, *lines); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	return 0
}

func defaultUsagePath(paths harnessenv.Paths) string {
	return filepath.Join(paths.StateHome, "model-router", "accounting.jsonl")
}

func runUsage(args []string) int {
	fs := flag.NewFlagSet("router usage", flag.ContinueOnError)
	days := fs.Int("days", 0, "limit to the last N days (0 = all)")
	by := fs.String("by", "day", "grouping: day or model")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *by != "day" && *by != "model" {
		return failUsage("unknown --by grouping: %s", *by)
	}
	path := fs.Arg(0)
	if path == "" {
		path = defaultUsagePath(routerDefaultPaths())
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
