// Llama refresh worker: parallel per-server probes, transactional
// publication, stale retention and idempotent shutdown. The worker owns
// the only llama publication path. It mirrors the catalog Worker
// lifecycle protections (single-attempt TryLock, scheduled
// success/failure backoff, restartable start, idempotent stop joining
// in-flight probes) without sharing its network path.
package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arhammahajan/model-router/internal/capability"
	"github.com/arhammahajan/model-router/internal/catalog"
	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

// Static llama failures. All are value-free sentinels: ordinary logs
// carry static reasons and counts only, never payloads, IDs, URLs,
// headers, bodies or raw errors.
var (
	ErrLlamaHTTP   = errors.New("llama: probe failed")
	ErrLlamaHealth = errors.New("llama: health check failed")
	ErrLlamaModels = errors.New("llama: models fetch failed")
	ErrLlamaShape  = errors.New("llama: response has no model list")
)

// llamaAuthScrubbed mirrors the credential header names: provider static
// headers can never leave a second credential beside the selected Bearer
// key on a probe. Catalog/client auth is never emitted here.
var llamaAuthScrubbed = map[string]bool{
	"authorization": true, "x-api-key": true, "api-key": true,
	"x-goog-api-key": true,
}

// llamaServer is the worker's private copy of one configured server.
type llamaServer struct {
	url     string
	name    string
	key     string
	headers map[string]string
}

// LlamaWorker refreshes one Registry from one [llama_cpp] table. Static
// providers and the trusted generated inventory are read-only and never
// mutated; per-request network discovery never happens (read paths serve
// the cached snapshot). Status lives in the registry snapshot;
// Failures/LastError are thin selectors over it, never duplicate mutable
// counters. Per-server retained routes stay worker-owned for stale
// retention.
type LlamaWorker struct {
	probeTimeout  float64
	refresh       float64
	prefix        string
	exposeAliases bool
	servers       []llamaServer
	reg           *router.Registry
	client        *http.Client
	clock         router.Clock
	jitter        func(ceil float64) float64
	sanitize      func(string) string

	attemptMu sync.Mutex // TryLock: overlapping refreshes return immediately
	stateMu   sync.Mutex
	active    map[string]bool
	retained  map[string][]router.LlamaEntry
	// retainedRuntime pins the last established engine per server across
	// transient failures, mirroring retained routes. A first failure
	// without a prior success leaves Unobserved, never invented.
	retainedRuntime map[string]capability.LocalRuntime
	cache           *CacheStore
	wake            chan struct{} // depth 1; attempt completion wakes the scheduler

	stopCh   chan struct{}
	stopOnce sync.Once
	loopDone chan struct{}
	started  bool // one periodic-loop lifetime; canceled loops cannot restart
}

// NewLlamaWorker builds the worker. It performs no I/O; the composition
// root owns the first bounded RefreshOnce.
func NewLlamaWorker(cfg *config.LlamaConfig, reg *router.Registry) *LlamaWorker {
	w := &LlamaWorker{
		probeTimeout:    cfg.ProbeTimeout,
		refresh:         cfg.RefreshSeconds,
		prefix:          cfg.Prefix,
		exposeAliases:   cfg.ExposeAliases,
		reg:             reg,
		client:          fetchClient(0), // no client bound: each server owns a whole-probe context timeout
		clock:           reg.Clock(),
		jitter:          defaultJitter,
		sanitize:        reg.Sanitize,
		active:          map[string]bool{},
		retained:        map[string][]router.LlamaEntry{},
		retainedRuntime: map[string]capability.LocalRuntime{},
		stopCh:          make(chan struct{}),
		loopDone:        make(chan struct{}),
		wake:            make(chan struct{}, 1),
	}
	for _, s := range cfg.Servers {
		// Trusted global+server headers seed both probe and generated
		// headers; the server table wins on conflict.
		merged := http.Header{}
		for k, v := range cfg.Headers {
			merged.Set(k, v)
		}
		for k, v := range s.Headers {
			merged.Set(k, v)
		}
		headers := make(map[string]string, len(merged))
		for k := range merged {
			headers[k] = merged.Get(k)
		}
		w.servers = append(w.servers, llamaServer{
			url:     s.URL,
			name:    s.Name,
			key:     s.APIKey,
			headers: headers,
		})
	}
	return w
}

