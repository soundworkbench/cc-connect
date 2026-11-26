package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestParsePromptUsage(t *testing.T) {
	u, ok := parsePromptUsage(json.RawMessage(`{"stopReason":"end_turn","usage":{"inputTokens":100,"outputTokens":20,"thoughtTokens":7,"totalTokens":127,"cachedReadTokens":60,"cachedWriteTokens":5}}`))
	want := promptUsage{Input: 100, Output: 20, Thought: 7, Total: 127, CachedRead: 60, CachedWrite: 5}
	if !ok || u != want {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	if _, ok := parsePromptUsage(json.RawMessage(`{"stopReason":"end_turn"}`)); ok {
		t.Fatal("no usage must report ok=false")
	}
	if _, ok := parsePromptUsage(json.RawMessage(`{"usage":{}}`)); ok {
		t.Fatal("all-zero usage must report ok=false")
	}
}

// runPrompt answers one session/prompt with the given usage_update used
// values (sent first) and result, and returns the EventResult.
func runPrompt(t *testing.T, used []int, result string) core.Event {
	t.Helper()
	s, wResp, rReq := newTestSession(t, nil)
	go func() {
		br := bufio.NewReader(rReq)
		line, err := br.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(line, &req)
		for _, u := range used {
			fmt.Fprintf(wResp, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"test-session-id","update":{"sessionUpdate":"usage_update","used":%d,"size":1000000}}}`+"\n", u)
		}
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", req.ID, result)
	}()
	if err := s.Send("hi", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-s.events:
			if ev.Type == core.EventResult {
				return ev
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no EventResult")
		}
	}
}

func TestSend_ResultCarriesFinalUsageOnce(t *testing.T) {
	ev := runPrompt(t, []int{500, 900},
		`{"stopReason":"end_turn","usage":{"inputTokens":100,"outputTokens":20,"thoughtTokens":7,"totalTokens":127,"cachedReadTokens":60,"cachedWriteTokens":5}}`)
	if ev.InputTokens != 100 || ev.OutputTokens != 27 || ev.CacheReadInputTokens != 60 || ev.CacheCreationInputTokens != 5 {
		t.Fatalf("event usage = in %d out %d cr %d cw %d", ev.InputTokens, ev.OutputTokens, ev.CacheReadInputTokens, ev.CacheCreationInputTokens)
	}
}

func TestSend_UsageUpdateAloneCountedOnce(t *testing.T) {
	ev := runPrompt(t, []int{500, 900}, `{"stopReason":"end_turn"}`)
	if ev.InputTokens != 900 || ev.OutputTokens != 0 {
		t.Fatalf("event usage = in %d out %d, want 900/0", ev.InputTokens, ev.OutputTokens)
	}
}

func TestSend_NoUsageRecordsZero(t *testing.T) {
	ev := runPrompt(t, nil, `{"stopReason":"end_turn"}`)
	if ev.InputTokens != 0 || ev.OutputTokens != 0 {
		t.Fatalf("event usage = in %d out %d, want 0/0", ev.InputTokens, ev.OutputTokens)
	}
}
