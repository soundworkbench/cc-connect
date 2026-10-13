package core

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func TestReviewQueueBatch_FullHoldMustWakeImmediateInput(t *testing.T) {
	for _, kind := range []string{"human", "urgent", "immediate-source"} {
		t.Run(kind, func(t *testing.T) {
			e, p, s := batchEngine(t)
			cfg := DefaultQueueBatchConfig()
			cfg.Hold = time.Hour
			if kind == "immediate-source" {
				cfg.ImmediateSources = []string{"assistant"}
			}
			e.SetQueueBatchConfig(cfg)
			for i := 1; i <= 5; i++ {
				m := batchMessage(i)
				m.Source = "relay"
				e.ReceiveMessage(p, m)
			}
			m := batchMessage(6)
			if kind == "urgent" {
				m.Source = "relay"
				m.Urgent = true
			}
			if kind == "immediate-source" {
				m.Source = "assistant"
			}
			e.ReceiveMessage(p, m)
			select {
			case d := <-s.deliveries:
				prev := -1
				for i := 1; i <= 6; i++ {
					pos := strings.Index(d.prompt, fmt.Sprintf("内容%d", i))
					if pos <= prev {
						t.Fatalf("waiting input missing or reordered: %q", d.prompt)
					}
					prev = pos
				}
				// Once running, the same FIFO still enforces max_depth.
				for i := 7; i <= 12; i++ {
					e.ReceiveMessage(p, batchMessage(i))
				}
				if sent := strings.Join(p.getSent(), "\n"); !strings.Contains(sent, e.i18n.Tf(MsgQueueFull, 5)) {
					t.Fatalf("running queue did not reject overflow: %s", sent)
				}
				s.events <- Event{Type: EventResult, Content: "done", Done: true}
				queued := cc27Take(t, s.deliveries)
				prev = -1
				for i := 7; i <= 11; i++ {
					pos := strings.Index(queued.prompt, fmt.Sprintf("内容%d", i))
					if pos <= prev {
						t.Fatalf("running input missing or reordered: %q", queued.prompt)
					}
					prev = pos
				}
				if strings.Contains(queued.prompt, "内容12") {
					t.Fatalf("overflow admitted: %q", queued.prompt)
				}
				s.events <- Event{Type: EventResult, Content: "queued done", Done: true}
				cc27WaitIdle(t, e, "test:user")
				if len(s.deliveries) != 0 {
					t.Fatal("waiting inputs delivered more than once")
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("no Agent.Send after immediate %s; visible=%v", kind, p.getSent())
			}
		})
	}
}

func TestReviewQueueBatch_CrossPlatformFinalRoute(t *testing.T) {
	e, p, s := batchEngine(t)
	p2 := &batchPlatform{stubPlatformEngine: stubPlatformEngine{n: "other"}, routes: make(chan any, 100), merged: make(chan any, 100)}
	e.ReceiveMessage(p, batchMessage(0))
	cc27Take(t, s.deliveries)
	for i := 1; i <= 3; i++ {
		m := batchMessage(i)
		if i == 3 {
			m.Platform = p2.Name()
			e.ReceiveMessage(p2, m)
		} else {
			e.ReceiveMessage(p, m)
		}
	}
	s.events <- Event{Type: EventResult, Content: "initial answer", Done: true}
	d := cc27Take(t, s.deliveries)
	for i := 1; i <= 3; i++ {
		if !strings.Contains(d.prompt, fmt.Sprintf("内容%d", i)) {
			t.Fatal(d.prompt)
		}
	}
	s.events <- Event{Type: EventResult, Content: "unified answer", Done: true}
	cc27WaitIdle(t, e, "test:user")
	if !strings.Contains(strings.Join(p2.getSent(), "\n"), "unified answer") {
		t.Fatalf("last platform has no reply: %v", p2.getSent())
	}
	if strings.Contains(strings.Join(p.getSent(), "\n"), "unified answer") {
		t.Fatal("reply sent to first platform")
	}
	var last any
	for len(p2.routes) > 0 {
		last = <-p2.routes
	}
	if last != "3" {
		t.Fatalf("last replyCtx=%v", last)
	}
}

func TestReviewQueueBatch_TimerArrivalInterleaving(t *testing.T) {
	for trial := 0; trial < 30; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			e, p, s := batchEngine(t)
			cfg := DefaultQueueBatchConfig()
			cfg.Hold = 2 * time.Millisecond
			e.SetQueueBatchConfig(cfg)
			first := batchMessage(1)
			first.Source = "relay"
			e.ReceiveMessage(p, first)
			time.Sleep(time.Duration(trial%4) * time.Millisecond)
			e.ReceiveMessage(p, batchMessage(2))
			count1, count2 := 0, 0
			for count1 == 0 || count2 == 0 {
				d := cc27Take(t, s.deliveries)
				count1 += strings.Count(d.prompt, "内容1")
				count2 += strings.Count(d.prompt, "内容2")
				s.events <- Event{Type: EventResult, Content: "answer", Done: true}
			}
			cc27WaitIdle(t, e, "test:user")
			if count1 != 1 || count2 != 1 || len(s.deliveries) != 0 {
				t.Fatalf("not once: counts=%d,%d more=%d", count1, count2, len(s.deliveries))
			}
		})
	}
}