// LlamaFleetBound covers every wave of one fleet sweep: waves =
// ceil(n/workers) whole-probe timeouts plus one second for scheduling and
// publication. An equal parent/child deadline would cancel the sweep as
// its last timed-out server returns, discarding successful peers.
// The bound saturates at the largest duration without shortening valid
// per-server timeouts.
// There is no separate shared bound that cancels later waves when the
// first workers use one interval; explicit ctx cancellation still stops
// everything promptly.
func LlamaFleetBound(cfg *config.LlamaConfig) time.Duration {
	n := len(cfg.Servers)
	if n < 1 {
		n = 1
	}
	workers := min(8, n)
	waves := 1 + (n-1)/workers
	sec := cfg.ProbeTimeout * float64(waves)
	if math.IsNaN(sec) || math.IsInf(sec, 0) || sec <= 0 {
		sec = cfg.ProbeTimeout
	}
	const maxBound = time.Duration(1<<63 - 1)
	if sec >= float64(maxBound)/float64(time.Second)-1 {
		return maxBound
	}
	return time.Duration(sec*float64(time.Second)) + time.Second
}

// Failures reports consecutive fleet failures from the pinned snapshot.
func (w *LlamaWorker) Failures() int {
	return w.reg.LlamaAggregate().Failures
}

// LastError returns the static failure reason, or "" when healthy.
func (w *LlamaWorker) LastError() string {
	return w.reg.LlamaAggregate().Err
}

