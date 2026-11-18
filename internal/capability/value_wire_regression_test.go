package capability_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func TestEmptyCapabilityListsRenderAsArrays(t *testing.T) {
	for _, path := range []string{
		"reasoning.effort.values", "protocols.known_lossy_features",
		"sampling.samplers.default", "sampling.temperature.constraints",
	} {
		t.Run(path, func(t *testing.T) {
			value, ok := capability.StringListValue(nil)
			if !ok {
				t.Fatal("empty lists are valid capability assertions")
			}
			contract := capability.Empty("openai-chat")
			source := "override." + path
			if err := contract.Put(path, value, &source, capability.KindOverride); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(capability.Render(contract, nil, capability.StaticDiscovery(), 0))
			if err != nil {
				t.Fatal(err)
			}
			var node any
			if err := json.Unmarshal(raw, &node); err != nil {
				t.Fatal(err)
			}
			for _, key := range strings.Split(path+".value", ".") {
				node = node.(map[string]any)[key]
			}
			if list, ok := node.([]any); !ok || len(list) != 0 {
				t.Fatalf("explicit empty list rendered as %#v, want []", node)
			}
		})
	}
}