// Only the first message is recalled, through the real ReceiveMessage fallback.
type reviewRecallPlatform struct{ *batchPlatform }

func (p *reviewRecallPlatform) IsMessageRecalled(_ context.Context, rc any) (bool, error) {
	return rc == "0", nil
}

func TestExecuteCronJob_DefaultHoldAcceptsThenReplies(t *testing.T) {
	p := &stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := &batchSession{cujAgentSession: newCUJAgentSession(), deliveries: make(chan batchDelivery, 100)}
	e := NewEngine("cron-hold", &resultAgent{session: s}, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	e.SetDataDir(t.TempDir())
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	t.Cleanup(func() { e.cancel(); _ = e.Stop() })
	job := &CronJob{ID: "hold-job", SessionKey: "test:channel:user", Prompt: "summarize activity", Description: "Daily summary"}
	if err := e.ExecuteCronJob(job); err != nil {
		t.Fatal(err)
	}
	if sent := p.getSent(); len(sent) != 1 || sent[0] != "⏰ Daily summary" {
		t.Fatalf("acceptance must only send start notice: %v", sent)
	}
	select {
	case d := <-s.deliveries:
		t.Fatalf("default cron hold sent to agent before wake: %+v", d)
	default:
	}
	e.ReceiveMessage(p, &Message{SessionKey: job.SessionKey, Platform: p.Name(), UserID: "user", Content: "include new activity", MessageID: "wake", ReplyCtx: "wake"})
	d := cc27Take(t, s.deliveries)
	if !strings.Contains(d.prompt, "summarize activity") || strings.Index(d.prompt, "include new activity") <= strings.Index(d.prompt, "summarize activity") {
		t.Fatalf("accepted cron input lost or reordered: %q", d.prompt)
	}
	s.events <- Event{Type: EventResult, Content: "cron complete after wake", Done: true}
	cc27WaitIdle(t, e, job.SessionKey)
	count := 0
	for _, sent := range p.getSent() {
		if sent == "cron complete after wake" {
			count++
		}
	}
	if count != 1 || len(s.deliveries) != 0 {
		t.Fatalf("accepted cron must reply once: %v", p.getSent())
	}
}

func TestQueueBatch_IdleClosePreservesHoldBeforeAdmission(t *testing.T) {
	e, p, s := batchEngine(t)
	// holdIdleMessage publishes the hold before persisting the first input.
	// An already expired idle-close token must not retire that state in between.
	state := &interactiveState{agentSession: s, platform: p, agentSessionIdleToken: 1,
		hold: &queueHold{until: time.Now().Add(time.Hour), wake: make(chan struct{}, 1)}}
	e.interactiveMu.Lock()
	e.interactiveStates["test:user"] = state
	e.interactiveMu.Unlock()
	e.cleanupInteractiveStateForIdleToken("test:user", state, 1, time.Millisecond)
	if !s.Alive() {
		t.Fatal("idle timeout closed an unadmitted waiting window")
	}
	e.interactiveMu.Lock()
	current := e.interactiveStates["test:user"]
	e.interactiveMu.Unlock()
	if current != state {
		t.Fatal("idle timeout removed the waiting window before admission")
	}
}

func TestReviewQueueBatch_RecallFallbackCreatesStateBeforeHold(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			base := &batchPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, routes: make(chan any, 100), merged: make(chan any, 100)}
			p := &reviewRecallPlatform{base}
			s := &batchSession{cujAgentSession: newCUJAgentSession(), deliveries: make(chan batchDelivery, 100)}
			e := NewEngine("review-recall", &controllableAgent{nextSession: s}, []Platform{p}, t.TempDir()+"/sessions.json", LangChinese)
			e.SetDataDir(t.TempDir())
			e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
			cfg := DefaultQueueBatchConfig()
			cfg.Enabled = enabled
			e.SetQueueBatchConfig(cfg)
			panicked := false
			t.Cleanup(func() {
				e.cancel()
				if !panicked {
					_ = e.Stop()
				}
			})
			e.ReceiveMessage(p, batchMessage(0))
			cc27Take(t, s.deliveries)
			m := batchMessage(1)
			m.Source = "relay"
			defer func() {
				if r := recover(); r != nil {
					panicked = true
					t.Errorf("ReceiveMessage panicked after recalling old turn: %v\n%s", r, debug.Stack())
				}
			}()
			e.ReceiveMessage(p, m)
		})
	}
}
