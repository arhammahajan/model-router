package server

import (
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func TestCapabilityOverridesOwnedByPinnedSnapshot(t *testing.T) {
	configFor := func(value string) string {
		return writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[capabilities.overrides.a.tools]
supported = `+value+"\n")
	}
	cfg := mustLoad(t, configFor("true"))
	replacement := mustLoad(t, configFor("false"))
	s := New(cfg)
	assertSupported := func() {
		t.Helper()
		body := listedBody(t, s, nil)
		row := body["data"].([]any)[0].(map[string]any)
		contract := rowRouter(t, row)
		fact := contract["tools"].(map[string]any)["supported"].(map[string]any)
		if fact["value"] != true || fact["kind"] != "override" {
			t.Fatalf("caller config changed snapshot-owned capability: %v", fact)
		}
	}
	assertSupported()
	cfg.CapabilityOverrides["a"] = replacement.CapabilityOverrides["a"]
	assertSupported()
}

// TestCapabilityOverridesDeepCopiedAtSnapshot proves the snapshot owns its
// facts: nested mutation of the source config and mutation of a returned
// reader copy both leave the rendered listing unchanged.
func TestCapabilityOverridesDeepCopiedAtSnapshot(t *testing.T) {
	p := writeStaticCfg(t, staticBase+`
[[models]]
name = "a"
provider = "up"
model = "x"
[capabilities.overrides.a.tools]
supported = true
`)
	cfg := mustLoad(t, p)
	s := New(cfg)
	// Representative mutation 1: nested map write into the source config.
	cfg.CapabilityOverrides["a"]["tools.supported"] = capability.BoolValue(false)
	// Representative mutation 2: write into a returned reader copy.
	returned := s.Registry().Current().CapabilityOverrides("a")
	returned["tools.supported"] = capability.BoolValue(false)
	body := listedBody(t, s, nil)
	row := body["data"].([]any)[0].(map[string]any)
	contract := rowRouter(t, row)
	fact := contract["tools"].(map[string]any)["supported"].(map[string]any)
	if fact["value"] != true || fact["kind"] != "override" {
		t.Fatalf("snapshot-owned facts must survive caller mutation: %v", fact)
	}
}
