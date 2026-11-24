package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/arhammahajan/router/internal/routercli/routerctl"
)

var routerFetchProviders = routerctl.FetchProviders

func runProviders(args []string) int {
	fs := flag.NewFlagSet("router providers", flag.ContinueOnError)
	configPath := fs.String("config", "", "router config path (default: detected)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return failUsage("providers takes no positional arguments")
	}

	opts, _, _ := routerOptions()
	if *configPath != "" {
		opts.RouterConfig = *configPath
	}

	items, err := routerFetchProviders(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(items); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
			return 1
		}
		return 0
	}
	printProvidersTable(items)
	return 0
}

func printProvidersTable(items []routerctl.ProviderInfo) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tSTATUS\tHOST:PORT\tDIALECTS\tCOMPAT")
	for _, it := range items {
		compat := it.Compatibility
		if compat == "" {
			compat = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			it.Name, it.Status, formatHostPort(it), strings.Join(it.SupportedDialects, ","), compat)
	}
	_ = tw.Flush()
}

func formatHostPort(it routerctl.ProviderInfo) string {
	if len(it.Servers) > 1 {
		parts := make([]string, 0, len(it.Servers))
		for _, s := range it.Servers {
			if s.Port > 0 {
				parts = append(parts, net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
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
		return net.JoinHostPort(it.Host, strconv.Itoa(it.Port))
	}
	return it.Host
}