// RefreshOnce performs a single fleet attempt. Overlapping calls return
// immediately, even forced ones (matching Python). Each server probes
// under its own whole-probe timeout; a canceled sweep publishes nothing
// and changes no state. One complete fleet commits in a single
// publication: reachable servers advance with fresh success facts, later
// failures retain their last good routes as stale with retained facts,
// and initial failures leave the provider inactive. Completion wakes the
// scheduler after unlocking, including cancellation without publication.
// False means another attempt owns the lock, not a failed probe. The aggregate
// keeps prior last_success on partial failure while fresh servers advance theirs.
func (w *LlamaWorker) RefreshOnce(ctx context.Context) bool {
	if !w.attemptMu.TryLock() {
		return false
	}
	defer func() {
		w.attemptMu.Unlock()
		w.signalWake()
	}()
	select {
	case <-w.stopCh:
		return true
	default:
	}
	if ctx.Err() != nil {
		return true
	}

	// Stop cancels in-flight probes: link the worker lifetime into the
	// attempt without taking ownership of the caller's context.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopReq := make(chan struct{})
	defer close(stopReq)
	go func() {
		select {
		case <-w.stopCh:
			cancel()
		case <-stopReq:
		}
	}()

	type outcome struct {
		entries []router.LlamaEntry
		mode    string
		runtime capability.LocalRuntime
		err     error
	}
	start := w.clock.Sample()
	n := len(w.servers)
	outcomes := make([]outcome, n)
	if n > 0 {
		// Fixed pool of min(8,n) workers over a job queue: later waves
		// are never cancelled by an earlier wave's probe bound.
		jobs := make(chan int, n)
		for i := range w.servers {
			jobs <- i
		}
		close(jobs)
		var wg sync.WaitGroup
		for k := 0; k < min(8, n); k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					entries, mode, rt, err := w.probeServer(reqCtx, w.servers[i])
					outcomes[i] = outcome{entries: entries, mode: mode, runtime: rt, err: err}
				}
			}()
		}
		wg.Wait()
	}
	end := w.clock.Sample()

	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	select {
	case <-w.stopCh:
		return true
	default:
	}
	if reqCtx.Err() != nil {
		return true // explicit cancellation: no partial publication
	}

	priorEv := w.reg.ServerEvents()
	priorAgg := w.reg.LlamaAggregate()
	layer := router.LlamaLayer{Servers: make(map[string]router.LlamaServerSnapshot, n)}
	servers := make(map[string]router.ServerEvent, n)
	var aggErrs []string
	failed := 0
	total := 0
	for i, srv := range w.servers {
		out := outcomes[i]
		if out.err != nil {
			failed++
			perr := w.sanitize(srv.url + ": " + out.err.Error())
			aggErrs = append(aggErrs, perr)
			if w.active[srv.name] {
				retained := cloneLlamaEntries(w.retained[srv.name])
				layer.Servers[srv.name] = router.LlamaServerSnapshot{
					State:   router.LlamaUnreachableStale,
					Runtime: w.retainedRuntime[srv.name],
					Entries: retained,
				}
				total += len(retained)
				pe := priorEv[srv.name]
				servers[srv.name] = router.ServerEvent{
					URL:    srv.url,
					Models: pe.Models,
					SourceEvent: router.SourceEvent{
						LastSuccess: pe.LastSuccess,
						LastAttempt: monoPtr(start.Mono),
						Failures:    pe.Failures + 1,
						Err:         perr,
						Cached:      pe.Cached,
					},
				}
			} else {
				layer.Servers[srv.name] = router.LlamaServerSnapshot{}
				pe := priorEv[srv.name]
				servers[srv.name] = router.ServerEvent{
					URL: srv.url,
					SourceEvent: router.SourceEvent{
						LastAttempt: monoPtr(start.Mono),
						Failures:    pe.Failures + 1,
						Err:         perr,
					},
				}
			}
			continue
		}
		mode := out.mode
		layer.Servers[srv.name] = router.LlamaServerSnapshot{
			State:   router.LlamaReachable,
			Runtime: out.runtime,
			Entries: cloneLlamaEntries(out.entries),
		}
		total += len(out.entries)
		servers[srv.name] = router.ServerEvent{
			URL:    srv.url,
			Mode:   &mode,
			Models: len(out.entries),
			SourceEvent: router.SourceEvent{
				LastSuccess: monoPtr(end.Mono),
				LastAttempt: monoPtr(start.Mono),
			},
		}
	}
	interval := w.refresh
	var agg router.SourceEvent
	if failed > 0 {
		failures := priorAgg.Failures + 1
		agg = router.SourceEvent{
			LastSuccess: priorAgg.LastSuccess,
			LastAttempt: monoPtr(start.Mono),
			NextAttempt: nextAfter(end.Mono, interval, w.jitter(llamaFailureBackoff(failures, interval))),
			Failures:    failures,
			Err:         strings.Join(aggErrs, "; "),
		}
	} else {
		agg = router.SourceEvent{
			LastSuccess: monoPtr(end.Mono),
			LastAttempt: monoPtr(start.Mono),
			NextAttempt: nextAfter(end.Mono, interval, interval),
		}
	}
	if perr := w.reg.CommitLlama(router.LlamaAttempt{
		Layer:     layer,
		AtWall:    end.Wall,
		Aggregate: agg,
		Servers:   servers,
	}); perr != nil {
		// Invalid layer: record the failure status while keeping prior
		// routes, times and facts.
		failures := priorAgg.Failures + 1
		w.reg.NoteLlamaFailure(router.SourceEvent{
			LastSuccess: priorAgg.LastSuccess,
			LastAttempt: monoPtr(start.Mono),
			NextAttempt: nextAfter(end.Mono, interval, w.jitter(llamaFailureBackoff(failures, interval))),
			Failures:    failures,
			Err:         w.sanitize(perr.Error()),
		}, priorEv)
		log.Printf("llama refresh failed: %v (models=%d failures=%d)", perr, total, failures)
		return true
	}
	// Retained routing state commits with the validated registry layer.
	// A rejected publication must not activate a provider for later retries.
	cache := w.cache
	reachable := 0
	for i, srv := range w.servers {
		if outcomes[i].err == nil {
			w.active[srv.name] = true
			w.retained[srv.name] = cloneLlamaEntries(outcomes[i].entries)
			w.retainedRuntime[srv.name] = outcomes[i].runtime
			reachable++
		}
	}
	if cache != nil && (reachable > 0 || n == 0) {
		cache.NotifyDirty()
	}
	if failed > 0 {
		log.Printf("llama refresh partial: %d of %d servers unreachable (models=%d failures=%d)",
			failed, n, total, agg.Failures)
		return true
	}
	log.Printf("llama refresh succeeded (servers=%d models=%d)", n, total)
	return true
}

