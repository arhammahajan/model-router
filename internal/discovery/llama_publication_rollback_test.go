package discovery

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/router"
)

func exportRoutes(snap *router.Snapshot) string {
	names := []string{}
	for _, m := range snap.Models() {
		names = append(names, m.Name+"=>"+m.Provider)
	}
	return strings.Join(names, ",")
}

func TestLlamaPublicationFailureDoesNotActivateWorkerState(t *testing.T) {
	srv := httptest.NewServer((&llamaProbe{propsOn: true, props: `{}`}).handler())
	defer srv.Close()
	cfg := llamaWorkerCfg(srv.URL, "untrusted")
	// Deliberately mismatched trusted inventory exercises publication rejection.
	reg := router.NewRegistry(&config.Config{})
	before := reg.Current()
	w := NewLlamaWorker(cfg.Llama, reg)
	defer w.CloseIdleConnections()
	defer w.Stop()
	w.RefreshOnce(context.Background())
	beforeInv := exportRoutes(before)
	afterInv := exportRoutes(reg.Current())
	if beforeInv != afterInv {
		t.Fatalf("rejected publication changed routes: %q vs %q", beforeInv, afterInv)
	}
	if len(w.active) != 0 || len(w.retained) != 0 {
		t.Fatalf("rejected publication changed retained state: active=%v entries=%d", w.active, len(w.retained))
	}
	if w.Failures() != 1 || w.LastError() == "" {
		t.Fatal("publication rejection must still report failure")
	}
}
