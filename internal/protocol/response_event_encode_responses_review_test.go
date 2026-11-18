package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestResponsesEncoderRejectsEmptyToolFinishBeforeCommit(t *testing.T) {
	var out bytes.Buffer
	frames := ""
	for _, frame := range csTerm("tool_calls", 1, 1) {
		frames += "data: " + frame + "\n\n"
	}
	tr := ChatToResponsesSSETranslator{PublicModel: "public", Limits: DefaultLimits()}
	err := tr.Translate(strings.NewReader(frames), &out, nil)
	if err == nil {
		t.Fatal("call-less tool finish accepted")
	}
	if out.Len() != 0 {
		t.Fatalf("unrepresentable terminal committed %d bytes", out.Len())
	}
}

func TestResponsesEncoderPreservesSummaryProvenance(t *testing.T) {
	enc := NewResponsesEventEncoder("public", DefaultLimits())
	if err := enc.Encode(nil, nil, ResponseStart{ID: "actual-id", Model: "actual-model", Source: DialectResponses}); err != nil {
		t.Fatal(err)
	}
	usage, err := NewTerminalUsage(DialectResponses, 1, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(nil, nil, UsageEvent{Usage: usage}); err != nil {
		t.Fatal(err)
	}
	resp, err := enc.assembleResponse(Finish{Kind: FinishStop})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "actual-id" || resp.Model != "actual-model" || resp.Source != DialectResponses {
		t.Fatalf("source provenance changed: id=%s model=%s source=%s", resp.ID, resp.Model, resp.Source)
	}
}
