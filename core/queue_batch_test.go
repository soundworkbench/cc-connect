package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestQueueBatch_QueuedMessagesSendOnceInOrder(t *testing.T) {
	p := &queueTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	s := queueTestSession()
	e := queueTestEngine(t, t.TempDir(), p, s)
	e.SetQueueBatchConfig(DefaultQueueBatchConfig())
	queueTestSend(e, p, "first", "active")
	cc27Take(t, s.sent)
	for i := 1; i <= 3; i++ {
		e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: p.Name(), UserID: "user", MessageID: fmt.Sprint(i), Content: fmt.Sprintf("queued%d", i), ReplyCtx: fmt.Sprint(i), Files: []FileAttachment{{FileName: fmt.Sprintf("file%d", i), Data: []byte{byte(i)}}}, UserMessageTimeMs: int64(i + 1000)})
	}
	s.events <- Event{Type: EventResult, Content: "first done", Done: true}
	got := cc27Take(t, s.sent)
	prev := -1
	for i := 1; i <= 3; i++ {
		pos := strings.Index(got, fmt.Sprintf("queued%d", i))
		if pos <= prev {
			t.Fatalf("queued inputs missing or out of order: %q", got)
		}
		prev = pos
	}
	s.events <- Event{Type: EventResult, Content: "batch done", Done: true}
	cc27WaitIdle(t, e, "test:user")
	if len(s.sent) != 0 {
		t.Fatal("extra agent Send")
	}
	history := e.sessions.GetOrCreateActive("test:user").GetHistory(0)
	if len(history) != 4 || !strings.Contains(history[2].Content, "queued3") {
		t.Fatalf("history=%v", history)
	}
}

type batchDelivery struct {
	prompt, id string
	images     []ImageAttachment
	files      []FileAttachment
}
type batchSession struct {
	*cujAgentSession
	deliveries chan batchDelivery
}

func (s *batchSession) Send(prompt, id string, images []ImageAttachment, files []FileAttachment) error {
	s.deliveries <- batchDelivery{prompt, id, images, files}
	return nil
}

type batchPlatform struct {
	stubPlatformEngine
	routes chan any
	merged chan any
}

