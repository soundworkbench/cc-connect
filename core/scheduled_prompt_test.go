package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type scheduledTestPlatform struct{ *steerReactionPlatform }

func (p *scheduledTestPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }

type scheduledTestSession struct {
	*steeringJourneySession
	modes chan string
	ids   chan string
}

func (s *scheduledTestSession) Send(prompt, id string, images []ImageAttachment, files []FileAttachment) error {
	s.ids <- id
	return s.steeringJourneySession.Send(prompt, id, images, files)
}

func (s *scheduledTestSession) SetLiveMode(mode string) bool { s.modes <- mode; return true }

func cc27Take[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for input/feedback")
		var zero T
		return zero
	}
}
func cc27WaitIdle(t *testing.T, e *Engine, key string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for e.sessions.GetOrCreateActive(key).Busy() {
		if time.Now().After(deadline) {
			t.Fatal("session still busy")
		}
		time.Sleep(time.Millisecond)
	}
}

// Busy scheduled prompts join the actual Engine FIFO even when the normal
// user queue is full. User -> timer -> cron -> timer must reach the agent once,
// in order, retaining agent delivery and each scheduled permission mode.
func TestScheduledPromptBusyFIFO(t *testing.T) {
	p := &scheduledTestPlatform{&steerReactionPlatform{stubPlatformEngine: stubPlatformEngine{n: "scheduled"}, reactions: make(chan string, 16)}}
	s := &scheduledTestSession{steeringJourneySession: &steeringJourneySession{cujAgentSession: newCUJAgentSession(), started: make(chan string, 16), steered: make(chan string, 16)}, modes: make(chan string, 16), ids: make(chan string, 16)}
	e := NewEngine("scheduled", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.cancel()
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	e.maxQueuedMessages = 1
	key := "scheduled:chat:user"
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "u1", UserID: "user", Content: "first", ReplyCtx: "first"})
	cc27Take(t, s.started)
	// Force the ordinary user-message fallback, then exceed its queue bound with jobs.
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "u2", UserID: "user", Content: "user queued", Files: []FileAttachment{{Data: []byte("file")}}, ReplyCtx: "u2"})
	timer := &TimerJob{ID: "t1", SessionKey: key, Prompt: "timer one", ScheduledAt: time.Unix(100, 0), Mute: true, Mode: "auto"}
	if err := e.ExecuteTimerJob(timer); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteTimerJob(timer); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteCronJob(&CronJob{ID: "c1", SessionKey: key, Prompt: "cron two", Mute: true, Mode: "plan"}); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteTimerJob(&TimerJob{ID: "t2", SessionKey: key, Prompt: "timer three", ScheduledAt: time.Unix(101, 0), Mute: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"user queued", "timer one", "cron two", "timer three"} {
		s.events <- Event{Type: EventResult, Content: "completed", Done: true}
		got := cc27Take(t, s.started)
		if !strings.Contains(got, want) {
			t.Fatalf("FIFO: got %q want %q", got, want)
		}
	}
	s.events <- Event{Type: EventResult, Content: "completed", Done: true}
	cc27WaitIdle(t, e, key)
	for _, want := range []string{"auto", "default", "plan", "default"} {
		if got := cc27Take(t, s.modes); got != want {
			t.Fatalf("mode got %s want %s", got, want)
		}
	}
	if len(s.started) != 0 {
		t.Fatal("scheduled duplicate was replayed")
	}
	for _, want := range []string{"u1", "u2", "", "", ""} {
		if got := cc27Take(t, s.ids); got != want {
			t.Fatalf("synthetic scheduler ID leaked to platform/agent message ID: got %q want %q", got, want)
		}
	}
	sent := p.getSent()
	count := 0
	for _, v := range sent {
		if v == "completed" {
			count++
		}
	}
	if count != 5 {
		t.Fatalf("scheduled agent responses missing: %v", sent)
	}
}

// Models a capability that becomes ready AFTER initial Send has completed,
// e.g. the first resumed CLI turn or a temporary stdin-lock fallback.
type resumedSteerSession struct {
	*steeringJourneySession
	available atomic.Bool
}

