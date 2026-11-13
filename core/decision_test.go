package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type decisionPlatform struct {
	stubCardPlatform
	dmu     sync.Mutex
	idCards []*Card
	patched map[string]*Card
}

func (p *decisionPlatform) SendCardID(_ context.Context, _ any, card *Card) (string, error) {
	p.dmu.Lock()
	defer p.dmu.Unlock()
	p.idCards = append(p.idCards, card)
	return "om_test", nil
}

func (p *decisionPlatform) PatchCard(_ context.Context, id, _ string, card *Card) error {
	p.dmu.Lock()
	defer p.dmu.Unlock()
	if p.patched == nil {
		p.patched = map[string]*Card{}
	}
	p.patched[id] = card
	return nil
}

func (p *decisionPlatform) patchedCard(id string) *Card {
	p.dmu.Lock()
	defer p.dmu.Unlock()
	return p.patched[id]
}

// promptSession records every prompt the agent receives and finishes each turn.
type promptSession struct {
	mu      sync.Mutex
	events  chan Event
	prompts []string
}

func (s *promptSession) Send(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()
	s.events <- Event{Type: EventResult, Content: "ok", Done: true}
	return nil
}
func (s *promptSession) RespondPermission(string, PermissionResult) error { return nil }
func (s *promptSession) Events() <-chan Event                             { return s.events }
func (s *promptSession) CurrentSessionID() string                         { return "decision-session" }
func (s *promptSession) Alive() bool                                      { return true }
func (s *promptSession) Close() error                                     { return nil }
func (s *promptSession) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

const decKey = "test:chat:user"

func newDecisionEngine(t *testing.T, dir string) (*Engine, *decisionPlatform, *promptSession, *DecisionStore) {
	t.Helper()
	p := &decisionPlatform{stubCardPlatform: stubCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}
	sess := &promptSession{events: make(chan Event, 16)}
	e := NewEngine("test", &resultAgent{session: sess}, []Platform{p}, "", LangChinese)
	e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewDecisionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.SetDecisionStore(store, 0)
	return e, p, sess, store
}

func cardText(c *Card) string {
	var b strings.Builder
	if c.Header != nil {
		b.WriteString(c.Header.Title + "\n")
	}
	b.WriteString(c.RenderText())
	return b.String()
}

func waitPrompts(t *testing.T, s *promptSession, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g := s.got(); len(g) >= n {
			return g
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("want %d prompts, got %v", n, s.got())
	return nil
}

func sendTestDecision(t *testing.T, e *Engine, req DecisionRequest) string {
	t.Helper()
	req.SessionKey = decKey
	id, err := e.SendDecisionCard(req)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDecisionCardRecommendedFirstWithDeadline(t *testing.T) {
	e, p, _, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "测试：决定卡片显示是否正常？", Options: []string{"正常", "有问题"}, DeadlineMinutes: 10})
	if len(p.idCards) != 1 {
		t.Fatalf("cards sent = %d", len(p.idCards))
	}
	card := p.idCards[0]
	txt := cardText(card)
	for _, want := range []string{"需要你决定", "测试：决定卡片显示是否正常？", "到期", "按推荐处理：正常"} {
		if want == "到期" {
			want = "前未选择"
		}
		if !strings.Contains(txt, want) {
			t.Fatalf("card lacks %q:\n%s", want, txt)
		}
	}
	var btns []CardButton
	for _, el := range card.Elements {
		if a, ok := el.(CardActions); ok {
			btns = append(btns, a.Buttons...)
		}
	}
	if len(btns) != 2 || btns[0].Text != "正常（推荐）" || btns[0].Type != "primary" || btns[1].Text != "有问题" {
		t.Fatalf("buttons = %#v", btns)
	}
	if btns[0].Value != "act:/decision "+id+" 1" || btns[1].Value != "act:/decision "+id+" 2" {
		t.Fatalf("button values = %q %q", btns[0].Value, btns[1].Value)
	}
	d := store.Get(id)
	if d.MessageID != "om_test" || time.Until(d.Deadline) > 10*time.Minute || time.Until(d.Deadline) < 9*time.Minute {
		t.Fatalf("stored decision = %#v", d)
	}
}

func TestDecisionCardDefaultDeadlineIs12Hours(t *testing.T) {
	e, _, _, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "Q", Options: []string{"A", "B"}})
	if got := time.Until(store.Get(id).Deadline); got < 11*time.Hour+55*time.Minute || got > 12*time.Hour {
		t.Fatalf("deadline in %v", got)
	}
}

