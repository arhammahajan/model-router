package server

import (
	"net/http"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/protocol"
	"github.com/arhammahajan/model-router/internal/router"
)

// prepareSession is the single shared affinity prep for all sixteen
// generation paths (four native, twelve bridged). It selects the
// request-scoped identity and, on translated routes with an enabled
// prompt_cache_key fallback, consumes that top-level body field before
// protocol validation. Native bodies pass through byte-identical. fb is
// the final typed Gemini compat fallback, selected only when no earlier
// candidate won.
//
// translated must reflect the dispatched mode (mode != "native"). The
// caller renders rerr in its ingress envelope; no upstream I/O has happened
// when rerr is non-nil. Token counting never calls this: counting is not
// generation and keeps its current behavior.
func prepareSession(p *config.Provider, hdr http.Header, body []byte, translated bool, fb router.CompatFallback) (router.Session, []byte, *protocol.RouterError) {
	return router.PrepareSession(p, hdr, body, translated, fb)
}