func (p *batchPlatform) Send(ctx context.Context, rc any, text string) error {
	p.routes <- rc
	return p.stubPlatformEngine.Send(ctx, rc, text)
}
func (p *batchPlatform) Reply(ctx context.Context, rc any, text string) error {
	p.routes <- rc
	return p.stubPlatformEngine.Reply(ctx, rc, text)
}
func (p *batchPlatform) StartSteerReaction(_ context.Context, rc any) func(bool) {
	return func(done bool) {
		if done {
			p.merged <- rc
		}
	}
}
func batchEngine(t *testing.T) (*Engine, *batchPlatform, *batchSession) {
	t.Helper()
	p := &batchPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, routes: make(chan any, 100), merged: make(chan any, 100)}
	s := &batchSession{cujAgentSession: newCUJAgentSession(), deliveries: make(chan batchDelivery, 100)}
	e := NewEngine("batch", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
	e.SetDataDir(t.TempDir())
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	t.Cleanup(func() { e.cancel(); _ = e.Stop() })
	return e, p, s
}
func batchMessage(n int) *Message {
	return &Message{SessionKey: "test:user", Platform: "test", MessageID: fmt.Sprint(n), UserID: "person", UserName: "Alice", Content: fmt.Sprintf("内容%d", n), ReplyCtx: fmt.Sprint(n), UserMessageTimeMs: 1700000000000 + int64(n), Images: []ImageAttachment{{Data: []byte{byte(n)}, MimeType: "image/png"}}, Files: []FileAttachment{{FileName: fmt.Sprintf("file%d", n), Data: []byte{byte(n)}}}}
}
func TestQueueBatch_AttachmentsMarkersAndFinalReplyBothPaths(t *testing.T) {
	for _, path := range []string{"event-result", "drain", "orphan-start"} {
		t.Run(path, func(t *testing.T) {
			e, p, s := batchEngine(t)
			var lockGen uint64
			if path == "event-result" {
				e.ReceiveMessage(p, batchMessage(0))
				cc27Take(t, s.deliveries)
			} else {
				session := e.sessions.GetOrCreateActive("test:user")
				lockGen, _ = session.TryLock()
				e.ensureInteractiveStateForQueueing("test:user", p, "0", session)
				if path == "drain" {
					e.interactiveStates["test:user"].agentSession = s
				}
			}
			for i := 1; i <= 3; i++ {
				e.ReceiveMessage(p, batchMessage(i))
			}
			if path == "event-result" {
				s.events <- Event{Type: EventResult, Content: "first", Done: true}
			} else {
				go e.drainOrphanedQueue(e.sessions.GetOrCreateActive("test:user"), e.sessions, "test:user", e.agent, "", lockGen)
			}
			got := cc27Take(t, s.deliveries)
			if got.id != "3" || len(got.images) != 3 || len(got.files) != 3 {
				t.Fatalf("delivery=%+v", got)
			}
			pos := -1
			for i := 1; i <= 3; i++ {
				marker := fmt.Sprintf("【消息 %d/3｜来源：human test / Alice｜", i)
				next := strings.Index(got.prompt, marker)
				if next <= pos || !strings.Contains(got.prompt, fmt.Sprintf("内容%d", i)) || got.images[i-1].Data[0] != byte(i) || got.files[i-1].Data[0] != byte(i) {
					t.Fatalf("order or attachments=%+v", got)
				}
				pos = next
			}
			seen := map[any]bool{}
			for i := 0; i < 2; i++ {
				seen[cc27Take(t, p.merged)] = true
			}
			if !seen["1"] || !seen["2"] {
				t.Fatal(seen)
			}
			s.events <- Event{Type: EventResult, Content: "batch answer", Done: true}
			cc27WaitIdle(t, e, "test:user")
			if len(s.deliveries) != 0 {
				t.Fatal("more than one queued Send")
			}
			if sent := p.getSent(); !slices.Contains(sent, "batch answer") {
				t.Fatal(sent)
			}
			var last any
			for len(p.routes) > 0 {
				last = <-p.routes
			}
			if last != "3" {
				t.Fatalf("reply route=%v", last)
			}
		})
	}
}

func TestQueueBatch_LimitsBoundariesAndDisabled(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		maxMessages, maxChars int
		disabled              bool
		boundary              string
		want                  []int
	}{
		{"messages", 2, 20000, false, "", []int{2, 1}},
		{"characters", 20, 1, false, "", []int{1, 1, 1}},
		{"disabled", 20, 20000, true, "", []int{1, 1, 1}},
		{"slash", 20, 20000, false, "/unknown", []int{1, 1, 1}},
		{"quoted-slash", 20, 20000, false, "quoted-slash", []int{1, 1, 1}},
		{"mode", 20, 20000, false, "mode", []int{1, 1, 1}},
		{"receipt", 20, 20000, false, "receipt", []int{1, 1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, p, s := batchEngine(t)
			cfg := DefaultQueueBatchConfig()
			cfg.Enabled = !tc.disabled
			cfg.MaxMessages = tc.maxMessages
			cfg.MaxChars = tc.maxChars
			cfg.Hold = 0
			e.SetQueueBatchConfig(cfg)
			e.ReceiveMessage(p, batchMessage(0))
			cc27Take(t, s.deliveries)
			for i := 1; i <= 3; i++ {
				m := batchMessage(i)
				if i == 2 {
					switch tc.boundary {
					case "/unknown":
						m.Content = "/unknown"
					case "quoted-slash":
						m.Content = "/unknown"
						m.ExtraContent = "quoted context"
					case "mode":
						m.ModeOverride = "plan"
					case "receipt":
						m.IsPermissionResponse = true
					}
				}
				// Permission callbacks are normally consumed before queue admission.
				if tc.boundary == "receipt" && i == 2 {
					if handled, err := e.admitBusyMessage(p, m, "test:user", e.sessions.GetOrCreateActive("test:user"), e.sessions); !handled || err != nil {
						t.Fatal(err)
					}
				} else {
					e.ReceiveMessage(p, m)
				}
			}
			next := 1
			for _, count := range tc.want {
				s.events <- Event{Type: EventResult, Content: "done", Done: true}
				got := cc27Take(t, s.deliveries)
				if len(got.files) != count {
					t.Fatalf("files=%d want=%d prompt=%q", len(got.files), count, got.prompt)
				}
				for i := next; i < next+count; i++ {
					if got.files[i-next].Data[0] != byte(i) {
						t.Fatal("order lost")
					}
				}
				if tc.disabled && got.prompt != fmt.Sprintf("内容%d", next) {
					t.Fatalf("disabled changed bytes: %q", got.prompt)
				}
				next += count
			}
			s.events <- Event{Type: EventResult, Content: "done", Done: true}
			cc27WaitIdle(t, e, "test:user")
		})
	}
}