func TestDecisionCardNoDefault(t *testing.T) {
	e, p, _, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "要付费开通吗？", Options: []string{"开通", "不开通"}, NoDefault: true})
	txt := cardText(p.idCards[0])
	if !strings.Contains(txt, "需要你选择") || strings.Contains(txt, "推荐") || strings.Contains(txt, "按推荐") {
		t.Fatalf("no-default card:\n%s", txt)
	}
	if !store.Get(id).Deadline.IsZero() {
		t.Fatal("no-default card must have no deadline")
	}
	if due := store.Due(time.Now().Add(1000 * time.Hour)); len(due) != 0 {
		t.Fatalf("no-default card became due: %v", due)
	}
}

func TestDecisionTapDeliversMessageOnceAndGoesInert(t *testing.T) {
	e, _, sess, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "用哪个方案？", Options: []string{"甲", "乙"}})

	card := e.handleCardNav("act:/decision "+id+" 2", decKey)
	if card == nil || !strings.Contains(card.Header.Title, "已决定：乙") || strings.Contains(cardText(card), "act:/decision") {
		t.Fatalf("answered card = %#v", card)
	}
	got := waitPrompts(t, sess, 1)
	if !strings.Contains(got[0], "【决定】用哪个方案？：乙") {
		t.Fatalf("prompt = %q", got[0])
	}
	// second tap and a tap on the other option after answering: card shows the first answer, nothing delivered
	again := e.handleCardNav("act:/decision "+id+" 1", decKey)
	if again == nil || !strings.Contains(again.Header.Title, "已决定：乙") {
		t.Fatalf("second tap card = %#v", again)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(sess.got()); n != 1 {
		t.Fatalf("prompts = %d after double tap", n)
	}
	if d := store.Get(id); d.State != decisionAnswered || !d.Delivered {
		t.Fatalf("stored = %#v", d)
	}
}

func TestDecisionDeadlineAppliesRecommendedAndUpdatesCard(t *testing.T) {
	e, p, sess, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "测试：决定卡片显示是否正常？", Options: []string{"正常", "有问题"}, DeadlineMinutes: 10})

	store.mu.Lock()
	store.find(id).Deadline = time.Now().Add(-time.Second)
	store.mu.Unlock()
	e.wakeDecisions()

	got := waitPrompts(t, sess, 1)
	if !strings.Contains(got[0], "【到期默认】测试：决定卡片显示是否正常？：正常") {
		t.Fatalf("prompt = %q", got[0])
	}
	card := p.patchedCard("om_test")
	if card == nil || !strings.Contains(card.Header.Title, "已到期，按推荐处理：正常") {
		t.Fatalf("patched card = %#v", card)
	}
	// late tap after expiry: inert, no second message
	late := e.handleCardNav("act:/decision "+id+" 2", decKey)
	if late == nil || !strings.Contains(late.Header.Title, "已到期") {
		t.Fatalf("late tap card = %#v", late)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(sess.got()); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
}

func TestDecisionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	e1, _, _, store1 := newDecisionEngine(t, dir)
	pendingID := sendTestDecision(t, e1, DecisionRequest{Question: "过期的", Options: []string{"A", "B"}, DeadlineMinutes: 10})
	futureID := sendTestDecision(t, e1, DecisionRequest{Question: "未到期的", Options: []string{"A", "B"}})
	// answered just before the service stopped, but never handed to the session
	answeredID := sendTestDecision(t, e1, DecisionRequest{Question: "点过的", Options: []string{"A", "B"}})
	// Stop first, so the old engine's deadline loop cannot apply the default.
	_ = e1.Stop()
	store1.mu.Lock()
	store1.find(pendingID).Deadline = time.Now().Add(-time.Hour)
	store1.mu.Unlock()
	if _, ok := store1.Resolve(answeredID, 2, decisionAnswered); !ok {
		t.Fatal("resolve failed")
	}

	// a fresh process: new store from the same dir, new engine
	e2, p2, sess2, store2 := newDecisionEngine(t, dir)
	got := strings.Join(waitPrompts(t, sess2, 2), "|")
	if !strings.Contains(got, "【到期默认】过期的：A") || !strings.Contains(got, "【决定】点过的：B") {
		t.Fatalf("prompts after restart = %s", got)
	}
	if p2.patchedCard("om_test") == nil {
		t.Fatal("expired card was not updated after restart")
	}
	if d := store2.Get(futureID); d.State != decisionPending {
		t.Fatalf("future decision = %#v", d)
	}
	// a tap on the surviving card still works after restart
	if c := e2.handleCardNav("act:/decision "+futureID+" 1", decKey); c == nil || !strings.Contains(c.Header.Title, "已决定：A") {
		t.Fatalf("tap after restart = %#v", c)
	}
	time.Sleep(200 * time.Millisecond)
}

