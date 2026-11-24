package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/arhammahajan/router/internal/routercli/routerctl"
)

var (
	routerFetchProviders = routerctl.FetchProviders
	routerFetchProvider  = routerctl.FetchProvider
)

func providerHelp() int {
	fmt.Println(`router provider [list | status [NAME] | NAME] [options]

Commands:
  list [--config PATH] [--json]           list all configured providers (default)
  status [NAME] [--config PATH] [--json]  show per-provider status and shared fields
  <NAME> [--config PATH] [--json]         show status and shared fields for one provider`)
	return 0
}

func runProvider(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		return providerHelp()
	}

	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "list", "ls":
			sub = "list"
			rest = args[1:]
		case "status":
			sub = "status"
			rest = args[1:]
		default:
			sub = "status"
			rest = args
		}
	}

	var (
		configPath string
		jsonOut    bool
		positionals []string
	)
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "-h" || arg == "--help":
			return providerHelp()
		case arg == "--json":
			jsonOut = true
		case arg == "--config" || arg == "-config":
			if i+1 >= len(rest) {
				return failUsage("flag needs an argument: %s", arg)
			}
			i++
			configPath = rest[i]
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "-config="):
			configPath = strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "-"):
			return failUsage("unknown provider flag: %s", arg)
		default:
			positionals = append(positionals, arg)
		}
	}

	var name string
	switch len(positionals) {
	case 0:
	case 1:
		if sub == "list" {
			return failUsage("provider list takes no positional arguments")
		}
		name = positionals[0]
	default:
		return failUsage("too many arguments for router provider")
	}

	opts, _, _ := routerOptions()
	if configPath != "" {
		opts.RouterConfig = configPath
	}

	if name != "" {
		info, err := routerFetchProvider(context.Background(), opts, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		if jsonOut {
			return writeJSON(info)
		}
		printProviderDetail(info)
		return 0
	}

	items, err := routerFetchProviders(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if jsonOut {
		return writeJSON(items)
	}
	if sub == "status" {
		for i, item := range items {
			if i > 0 {
				fmt.Println()
			}
			printProviderDetail(item)
		}
		return 0
	}
	printProviderTable(items)
	return 0
}

func writeJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	return 0
}

func printProviderTable(items []routerctl.ProviderInfo) {
	if len(items) == 0 {
		fmt.Println("no providers configured")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tSTATUS\tHOST:PORT\tDIALECTS\tCOMPAT\tTIMEOUT")
	for _, it := range items {
		hostPort := formatHostPort(it)
		dialects := strings.Join(it.SupportedDialects, ",")
		timeout := fmt.Sprintf("%.0fs", it.TimeoutSeconds)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			it.Name, it.Kind, it.Status, hostPort, dialects, it.Compatibility, timeout)
	}
	_ = tw.Flush()
}

func printProviderDetail(it routerctl.ProviderInfo) {
	fmt.Printf("name:          %s\n", it.Name)
	fmt.Printf("kind:          %s\n", it.Kind)
	fmt.Printf("status:        %s\n", it.Status)
	if it.Mode != "" {
		fmt.Printf("mode:          %s\n", it.Mode)
	}
	fmt.Printf("base_url:      %s\n", it.BaseURL)
	if it.Host != "" {
		fmt.Printf("host:          %s\n", it.Host)
	}
	if it.Port > 0 {
		fmt.Printf("port:          %d\n", it.Port)
	}
	fmt.Printf("dialect:       %s\n", it.DefaultDialect)
	fmt.Printf("dialects:      %s\n", strings.Join(it.SupportedDialects, ", "))
	fmt.Printf("compatibility: %s\n", it.Compatibility)
	fmt.Printf("auth_header:   %s\n", it.AuthHeader)
	fmt.Printf("api_key:       %t\n", it.APIKeyConfigured)
	fmt.Printf("timeout:       %.0fs\n", it.TimeoutSeconds)
	if it.ProbeTimeoutSeconds > 0 {
		fmt.Printf("probe_timeout: %gs\n", it.ProbeTimeoutSeconds)
	}
	if it.Prefix != "" {
		fmt.Printf("prefix:        %s\n", it.Prefix)
	}
}

func formatHostPort(it routerctl.ProviderInfo) string {
	if it.Host == "" {
		return "-"
	}
	if it.Port > 0 {
		return fmt.Sprintf("%s:%d", it.Host, it.Port)
	}
	return it.Host
}
