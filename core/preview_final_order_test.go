package core

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type finalOrderPlatform struct {
	lifecyclePlatform
	orderMu                    sync.Mutex
	calls                      []string
	failSends, failDeletes     int
	blockDelete, enteredDelete chan struct{}
}

func (p *finalOrderPlatform) record(s string) {
	p.orderMu.Lock()
	defer p.orderMu.Unlock()
	p.calls = append(p.calls, s)
}
func (p *finalOrderPlatform) snapshot() []string {
	p.orderMu.Lock()
	defer p.orderMu.Unlock()
	return append([]string(nil), p.calls...)
}
func (p *finalOrderPlatform) Send(ctx context.Context, rc any, text string) error {
	p.record("send:" + text)
	p.orderMu.Lock()
	failed := p.failSends > 0
	if failed {
		p.failSends--
	}
	p.orderMu.Unlock()
	if failed {
		return fmt.Errorf("send rejected")
	}
	return p.stubPlatformEngine.Send(ctx, rc, text)
}
func (p *finalOrderPlatform) Reply(ctx context.Context, rc any, text string) error {
	return p.Send(ctx, rc, text)
}
func (p *finalOrderPlatform) SendPreviewStart(ctx context.Context, rc any, text string) (any, error) {
	p.record("start:" + text)
	return p.lifecyclePlatform.SendPreviewStart(ctx, rc, text)
}
func (p *finalOrderPlatform) UpdateMessage(ctx context.Context, id any, text string) error {
	p.record("update:" + text)
	return p.lifecyclePlatform.UpdateMessage(ctx, id, text)
}
func (p *finalOrderPlatform) DeletePreviewMessage(ctx context.Context, id any) error {
	p.record("delete:" + fmt.Sprint(id))
	if p.enteredDelete != nil {
		close(p.enteredDelete)
		<-p.blockDelete
	}
	p.orderMu.Lock()
	failed := p.failDeletes > 0
	if failed {
		p.failDeletes--
	}
	p.orderMu.Unlock()
	if failed {
		return fmt.Errorf("recall rejected")
	}
	return p.lifecyclePlatform.DeletePreviewMessage(ctx, id)
}
func newFinalOrderEngine(t *testing.T, p *finalOrderPlatform, a Agent) *Engine {
	t.Helper()
	p.n = "order"
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	return e
}
func TestPreviewLifecycleFinalOrderRecovery(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sends, deletes int
		want           []string
	}{
		{"success", 0, 0, []string{"start:思考中…", "send:answer", "delete:card-1"}},
		{"retry", 1, 0, []string{"start:思考中…", "send:answer", "send:answer", "delete:card-1"}},
		{"failure", 2, 0, []string{"start:思考中…", "send:answer", "send:answer", "update:"}},
		{"recall_failure", 0, 1, []string{"start:思考中…", "send:answer", "delete:card-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &finalOrderPlatform{failSends: tc.sends, failDeletes: tc.deletes}
			e := newFinalOrderEngine(t, p, &stubAgent{})
			as := newControllableSession("order")
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
			as.events <- Event{Type: EventResult, Content: "answer", Done: true}
			e.processInteractiveEvents(state, e.sessions.GetOrCreateActive("order"), e.sessions, "order", "m", time.Now(), nil, nil, "ctx")
			got := p.snapshot()
			if tc.name == "failure" && len(got) == 4 && strings.HasPrefix(got[3], "update:") && strings.Contains(got[3], e.i18n.T(MsgFinalDeliveryFailed)) {
				got[3] = "update:"
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
func TestPreviewLifecycleFinalOrderMultipart(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			p := &finalOrderPlatform{}
			e := newFinalOrderEngine(t, p, &stubAgent{})
			body := strings.Repeat("x", maxPlatformMessageLen+100)
			chunks := SplitMessageCodeFenceAware(body, maxPlatformMessageLen)
			sp := newStreamPreview(DefaultStreamPreviewCfg(), p, "ctx", e.ctx, nil)
			sp.startPlaceholder("thinking")
			sp.replacePreview("progress")
			count := 0
			send := func(platform Platform, rc any, text string) error {
				count++
				if fail && count == 2 {
					p.orderMu.Lock()
					p.failSends = 2
					p.orderMu.Unlock()
				}
				return e.sendWithError(platform, rc, text)
			}
			if got := sp.deliverFinal(body, "footer", send, "delivery failed"); got == fail {
				t.Fatal("unexpected delivery result")
			}
			sp.discard()
			want := []string{"start:thinking", "update:progress", "send:" + chunks[0], "send:" + appendReplyFooter(chunks[1], "footer")}
			if fail {
				want = append(want, "send:"+appendReplyFooter(chunks[1], "footer"), "update:"+appendReplyFooter("progress", "delivery failed"))
			} else {
				want = append(want, "delete:card-1")
			}
			if got := p.snapshot(); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
}
func TestPreviewLifecycleFinalOrderErrorAndSilent(t *testing.T) {
	for _, silent := range []bool{false, true} {
		t.Run(fmt.Sprint(silent), func(t *testing.T) {
			p := &finalOrderPlatform{}
			e := newFinalOrderEngine(t, p, &stubAgent{})
			as := newControllableSession("order")
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
			ev := Event{Type: EventError, Error: fmt.Errorf("failed")}
			if silent {
				ev = Event{Type: EventResult, Content: "NO_REPLY", Done: true}
			}
			as.events <- ev
			e.processInteractiveEvents(state, e.sessions.GetOrCreateActive("order"), e.sessions, "order", "m", time.Now(), nil, nil, "ctx")
			calls := p.snapshot()
			if silent {
				if !reflect.DeepEqual(calls, []string{"start:思考中…", "delete:card-1"}) {
					t.Fatal(calls)
				}
			} else {
				if len(calls) != 3 || !strings.HasPrefix(calls[1], "send:") || calls[2] != "delete:card-1" {
					t.Fatal(calls)
				}
			}
		})
	}
}
func TestPreviewLifecycleStopSendsBeforeRecall(t *testing.T) {
	for _, command := range []string{"/stop", "/cancel"} {
		t.Run(command, func(t *testing.T) {
			p := &finalOrderPlatform{blockDelete: make(chan struct{}), enteredDelete: make(chan struct{})}
			as := &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 3)}
			e := newFinalOrderEngine(t, p, &controllableAgent{nextSession: as})
			e.ReceiveMessage(p, &Message{SessionKey: "order:stop", MessageID: "m1", Content: "work", ReplyCtx: "ctx"})
			deadline := time.Now().Add(3 * time.Second)
			for len(p.snapshot()) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("no placeholder")
				}
				time.Sleep(time.Millisecond)
			}
			done := make(chan struct{})
			go func() {
				e.ReceiveMessage(p, &Message{SessionKey: "order:stop", Content: command, ReplyCtx: "ctx"})
				close(done)
			}()
			select {
			case <-p.enteredDelete:
			case <-time.After(3 * time.Second):
				t.Fatal("no recall")
			}
			calls := p.snapshot()
			close(p.blockDelete)
			<-done
			if len(calls) != 3 || !strings.HasPrefix(calls[1], "send:") || calls[2] != "delete:card-1" {
				t.Fatal(calls)
			}
		})
	}
}

func TestPreviewLifecycleFinalOrderEmpty(t *testing.T) {
	p := &finalOrderPlatform{}
	e := newFinalOrderEngine(t, p, &stubAgent{})
	sp := newStreamPreview(DefaultStreamPreviewCfg(), p, "ctx", e.ctx, nil)
	sp.startPlaceholder("thinking")
	if !sp.deliverFinal("", "", e.sendWithError, "failed") {
		t.Fatal("empty final failed")
	}
	sp.discard()
	if got := p.snapshot(); !reflect.DeepEqual(got, []string{"start:thinking", "delete:card-1"}) {
		t.Fatal(got)
	}
}