func TestDecisionRequestValidation(t *testing.T) {
	e, _, _, _ := newDecisionEngine(t, t.TempDir())
	for name, req := range map[string]DecisionRequest{
		"no question":  {SessionKey: decKey, Options: []string{"A", "B"}},
		"one option":   {SessionKey: decKey, Question: "Q", Options: []string{"A"}},
		"empty option": {SessionKey: decKey, Question: "Q", Options: []string{"A", " "}},
		"no session":   {Question: "Q", Options: []string{"A", "B"}},
	} {
		if _, err := e.SendDecisionCard(req); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func decisionButtons(c *Card) []CardButton {
	var btns []CardButton
	for _, el := range c.Elements {
		if a, ok := el.(CardActions); ok {
			btns = append(btns, a.Buttons...)
		}
	}
	return btns
}

func decisionFormOf(t *testing.T, c *Card) CardForm {
	t.Helper()
	for _, el := range c.Elements {
		if f, ok := el.(CardForm); ok {
			return f
		}
	}
	t.Fatalf("card has no form: %#v", c)
	return CardForm{}
}

func TestDangerDecisionHasRedConfirmedButtonsAndNoDefault(t *testing.T) {
	e, p, _, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "删除 3 个旧文件？", Options: []string{"删除", "不删"}, Danger: true, DeadlineMinutes: 5})
	card := p.idCards[0]
	if card.Header.Title != "需要你确认" || card.Header.Color != "red" || strings.Contains(cardText(card), "推荐") {
		t.Fatalf("danger card:\n%s", cardText(card))
	}
	for _, b := range decisionButtons(card) {
		if b.Type != "danger" || b.Confirm == nil || b.Confirm.Title == "" || !strings.Contains(b.Confirm.Text, b.Text) {
			t.Fatalf("button=%#v", b)
		}
	}
	if d := store.Get(id); d.HasDefault || !d.Deadline.IsZero() {
		t.Fatalf("danger decision got a default: %#v", d)
	}
}

func TestMultiDecisionSubmitsSeveralPicks(t *testing.T) {
	e, p, sess, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "周报包含哪些部分？", Options: []string{"进展", "风险", "计划"}, Multi: true})
	f := decisionFormOf(t, p.idCards[0])
	if len(decisionButtons(p.idCards[0])) != 0 || len(f.Fields) != 1 || f.Fields[0].Kind != "multi_select" || len(f.Fields[0].Options) != 3 ||
		f.SubmitText != "提交" || f.SubmitValue != "act:/decision-form "+id {
		t.Fatalf("form=%#v", f)
	}
	if card := e.handleCardNav(f.SubmitValue+` {"choice":[]}`, decKey); card == nil || card.Toast != "请先选择" {
		t.Fatalf("empty submit card=%#v", card)
	}
	card := e.handleCardNav(f.SubmitValue+` {"choice":["3","1"]}`, decKey)
	if card == nil || !strings.Contains(card.Header.Title, "已决定：进展、计划") {
		t.Fatalf("answered card=%#v", card)
	}
	if got := waitPrompts(t, sess, 1); !strings.Contains(got[0], "【决定】周报包含哪些部分？：进展、计划") {
		t.Fatalf("prompt=%q", got[0])
	}
	if d := store.Get(id); d.HasDefault || len(d.ChosenSet) != 2 {
		t.Fatalf("stored=%#v", d)
	}
}

func TestManyOptionsBecomeADropdown(t *testing.T) {
	e, p, sess, _ := newDecisionEngine(t, t.TempDir())
	opts := []string{"一", "二", "三", "四", "五", "六", "七"}
	id := sendTestDecision(t, e, DecisionRequest{Question: "选哪一周？", Options: opts, Danger: true})
	f := decisionFormOf(t, p.idCards[0])
	if f.Fields[0].Kind != "select" || len(f.Fields[0].Options) != 7 || f.SubmitType != "danger" || f.Confirm == nil {
		t.Fatalf("form=%#v", f)
	}
	// a single choice keeps one pick
	if card := e.handleCardNav("act:/decision-form "+id+` {"choice":["2","3"]}`, decKey); card == nil || card.Toast == "" {
		t.Fatalf("two picks on a single choice: %#v", card)
	}
	if card := e.handleCardNav("act:/decision-form "+id+` {"choice":"7"}`, decKey); card == nil || !strings.Contains(card.Header.Title, "已决定：七") {
		t.Fatalf("card=%#v", card)
	}
	if got := waitPrompts(t, sess, 1); !strings.Contains(got[0], "选哪一周？：七") {
		t.Fatalf("prompt=%q", got[0])
	}
	var tooMany []string
	for i := 0; i <= decisionMaxOptions; i++ {
		tooMany = append(tooMany, fmt.Sprint(i))
	}
	if _, err := e.SendDecisionCard(DecisionRequest{SessionKey: decKey, Question: "Q", Options: tooMany}); err == nil {
		t.Fatal("too many options accepted")
	}
}
