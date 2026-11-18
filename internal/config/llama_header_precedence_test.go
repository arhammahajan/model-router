package config

import (
	"strings"
	"testing"
)

func TestLlamaHeadersServerWinsAcrossCasing(t *testing.T) {
	cfg := loadLlama(t, `[llama_cpp]
headers = { "X-Shared" = "global" }
servers = [{ url = "http://127.0.0.1:9931", name = "llama-test", headers = { "x-shared" = "server" } }]
`)
	for _, headers := range []map[string]string{cfg.Llama.Servers[0].Headers, cfg.LlamaProviders()["llama-test"].Headers} {
		count := 0
		for k, v := range headers {
			if strings.EqualFold(k, "x-shared") {
				count++
				if v != "server" {
					t.Fatalf("global header survived server override: %q", v)
				}
			}
		}
		if count != 1 {
			t.Fatalf("want one case-insensitive header, got %d", count)
		}
	}
}