// llamaFailureBackoff caps failure backoff at the configured refresh interval
// when positive so on-demand local llama.cpp servers are discovered promptly
// after starting, rather than waiting up to the 5-minute remote catalog cap.
func llamaFailureBackoff(failures int, interval float64) float64 {
	ceil := BackoffCeiling(failures)
	if interval > 0 && ceil > interval {
		return interval
	}
	return ceil
}

// signalWake wakes the scheduler after a completed attempt.
func (w *LlamaWorker) signalWake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// probeServer runs the exact probe order under one whole-probe context:
// GET {base}/health (3xx never followed and never itself a failure,
// >=400 fails), then GET {base}/v1/models (2xx only with a model list),
// then GET {base}/props?autoload=false (optional: <400 JSON objects only,
// anything else is {} and the attempt continues). The server runtime is
// derived from the raw server props on each successful probe: a Strata
// build token selects Strata, everything else (including absent or
// malformed props) defaults to LlamaCpp. Router/swap ignore global props
// for observations but still use them as server engine identity.
func (w *LlamaWorker) probeServer(parent context.Context, srv llamaServer) ([]router.LlamaEntry, string, capability.LocalRuntime, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(w.probeTimeout*float64(time.Second)))
	defer cancel()
	if ctx.Err() != nil {
		return nil, "", capability.RuntimeUnobserved, ErrLlamaHTTP
	}
	headers := llamaProbeHeaders(srv)
	base := strings.TrimRight(srv.url, "/")
	health, err := llamaGet(ctx, w.client, base+"/health", headers)
	if err != nil {
		return nil, "", capability.RuntimeUnobserved, ErrLlamaHTTP
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(health.Body, 1024))
	health.Body.Close()
	if health.StatusCode >= 400 {
		return nil, "", capability.RuntimeUnobserved, ErrLlamaHealth
	}
	models, err := llamaGet(ctx, w.client, base+"/v1/models", headers)
	if err != nil {
		return nil, "", capability.RuntimeUnobserved, ErrLlamaHTTP
	}
	defer models.Body.Close()
	if models.StatusCode < 200 || models.StatusCode > 299 {
		return nil, "", capability.RuntimeUnobserved, ErrLlamaModels
	}
	raw, err := readBoundedBody(models)
	if err != nil {
		return nil, "", capability.RuntimeUnobserved, errLlamaBody(err)
	}
	rows, err := decodeLlamaRows(raw)
	if err != nil {
		return nil, "", capability.RuntimeUnobserved, ErrLlamaShape
	}
	// Props are optional enrichment input only in this slice: the attempt
	// continues on any props failure with {}. Mode is a bounded fact from
	// props+rows (single|legacy|router|llama-swap); strata is an engine,
	// never a mode. Missing props fall back to single on live probes.
	props := w.fetchProps(ctx, base, headers)
	if ctx.Err() != nil {
		return nil, "", capability.RuntimeUnobserved, ErrLlamaHTTP
	}
	mode := llamaMode(props, rows)
	runtime := capability.LocalEngine(props)
	entries := w.mapRows(rows, props, mode)
	if ctx.Err() != nil {
		return nil, "", capability.RuntimeUnobserved, ErrLlamaHTTP
	}
	return entries, mode, runtime, nil
}