func TestQueueBatch_IdleHoldExpiryHumanUrgentAndZero(t *testing.T) {
	for _, kind := range []string{"expiry", "human", "urgent", "zero", "immediate-source", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			e, p, s := batchEngine(t)
			cfg := DefaultQueueBatchConfig()
			cfg.Hold = time.Hour
			if kind == "expiry" {
				cfg.Hold = 300 * time.Millisecond
			}
			switch kind {
			case "zero":
				cfg.Hold = 0
			case "immediate-source":
				cfg.ImmediateSources = []string{"relay"}
			case "disabled":
				cfg.Enabled = false
			}
			e.SetQueueBatchConfig(cfg)
			first := batchMessage(1)
			first.Source = "relay"
			if kind == "urgent" {
				first.Urgent = true
			}
			e.ReceiveMessage(p, first)
			switch kind {
			case "zero", "immediate-source", "disabled", "urgent":
				got := cc27Take(t, s.deliveries)
				if got.prompt != "内容1" {
					t.Fatal(got.prompt)
				}
			default:
				second := batchMessage(2)
				second.Source = "relay"
				e.ReceiveMessage(p, second)
				select {
				case got := <-s.deliveries:
					t.Fatalf("sent before hold: %q", got.prompt)
				case <-time.After(25 * time.Millisecond):
				}
				if kind == "human" {
					e.ReceiveMessage(p, batchMessage(3))
				}
				got := cc27Take(t, s.deliveries)
				if !strings.Contains(got.prompt, "内容1") || !strings.Contains(got.prompt, "内容2") {
					t.Fatal(got.prompt)
				}
				if kind == "human" && !strings.Contains(got.prompt, "内容3") {
					t.Fatal("human did not wake waiting inputs", got.prompt)
				}
			}
			s.events <- Event{Type: EventResult, Content: "batch answer", Done: true}
			cc27WaitIdle(t, e, "test:user")
			if len(s.deliveries) != 0 {
				t.Fatal("unexpected turn")
			}
		})
	}
}
func TestQueueBatch_ScheduledHoldAndShutdownPreserveSnapshot(t *testing.T) {
	e, p, s := batchEngine(t)
	cfg := DefaultQueueBatchConfig()
	cfg.Hold = time.Hour
	e.SetQueueBatchConfig(cfg)
	session := e.sessions.GetOrCreateActive("test:user")
	for i := 1; i <= 2; i++ {
		msg := batchMessage(i)
		msg.scheduledPrompt = true
		msg.scheduledInputID = fmt.Sprintf("timer:job%d:1", i)
		msg.UserMessageTimeMs = 0
		queued, _, err := e.admitScheduledPrompt(p, msg, session, e.sessions, e.agent, "test:user", "")
		if !queued || err != nil {
			t.Fatalf("admission=%v %v", queued, err)
		}
	}
	if len(s.deliveries) != 0 {
		t.Fatal("timer started during hold")
	}
	start := time.Now()
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("shutdown waited for hold")
	}
	records, err := readQueueFile(filepath.Join(e.dataDir, "queues", queueFileName(e.name, "test:user")), e.name)
	if err != nil || len(records) != 2 || records[0].QueuedAt.IsZero() || records[0].Source != "timer" {
		t.Fatalf("records=%v err=%v", records, err)
	}
}

