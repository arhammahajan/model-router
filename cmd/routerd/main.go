// Command routerd runs the Go router daemon.
//
// Usage:
//
//	routerd [--config PATH]                 serve (default)
//	router --validate-config [--config P]
//	router --check-readiness [--config P]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/arhammahajan/router/internal/config"
	"github.com/arhammahajan/router/internal/discovery"
	"github.com/arhammahajan/router/internal/redact"
	"github.com/arhammahajan/router/internal/server"
)

// drainTimeout bounds graceful shutdown: in-flight requests may complete
// within the bound, then remaining connections and idle upstream pools are
// force-closed so the process cannot hang on a long generation.
const drainTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func findDefaultConfig() (string, bool) {
	for _, name := range []string{
		"router.toml",
		"router.json",
		"config.toml",
		"config.json",
		"router.yaml",
		"config.yaml",
	} {
		if st, err := os.Stat(name); err == nil && !st.IsDir() {
			return name, true
		}
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		configHome = filepath.Join(home, ".config")
	}
	path := filepath.Join(configHome, ".router", "router.toml")
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return path, true
	}
	return "", false
}

func run(args []string) int {
	fs := flag.NewFlagSet("routerd", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to router.toml")
	fs.StringVar(cfgPath, "c", "", "alias of --config")
	hostOverride := fs.String("host", "", "override server.host")
	portOverride := fs.Int("port", 0, "override server.port")
	var apiKeys stringSlice
	fs.Var(&apiKeys, "api-key", "Require this router API key (repeatable). Overrides config.")
	var envFiles stringSlice
	fs.Var(&envFiles, "env-file", "Load KEY=value credentials from this file before reading the config (repeatable). Existing environment variables win.")
	noDefaultEnv := fs.Bool("no-default-env", false, "Do not auto-load ~/.config/ai/credentials.env")
	validate := fs.Bool("validate-config", false, "validate configuration and exit")
	checkReady := fs.Bool("check-readiness", false, "validate configuration and route availability and exit")
	checkAlias := fs.Bool("check", false, "alias of --check-readiness")
	offline := fs.Bool("offline", false, "forbid network work (only with --validate-config or --check-readiness)")
	showVersion := fs.Bool("version", false, "print version and exit")
	verbose := fs.Bool("verbose", false, "Enable router debug logs (no dependency body logging)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: routerd [--config PATH] [--validate-config] [--check-readiness] [--version]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Println("routerd", server.BuildVersion)
		return 0
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "routerd: unexpected positional arguments")
		return 2
	}
	provided := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	check := *checkReady || *checkAlias
	if *validate && check {
		fmt.Fprintf(os.Stderr, "routerd: --validate-config and --check-readiness/--check are mutually exclusive\n")
		return 2
	}
	if *offline && !check && !*validate {
		fmt.Fprintf(os.Stderr, "routerd: --offline requires --validate-config or --check-readiness/--check\n")
		fs.Usage()
		return 2
	}
	resolved := *cfgPath
	if resolved == "" && (provided["config"] || provided["c"]) {
		fmt.Fprintln(os.Stderr, "routerd: --config must be a non-empty path")
		return 2
	}
	if resolved == "" {
		def, found := findDefaultConfig()
		if !found {
			fmt.Fprintf(os.Stderr, "routerd: No config found; pass --config /path/to/router.toml\n")
			return 2
		}
		resolved = def
	}
	cfg, err := config.LoadWithOptions(resolved, config.LoadOptions{
		NoDefaultEnv: *noDefaultEnv,
		EnvFiles:     []string(envFiles),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "routerd: %v\n", err)
		return 2
	}
	if provided["host"] {
		trimmed := strings.TrimSpace(*hostOverride)
		if trimmed == "" {
			fmt.Fprintf(os.Stderr, "routerd: --host must be a non-empty string\n")
			return 2
		}
		cfg.Server.Host = trimmed
	}
	if provided["port"] {
		if *portOverride < 1 || *portOverride > 65535 {
			fmt.Fprintf(os.Stderr, "routerd: --port must be between 1 and 65535\n")
			return 2
		}
		cfg.Server.Port = *portOverride
	}
	if len(apiKeys) > 0 {
		trimmed := make([]string, 0, len(apiKeys))
		for _, k := range apiKeys {
			t := strings.TrimSpace(k)
			if t == "" {
				fmt.Fprintf(os.Stderr, "routerd: --api-key must be a non-empty string\n")
				return 2
			}
			trimmed = append(trimmed, t)
		}
		seen := map[string]bool{}
		deduped := trimmed[:0]
		for _, k := range trimmed {
			if !seen[k] {
				seen[k] = true
				deduped = append(deduped, k)
			}
		}
		cfg.Server.APIKeys = deduped
		cfg.Server.AuthMode = "required"
		if cfg.Server.BearerSessionCompat {
			fmt.Fprintf(os.Stderr, "routerd: server.bearer_session_compat requires unauthenticated client access\n")
			return 2
		}
	} else if cfg.Server.BearerSessionCompat && len(cfg.Server.APIKeys) > 0 {
		fmt.Fprintf(os.Stderr, "routerd: server.bearer_session_compat requires unauthenticated client access\n")
		return 2
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "routerd: debug config_loaded providers=%d configured_models=%d catalog=%t llama=%t cache=%t\n",
			len(cfg.Providers), len(cfg.Models), cfg.Catalog != nil, cfg.Llama != nil, cfg.DiscoveryCache != nil)
	}
	if *validate {
		// No App/probes/network/cache I/O: config validation only.
		fmt.Println(`{"status":"valid","network_checked":false}`)
		return 0
	}
	if check && *offline {
		// No FS, network or discovery: readiness is indeterminate.
		fmt.Println(`{"status":"indeterminate","network_checked":false,"reason":"offline_readiness_requires_discovery"}`)
		return 4
	}
	if *offline {
		fmt.Fprintf(os.Stderr, "routerd: --offline requires --validate-config or --check-readiness/--check\n")
		return 2
	}
	srv := server.New(cfg)
	// Workers perform no I/O until the composition root below runs the
	// first bounded refreshes. Server construction stays I/O-free.
	var catWorker *discovery.Worker
	if cfg.Catalog != nil {
		catWorker = discovery.NewCatalogWorker(cfg.Catalog, cfg.Providers, srv.Registry())
		// Covers readiness and startup/listen failures too.
		defer catWorker.CloseIdleConnections()
		defer catWorker.Stop()
	}
	var llamaWorker *discovery.LlamaWorker
	if cfg.Llama != nil {
		llamaWorker = discovery.NewLlamaWorker(cfg.Llama, srv.Registry())
		defer llamaWorker.CloseIdleConnections()
		defer llamaWorker.Stop()
	}
	// Optional persistence owner: one process-owned I/O owner, depth-1
	// coalesced. Workers only notify after accepted publications.
	var store *discovery.CacheStore
	if cfg.DiscoveryCache != nil {
		store = discovery.NewCacheStore(cfg, srv.Registry())
		if catWorker != nil {
			catWorker.CacheAttach(store)
		}
		if llamaWorker != nil {
			llamaWorker.CacheAttach(store)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			store.Close(ctx)
		}()
	}
	if check {
		// Restore before the bounded fleet attempts, then exactly one
		// bounded catalog attempt and one bounded llama attempt.
		if store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			store.Restore(ctx, llamaWorker)
			cancel()
		}
		if err := runOnlineCheck(cfg, catWorker, llamaWorker); err != nil {
			fmt.Fprintf(os.Stderr, "routerd: %v\n", sanitizeErr(cfg, err))
			return 1
		}
		report := srv.ReadinessReport()
		fmt.Println(string(srv.ReadinessReportJSON()))
		if ready, _ := report["ready"].(bool); ready {
			return 0
		}
		return 3
	}
	// Signal cancellation is installed BEFORE any startup I/O so a
	// shutdown signal cancels the first probes too. Both the channel
	// (serve/drain) and the context (startup/periodic probes) fire.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(ch)
	sigCtx, sigStop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer sigStop()
	// Cache load happens before the first probe under a 2s handoff
	// deadline; on cancel/timeout the load is skipped with an empty seed.
	if store != nil {
		store.Start()
		store.Restore(sigCtx, llamaWorker)
	}
	if catWorker != nil {
		// Startup inventory: exactly one attempt, even when
		// refresh_seconds is 0. Failure retains (empty) routes and
		// still serves; the error is logged with static reasons.
		ctx, cancel := context.WithTimeout(sigCtx, catalogBound(cfg))
		catWorker.RefreshOnce(ctx)
		cancel()
	}
	if llamaWorker != nil {
		// Separate fleet bound derived from the probe timeout: never one
		// shared bound for both sources.
		ctx, cancel := context.WithTimeout(sigCtx, discovery.LlamaFleetBound(cfg.Llama))
		llamaWorker.RefreshOnce(ctx)
		cancel()
	}
	if sigCtx.Err() != nil {
		return 0
	}
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "routerd: listen: %v\n", err)
		return 1
	}
	// Periodic refresh starts only after bind, and never when
	// refresh_seconds is 0 (Start is itself a no-op then).
	if catWorker != nil {
		catWorker.Start(sigCtx)
	}
	if llamaWorker != nil {
		llamaWorker.Start(sigCtx)
	}
	return serve(ln, srv, catWorker, llamaWorker, store, drainTimeout, ch)
}