// llamaMode classifies a llama.cpp endpoint the way pi-llama-cpp does:
// props src/role first, then the legacy row marker, else single.
func llamaMode(props map[string]any, rows []any) string {
	if props != nil {
		if src, _ := props["src"].(string); src == "llama-swap" {
			return "llama-swap"
		}
		if role, _ := props["role"].(string); role == "router" {
			return "router"
		}
	}
	if len(rows) > 0 {
		if m, ok := rows[0].(map[string]any); ok {
			if _, ok := m["max_model_len"]; ok {
				return "legacy"
			}
		}
	}
	return "single"
}

// fetchProps attempts GET {base}/props?autoload=false and returns exactly
// one JSON object or {}. A bad optional props body becomes no evidence
// without dropping valid model routes: non-2xx/3xx handled by status,
// non-object JSON, trailing data, decode errors and oversize bodies all
// yield {}. Exactly one probe runs per server per attempt (no per-model
// fan-out); timeouts, auth and route statuses are unchanged.
func (w *LlamaWorker) fetchProps(ctx context.Context, base string, headers map[string]string) map[string]any {
	resp, err := llamaGet(ctx, w.client, base+"/props?autoload=false", headers)
	if err != nil {
		return map[string]any{}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return map[string]any{}
	}
	raw, err := readBoundedBody(resp)
	if err != nil {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return map[string]any{}
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return map[string]any{}
	}
	if m, ok := payload.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// mapRows maps valid rows to canonical entries in fetch order, attaching
// one closed per-row observation from the shared global props (or unknown
// when router/swap gates them out). All statuses publish: the
// loaded/sleeping filter gates only the deferred optional per-model props
// fan-out, never publication here. Unknown, blank or duplicate IDs are
// skipped by the registry's first-wins build.
func (w *LlamaWorker) mapRows(rows []any, props map[string]any, mode string) []router.LlamaEntry {
	var out []router.LlamaEntry
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		rawID, ok := m["id"].(string)
		if !ok {
			continue
		}
		id := catalog.StripModelID(rawID)
		if id == "" {
			continue
		}
		public := id
		if id == "local" {
			public = "local-llama"
		}
		public = w.prefix + public
		aliases := llamaRowAliases(m)
		if !w.exposeAliases {
			aliases = nil
		} else {
			prefixed := make([]string, 0, len(aliases))
			for _, a := range aliases {
				if a == "" {
					continue
				}
				prefixed = append(prefixed, w.prefix+a)
			}
			aliases = prefixed
		}
		entry := router.LlamaEntry{Name: public, Upstream: id, Aliases: aliases, Obs: capability.ExtractLlamaObs(m, props, mode)}
		if meta, ok := m["metadata"].(map[string]any); ok {
			if raw, err := json.Marshal(meta); err == nil {
				entry.Metadata = raw
			}
		}
		out = append(out, entry)
	}
	return out
}

