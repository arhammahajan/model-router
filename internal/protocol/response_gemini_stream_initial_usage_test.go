package protocol

import "testing"

func TestGeminiStreamInitialUsagePreservesReportedCounts(t *testing.T) {
	for _, sse := range []bool{true, false} {
		d := NewGeminiStreamDecoder(DefaultLimits(), sse)
		first := gsBuild(gsTextPart("a"), "", `{"promptTokenCount":12,"cachedContentTokenCount":4,"candidatesTokenCount":1,"totalTokenCount":13}`, "")
		events, rerr := d.DecodeFrame(Frame{Data: first})
		if rerr != nil {
			t.Fatal(rerr)
		}
		start, ok := events[0].(ResponseStart)
		if !ok || start.InitialUsage == nil {
			t.Fatal("reported start accounting was dropped")
		}
		u := start.InitialUsage
		if u.Provenance.Source != DialectGemini || u.Provenance.Scope != UsageInitial ||
			u.Prompt == nil || *u.Prompt != 12 || u.CacheRead == nil || *u.CacheRead != 4 ||
			u.Completion == nil || *u.Completion != 1 || u.Total == nil || *u.Total != 13 {
			t.Fatalf("initial accounting changed: %+v", u)
		}
		// Initial accounting cannot substitute for the terminal chunk's own
		// counters, even when its values would otherwise be plausible.
		terminal := gsBuild(gsTextPart("b"), "STOP", "", "")
		if _, rerr := d.DecodeFrame(Frame{Data: terminal}); rerr == nil {
			t.Fatal("initial accounting was borrowed for a missing terminal usage")
		}
	}
}
