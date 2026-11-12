package core

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// askSession asks two questions in its first turn and goes on as soon as the
// tool is answered; later prompts just finish.
type askSession struct {
	mu        sync.Mutex
	events    chan Event
	prompts   []string
	responses []PermissionResult
}

var askQuestions = []UserQuestion{
	{Question: "周报发给谁？", Header: "收件人", Options: []UserQuestionOption{{Label: "全组", Description: "部门所有人"}, {Label: "只发我"}}},
	{Question: "包含哪些部分？", Options: []UserQuestionOption{{Label: "进展"}, {Label: "风险"}, {Label: "计划"}}, MultiSelect: true},
}

func (s *askSession) Send(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	first := len(s.prompts) == 1
	s.mu.Unlock()
	if first {
		s.events <- Event{Type: EventPermissionRequest, RequestID: "r1", ToolName: "AskUserQuestion", Questions: askQuestions}
		return nil
	}
	s.events <- Event{Type: EventResult, Content: "收到", Done: true}
	return nil
}

func (s *askSession) RespondPermission(_ string, r PermissionResult) error {
	s.mu.Lock()
	s.responses = append(s.responses, r)
	s.mu.Unlock()
	s.events <- Event{Type: EventResult, Content: "卡片发给你了，我先整理数据。", Done: true}
	return nil
}
func (s *askSession) Events() <-chan Event     { return s.events }
func (s *askSession) CurrentSessionID() string { return "ask-session" }
func (s *askSession) Alive() bool              { return true }
func (s *askSession) Close() error             { return nil }

func (s *askSession) snapshot() ([]string, []PermissionResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...), append([]PermissionResult(nil), s.responses...)
}

func TestAskUserQuestionBecomesDecisionCardsWithoutHoldingTheTurn(t *testing.T) {
	p := &decisionPlatform{stubCardPlatform: stubCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}
	sess := &askSession{events: make(chan Event, 16)}
	e := NewEngine("test", &resultAgent{session: sess}, []Platform{p}, "", LangChinese)
	e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
	t.Cleanup(func() { _ = e.Stop() })
	store, err := NewDecisionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.SetDecisionStore(store, 0)

	e.ReceiveMessage(p, &Message{SessionKey: decKey, Platform: "test", MessageID: "m1", UserID: "user", Content: "帮我出周报", ReplyCtx: "ctx"})
	deadline := time.Now().Add(3 * time.Second)
	for e.sessions.GetOrCreateActive(decKey).Busy() || len(p.getSent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("turn held open by the question")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, responses := sess.snapshot()
	if len(responses) != 1 || responses[0].Behavior != "deny" || responses[0].Message != "已发选择卡片，用户点选后会作为新消息到达；先结束本轮或继续别的事。" {
		t.Fatalf("tool answer = %+v", responses)
	}
	if sent := p.getSent(); !strings.Contains(sent[len(sent)-1], "卡片发给你了") {
		t.Fatalf("turn reply missing: %q", sent)
	}

	p.dmu.Lock()
	cards := append([]*Card(nil), p.idCards...)
	p.dmu.Unlock()
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want one per question", len(cards))
	}
	first, second := cardText(cards[0]), cardText(cards[1])
	if !strings.Contains(first, "需要你选择") || !strings.Contains(first, "周报发给谁？") || !strings.Contains(first, "全组：部门所有人") ||
		!strings.Contains(first, "不会自动处理") || strings.Contains(first, "推荐") {
		t.Fatalf("first card = %s", first)
	}
	if !strings.Contains(second, "包含哪些部分？") || strings.Contains(second, "推荐") {
		t.Fatalf("second card = %s", second)
	}
	store.mu.Lock()
	ds := append([]*Decision(nil), store.list...)
	store.mu.Unlock()
	if len(ds) != 2 || ds[0].HasDefault || !ds[0].Deadline.IsZero() || ds[0].SessionKey != decKey || ds[0].Multi || !ds[1].Multi || ds[1].HasDefault {
		t.Fatalf("decisions = %+v / %+v", ds[0], ds[1])
	}

	// The tap reaches the session as an ordinary decision message.
	if card := e.handleCardNav("act:/decision "+ds[0].ID+" 2", decKey); card == nil || !strings.Contains(card.Header.Title, "已决定：只发我") {
		t.Fatalf("answered card = %#v", card)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		prompts, _ := sess.snapshot()
		if len(prompts) == 2 {
			if !strings.Contains(prompts[1], "【决定】周报发给谁？：只发我") {
				t.Fatalf("delivered = %q", prompts[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tap not delivered: %q", prompts)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAskDecisionRequestsFallBackWhenACardCannotCarryTheQuestion(t *testing.T) {
	if reqs := askDecisionRequests("k", []UserQuestion{{Question: "继续吗？", Options: []UserQuestionOption{{Label: "继续"}}}}); reqs != nil {
		t.Fatalf("one option: %+v", reqs)
	}
	reqs := askDecisionRequests("k", []UserQuestion{{Header: "方案", Options: []UserQuestionOption{{Label: "甲"}, {Label: "乙"}}}})
	if len(reqs) != 1 || reqs[0].Question != "方案" || !reqs[0].NoDefault || reqs[0].Note != "" {
		t.Fatalf("reqs = %+v", reqs)
	}
	// Without decision cards the ask card is used as before.
	e := NewEngine("test", &stubAgent{}, nil, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	ev := Event{ToolName: "AskUserQuestion", Questions: askQuestions}
	if e.askAsDecisions(&askSession{events: make(chan Event, 1)}, decKey, &ev) {
		t.Fatal("sent as decision cards with decision cards off")
	}
}
