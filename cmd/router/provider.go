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
	fmt.Println(`router provider [list | status [NAME]] [options]

Commands:
  list [--config PATH] [--json]           list available router providers (default)
  status [NAME] [--config PATH] [--json]  show per-provider status, port, and shared fields`)
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
		configPath  string
		jsonOut     bool
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
		printProviderStatusTable(items)
		return 0
	}
	printProviderListTable(items)
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

func printProviderListTable(items []routerctl.ProviderInfo) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tENABLED\tDIALECTS")
	for _, it := range items {
		enabled := "no"
		if it.Enabled {
			enabled = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", it.Name, enabled, strings.Join(it.SupportedDialects, ","))
	}
	_ = tw.Flush()
}

func printProviderStatusTable(items []routerctl.ProviderInfo) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tSTATUS\tHOST:PORT\tDIALECTS\tCOMPAT\tTIMEOUT")
	for _, it := range items {
		hostPort := formatHostPort(it)
		dialects := strings.Join(it.SupportedDialects, ",")
		compat := it.Compatibility
		if compat == "" {
			compat = "-"
		}
		timeout := "-"
		if it.TimeoutSeconds > 0 {
			timeout = fmt.Sprintf("%.0fs", it.TimeoutSeconds)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			it.Name, it.Status, hostPort, dialects, compat, timeout)
	}
	_ = tw.Flush()
}

func printProviderDetail(it routerctl.ProviderInfo) {
	fmt.Printf("provider:      %s\n", it.Name)
	fmt.Printf("enabled:       %t\n", it.Enabled)
	fmt.Printf("status:        %s\n", it.Status)
	if it.Mode != "" {
		fmt.Printf("mode:          %s\n", it.Mode)
	}
	if it.BaseURL != "" {
		fmt.Printf("base_url:      %s\n", it.BaseURL)
	}
	if it.Host != "" {
		fmt.Printf("host:          %s\n", it.Host)
	}
	if it.Port > 0 {
		fmt.Printf("port:          %d\n", it.Port)
	}
	fmt.Printf("dialect:       %s\n", it.DefaultDialect)
	fmt.Printf("dialects:      %s\n", strings.Join(it.SupportedDialects, ", "))
	if it.Compatibility != "" {
		fmt.Printf("compatibility: %s\n", it.Compatibility)
	}
	if it.AuthHeader != "" {
		fmt.Printf("auth_header:   %s\n", it.AuthHeader)
	}
	if it.Enabled {
		fmt.Printf("api_key:       %t\n", it.APIKeyConfigured)
	}
	if it.TimeoutSeconds > 0 {
		fmt.Printf("timeout:       %.0fs\n", it.TimeoutSeconds)
	}
	if it.ProbeTimeoutSeconds > 0 {
		fmt.Printf("probe_timeout: %gs\n", it.ProbeTimeoutSeconds)
	}
	if it.Prefix != "" {
		fmt.Printf("prefix:        %s\n", it.Prefix)
	}
	if len(it.Servers) > 1 {
		fmt.Println("servers:")
		for _, s := range it.Servers {
			fmt.Printf("  - %s (%s:%d) [%s]\n", s.Name, s.Host, s.Port, s.Status)
		}
	}
}

func formatHostPort(it routerctl.ProviderInfo) string {
	if len(it.Servers) > 1 {
		parts := make([]string, 0, len(it.Servers))
		for _, s := range it.Servers {
			if s.Port > 0 {
				parts = append(parts, fmt.Sprintf("%s:%d", s.Host, s.Port))
			} else if s.Host != "" {
				parts = append(parts, s.Host)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, ",")
		}
	}
	if it.Host == "" {
		return "-"
	}
	if it.Port > 0 {
		return fmt.Sprintf("%s:%d", it.Host, it.Port)
	}
	return it.Host
}