// llamaRowAliases prefers nested meta.llamaswap aliases only when it is a
// list; otherwise it falls back to row aliases. A bare string means one
// alias. Non-string payload aliases are skipped, never coerced.
func llamaRowAliases(row map[string]any) []string {
	var candidates any
	if meta, ok := row["meta"].(map[string]any); ok {
		if swap, ok := meta["llamaswap"].(map[string]any); ok {
			if list, ok := swap["aliases"].([]any); ok {
				candidates = list
			}
		}
	}
	if candidates == nil {
		candidates = row["aliases"]
	}
	if s, ok := candidates.(string); ok {
		candidates = []any{s}
	}
	list, ok := candidates.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			continue
		}
		if trimmed := catalog.StripModelID(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// llamaProbeHeaders seeds trusted global/server headers, scrubs auth
// names, then emits the configured non-empty server key as Bearer. No
// placeholder is emitted when empty; catalog/client auth never applies.
func llamaProbeHeaders(srv llamaServer) map[string]string {
	out := make(map[string]string, len(srv.headers)+2)
	out["User-Agent"] = "model-router-go/" + routerVersion
	for k, v := range srv.headers {
		if llamaAuthScrubbed[strings.ToLower(k)] {
			continue
		}
		out[k] = v
	}
	if key := strings.TrimSpace(srv.key); key != "" {
		delete(out, "Authorization")
		out["Authorization"] = "Bearer " + key
	}
	return out
}

func llamaGet(ctx context.Context, client *http.Client, url string, headers map[string]string) (*http.Response, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	for _, k := range names {
		req.Header.Set(k, headers[k])
	}
	return client.Do(req)
}

// decodeLlamaRows enforces the JSON shape: a mapping yields
// payload["data"] else payload["models"] else the bare value, and the
// result must be a list.
func decodeLlamaRows(raw []byte) ([]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // preserve raw numeric literals in verbatim metadata
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return nil, ErrLlamaShape
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, ErrLlamaShape
	}
	rows := payload
	if m, ok := payload.(map[string]any); ok {
		rows = m["data"]
		if _, ok := rows.([]any); !ok {
			rows = m["models"]
		}
	}
	list, ok := rows.([]any)
	if !ok {
		return nil, ErrLlamaShape
	}
	return list, nil
}

// errLlamaBody maps the shared bounded-body failures onto static llama
// sentinels without copying their allowlists.
func errLlamaBody(err error) error {
	switch err {
	case ErrCatalogTooLarge:
		return ErrLlamaModels
	case ErrCatalogEncoding:
		return ErrLlamaModels
	case ErrCatalogBody:
		return ErrLlamaModels
	default:
		return ErrLlamaModels
	}
}

// seedRetained installs restored per-server retention under the worker's
// existing stateMu, marked UnreachableStale, never Reachable. Only called
// after an accepted restore and before the first probe. Unseeded, the
// first failed probe would see active=false and drop restored routes.
// Restored entries carry no observations (route-only v2 cache): the copy
// preserves that nil, never fabricating observations from metadata.
func (w *LlamaWorker) seedRetained(inv router.DiscoveryInventory) {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	for name, unit := range inv.Llama {
		cp := make([]router.LlamaEntry, 0, len(unit.Entries))
		for _, e := range unit.Entries {
			ec := router.LlamaEntry{Name: e.Name, Upstream: e.Upstream, Obs: e.Obs.Clone()}
			if e.Aliases != nil {
				ec.Aliases = append([]string(nil), e.Aliases...)
			}
			if e.Metadata != nil {
				ec.Metadata = append([]byte(nil), e.Metadata...)
			}
			cp = append(cp, ec)
		}
		w.active[name] = true
		w.retained[name] = cp
		// Restored entries carry no engine: the layer already pins
		// Unobserved, and retention must match it until a live probe.
		w.retainedRuntime[name] = capability.RuntimeUnobserved
	}
}

// cloneLlamaEntries deep-copies entries across the worker/registry
// boundary (aliases, metadata bytes and closed observations).
func cloneLlamaEntries(in []router.LlamaEntry) []router.LlamaEntry {
	if in == nil {
		return nil
	}
	out := make([]router.LlamaEntry, 0, len(in))
	for _, e := range in {
		ec := router.LlamaEntry{Name: e.Name, Upstream: e.Upstream, Obs: e.Obs.Clone()}
		if e.Aliases != nil {
			ec.Aliases = append([]string(nil), e.Aliases...)
		}
		if e.Metadata != nil {
			ec.Metadata = append([]byte(nil), e.Metadata...)
		}
		out = append(out, ec)
	}
	return out
}

// CacheAttach sets the persistence owner notified after accepted
// publications. A nil store disables notifications.
func (w *LlamaWorker) CacheAttach(s *CacheStore) {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	w.cache = s
}
