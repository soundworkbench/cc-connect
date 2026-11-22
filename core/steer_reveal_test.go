package core

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// revealPlatform records which message each turn card and each plain message
// was sent under.
type revealPlatform struct {
	cujSegmentedPlatform
	rmu      sync.Mutex
	cardCtxs []any
	cardIDs  []int
	msgs     []revealMsg
	rows     map[int][]string // progress rows per card id
	folded   []int            // cards collapsed to their steps
}

func (p *revealPlatform) AddPreviewProgress(_ context.Context, id any, text string) error {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	if p.rows == nil {
		p.rows = make(map[int][]string)
	}
	p.rows[id.(int)] = append(p.rows[id.(int)], text)
	return nil
}

func (p *revealPlatform) CollapsePreview(_ context.Context, id any) error {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	p.folded = append(p.folded, id.(int))
	return nil
}

// firstRow returns the first progress row of the n-th card.
func (p *revealPlatform) firstRow(n int) string {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	if n >= len(p.cardIDs) || len(p.rows[p.cardIDs[n]]) == 0 {
		return ""
	}
	return p.rows[p.cardIDs[n]][0]
}

func (p *revealPlatform) collapsed(n int) bool {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	for _, id := range p.folded {
		if n < len(p.cardIDs) && id == p.cardIDs[n] {
			return true
		}
	}
	return false
}

type revealMsg struct {
	ctx  any
	text string
}

func (p *revealPlatform) StartSegmentedPreview(ctx context.Context, rc any, cfg StreamPreviewCfg, i *I18n) (any, error) {
	id, err := p.cujSegmentedPlatform.StartSegmentedPreview(ctx, rc, cfg, i)
	p.rmu.Lock()
	p.cardCtxs = append(p.cardCtxs, rc)
	p.cardIDs = append(p.cardIDs, id.(int))
	p.rmu.Unlock()
	return id, err
}

func (p *revealPlatform) Reply(ctx context.Context, rc any, text string) error {
	p.record(rc, text)
	return p.cujSegmentedPlatform.Reply(ctx, rc, text)
}

func (p *revealPlatform) Send(ctx context.Context, rc any, text string) error {
	p.record(rc, text)
	return p.cujSegmentedPlatform.Send(ctx, rc, text)
}

func (p *revealPlatform) record(rc any, text string) {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	p.msgs = append(p.msgs, revealMsg{rc, text})
}

func (p *revealPlatform) cards() []any {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	return append([]any(nil), p.cardCtxs...)
}

func (p *revealPlatform) messages() []revealMsg {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	return append([]revealMsg(nil), p.msgs...)
}

// card returns what the n-th card shows now.
func (p *revealPlatform) card(n int) string {
	p.rmu.Lock()
	if n >= len(p.cardIDs) {
		p.rmu.Unlock()
		return ""
	}
	id := p.cardIDs[n]
	p.rmu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[id]
}

// deliveredUnder reports whether text reached the chat under the message ctx
// came from: on a card opened there, or as a reply to it.
func (p *revealPlatform) deliveredUnder(ctx any, text string) bool {
	for i, c := range p.cards() {
		if c == ctx && strings.Contains(p.card(i), text) {
			return true
		}
	}
	for _, m := range p.messages() {
		if m.ctx == ctx && strings.Contains(m.text, text) {
			return true
		}
	}
	return false
}

// deliveredElsewhere reports text sent as a message under any other message.
func (p *revealPlatform) deliveredElsewhere(ctx any, text string) bool {
	for _, m := range p.messages() {
		if m.ctx != ctx && strings.Contains(m.text, text) {
			return true
		}
	}
	return false
}

