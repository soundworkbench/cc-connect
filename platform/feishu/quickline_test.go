package feishu

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const quickSentence = "收到，我先去查昨晚洞察中心的发布记录。"

type quickGateway struct {
	hits    atomic.Int32
	release chan struct{} // when set, the reply waits for it
	status  int
	delay   time.Duration
	mu      sync.Mutex
	seen    map[string]any
	key     string
}

func (g *quickGateway) handler(w http.ResponseWriter, r *http.Request) {
	g.hits.Add(1)
	g.mu.Lock()
	g.key = r.Header.Get("x-api-key")
	g.seen = map[string]any{"path": r.URL.Path}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	for k, v := range body {
		g.seen[k] = v
	}
	g.mu.Unlock()
	if g.release != nil {
		<-g.release
	}
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	if g.status != 0 && g.status != http.StatusOK {
		w.WriteHeader(g.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":%q}]}`, quickSentence)
}

// runQuickTurn starts a turn with the quick line configured against gw and calls drive
// once the prompt reached the agent. It returns the card API calls.
func runQuickTurn(t *testing.T, gw *quickGateway, enabled bool, timeoutMs int, drive func(f *previewFixture, s *stepSession)) []previewCall {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(gw.handler))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("dummy-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, p := newPreviewFixture(t)
	p.keepPreviewOnFinish = true
	p.noReplyToTrigger = true
	s := &stepSession{events: make(chan core.Event, 16), sent: make(chan struct{}, 1)}
	e := core.NewEngine("feishu", &stepAgent{s}, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	cfg.QuickLine = core.QuickLineCfg{Enabled: enabled, BaseURL: srv.URL, Model: "co/mp-cx-jifang/codex/gpt-6-luna", Effort: "minimal", KeyFile: keyFile, TimeoutMs: timeoutMs}
	e.SetStreamPreviewCfg(cfg)
	e.SetDisplayConfig(core.DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true})
	t.Cleanup(func() { _ = e.Stop() })
	rc := replyContext{chatID: "oc", messageID: "om_in"}
	e.ReceiveMessage(p, &core.Message{SessionKey: "feishu:oc:u", Platform: "feishu", MessageID: "om_in", UserID: "u", Content: "查一下昨晚洞察中心的发布记录", ReplyCtx: rc})
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
	drive(f, s)
	return f.read()
}

func callsMention(calls []previewCall, text string) bool {
	for _, c := range calls {
		b, _ := json.Marshal(c.body)
		if strings.Contains(string(b), text) || strings.Contains(string(b), strings.ReplaceAll(text, "\n", `\n`)) {
			return true
		}
	}
	return false
}

func finishTurn(s *stepSession) {
	s.events <- core.Event{Type: core.EventResult, Content: "done", Done: true}
	time.Sleep(200 * time.Millisecond)
}

func TestQuickLineShowsInCardBeforeContent(t *testing.T) {
	gw := &quickGateway{}
	calls := runQuickTurn(t, gw, true, 5000, func(f *previewFixture, s *stepSession) {
		waitPreview(t, func() bool { return callsMention(f.read(), quickSentence) })
		finishTurn(s)
	})
	if !callsMention(calls, quickSentence) {
		t.Fatal("sentence never reached the card")
	}
	// It goes through an element update on the live card, not a new message.
	if got := countCalls(calls, "POST", "/open-apis/im/v1/messages"); got > 1 {
		t.Fatalf("want only the card message, got %d", got)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.seen["path"] != "/v1/messages" || gw.seen["model"] != "co/mp-cx-jifang/codex/gpt-6-luna" || gw.key != "dummy-key" {
		t.Fatalf("unexpected gateway request: path=%v model=%v", gw.seen["path"], gw.seen["model"])
	}
	if oc, _ := gw.seen["output_config"].(map[string]any); oc["effort"] != "minimal" {
		t.Fatalf("output_config = %v", gw.seen["output_config"])
	}
	if mt, _ := gw.seen["max_tokens"].(float64); mt <= 0 || mt > 100 {
		t.Fatalf("max_tokens = %v", gw.seen["max_tokens"])
	}
}

func TestQuickLineStaysAboveReplyWhileStreamingAndInFinal(t *testing.T) {
	gw := &quickGateway{}
	calls := runQuickTurn(t, gw, true, 5000, func(f *previewFixture, s *stepSession) {
		waitPreview(t, func() bool { return callsMention(f.read(), quickSentence) })
		s.events <- core.Event{Type: core.EventTextDelta, Content: "第一行\n第二行已经开始"}
		waitPreview(t, func() bool { return callsMention(f.read(), quickSentence+"\n\n第一行\n第二行已经开始") })
		s.events <- core.Event{Type: core.EventResult, Content: "第一行\n第二行已经开始\n最终结论", Done: true}
		waitPreview(t, func() bool { return callsMention(f.read(), quickSentence+"\n\n第一行\n第二行已经开始\n最终结论") })
		time.Sleep(200 * time.Millisecond)
	})
	// The settled card leads with the sentence once; its summary repeats the reply start.
	last := ""
	for _, c := range calls {
		if b, _ := json.Marshal(c.body); strings.Contains(string(b), "最终结论") {
			last = string(b)
		}
	}
	if n, m := strings.Count(last, quickSentence), strings.Count(last, "最终结论"); n != m {
		t.Fatalf("final card holds the sentence %d times for %d replies: %s", n, m, last)
	}
}

func TestQuickLineLeadsReplySentAsNewMessage(t *testing.T) {
	gw := &quickGateway{}
	calls := runQuickTurn(t, gw, true, 5000, func(f *previewFixture, s *stepSession) {
		waitPreview(t, func() bool { return callsMention(f.read(), quickSentence) })
		s.events <- core.Event{Type: core.EventResult, Content: "只有最终回复", Done: true}
		waitPreview(t, func() bool { return callsMention(f.read(), quickSentence+"\n\n只有最终回复") })
	})
	if !callsMention(calls, quickSentence+"\n\n只有最终回复") {
		t.Fatal("final reply does not lead with the sentence")
	}
}

func TestQuickLineDroppedWhenContentCameFirst(t *testing.T) {
	gw := &quickGateway{release: make(chan struct{})}
	calls := runQuickTurn(t, gw, true, 5000, func(f *previewFixture, s *stepSession) {
		s.events <- core.Event{Type: core.EventTextDelta, Content: "第一行\n第二行已经开始"}
		waitPreview(t, func() bool { return callsMention(f.read(), "第二行已经开始") })
		close(gw.release)
		time.Sleep(700 * time.Millisecond) // the sentence has had its chance to arrive
		finishTurn(s)
	})
	if callsMention(calls, quickSentence) {
		t.Fatal("sentence was written after real content started")
	}
	if gw.hits.Load() != 1 {
		t.Fatalf("gateway hits = %d", gw.hits.Load())
	}
}

func TestQuickLineTimeoutLeavesCardUnchanged(t *testing.T) {
	gw := &quickGateway{delay: 600 * time.Millisecond}
	calls := runQuickTurn(t, gw, true, 100, func(f *previewFixture, s *stepSession) {
		time.Sleep(900 * time.Millisecond)
		finishTurn(s)
	})
	if callsMention(calls, quickSentence) {
		t.Fatal("sentence shown although the call timed out")
	}
}

func TestQuickLineGatewayErrorLeavesCardUnchanged(t *testing.T) {
	gw := &quickGateway{status: http.StatusNotFound}
	calls := runQuickTurn(t, gw, true, 5000, func(f *previewFixture, s *stepSession) {
		waitPreview(t, func() bool { return gw.hits.Load() > 0 })
		time.Sleep(500 * time.Millisecond)
		finishTurn(s)
	})
	if callsMention(calls, quickSentence) {
		t.Fatal("sentence shown after a gateway error")
	}
}

func TestQuickLineDisabledMakesNoCall(t *testing.T) {
	gw := &quickGateway{}
	runQuickTurn(t, gw, false, 5000, func(f *previewFixture, s *stepSession) {
		time.Sleep(500 * time.Millisecond)
		finishTurn(s)
	})
	if gw.hits.Load() != 0 {
		t.Fatalf("disabled config made %d calls", gw.hits.Load())
	}
}
