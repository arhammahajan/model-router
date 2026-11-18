package protocol

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// validateGatewayBillingMetadata removes known gateway accounting fields after
// validation. Usage counters from the provider's protocol remain authoritative.
func validateGatewayBillingMetadata(obj map[string]json.RawMessage, path string, allowDrop bool) *RouterError {
	bad := &RouterError{Status: 502, Code: CodeUpstreamBad, Msg: "Upstream returned an invalid response"}
	unrep := func(what string) *RouterError {
		return &RouterError{Status: 502, Code: CodeUpstreamBad,
			Msg: "Upstream response feature " + strconv.Quote(what) + " has no representation"}
	}

	// usage.cost: numeric gateway cost.
	if raw, ok := obj["cost"]; ok {
		if isNull(raw) {
			delete(obj, "cost")
		} else {
			if !isGatewayNumber(raw) {
				return bad
			}
			if !allowDrop {
				return unrep(path + ".cost")
			}
			delete(obj, "cost")
		}
	}

	// usage.opencode: gateway billing object.
	raw, ok := obj["opencode"]
	if !ok {
		return nil
	}
	if isNull(raw) {
		delete(obj, "opencode")
		return nil
	}
	var outer map[string]json.RawMessage
	if len(raw) > 16<<10 || !utf8.Valid(raw) || json.Unmarshal(raw, &outer) != nil || outer == nil {
		return bad
	}
	for k := range outer {
		switch k {
		case "billing_source", "list_cost", "allowance", "tokens":
		default:
			return unrep(path + ".opencode")
		}
	}
	// billing_source: required string.
	rawBS, ok := outer["billing_source"]
	if !ok || isNull(rawBS) {
		return bad
	}
	var bs string
	if json.Unmarshal(rawBS, &bs) != nil || len(bs) > 1024 {
		return bad
	}
	// list_cost / allowance: required finite numbers.
	for _, k := range []string{"list_cost", "allowance"} {
		rawN, ok := outer[k]
		if !ok || isNull(rawN) {
			return bad
		}
		if !isGatewayNumber(rawN) {
			return bad
		}
	}
	// tokens: required closed object of non-negative ints.
	rawTokens, ok := outer["tokens"]
	if !ok || isNull(rawTokens) {
		return bad
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(rawTokens, &tokens); err != nil || tokens == nil {
		return bad
	}
	wantTokens := []string{"input", "output", "reasoning", "cache_read", "cache_write_5m", "cache_write_1h"}
	for k, value := range tokens {
		switch k {
		case "input", "output", "reasoning", "cache_read", "cache_write_5m", "cache_write_1h", "cache_write":
			if _, rerr := responseInt(value); rerr != nil {
				return bad
			}
		default:
			return unrep(path + ".opencode.tokens")
		}
	}
	for _, k := range wantTokens {
		rawN, ok := tokens[k]
		if !ok || isNull(rawN) {
			return bad
		}
	}
	if !allowDrop {
		return unrep(path + ".opencode")
	}
	delete(obj, "opencode")
	return nil
}

// isGatewayNumber reports whether raw is a finite JSON number literal
// of bounded length with a non-negative value.
func isGatewayNumber(raw json.RawMessage) bool {
	lit := strings.TrimSpace(string(raw))
	if lit == "" || len(lit) > 1024 || lit[0] == '"' {
		return false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return false
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return false
	}
	return true
}
