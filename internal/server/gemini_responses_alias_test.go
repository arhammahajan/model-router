package server

import (
	"encoding/json"
	"github.com/arhammahajan/router/internal/config"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestGeminiResponsesRequestedAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.toml")
	raw := `[server]
auth_mode="none"
[providers.up]
dialect="openai-responses"
base_url="http://127.0.0.1:9"
[[models]]
name="canonical"
aliases=["public-alias"]
provider="up"
model="upstream"
`
	if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	cfg, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	h := newRGBridgeHarness(t, cfg)
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(grResponsesJSONText))
	}
	status, _, body := postGeminiBridge(t, h.router.URL+"/v1beta/models/public-alias:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	var obj struct {
		Model string `json:"modelVersion"`
	}
	if e := json.Unmarshal(body, &obj); e != nil {
		t.Fatal(e)
	}
	if obj.Model != "public-alias" {
		t.Fatalf("modelVersion=%q, want requested alias", obj.Model)
	}
}
