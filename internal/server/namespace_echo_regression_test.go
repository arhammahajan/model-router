package server

import (
	"encoding/json"
	"testing"

	"github.com/arhammahajan/router/internal/protocol"
)

func TestNamespaceEchoRestoresDeclarationsAndChoice(t *testing.T) {
	body := []byte(`{"model":"m","input":"Hi","tools":[{"type":"namespace","name":"one","description":"","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":false}]},{"type":"function","name":"plain","parameters":{"type":"object"},"strict":false},{"type":"namespace","name":"two","tools":[]}],"tool_choice":{"type":"function","namespace":"one","name":"f"}}`)
	for _, decode := range []func([]byte, protocol.Limits) (*protocol.Generation, []protocol.Loss, *protocol.RouterError){protocol.DecodeResponsesRequest, protocol.DecodeResponsesForAnthropic, protocol.DecodeResponsesForGemini} {
		gen, losses, err := decode(body, protocol.DefaultLimits())
		if err != nil || len(losses) != 0 {
			t.Fatalf("decode %v %v", err, losses)
		}
		alias := gen.Tools[0].Name
		tools, choice := responsesRequestEcho(gen)
		var got []struct {
			Type, Name  string
			Description *string
			Tools       []struct{ Name string }
		}
		if err := json.Unmarshal(tools, &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got[0].Type != "namespace" || got[0].Name != "one" || got[0].Description == nil || *got[0].Description != "" || len(got[0].Tools) != 1 || got[0].Tools[0].Name != "f" || got[1].Name != "plain" || got[2].Type != "namespace" || got[2].Name != "two" || got[2].Tools == nil || len(got[2].Tools) != 0 {
			t.Fatalf("source declarations and empty groups must not expose flattened aliases: %s", tools)
		}
		var selected map[string]string
		if err := json.Unmarshal(choice, &selected); err != nil {
			t.Fatal(err)
		}
		if selected["name"] != "f" || selected["namespace"] != "one" {
			t.Fatalf("namespaced forced choice lost: %s", choice)
		}
		if gen.Tools[0].Name != alias || gen.Choice.Name != alias {
			t.Fatal("source echo mutated the upstream plan")
		}
	}
}

func TestNamespaceEchoSkipsDroppedCachedSearch(t *testing.T) {
	body := []byte(`{"model":"m","input":"Hi","tool_choice":"auto","tools":[
		{"type":"namespace","name":"empty","tools":[]},
		{"type":"web_search","external_web_access":false},
		{"type":"namespace","name":"group","tools":[{"type":"function","name":"lookup","strict":false}]},
		{"type":"function","name":"plain","strict":false}
	]}`)
	gen, losses, rerr := protocol.DecodeResponsesRequest(body, protocol.DefaultLimits())
	if rerr != nil || len(losses) != 1 || losses[0].Field != "tools[1]" {
		t.Fatalf("unexpected decode: %v, %v", rerr, losses)
	}
	tools, _ := responsesRequestEcho(gen)
	var got []struct {
		Name  string
		Tools []struct{ Name string }
	}
	if err := json.Unmarshal(tools, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "empty" || len(got[0].Tools) != 0 ||
		got[1].Name != "group" || len(got[1].Tools) != 1 || got[1].Tools[0].Name != "lookup" ||
		got[2].Name != "plain" {
		t.Fatalf("incorrect echo after tool drop: %s", tools)
	}
}
