package discovery

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arhammahajan/router/internal/router"
)

type catalogReviewTransport func(*http.Request) (*http.Response, error)

func (f catalogReviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCatalogReviewStopPreventsFurtherRefresh(t *testing.T) {
	cfg, providers := workerCfg(t, "http://catalog.invalid")
	reg := router.NewRegistry(cfg)
	w := NewCatalogWorker(cfg.Catalog, providers, reg)
	var calls atomic.Int32
	w.client = &http.Client{Transport: catalogReviewTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`["m1"]`))}, nil
	})}
	before := reg.Current()
	w.Stop()
	w.RefreshOnce(context.Background())
	if calls.Load() != 0 || reg.Current() != before {
		t.Fatalf("refresh after Stop performed I/O or published: calls=%d changed=%v", calls.Load(), reg.Current() != before)
	}
}

func TestCatalogReviewCanceledAttemptNeverDials(t *testing.T) {
	cfg, providers := workerCfg(t, "http://catalog.invalid")
	w := NewCatalogWorker(cfg.Catalog, providers, router.NewRegistry(cfg))
	defer w.Stop()
	var calls atomic.Int32
	w.client = &http.Client{Transport: catalogReviewTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.RefreshOnce(ctx)
	if calls.Load() != 0 {
		t.Fatal("canceled attempt still dialed")
	}
}

func TestCatalogReviewRejectsTrailingJSON(t *testing.T) {
	for _, body := range []string{`[] []`, `{"data":[]}garbage`, `[] null`} {
		if _, err := decodeRows([]byte(body)); err != ErrCatalogJSON {
			t.Errorf("trailing data accepted: %q, err=%v", body, err)
		}
	}
	if _, err := decodeRows([]byte("[] \n\t")); err != nil {
		t.Fatalf("trailing whitespace rejected: %v", err)
	}
}

func TestCatalogReviewRejectsStackedEncodingHeaders(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Encoding": {"identity", "br"}}, Body: io.NopCloser(strings.NewReader(`[]`))}
	if _, err := readBoundedBody(resp); err != ErrCatalogEncoding {
		t.Fatalf("stacked encoding accepted: %v", err)
	}
}

func TestCatalogReviewCanceledLoopCannotRestart(t *testing.T) {
	cfg, providers := workerCfg(t, "http://catalog.invalid")
	cfg.Catalog.RefreshSeconds = 0.001
	w := NewCatalogWorker(cfg.Catalog, providers, router.NewRegistry(cfg))
	defer w.Stop()
	called := make(chan struct{}, 1)
	w.client = &http.Client{Transport: catalogReviewTransport(func(r *http.Request) (*http.Response, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Start(ctx)
	select {
	case <-w.loopDone:
	case <-time.After(time.Second):
		t.Fatal("canceled worker did not exit")
	}
	w.Start(context.Background())
	select {
	case <-called:
		t.Fatal("worker restarted a completed loop lifetime")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestCatalogReviewCanceledMappingCannotPublishPartialRows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	entries, unmapped, err := mapRows(ctx, []any{"exact-model", "ghost"}, testCatalog("http://catalog.invalid"), testProviders())
	if err != ErrCatalogHTTP || entries != nil || unmapped != nil {
		t.Fatalf("canceled mapping returned partial state: entries=%v unmapped=%v err=%v", entries, unmapped, err)
	}
}
