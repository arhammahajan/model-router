package protocol

import (
	"encoding/json"
	"testing"
)

func TestNamespaceParentFieldsCannotDisappear(t *testing.T) {
	for _, group := range []string{
		`{"type":"namespace","name":"n","tools":[],"private_semantics":true}`,
		`{"type":"namespace","name":"n","children":[]}`,
		`{"type":"namespace","name":"n","tools":[],"children":[]}`,
		`{"type":"namespace","name":"n","tools":[],"description":17}`,
	} {
		body := []byte(`{"model":"m","input":"Hi","tools":[` + group + `]}`)
		for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){DecodeResponsesRequest, DecodeResponsesForAnthropic, DecodeResponsesForGemini} {
			if _, _, err := decode(body, DefaultLimits()); err == nil {
				t.Fatalf("namespace fields must validate rather than disappear: %s", group)
			}
		}
	}
}

func TestNamespaceWhitespaceIdentityIsPreserved(t *testing.T) {
	body := []byte(`{"model":"m","input":"Hi","tools":[{"type":"namespace","name":" ","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}]}`)
	gen, _, err := DecodeResponsesRequest(body, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	call, err := NewToolCallItem("i", "c", gen.Tools[0].Name, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := gen.ToolPlan.RestoreResponse(Response{Source: DialectChat, Items: []ResponseItem{call}}, DefaultLimits())
	if err != nil || restored.Items[0].(ToolCallItem).Namespace != " " {
		t.Fatalf("namespace is an opaque nonempty source identity: %+v %v", restored.Items, err)
	}
	open, err := NewBlockOpenWithNamespace("i", BlockTool, "c", " ", "f")
	if err != nil || open.Namespace != " " {
		t.Fatalf("stream namespace normalization loses source identity: %+v %v", open, err)
	}
}

func TestNamespaceHistoryWithoutCurrentDeclaration(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"function_call","namespace":"history","name":"f","call_id":"call-1","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"done"}]}`)
	for _, decode := range []func([]byte, Limits) (*Generation, []Loss, *RouterError){DecodeResponsesRequest, DecodeResponsesForAnthropic, DecodeResponsesForGemini} {
		gen, _, err := decode(body, DefaultLimits())
		if err != nil {
			t.Fatalf("complete history binds identities independently of currently exposed tools: %v", err)
		}
		if alias, ok := gen.ToolPlan.AliasFor("history", "f"); !ok || alias != NamespaceAlias("history", "f") {
			t.Fatal("historical namespace missing its explicit binding")
		}
	}
}

func TestNamespaceRejectsInvalidUnicodeAtEveryReference(t *testing.T) {
	const tools = `"tools":[{"type":"namespace","name":"\ufffd","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":false}]}]`
	for _, body := range []string{
		`{"model":"m","input":"Hi","tools":[{"type":"namespace","name":"\ud800","tools":[]}]}`,
		`{"model":"m","input":[{"type":"function_call","namespace":"\udc00","name":"f","call_id":"call-1","arguments":"{}"}],` + tools + `}`,
		`{"model":"m","input":"Hi",` + tools + `,"tool_choice":{"type":"function","namespace":"\ud800","name":"f"}}`,
	} {
		if _, _, err := DecodeResponsesRequest([]byte(body), DefaultLimits()); err == nil || err.Status != 400 {
			t.Fatalf("invalid Unicode identity must fail instead of becoming a replacement character: %v", err)
		}
	}
	body := []byte(`{"model":"m","input":"Hi","tools":[{"type":"namespace","name":"\ud83d\ude00","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":false}]}],"tool_choice":{"type":"function","namespace":"😀","name":"f"}}`)
	gen, losses, err := DecodeResponsesRequest(body, DefaultLimits())
	if err != nil || len(losses) != 0 || gen.Choice.Name != NamespaceAlias("😀", "f") {
		t.Fatalf("an escaped surrogate pair and its UTF-8 spelling must share an identity: %v %v", err, losses)
	}
}

func TestNamespaceDescriptionHasExplicitLoss(t *testing.T) {
	body := []byte(`{"model":"m","input":"Hi","tools":[{"type":"namespace","name":"n","description":"Instructions for the group","tools":[]}]}`)
	_, losses, err := DecodeResponsesRequest(body, DefaultLimits())
	if err != nil || len(losses) != 1 || losses[0].Field != "tools[0].description" || losses[0].Action != "dropped" {
		t.Fatalf("group description must surface as a loss: %v %v", err, losses)
	}
}

func TestNamespaceChildValidationUsesSourcePath(t *testing.T) {
	body := []byte(`{"model":"m","input":"Hi","tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"f","parameters":[]}]}]}`)
	_, _, err := DecodeResponsesRequest(body, DefaultLimits())
	if err == nil || err.Msg != "'tools[0].tools[0].parameters' must be a JSON object" {
		t.Fatalf("child schema errors must point to the original field: %v", err)
	}
}
