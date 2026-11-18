// Independent review: native/translated end-to-end edge boundaries.
// Hermetic fakes only.
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/arhammahajan/model-router/internal/capability"
)

func TestAuditServerNativeNonePreservesBudget(t *testing.T) {
	// End to end: none consumes effort, defaults kwargs, and leaves an
	// existing budget member untouched (including null).
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	for _, body := range []string{
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none","thinking_budget_tokens":777}`,
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none","thinking_budget_tokens":null}`,
	} {
		status, _, _ := postChat(t, rsrv.URL+"/v1/chat/completions", body)
		if status != 200 {
			t.Fatalf("none+budget: %d", status)
		}
		got := up.last(t)
		if _, has := got["reasoning_effort"]; has {
			t.Fatalf("effort must be consumed: %v", got)
		}
		if _, has := got["thinking_budget_tokens"]; !has {
			t.Fatalf("none must preserve budget member: %v", got)
		}
		kwargs, ok := got["chat_template_kwargs"].(map[string]any)
		if !ok || kwargs["enable_thinking"] != false {
			t.Fatalf("none kwargs: %v", got)
		}
	}
}

func TestAuditServerNativeBudgetPreservesKwargs(t *testing.T) {
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	status, _, _ := postChat(t, rsrv.URL+"/v1/chat/completions",
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","chat_template_kwargs":{"enable_thinking":true,"tag":"k"}}`)
	if status != 200 {
		t.Fatalf("budget+kwargs: %d", status)
	}
	got := up.last(t)
	kwargs, ok := got["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != true || kwargs["tag"] != "k" {
		t.Fatalf("budget must preserve kwargs: %v", got)
	}
	raw, _ := json.Marshal(got["thinking_budget_tokens"])
	if string(raw) != "2048" {
		t.Fatalf("budget: %v", got)
	}
}

func TestAuditServerStrataPassthroughExact(t *testing.T) {
	// Strata native passes the effort member through untouched.
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeStrata, "")
	body := `{"model":"llama-chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`
	status, _, _ := postChat(t, rsrv.URL+"/v1/chat/completions", body)
	if status != 200 {
		t.Fatalf("strata: %d", status)
	}
	got := up.last(t)
	if got["reasoning_effort"] != "low" {
		t.Fatalf("strata must pass effort through: %v", got)
	}
	if _, has := got["thinking_budget_tokens"]; has {
		t.Fatalf("strata must not add budget: %v", got)
	}
}

func TestAuditServerUnobservedAbsentNoRefusal(t *testing.T) {
	// Unobserved refuses only supplied effort; absent effort still routes.
	up, rsrv, _ := llamaVerticalServer(t, capability.RuntimeUnobserved, "")
	_ = up
	status, _, _ := postChat(t, rsrv.URL+"/v1/chat/completions",
		`{"model":"llama-chat","messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("unobserved absent must route: %d", status)
	}
	// Translated absent effort on Unobserved also routes (no pre-I/O refusal).
	status, _, data := postRev(t, rsrv.URL+"/v1/responses",
		`{"model":"llama-chat","input":"Hi"}`)
	if status != 200 {
		t.Fatalf("unobserved translated absent must route: %d %s", status, data)
	}
}

func TestAuditServerListingNoInventedClaims(t *testing.T) {
	_, rsrv, _ := llamaVerticalServer(t, capability.RuntimeLlamaCpp, "")
	req, _ := http.NewRequest(http.MethodGet, rsrv.URL+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	for _, r := range payload["data"].([]any) {
		row := r.(map[string]any)
		if row["id"] != "llama-chat" {
			continue
		}
		raw, _ := json.Marshal(row["metadata"])
		s := string(raw)
		if !strings.Contains(s, "router.llama_thinking_map.supported") {
			t.Fatalf("listing must carry mapper facts: %s", s)
		}
		if strings.Contains(s, `"reasoning":{"supported":{"value":true`) {
			t.Fatalf("listing must not invent reasoning.supported: %s", s)
		}
		if strings.Contains(s, "budget") && strings.Contains(s, `"supported":{"value":true`) {
			t.Fatalf("listing must not invent budget support: %s", s)
		}
		return
	}
	t.Fatalf("llama-chat missing")
}
