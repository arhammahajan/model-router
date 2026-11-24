package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/arhammahajan/router/internal/routercli/routerctl"
)

// Seams so proxy tests never touch the network or the real config.
var (
	routerFetchProxyStatus  = routerctl.FetchProxyStatus
	routerListProxyModels   = routerctl.ListProxyModels
	routerListProxyPlugins  = routerctl.ListProxyPlugins
	routerSetProxyPlugin    = routerctl.SetProxyPluginEnabled
	routerListProxyAccounts = routerctl.ListProxyAccounts
)

func runProxy(args []string) int {
	if len(args) == 0 {
		return proxyHelp()
	}
	switch args[0] {
	case "-h", "--help":
		return proxyHelp()
	case "status":
		return runProxyStatus(args[1:])
	case "models":
		return runProxyModels(args[1:])
	case "plugins":
		return runProxyPlugins(args[1:])
	case "plugin":
		return runProxyPlugin(args[1:])
	case "accounts":
		return runProxyAccounts(args[1:])
	default:
		return failUsage("unknown proxy command: %s", args[0])
	}
}

func proxyHelp() int {
	fmt.Println(`router proxy <command> [options]

Inspect the CLIProxyAPI proxy fronted by model-router. The router owns
translation only; upstream credentials and plugins belong to the proxy.

Commands:
  status [--json]                          show proxy mode, endpoint and reachability
  models [--json]                          list models served by the proxy
  plugins [--json]                         list proxy plugins and their flags
  plugin enable|disable --id ID [--json]   enable or disable one proxy plugin
  accounts [--json]                        summarize proxy credentials (no secrets)

All commands accept --config PATH.

Environment:
  CLIPROXYAPI_MANAGEMENT_KEY   proxy management key for plugins/plugin/accounts.
                               Separate from the proxy inference key in the
                               router config; no fallback is attempted.`)
	return 0
}

func proxyJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	return 0
}

func proxyConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "router config path (default: detected)")
}

func proxyOptionsWithConfig(path string) routerctl.Options {
	opts, _, _ := routerOptions()
	if path != "" {
		opts.RouterConfig = path
	}
	return opts
}

func runProxyStatus(args []string) int {
	fs := flag.NewFlagSet("router proxy status", flag.ContinueOnError)
	config := proxyConfigFlag(fs)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("proxy status takes no positional arguments")
	}
	status, err := routerFetchProxyStatus(context.Background(), proxyOptionsWithConfig(*config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if *jsonOut {
		if code := proxyJSON(status); code != 0 {
			return code
		}
	} else {
		printProxyStatus(status)
	}
	if !status.Reachable && status.Mode != "disabled" {
		return 1
	}
	return 0
}

func printProxyStatus(status routerctl.ProxyStatus) {
	fmt.Printf("proxy:      %s\n", status.Mode)
	if status.Endpoint != "" {
		fmt.Printf("endpoint:   %s\n", status.Endpoint)
	} else {
		fmt.Printf("endpoint:   -\n")
	}
	fmt.Printf("reachable:  %t\n", status.Reachable)
	fmt.Printf("models:     %d\n", status.Models)
	fmt.Printf("management: %s\n", status.Management)
	if status.Note != "" {
		fmt.Printf("note:       %s\n", status.Note)
	}
}

func runProxyModels(args []string) int {
	fs := flag.NewFlagSet("router proxy models", flag.ContinueOnError)
	config := proxyConfigFlag(fs)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("proxy models takes no positional arguments")
	}
	models, err := routerListProxyModels(context.Background(), proxyOptionsWithConfig(*config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if *jsonOut {
		return proxyJSON(models)
	}
	for _, m := range models {
		if m.OwnedBy != "" {
			fmt.Printf("%s (%s)\n", m.ID, m.OwnedBy)
		} else {
			fmt.Println(m.ID)
		}
	}
	return 0
}

func runProxyPlugins(args []string) int {
	fs := flag.NewFlagSet("router proxy plugins", flag.ContinueOnError)
	config := proxyConfigFlag(fs)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("proxy plugins takes no positional arguments")
	}
	plugins, err := routerListProxyPlugins(context.Background(), proxyOptionsWithConfig(*config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if *jsonOut {
		return proxyJSON(plugins)
	}
	printProxyPlugins(plugins)
	return 0
}

func printProxyPlugins(plugins []routerctl.ProxyPlugin) {
	if len(plugins) == 0 {
		fmt.Println("no proxy plugins")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tENABLED\tEFFECTIVE\tREGISTERED\tOAUTH\tQUOTA\tVERSION")
	for _, p := range plugins {
		oauth := strings.Join(p.OAuthProviders, ",")
		if oauth == "" {
			oauth = "-"
		}
		version := p.Version
		if version == "" {
			version = "-"
		}
		fmt.Fprintf(tw, "%s\t%t\t%t\t%t\t%s\t%t\t%s\n",
			p.ID, p.Enabled, p.EffectiveEnabled, p.Registered, oauth, p.SupportsQuota, version)
	}
	_ = tw.Flush()
}

func runProxyPlugin(args []string) int {
	if len(args) == 0 {
		return failUsage("usage: router proxy plugin enable|disable --id ID")
	}
	if args[0] == "--help" || args[0] == "-h" {
		return proxyHelp()
	}
	if args[0] != "enable" && args[0] != "disable" {
		return failUsage("unknown plugin action: %s", args[0])
	}
	fs := flag.NewFlagSet("router proxy plugin "+args[0], flag.ContinueOnError)
	var id, config string
	var jsonOut bool
	fs.StringVar(&id, "id", "", "plugin ID")
	fs.StringVar(&config, "config", "", "router config path (default: detected)")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("plugin action takes no positional arguments")
	}
	if strings.TrimSpace(id) == "" {
		return failUsage("plugin id is required: pass --id ID")
	}
	enabled := args[0] == "enable"
	err := routerSetProxyPlugin(context.Background(), proxyOptionsWithConfig(config), id, enabled)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if jsonOut {
		return proxyJSON(map[string]any{"id": id, "enabled": enabled, "status": "ok"})
	}
	fmt.Printf("plugin %s enabled=%t\n", id, enabled)
	return 0
}

func runProxyAccounts(args []string) int {
	fs := flag.NewFlagSet("router proxy accounts", flag.ContinueOnError)
	config := proxyConfigFlag(fs)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("proxy accounts takes no positional arguments")
	}
	accounts, err := routerListProxyAccounts(context.Background(), proxyOptionsWithConfig(*config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if *jsonOut {
		return proxyJSON(accounts)
	}
	printProxyAccounts(accounts)
	return 0
}

func printProxyAccounts(accounts []routerctl.ProxyAccount) {
	if len(accounts) == 0 {
		fmt.Println("no proxy accounts")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tSTATUS\tDISABLED\tUNAVAILABLE\tEMAIL")
	for _, a := range accounts {
		typ, status, email := a.Type, a.Status, a.Email
		if typ == "" {
			typ = "-"
		}
		if status == "" {
			status = "-"
		}
		if email == "" {
			email = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%t\t%s\n",
			a.Name, typ, status, a.Disabled, a.Unavailable, email)
	}
	_ = tw.Flush()
}