func newRevealEnv(t *testing.T, product ...bool) (*revealPlatform, *steeringSession, func(m *Message)) {
	t.Helper()
	p := &revealPlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}
	s := &steeringSession{queuedLifecycleSession: &queuedLifecycleSession{cujAgentSession: newCUJAgentSession(), sent: make(chan string, 4)}, steered: make(chan string, 8)}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	if len(product) > 0 && product[0] {
		cfg = ProductStreamPreviewCfg()
	}
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	receive := func(m *Message) {
		m.SessionKey, m.Platform = "segmented:user", p.Name()
		e.ReceiveMessage(p, m)
	}
	return p, s, receive
}

func waitPrompt(t *testing.T, s *steeringSession) {
	t.Helper()
	select {
	case <-s.sent:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt missing")
	}
}

// 11-22 10:07: Jason wrote while a webhook turn ran. The turn had no card, so
// all he saw was the reaction until the turn ended almost two minutes later.
func TestSteerIntoWebhookTurnOpensTheTurnCard(t *testing.T) {
	t.Run("step list", func(t *testing.T) { testSteerIntoWebhookTurn(t, false) })
	t.Run("product card", func(t *testing.T) { testSteerIntoWebhookTurn(t, true) })
}

func testSteerIntoWebhookTurn(t *testing.T, product bool) {
	p, s, receive := newRevealEnv(t, product)
	receive(&Message{UserID: "webhook", UserName: "webhook", Content: "butler:websearch-down", ReplyCtx: "ctx-hook"})
	waitPrompt(t, s)
	s.events <- Event{Type: EventProgress, Content: "检查搜索服务"}
	time.Sleep(50 * time.Millisecond)
	if got := p.cards(); len(got) != 0 {
		t.Fatalf("webhook turn showed a card before anyone wrote: %v", got)
	}

	receive(&Message{MessageID: "m2", UserID: "user", UserName: "Jason", Content: "顺便看下备份", ReplyCtx: "ctx-2"})
	steered := steerOne(t, s)
	waitUntil(t, "turn card under the steered message", func() bool { return len(p.cards()) == 1 })
	if took := time.Since(steered); took > 2*time.Second {
		t.Fatalf("card took %v", took)
	}
	if got := p.cards(); got[0] != "ctx-2" {
		t.Fatalf("card opened under %v, want the steered message", got[0])
	}

	s.events <- Event{Type: EventProgress, Content: "正在核对备份"}
	s.events <- Event{Type: EventResult, Content: "搜索已恢复，备份正常。", Done: true}
	waitUntil(t, "final reply under the steered message", func() bool { return p.deliveredUnder("ctx-2", "搜索已恢复") })
	if p.deliveredElsewhere("ctx-2", "搜索已恢复") {
		t.Fatalf("final reply also went elsewhere: %+v", p.messages())
	}
}

// 11-20 11:00: the same for a timer turn that ran from the queue, plus a second
// message, which only follows the card that is already there.
func TestSteerIntoQueuedTimerTurnOpensOneCard(t *testing.T) {
	p, s, receive := newRevealEnv(t)
	receive(&Message{MessageID: "m1", UserID: "user", Content: "先做这个", ReplyCtx: "ctx-1"})
	waitPrompt(t, s)
	receive(&Message{UserID: "timer", UserName: "timer", Content: "提醒续费", ReplyCtx: "ctx-timer", scheduledPrompt: true, scheduledInputID: "timer:renew:1"})
	s.events <- Event{Type: EventResult, Content: "做完了", Done: true}
	waitPrompt(t, s)
	waitUntil(t, "first turn finished", func() bool { return p.deliveredUnder("ctx-1", "做完了") })
	if got := p.cards(); len(got) != 1 {
		t.Fatalf("timer turn opened a card on its own: %v", got)
	}

	receive(&Message{MessageID: "m2", UserID: "user", Content: "这个也一起", ReplyCtx: "ctx-2"})
	steerOne(t, s)
	waitUntil(t, "turn card under the steered message", func() bool { return len(p.cards()) == 2 })
	receive(&Message{MessageID: "m3", UserID: "user", Content: "还有一件", ReplyCtx: "ctx-3"})
	steerOne(t, s)
	time.Sleep(100 * time.Millisecond)
	if got := p.cards(); len(got) != 2 || got[1] != "ctx-2" {
		t.Fatalf("cards=%v, want one new card under the first steered message", got)
	}
	s.events <- Event{Type: EventResult, Content: "续费已提醒，两件也办了。", Done: true}
	waitUntil(t, "final reply under the steered message", func() bool { return p.deliveredUnder("ctx-2", "续费已提醒") })
}

