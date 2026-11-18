// Provenance-aware cache accounting: disjoint vs overlapping shares.
//
// Anthropic (input excludes cache buckets) and Responses (r2a rejects
// cache_write > input-cacheRead) report disjoint portions: creation+
// read must fit inside the prompt. Chat prompt_tokens_details shares may
// overlap (no legacy proof of disjointness), so only individual bounds
// plus overflow safety apply there.
package protocol

import (
	"testing"
)

func TestResponseIRCacheProvenanceCombined(t *testing.T) {
	// Anthropic: 8+8 in prompt 10 is contradictory (disjoint portions).
	ua, rerr := NewTerminalUsage(DialectAnthropic, 10, 2, 12)
	if rerr != nil {
		t.Fatalf("NewTerminalUsage: %v", rerr)
	}
	if _, rerr := ua.WithCache(intPtr(8), intPtr(8)); rerr == nil {
		t.Error("anthropic 8+8/prompt 10 accepted; must refuse combined overflow of disjoint shares")
	}
	// Responses: same disjoint policy (r2aParseUsage rejects it).
	ur, rerr := NewTerminalUsage(DialectResponses, 10, 2, 12)
	if rerr != nil {
		t.Fatalf("NewTerminalUsage: %v", rerr)
	}
	if _, rerr := ur.WithCache(intPtr(8), intPtr(8)); rerr == nil {
		t.Error("responses 8+8/prompt 10 accepted; must refuse")
	}
	// Literals with the same contradiction fail ValidateAuthoritative
	// exactly like malformed wire bytes (no bool+int smuggling).
	bad := mustTerminalUsage(t, 10, 2)
	bad.Provenance.Source = DialectAnthropic
	eight := 8
	bad.CacheCreation, bad.CacheRead = &eight, &eight
	ten := 10
	bad.Prompt = &ten
	if rerr := bad.ValidateAuthoritative(); rerr == nil {
		t.Error("anthropic literal 8+8/prompt 10 accepted as authoritative")
	}
	// Chat: overlapping shares may exceed the prompt (extension
	// cache_write may overlap cached share); only individual bounds
	// apply. No blanket combined rejection without source proof.
	uc, rerr := NewTerminalUsage(DialectChat, 10, 2, 12)
	if rerr != nil {
		t.Fatalf("NewTerminalUsage: %v", rerr)
	}
	uc, rerr = uc.WithCache(intPtr(8), intPtr(8))
	if rerr != nil {
		t.Fatalf("chat overlapping 8+8/prompt 10 refused; must allow: %v", rerr)
	}
	if rerr := uc.ValidateAuthoritative(); rerr != nil {
		t.Fatalf("chat overlapping authoritative: %v", rerr)
	}
	// Overflow safety still holds everywhere: maxint shares fail.
	huge := int(^uint(0) >> 1)
	uh := mustTerminalUsage(t, 5, 2)
	if _, rerr := uh.WithCache(intPtr(huge), intPtr(1)); rerr == nil {
		t.Error("overflowing cache combination accepted")
	}
}
