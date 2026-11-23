package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/config"
	"github.com/arhammahajan/model-router/internal/discovery"
	"github.com/arhammahajan/model-router/internal/server"
)

func TestCLIProxyAPIDiscoveryAndTranslation(t *testing.T) {
	const model = "opencode-go/test-model"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer proxy-key" {
			t.Errorf("proxy did not receive its configured access key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Api-Key") != "" {
			t.Error("client API key reached proxy")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, model)
		case "/v1/chat/completions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body["model"] != model {
				t.Errorf("proxy model = %v, want opaque ID %q", body["model"], model)
			}
			if _, ok := body["messages"].([]any); !ok {
				t.Error("request was not translated into Chat messages")
			}
			if body["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-proxy\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n", model)
				fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-proxy\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n", model)
				return
			}
			fmt.Fprintf(w, `{"id":"chatcmpl-proxy","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`, model)
		default:
			t.Errorf("unexpected proxy path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer proxy.Close()
	path := filepath.Join(t.TempDir(), "router.toml")
	raw := fmt.Sprintf(`[server]
api_keys = ["client-key"]
allow_dynamic_routes = false
[cliproxyapi]
base_url = %q
api_key = "proxy-key"
`, proxy.URL+"/v1")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWithOptions(path, config.LoadOptions{NoDefaultEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	app := server.New(cfg)
	defer app.CloseIdleConnections()
	worker := discovery.NewCatalogWorker(cfg.Catalog, cfg.Providers, app.Registry())
	defer worker.CloseIdleConnections()
	worker.RefreshOnce(context.Background())
	router := httptest.NewServer(app.Handler())
	defer router.Close()
	cases := []struct{ name, path, body, want string }{
		{"models", "/v1/models", "", model},
		{"chat", "/v1/chat/completions", `{"model":"opencode-go/test-model","messages":[{"role":"user","content":"Hi"}]}`, `"content":"hello"`},
		{"responses", "/v1/responses", `{"model":"opencode-go/test-model","input":"Hi"}`, `"output_text"`},
		{"anthropic", "/v1/messages", `{"model":"opencode-go/test-model","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`, `"text":"hello"`},
		{"chat stream", "/v1/chat/completions", `{"model":"opencode-go/test-model","messages":[{"role":"user","content":"Hi"}],"stream":true}`, "data: [DONE]"},
		{"responses stream", "/v1/responses", `{"model":"opencode-go/test-model","input":"Hi","stream":true}`, "response.completed"},
		{"gemini", "/v1beta/models/opencode-go/test-model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`, `"text":"hello"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodPost
			if tc.body == "" {
				method = http.MethodGet
			}
			req, err := http.NewRequest(method, router.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer client-key")
			req.Header.Set("Content-Type", "application/json")
			resp, err := router.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), tc.want) {
				t.Fatalf("status=%d body=%s, want %s", resp.StatusCode, body, tc.want)
			}
		})
	}
}
