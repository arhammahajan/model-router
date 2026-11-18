package config

// Independent review probes for the [capabilities] load boundary (U2):
// every malformed override shape is a value-free startup error naming the
// model key and path; admission stays exactly as permissive as the Python
// source validator (no more, no less). Configs are temp TOML files decoded
// by the real loader, so go-toml types are exercised, not mocked.

import (
	"strings"
	"testing"
)

// Startup errors name the model key and path but never the value: a
// secret-looking value must not leak into the error text.
func TestReviewOverrideErrorsAreValueFree(t *testing.T) {
	p := writeCapCfg(t, capBase+`
[capabilities.overrides.a.tools]
supported = "s3cr3t-yes"
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("bad value must be a startup error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"a"`) || !strings.Contains(msg, "tools.supported") {
		t.Fatalf("error must name model key and path: %v", err)
	}
	if strings.Contains(msg, "s3cr3t") {
		t.Fatalf("error must never echo the value: %v", err)
	}
}

func TestReviewOverrideMalformedShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bool-as-int", "[capabilities.overrides.a.reasoning.budget]\nmax_tokens = true\n", "reasoning.budget.max_tokens"},
		{"negative-int", "[capabilities.overrides.a.reasoning.budget]\nmax_tokens = -1\n", "reasoning.budget.max_tokens"},
		{"float-as-int", "[capabilities.overrides.a.reasoning.budget]\nmax_tokens = 5.5\n", "reasoning.budget.max_tokens"},
		{"int-as-bool", "[capabilities.overrides.a]\nstreaming = 1\n", "streaming"},
		{"string-as-bool", "[capabilities.overrides.a]\nstreaming = \"true\"\n", "streaming"},
		{"scalar-values", "[capabilities.overrides.a.reasoning.effort]\nvalues = \"low\"\n", "reasoning.effort.values"},
		{"dup-values", "[capabilities.overrides.a.reasoning.effort]\nvalues = [\"low\", \"low\"]\n", "reasoning.effort.values"},
		{"empty-elem-values", "[capabilities.overrides.a.reasoning.effort]\nvalues = [\"low\", \"\"]\n", "reasoning.effort.values"},
		{"empty-default", "[capabilities.overrides.a.reasoning.effort]\ndefault = \"\"\n", "reasoning.effort.default"},
		{"default-outside-values", "[capabilities.overrides.a.reasoning.effort]\nvalues = [\"low\"]\ndefault = \"high\"\n", "reasoning.effort.default"},
		{"default-range", "[capabilities.overrides.a.reasoning.budget]\nmin_tokens = 1\nmax_tokens = 9\ndefault_tokens = 99\n", "reasoning.budget.default_tokens"},
		{"deep-unknown", "[capabilities.overrides.a.reasoning.effort]\nbogus = true\n", "reasoning.effort.bogus"},
		{"sampling-string-default", "[capabilities.overrides.a.sampling.temperature]\ndefault = \"hot\"\n", "sampling.temperature.default"},
		{"native-bad-protocol", "[capabilities.overrides.a.reasoning.effort]\n[capabilities.overrides.a.reasoning.effort.native_parameter]\nprotocol = \"http\"\npath = \"p\"\n", "reasoning.effort.native_parameter"},
		{"native-empty-path", "[capabilities.overrides.a.reasoning.effort]\n[capabilities.overrides.a.reasoning.effort.native_parameter]\nprotocol = \"openai-chat\"\npath = \"\"\n", "reasoning.effort.native_parameter"},
		{"native-extra-key", "[capabilities.overrides.a.reasoning.effort]\n[capabilities.overrides.a.reasoning.effort.native_parameter]\nprotocol = \"openai-chat\"\npath = \"p\"\nextra = 1\n", "reasoning.effort.native_parameter"},
		{"native-float-value", "[capabilities.overrides.a.reasoning.effort]\n[capabilities.overrides.a.reasoning.effort.native_parameter.values]\nx = 1.5\n", "reasoning.effort.native_parameter"},
		{"limits-prefix", "[capabilities.overrides.a.limits]\ncontext_window = 100\n", "limits"},
		{"empty-sampling-prefix", "[capabilities.overrides.a.sampling]\n", "sampling"},
		{"overrides-string", "[capabilities]\noverrides = \"x\"\n", "capabilities.overrides"},
		{"capabilities-scalar", "capabilities = 1\n", "capabilities"},
		{"blank-model", "[capabilities.overrides.\"   \".tools]\nsupported = true\n", "capabilities.overrides"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCapCfg(t, capBase+tc.body))
			if err == nil {
				t.Fatalf("%s: must be a startup error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: error must name %q: %v", tc.name, tc.want, err)
			}
		})
	}
}

func TestReviewOverrideAdmittedBoundary(t *testing.T) {
	p := writeCapCfg(t, capBase+`
[capabilities.overrides.a.sampling.temperature]
default = 1
[capabilities.overrides.a.reasoning.effort]
values = ["low", "high"]
default = "high"
[capabilities.overrides.a.reasoning.effort.native_parameter]
protocol = "openai-chat"
path = "reasoning_effort"
[capabilities.overrides.a.reasoning.effort.native_parameter.values]
off = "none"
blank = ""
[capabilities.overrides."a-alias".tools]
supported = true
[capabilities.overrides.zzz-unknown.tools]
supported = false
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("boundary admission: %v", err)
	}
	intV, ok := cfg.CapabilityOverrides["a"]["sampling.temperature.default"].Int()
	if !ok || intV != 1 {
		t.Fatalf("int sampling default must be admitted: %v", cfg.CapabilityOverrides["a"])
	}
	np, ok := cfg.CapabilityOverrides["a"]["reasoning.effort.native_parameter"].Native()
	if !ok {
		t.Fatal("native parameter must be admitted")
	}
	vals := np.Values()
	if vals["off"].Tag() != "string" || vals["blank"].Tag() != "string" {
		t.Fatalf("native mapping incl empty value must round-trip: %v", vals)
	}
	if _, ok := cfg.CapabilityOverrides["a-alias"]; !ok {
		t.Fatal("alias-keyed overrides validate (lookup stays canonical at listing)")
	}
	if _, ok := cfg.CapabilityOverrides["zzz-unknown"]; !ok {
		t.Fatal("unknown names must validate without warnings (later discovery may add them)")
	}
}