func TestQueueBatch_AssistantUrgentWakesIdle(t *testing.T) {
	_, e, s, c, key := cc17Fixture(t)
	cfg := DefaultQueueBatchConfig()
	cfg.Hold = time.Hour
	e.SetQueueBatchConfig(cfg)
	for i := 1; i <= 3; i++ {
		request := assistantRequest{Project: "project", SessionKey: key, RequestID: fmt.Sprint(i), Message: fmt.Sprintf("automation%d", i), Urgent: i == 3}
		if i == 2 {
			request.Source = "relay"
		}
		status, result := cc17Post(t, c, "/assistant/inject", request)
		if status != 200 || result["queued"] != true {
			t.Fatalf("receipt=%d %v", status, result)
		}
		if i < 3 && len(s.started) != 0 {
			t.Fatal("automation started before urgent wake")
		}
	}
	got := cc27Take(t, s.started)
	for i := 1; i <= 3; i++ {
		if !strings.Contains(got, fmt.Sprintf("automation%d", i)) {
			t.Fatal(got)
		}
	}
	s.events <- Event{Type: EventResult, Content: "unified response", Done: true}
	cc27WaitIdle(t, e, key)
	if len(s.started) != 0 {
		t.Fatal("more than one turn")
	}
}

func TestQueueBatch_StopOrNewDuringHold(t *testing.T) {
	for _, command := range []string{"/stop", "/new"} {
		t.Run(command, func(t *testing.T) {
			e, p, s := batchEngine(t)
			cfg := DefaultQueueBatchConfig()
			cfg.Hold = time.Hour
			e.SetQueueBatchConfig(cfg)
			for i := 1; i <= 2; i++ {
				msg := batchMessage(i)
				msg.Source = "relay"
				e.ReceiveMessage(p, msg)
			}
			e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: p.Name(), Content: command, ReplyCtx: "command"})
			if command == "/stop" {
				got := cc27Take(t, s.deliveries)
				if !strings.Contains(got.prompt, "内容1") || !strings.Contains(got.prompt, "内容2") {
					t.Fatal(got.prompt)
				}
				s.events <- Event{Type: EventResult, Content: "continued", Done: true}
				cc27WaitIdle(t, e, "test:user")
			} else {
				select {
				case got := <-s.deliveries:
					t.Fatalf("reset input was sent: %q", got.prompt)
				case <-time.After(30 * time.Millisecond):
				}
				visible := strings.Join(p.getSent(), "\n")
				if !strings.Contains(visible, "内容1") || !strings.Contains(visible, "内容2") {
					t.Fatalf("discard notices=%v", p.getSent())
				}
			}
		})
	}
}

