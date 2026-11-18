package server

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchNoCallRespectsOutputBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h := newSearchHarness(t, searchTestCfg(t))
			h.chatHandler = func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(chatCompletionText("over cap", 7, 3)))
			}
			body := fmt.Sprintf(`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":%t,"max_output_tokens":2}`, stream)
			status, headers, reply := postSearch(t, h.router.URL+"/v1/responses", body, nil)
			if status != http.StatusBadGateway || strings.HasPrefix(headers.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("uncalled search above cap must fail before commitment: %d %s", status, reply)
			}
			if h.chatCalls != 1 || atomic.LoadInt64(&h.searchCalls) != 0 {
				t.Fatal("no-call branch must use only one model request")
			}
		})
	}
}

func TestSearchStreamDeliversBeforeUpstreamTerminalAndClosesOnDone(t *testing.T) {
	h := newSearchHarness(t, searchTestCfg(t))
	releaseTerminal := make(chan struct{}, 1)
	defer func() {
		select {
		case releaseTerminal <- struct{}{}:
		default:
		}
	}()
	upstreamClosed := make(chan struct{})
	h.chatHandler = func(call int, w http.ResponseWriter, r *http.Request) {
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionWithCall("one", `{"query":"public example"}`)))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", searchStreamChunk(`{"content":"first delta"}`, ""))
		w.(http.Flusher).Flush()
		select {
		case <-releaseTerminal:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n",
			searchStreamChunk(`{}`, "stop"), searchStreamUsageChunk(11, 5))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamClosed)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(h.router.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"search-bound","input":"Hi","tools":[{"type":"web_search_preview"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("first answer delta must arrive before the upstream terminal: %v", err)
		}
		if strings.Contains(line, `"delta":"first delta"`) {
			break
		}
	}
	releaseTerminal <- struct{}{}
	done, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(done), "event: response.completed") {
		t.Fatalf("stream did not complete: %q (%v)", done, err)
	}
	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream kept open after a complete stream")
	}
}
