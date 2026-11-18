package capability

import (
	"encoding/json"
	"testing"
)

func TestLlamaFlatOutputDefaultsKeepTheirReportedPath(t *testing.T) {
	for _, key := range []string{"n_predict", "max_tokens"} {
		t.Run(key, func(t *testing.T) {
			props := map[string]any{
				"default_generation_settings": map[string]any{key: json.Number("128")},
			}
			obs := ExtractLlamaObs(nil, props, "single")
			fact := llamaObsFact(t, llamaObsWire(t, obs, 1000), "limits.default_output_tokens")
			want := "/props.default_generation_settings." + key
			if fact["source"] != want || fact["kind"] != KindReported || fact["value"] != float64(128) {
				t.Fatalf("flat default must retain its actual source %s: %v", want, fact)
			}
		})
	}
}