// runOnlineCheck performs exactly one bounded attempt per configured
// source, catalog-then-llama (the deliberate startup order). Any runtime
// failure surfaces as an error for a sanitized exit-1 report.
func runOnlineCheck(cfg *config.Config, catWorker *discovery.Worker, llamaWorker *discovery.LlamaWorker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("readiness check failed: %v", r)
		}
	}()
	if catWorker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), catalogBound(cfg))
		catWorker.RefreshOnce(ctx)
		cancel()
	}
	if llamaWorker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), discovery.LlamaFleetBound(cfg.Llama))
		llamaWorker.RefreshOnce(ctx)
		cancel()
	}
	return nil
}

// sanitizeErr redacts a runtime check failure with the app-owned secret
// corpus before it reaches stderr.
func sanitizeErr(cfg *config.Config, err error) string {
	return redact.Redact(err.Error(), redact.ExpandCorpus(config.ConfigSecrets(cfg)))
}

// catalogBound is the whole-attempt bound for startup/readiness probes:
// the configured catalog timeout (finite positive by validation).
func catalogBound(cfg *config.Config) time.Duration {
	return time.Duration(cfg.Catalog.Timeout * float64(time.Second))
}

// stopWorkers cancels in-flight probes and joins both workers before the
// drain bound. Either worker may be nil (source unconfigured).
func stopWorkers(catWorker *discovery.Worker, llamaWorker *discovery.LlamaWorker) {
	if catWorker != nil {
		catWorker.Stop()
	}
	if llamaWorker != nil {
		llamaWorker.Stop()
	}
}

