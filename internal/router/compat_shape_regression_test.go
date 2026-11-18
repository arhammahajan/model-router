package router

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCompatMarkedChatNullStopsMerge(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"system","content":null},{"role":"system","content":"b"}]}`,
		`{"messages":[{"role":"system","content":"a"},{"role":"system","content":null},{"role":"system","content":"b"}]}`,
	} {
		out, _, err := PrepareChatCompat(http.Header{"Authorization": {"Bearer " + compatTokA}}, []byte(body))
		if err != nil || string(out) != body {
			t.Fatalf("null must stop merge without changing the body: %s, %v", out, err)
		}
	}
	body := `{"messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},{"role":"system","content":null},{"role":"system","content":"c"}]}`
	out, _, err := PrepareChatCompat(http.Header{"Authorization": {"Bearer " + compatTokA}}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct{ Content json.RawMessage }
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || string(got.Messages[0].Content) != `"a\n\nb"` || string(got.Messages[1].Content) != "null" || string(got.Messages[2].Content) != `"c"` {
		t.Fatalf("merge crossed null barrier: %s", out)
	}
}

func TestCompatMarkedChatMinimalContentShape(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"messages":null}`,
		`{"messages":[{"role":"system","content":{}}]}`,
		`{"messages":[{"role":"system","content":["x"]}]}`,
		`{"messages":[{"role":"system","content":[42]}]}`,
		`{"messages":[{"role":"system","content":[null]}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, _, err := PrepareChatCompat(http.Header{"Authorization": {"Bearer " + compatTokA}}, []byte(body)); err == nil || err.Status != 400 {
				t.Fatalf("marked malformed body must fail: %v", err)
			}
			out, _, err := PrepareChatCompat(http.Header{}, []byte(body))
			if err != nil || string(out) != body {
				t.Fatalf("unmarked body must stay untouched: %s %v", out, err)
			}
		})
	}
	for _, body := range []string{
		`{"messages":[]}`,
		`{"messages":[{"role":"future","content":[{}]}]}`,
		`{"messages":[{"role":"system","content":""},{"role":"system","content":""}]}`,
	} {
		if _, _, err := PrepareChatCompat(http.Header{"Authorization": {"Bearer " + compatTokA}}, []byte(body)); err != nil {
			t.Fatalf("valid minimal shape refused: %v", err)
		}
	}
}

func TestCompatMarkedChatMergePreservesJSONStrings(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"\ud800"},{"role":"system","content":"a\\b\"c\udfff"},{"role":"user","content":"\ud800","n":9007199254740993}]}`)
	out, _, err := PrepareChatCompat(http.Header{"Authorization": {"Bearer " + compatTokA}}, body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct {
			Content json.RawMessage
			N       json.RawMessage
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || string(got.Messages[0].Content) != `"\ud800\n\na\\b\"c\udfff"` || string(got.Messages[1].Content) != `"\ud800"` || string(got.Messages[1].N) != `9007199254740993` {
		t.Fatalf("string merge must preserve surrogate code units, escapes and unrelated raw values: %s", out)
	}
}
