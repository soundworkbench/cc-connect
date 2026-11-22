package core

import (
	"testing"
	"time"
)

func shortAutoCardDelay(t *testing.T) {
	t.Helper()
	old := autoCardDelay
	autoCardDelay = 150 * time.Millisecond
	t.Cleanup(func() { autoCardDelay = old })
}

// finishFirstTurn runs a person's turn to its end so the agent's later output
// arrives as a background turn.
func finishFirstTurn(t *testing.T, p *revealPlatform, s *steeringSession, receive func(*Message)) {
	t.Helper()
	receive(&Message{MessageID: "m1", UserID: "user", Content: "跑一下后台任务", ReplyCtx: "ctx-1"})
	waitPrompt(t, s)
	s.events <- Event{Type: EventResult, Content: "已在后台运行", Done: true}
	waitUntil(t, "first turn finished", func() bool { return p.deliveredUnder("ctx-1", "已在后台运行") })
}

// 11-22 09:21 (Beijing): after the task card showed done, the agent ran
// commands for minutes with nothing on screen. A background turn that runs
// past the delay and calls a tool now opens a card saying what it is doing.
func TestBackgroundTurnOpensCardAfterDelayWithTools(t *testing.T) {
	shortAutoCardDelay(t)
	p, s, receive := newRevealEnv(t)
	finishFirstTurn(t, p, s, receive)

	s.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "ls"}
	waitUntil(t, "background card", func() bool { return len(p.cards()) == 2 })
	if got := p.firstRow(1); got != "处理后台任务结果" {
		t.Fatalf("first row %q", got)
	}
	s.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "cat log"}
	s.events <- Event{Type: EventResult, Content: "后台任务的结论", Done: true}
	waitUntil(t, "background reply on its card", func() bool { return p.deliveredUnder("ctx-1", "后台任务的结论") })
}

// A background turn that ends quickly, or that calls no tool, opens no card.
func TestBackgroundTurnOpensNoCardWhenQuickOrToolless(t *testing.T) {
	shortAutoCardDelay(t)
	p, s, receive := newRevealEnv(t)
	finishFirstTurn(t, p, s, receive)

	s.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "ls"}
	s.events <- Event{Type: EventResult, Content: "很快就好", Done: true}
	waitUntil(t, "quick reply", func() bool { return p.deliveredUnder("ctx-1", "很快就好") })

	s.events <- Event{Type: EventText, Content: "只是写字"}
	time.Sleep(3 * autoCardDelay)
	s.events <- Event{Type: EventResult, Content: "只是写字", Done: true}
	waitUntil(t, "toolless reply", func() bool { return p.deliveredUnder("ctx-1", "只是写字") })
	time.Sleep(2 * autoCardDelay)
	if got := p.cards(); len(got) != 1 {
		t.Fatalf("cards=%v, want only the first turn's card", got)
	}
}

// A webhook turn (another assistant waking this one) opens its card the same
// way; when it ends silent the card collapses instead of being recalled.
func TestWebhookTurnOpensCardAfterDelayWithTools(t *testing.T) {
	shortAutoCardDelay(t)
	p, s, receive := newRevealEnv(t)
	receive(&Message{UserID: "webhook", UserName: "webhook", Content: "butler:websearch-down", ReplyCtx: "ctx-hook"})
	waitPrompt(t, s)
	s.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "curl"}
	waitUntil(t, "webhook card", func() bool { return len(p.cards()) == 1 })
	if got := p.cards(); got[0] != "ctx-hook" {
		t.Fatalf("card under %v", got[0])
	}
	if got := p.firstRow(0); got != "处理其他助理转来的事" {
		t.Fatalf("first row %q", got)
	}
	s.events <- Event{Type: EventResult, Content: "NO_REPLY", Done: true}
	waitUntil(t, "card collapsed", func() bool { return p.collapsed(0) })
	if p.card(0) == "" {
		t.Fatal("silent end recalled the card")
	}
}

// A person writing into a turn whose card is already up takes that card over;
// no second card opens and the reply goes under their message.
func TestSteerIntoOpenedAutoCardReusesIt(t *testing.T) {
	shortAutoCardDelay(t)
	p, s, receive := newRevealEnv(t)
	receive(&Message{UserID: "webhook", UserName: "webhook", Content: "butler:check", ReplyCtx: "ctx-hook"})
	waitPrompt(t, s)
	s.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "curl"}
	waitUntil(t, "webhook card", func() bool { return len(p.cards()) == 1 })

	receive(&Message{MessageID: "m2", UserID: "user", Content: "这个怎么样了", ReplyCtx: "ctx-2"})
	steerOne(t, s)
	time.Sleep(100 * time.Millisecond)
	s.events <- Event{Type: EventResult, Content: "检查完了，一切正常。", Done: true}
	waitUntil(t, "reply under the person's message", func() bool { return p.deliveredUnder("ctx-2", "检查完了") })
	if got := p.cards(); len(got) != 1 {
		t.Fatalf("cards=%v, want the turn's own card only", got)
	}
}

// A person's new message during a background turn with an open card takes the
// card over instead of opening a second one.
func TestNewMessageAdoptsBackgroundCard(t *testing.T) {
	shortAutoCardDelay(t)
	p, s, receive := newRevealEnv(t)
	finishFirstTurn(t, p, s, receive)
	s.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "ls"}
	waitUntil(t, "background card", func() bool { return len(p.cards()) == 2 })

	receive(&Message{MessageID: "m2", UserID: "user", Content: "现在到哪一步了", ReplyCtx: "ctx-2"})
	waitPrompt(t, s)
	s.events <- Event{Type: EventResult, Content: "做到第三步了", Done: true}
	waitUntil(t, "reply under the new message", func() bool { return p.deliveredUnder("ctx-2", "做到第三步了") })
	if got := p.cards(); len(got) != 2 {
		t.Fatalf("cards=%v, want the background card reused", got)
	}
}
