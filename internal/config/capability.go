// Capability overrides: sparse, validated, keyed by canonical route name.
//
// [capabilities] admits ONLY the overrides table; any other key is a
// startup error. Validation is exact-ported from Python
// router_metadata.validate_overrides via the neutral capability module:
// unknown field / dotted key / empty object / bad value / broken relation
// is a startup error naming the model key and path (value-free). Unknown
// or shadowed names validate without warnings and apply only when a
// canonical route of that name exists; alias keys never apply (listing
// resolves canonical rows only).
package config

import (
	"fmt"

	"github.com/arhammahajan/router/internal/capability"
)

var capabilityKeys = map[string]bool{"overrides": true}

func parseCapabilityOverrides(doc map[string]any) (map[string]capability.Overrides, error) {
	raw, ok := doc["capabilities"]
	if !ok {
		return map[string]capability.Overrides{}, nil
	}
	tab, ok := asTable(raw)
	if !ok {
		return nil, fmt.Errorf("capabilities: expected a table containing only 'overrides'")
	}
	if err := rejectUnknown(tab, capabilityKeys, "capabilities",
		" (supported: overrides)"); err != nil {
		// Normalize to the frozen Python shape: only 'overrides' allowed.
		return nil, fmt.Errorf("capabilities: expected a table containing only 'overrides'")
	}
	var overridesRaw any = map[string]any{}
	if v, has := tab["overrides"]; has {
		overridesRaw = v
	}
	validated, err := capability.ValidateOverrides(overridesRaw)
	if err != nil {
		return nil, err
	}
	return validated, nil
}
