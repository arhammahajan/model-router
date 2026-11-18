package protocol

import (
	"strings"
	"testing"
)

func TestReviewAllowedToolsNamespaceFlattensToLoss(t *testing.T) {
	raw := `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"muse","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":false}]}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"lookup","namespace":"muse"}]}}`
	for _, dest := range []responsesDestination{responsesDestChat, responsesDestAnthropic, responsesDestGemini} {
		gen, losses, rerr := decodeResponsesSource([]byte(raw), DefaultLimits(), dest)
		if rerr != nil {
			t.Fatalf("dest %d: namespaced allowed_tools member must validate then drop as loss, got hard error %v", dest, rerr)
		}
		if len(losses) != 1 || losses[0].Field != "tool_choice" || losses[0].Action != "dropped" {
			t.Fatalf("dest %d: expected one dropped tool_choice loss, got %v", dest, losses)
		}
		if _, err := EnforcePolicy(losses, false); err == nil || err.Code != CodeTranslationLoss {
			t.Fatalf("dest %d: strict policy must refuse the loss: %v", dest, err)
		}
		if _, err := EnforcePolicy(losses, true); err != nil || gen.Choice != nil {
			t.Fatalf("dest %d: lossy policy must admit with no forced choice: %v", dest, err)
		}
		for _, namespace := range []string{`"missing"`, `17`, `"\ud800"`} {
			invalid := strings.Replace(raw, `"namespace":"muse"`, `"namespace":`+namespace, 1)
			if _, _, err := decodeResponsesSource([]byte(invalid), DefaultLimits(), dest); err == nil || err.Code != CodeInvalidRequest {
				t.Fatalf("dest %d: invalid namespace member must fail before loss policy: %v", dest, err)
			}
		}
	}
}