func TestQueueBatch_ExactUnicodeLimitAndPersistenceFailure(t *testing.T) {
	e, p, _ := batchEngine(t)
	messages := []queuedMessage{{content: "甲", platform: p, msgPlatform: p.Name(), msgSessionKey: "test:user", source: "relay", queuedAt: time.Unix(1, 0)}, {content: "乙", platform: p, msgPlatform: p.Name(), msgSessionKey: "test:user", source: "relay", queuedAt: time.Unix(2, 0)}, {content: "丙", platform: p, msgPlatform: p.Name(), msgSessionKey: "test:user", source: "relay", queuedAt: time.Unix(3, 0)}}
	cfg := DefaultQueueBatchConfig()
	cfg.MaxChars = utf8.RuneCountInString(e.batchContent(messages[:2]))
	e.SetQueueBatchConfig(cfg)
	state := &interactiveState{pendingMessages: messages, queueKey: "test:user", queueSessionID: "s1"}
	q, ok := e.popQueuedBatchLocked(state)
	if !ok || len(q.mergedFrom) != 2 || len(state.pendingMessages) != 1 || state.pendingMessages[0].content != "丙" {
		t.Fatalf("q=%v pending=%v", q, state.pendingMessages)
	}
	records, err := readQueueFile(filepath.Join(e.dataDir, "queues", queueFileName(e.name, "test:user")), e.name)
	if err != nil || len(records) != 1 || records[0].Content != "丙" {
		t.Fatalf("durable remainder=%v %v", records, err)
	}
	// A failed durable removal must retain the entire batch for a later attempt.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	e.SetDataDir(blocker)
	state.pendingMessages = messages
	if _, ok := e.popQueuedBatchLocked(state); ok || len(state.pendingMessages) != 3 {
		t.Fatal("failed persistence removed inputs")
	}
	cfg.MaxChars--
	e.SetQueueBatchConfig(cfg)
	state.queueKey = ""
	q, ok = e.popQueuedBatchLocked(state)
	if !ok || q.content != "甲" || len(state.pendingMessages) != 2 {
		t.Fatal("Unicode cap did not stop before boundary")
	}
}

func TestQueueBatch_IdleDeadAgentCanHoldAndResume(t *testing.T) {
	e, p, s := batchEngine(t)
	cfg := DefaultQueueBatchConfig()
	cfg.Hold = time.Hour
	e.SetQueueBatchConfig(cfg)
	old := newCUJAgentSession()
	_ = old.Close()
	session := e.sessions.GetOrCreateActive("test:user")
	e.ensureInteractiveStateForQueueing("test:user", p, "old", session)
	e.interactiveStates["test:user"].agentSession = old
	msg := batchMessage(1)
	msg.Source = "relay"
	e.ReceiveMessage(p, msg)
	if len(s.deliveries) != 0 {
		t.Fatal("dead process resumed before hold")
	}
	e.ReceiveMessage(p, batchMessage(2))
	got := cc27Take(t, s.deliveries)
	if !strings.Contains(got.prompt, "内容1") || !strings.Contains(got.prompt, "内容2") {
		t.Fatal(got.prompt)
	}
	s.events <- Event{Type: EventResult, Content: "resumed", Done: true}
	cc27WaitIdle(t, e, "test:user")
}

func TestQueueBatch_CompletedScheduledReplayDoesNotHoldSession(t *testing.T) {
	e, p, s := batchEngine(t)
	cfg := DefaultQueueBatchConfig()
	cfg.Hold = 0
	e.SetQueueBatchConfig(cfg)
	session := e.sessions.GetOrCreateActive("test:user")
	msg := batchMessage(1)
	msg.scheduledPrompt = true
	msg.scheduledInputID = "timer:one:1"
	queued, lockGen, err := e.admitScheduledPrompt(p, msg, session, e.sessions, e.agent, "test:user", "")
	if err != nil || queued {
		t.Fatalf("initial admission=%v %v", queued, err)
	}
	go e.processInteractiveMessageWith(p, msg, session, e.agent, e.sessions, "test:user", "", "test:user", lockGen)
	cc27Take(t, s.deliveries)
	s.events <- Event{Type: EventResult, Content: "first", Done: true}
	cc27WaitIdle(t, e, "test:user")
	cfg.Hold = time.Hour
	e.SetQueueBatchConfig(cfg)
	queued, _, err = e.admitScheduledPrompt(p, msg, session, e.sessions, e.agent, "test:user", "")
	if err != nil || !queued || session.Busy() {
		t.Fatalf("replay created a wait or turn: queued=%v busy=%v err=%v", queued, session.Busy(), err)
	}
	if len(s.deliveries) != 0 {
		t.Fatal("completed occurrence replayed")
	}
}
