package config

import "testing"

func TestLlamaGeneratedNamesPreserveConfiguredURLSpelling(t *testing.T) {
	for _, tc := range []struct{ path, suffix string }{
		{"/v%31", "v-31"},
		{"/a%20b", "a-20b"},
		{"/café", "caf"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			cfg := loadLlama(t, `[providers.llama_cpp]
servers = ["http://127.0.0.1:9931`+tc.path+`"]
`)
			want := "llama-127.0.0.1-9931-" + tc.suffix
			if got := cfg.Llama.Servers[0].Name; got != want {
				t.Fatalf("generated provider identity=%q want %q", got, want)
			}
		})
	}
}
