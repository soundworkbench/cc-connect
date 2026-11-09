package feishu

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type stepAgent struct{ s *stepSession }

func (a *stepAgent) Name() string { return "stepagent" }
func (a *stepAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return a.s, nil
}
func (a *stepAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) { return nil, nil }
func (a *stepAgent) Stop() error                                                   { return nil }

type stepSession struct {
	events chan core.Event
	sent   chan struct{}
}

func (s *stepSession) Send(string, string, []core.ImageAttachment, []core.FileAttachment) error {
	s.sent <- struct{}{}
	return nil
}
func (s *stepSession) RespondPermission(string, core.PermissionResult) error { return nil }
func (s *stepSession) Events() <-chan core.Event                             { return s.events }
func (s *stepSession) CurrentSessionID() string                              { return "step-session" }
func (s *stepSession) Alive() bool                                           { return true }
func (s *stepSession) Close() error                                          { return nil }

// runStepListTurn drives one turn through the engine and the Feishu platform
// against the API fixture: a tool call with a description, the answer, a result.
func runStepListTurn(t *testing.T, f *previewFixture, p *Platform, keep bool) []previewCall {
	t.Helper()
	p.keepPreviewOnFinish = keep
	p.noReplyToTrigger = true
	s := &stepSession{events: make(chan core.Event, 16), sent: make(chan struct{}, 1)}
	e := core.NewEngine("feishu", &stepAgent{s}, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	e.SetDisplayConfig(core.DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true})
	t.Cleanup(func() { _ = e.Stop() })
	rc := replyContext{chatID: "oc", messageID: "om_in"}
	e.ReceiveMessage(p, &core.Message{SessionKey: "feishu:oc:u", Platform: "feishu", MessageID: "om_in", UserID: "u", Content: "hi", ReplyCtx: rc})
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
	s.events <- core.Event{Type: core.EventToolUse, ToolName: "Bash", ProgressID: "t1", Metadata: map[string]any{"public_tool_progress": "运行命令", "step_label": "清理测试对话"}}
	time.Sleep(500 * time.Millisecond) // let a frame carry the step
	s.events <- core.Event{Type: core.EventTextDelta, Content: "answer"}
	s.events <- core.Event{Type: core.EventResult, Content: "answer", Done: true}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := 0
		for _, c := range f.read() {
			if c.method == "POST" && c.path == "/open-apis/im/v1/messages" {
				n++
			}
		}
		if n >= 2 || (n == 1 && keepSettled(f.read())) {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	return f.read()
}

// keepSettled reports a whole-card update, how a card settles into the reply.
func keepSettled(calls []previewCall) bool {
	for _, c := range calls {
		if c.method == "PUT" && c.body["card"] != nil {
			return true
		}
	}
	return false
}

func countCalls(calls []previewCall, method, path string) int {
	n := 0
	for _, c := range calls {
		if c.method == method && c.path == path {
			n++
		}
	}
	return n
}

func TestStepListRecallsCardWhenNotKept(t *testing.T) {
	f, p := newPreviewFixture(t)
	calls := runStepListTurn(t, f, p, false)
	if got := countCalls(calls, "DELETE", "/open-apis/im/v1/messages/om_preview"); got != 1 {
		t.Fatalf("card recalled %d times: %v", got, calls)
	}
	// One POST made the card, one sent the reply.
	if got := countCalls(calls, "POST", "/open-apis/im/v1/messages"); got != 2 {
		t.Fatalf("want the card and one reply, got %d messages", got)
	}
	if keepSettled(calls) {
		t.Fatal("card was settled although it is recalled")
	}
	var labelled bool
	for _, c := range calls {
		if el, ok := c.body["element"].(string); ok && strings.Contains(el, "清理测试对话") {
			labelled = true
		}
	}
	if !labelled {
		t.Fatal("tool step did not carry the tool description")
	}
}

// A delete that is accepted but leaves the card visible must not end with the
// reply twice: the card itself becomes the reply.
func TestStepListSettlesInPlaceWhenRecallLeavesCard(t *testing.T) {
	f, p := newPreviewFixture(t)
	f.ignoreDelete = true
	calls := runStepListTurn(t, f, p, false)
	if got := countCalls(calls, "POST", "/open-apis/im/v1/messages"); got != 1 {
		t.Fatalf("reply sent as a second message (%d messages)", got)
	}
	if !keepSettled(calls) {
		t.Fatal("card did not become the reply")
	}
}

func TestStepListKeptCardSettlesWithoutRecall(t *testing.T) {
	f, p := newPreviewFixture(t)
	calls := runStepListTurn(t, f, p, true)
	if countCalls(calls, "DELETE", "/open-apis/im/v1/messages/om_preview") != 0 || countCalls(calls, "POST", "/open-apis/im/v1/messages") != 1 || !keepSettled(calls) {
		t.Fatalf("kept card: %v", calls)
	}
}