// serve blocks serving ln until stop fires, then closes listener admission
// and stops both workers (canceling in-flight probes and joining before
// the drain bound), drains bounded by timeout, and force-closes remaining
// connections and idle upstream pools. A nil worker means its source is
// unconfigured. The cache closes under the same absolute drain deadline
// with at most one pending flush and no post-drain flush. It returns the
// process exit code.
func serve(ln net.Listener, srv *server.Server, catWorker *discovery.Worker, llamaWorker *discovery.LlamaWorker, store *discovery.CacheStore, timeout time.Duration, stop <-chan os.Signal) int {
	defer srv.CloseIdleConnections()
	if catWorker != nil {
		defer catWorker.CloseIdleConnections()
	}
	if llamaWorker != nil {
		defer llamaWorker.CloseIdleConnections()
	}
	// Admission and worker lifetimes end before draining on every exit:
	// stop the workers (cancel + join) on serve errors too, not just on
	// signals, so no probe outlives the listener.
	defer stopWorkers(catWorker, llamaWorker)
	ln = srv.LimitListener(ln)
	httpSrv := srv.HTTPServer(ln.Addr().String())
	defer httpSrv.Close()
	serveErr := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "routerd %s listening on %s\n", server.BuildVersion, ln.Addr())
		serveErr <- httpSrv.Serve(ln)
	}()
	select {
	case <-stop:
		fmt.Fprintf(os.Stderr, "routerd: shutting down\n")
	case err := <-serveErr:
		if err != nil && err.Error() != "http: Server closed" {
			fmt.Fprintf(os.Stderr, "routerd: %v\n", err)
			return 1
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Stop admission before waiting for refresh cancellation. Shutdown below
	// still drains accepted connections under the same absolute bound.
	_ = ln.Close()
	stopWorkers(catWorker, llamaWorker)
	_ = httpSrv.Shutdown(ctx)
	if store != nil {
		store.Close(ctx)
	}
	// Forced bound: close anything still open and idle upstream pools.
	_ = httpSrv.Close()
	srv.CloseIdleConnections()
	select {
	case err := <-serveErr:
		if err != nil && err.Error() != "http: Server closed" && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintf(os.Stderr, "routerd: %v\n", err)
			return 1
		}
	case <-time.After(timeout + 2*time.Second):
		fmt.Fprintf(os.Stderr, "routerd: shutdown timed out\n")
		return 1
	}
	return 0
}