// A turn that already shows its card keeps it: the steered message gets only
// the reaction, no second card and no extra line.
func TestSteerIntoVisibleTurnKeepsItsCard(t *testing.T) {
	p, s, receive := newRevealEnv(t)
	receive(&Message{MessageID: "m1", UserID: "user", Content: "查一下", ReplyCtx: "ctx-1"})
	waitPrompt(t, s)
	waitUntil(t, "turn card", func() bool { return len(p.cards()) == 1 })
	receive(&Message{MessageID: "m2", UserID: "user", Content: "再补充一点", ReplyCtx: "ctx-2"})
	steerOne(t, s)
	time.Sleep(100 * time.Millisecond)
	s.events <- Event{Type: EventResult, Content: "查好了", Done: true}
	waitUntil(t, "final reply under the first message", func() bool { return p.deliveredUnder("ctx-1", "查好了") })
	if got := p.cards(); len(got) != 1 {
		t.Fatalf("cards=%v, want the original card only", got)
	}
	for _, m := range p.messages() {
		if strings.Contains(m.text, "完成后一起回复你") {
			t.Fatalf("visible turn got the busy line: %+v", m)
		}
	}
}

type backgroundSteerSession struct {
	*backgroundJourneySession
	steered chan string
}

func (s *backgroundSteerSession) Steer(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) (bool, error) {
	s.steered <- prompt
	return true, nil
}

// A background turn (an agent that reserves the session for work it started)
// has no event loop to carry a card: the message gets one line at once, and
// the background reply goes under it.
func TestSteerIntoBackgroundTurnSaysTheReplyWillCome(t *testing.T) {
	p := &revealPlatform{cujSegmentedPlatform: cujSegmentedPlatform{stubPlatformEngine: stubPlatformEngine{n: "segmented"}}}
	s := &backgroundSteerSession{backgroundJourneySession: &backgroundJourneySession{cujAgentSession: newCUJAgentSession()}, steered: make(chan string, 4)}
	e := NewEngine("segmented", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	cfg := DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	send := func(id, content string) {
		e.ReceiveMessage(p, &Message{SessionKey: "segmented:user", Platform: p.Name(), MessageID: id, UserID: "user", Content: content, ReplyCtx: "ctx-" + id})
	}
	send("1", "开始任务")
	waitUntil(t, "first turn finished", func() bool { return p.deliveredUnder("ctx-1", "ok") })
	waitUntil(t, "background turn holds the session", s.beginBackground)

	send("2", "进度怎样")
	var at time.Time
	select {
	case <-s.steered:
		at = time.Now()
	case <-time.After(3 * time.Second):
		t.Fatal("message not steered into the background turn")
	}
	line := e.i18n.T(MsgSteerJoinsRunningWork)
	waitUntil(t, "busy line", func() bool {
		for _, m := range p.messages() {
			if m.text == line && m.ctx == "ctx-2" {
				return true
			}
		}
		return false
	})
	if took := time.Since(at); took > 2*time.Second {
		t.Fatalf("busy line took %v", took)
	}

	s.events <- Event{Type: EventResult, Content: "后台任务完成", Done: true}
	waitUntil(t, "background reply under the message", func() bool {
		for _, m := range p.messages() {
			if strings.Contains(m.text, "后台任务完成") {
				return m.ctx == "ctx-2"
			}
		}
		return false
	})
	if got := p.cards(); len(got) != 1 {
		t.Fatalf("cards=%v", got)
	}
}