func (s *resumedSteerSession) Steer(prompt, id string, images []ImageAttachment, files []FileAttachment) (bool, error) {
	if !s.available.Load() {
		return false, nil
	}
	return s.steeringJourneySession.Steer(prompt, id, images, files)
}
func cc27ResumedJourney(t *testing.T, timerFirst, eventRetry bool) {
	t.Helper()
	p := &scheduledTestPlatform{&steerReactionPlatform{stubPlatformEngine: stubPlatformEngine{n: "resumed"}, reactions: make(chan string, 16)}}
	s := &resumedSteerSession{steeringJourneySession: &steeringJourneySession{cujAgentSession: newCUJAgentSession(), started: make(chan string, 16), steered: make(chan string, 16), accept: true}}
	file := filepath.Join(t.TempDir(), "sessions.json")
	saved := NewSessionManager(file)
	key := "resumed:chat:user"
	saved.GetOrCreateActive(key).SetAgentSessionID("cuj-agent-session", "controllable")
	saved.Save()
	resumed := make(chan string, 1)
	agent := &controllableAgent{startSessionFn: func(_ context.Context, id string) (AgentSession, error) { resumed <- id; return s, nil }}
	e := NewEngine("resumed", agent, []Platform{p}, file, LangEnglish)
	defer e.cancel()
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	done := make(chan error, 1)
	if timerFirst {
		go func() {
			done <- e.ExecuteTimerJob(&TimerJob{ID: "first", SessionKey: key, Prompt: "first", Mute: true})
		}()
	} else {
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "first", UserID: "user", Content: "first", ReplyCtx: "first"})
	}
	if id := cc27Take(t, resumed); id != "cuj-agent-session" {
		t.Fatalf("not restored: %s", id)
	}
	cc27Take(t, s.started)
	// Wait until the Send completion has been consumed; the old code only retries there.
	deadline := time.Now().Add(time.Second)
	for {
		e.interactiveMu.Lock()
		state := e.interactiveStates[key]
		e.interactiveMu.Unlock()
		state.mu.Lock()
		active := state.activePreview != nil
		state.mu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event loop not started")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	send := func(n int) {
		msg := &Message{SessionKey: key, Platform: p.Name(), MessageID: fmt.Sprintf("m%d", n), UserID: "user", Content: fmt.Sprintf("supplement%d", n), ReplyCtx: fmt.Sprint(n)}
		if n == 1 {
			msg.Images = []ImageAttachment{{Data: []byte("image"), MimeType: "image/png"}}
		}
		e.ReceiveMessage(p, msg)
	}
	send(1) // Rejected without writing; now queued.
	s.available.Store(true)
	if eventRetry {
		s.events <- Event{Type: EventToolUse, ToolName: "work", ToolInput: "ready"}
	} else {
		send(2)
	}
	if got := cc27Take(t, s.steered); !strings.Contains(got, "supplement1") {
		t.Fatal(got)
	}
	if eventRetry {
		send(2)
	}
	if got := cc27Take(t, s.steered); !strings.Contains(got, "supplement2") {
		t.Fatal(got)
	}
	send(3)
	if got := cc27Take(t, s.steered); !strings.Contains(got, "supplement3") {
		t.Fatal(got)
	}
	send(3) // duplicate event
	for i := 0; i < 6; i++ {
		got := cc27Take(t, p.reactions)
		if got != "received" && got != "done" {
			t.Fatal(got)
		}
	}
	s.events <- Event{Type: EventResult, Content: "final", Done: true}
	cc27WaitIdle(t, e, key)
	if timerFirst {
		if err := cc27Take(t, done); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.started) != 0 || len(s.steered) != 0 {
		t.Fatal("duplicate or separate queued turn")
	}
	notices := 0
	for _, v := range p.getSent() {
		if v == e.i18n.T(MsgMessageQueued) {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("later messages should use reactions: %v", p.getSent())
	}
}

func cc27ImageJourney(t *testing.T) {
	p := &scheduledTestPlatform{&steerReactionPlatform{stubPlatformEngine: stubPlatformEngine{n: "image-steer"}, reactions: make(chan string, 16)}}
	s := &steeringJourneySession{cujAgentSession: newCUJAgentSession(), started: make(chan string, 16), steered: make(chan string, 16), accept: true}
	file := filepath.Join(t.TempDir(), "sessions.json")
	key := "image-steer:chat:user"
	saved := NewSessionManager(file)
	saved.GetOrCreateActive(key).SetAgentSessionID("cuj-agent-session", "controllable")
	saved.Save()
	e := NewEngine("image-steer", &controllableAgent{nextSession: s}, []Platform{p}, file, LangEnglish)
	defer e.cancel()
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "first", UserID: "user", Content: "first", ReplyCtx: "first"})
	cc27Take(t, s.started)
	for i := 1; i <= 3; i++ {
		msg := &Message{SessionKey: key, Platform: p.Name(), MessageID: fmt.Sprint(i), UserID: "user", Content: fmt.Sprintf("request%d", i), ReplyCtx: i}
		if i != 2 {
			msg.Images = []ImageAttachment{{Data: []byte("image"), MimeType: "image/png"}}
		}
		if i == 1 {
			msg.Content = ""
		}
		e.ReceiveMessage(p, msg)
		if got := cc27Take(t, s.steered); !strings.Contains(got, msg.Content) {
			t.Fatal(got)
		}
	}
	for i := 0; i < 6; i++ {
		cc27Take(t, p.reactions)
	}
	s.events <- Event{Type: EventResult, Content: "image and text handled", Done: true}
	cc27WaitIdle(t, e, key)
	for _, v := range p.getSent() {
		if v == e.i18n.T(MsgMessageQueued) {
			t.Fatalf("queued feedback instead of reactions: %v", p.getSent())
		}
	}
	if len(s.started) != 0 {
		t.Fatal("image/text replayed as queued turns")
	}
}
