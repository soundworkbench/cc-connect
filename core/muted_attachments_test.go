package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type mutedAttachmentPlatform struct{ stubMediaPlatform }

func (p *mutedAttachmentPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }
func (p *mutedAttachmentPlatform) SendAudio(ctx context.Context, rc any, audio []byte, format string) error {
	return p.Send(ctx, rc, "audio:"+format+":"+string(audio))
}
func (p *mutedAttachmentPlatform) SendVideo(ctx context.Context, rc any, video []byte, format, name string) error {
	return p.Send(ctx, rc, "video:"+format+":"+name+":"+string(video))
}

type mutedCardPlatform struct{ *mutedAttachmentPlatform }

func (p *mutedCardPlatform) ReplyCard(ctx context.Context, rc any, card *Card) error {
	return p.SendCard(ctx, rc, card)
}
func (p *mutedCardPlatform) SendCard(ctx context.Context, rc any, card *Card) error {
	return p.Send(ctx, rc, "agent card")
}

func newMutedJourney(t *testing.T) (*Engine, *mutedAttachmentPlatform, *steeringJourneySession) {
	t.Helper()
	p := &mutedAttachmentPlatform{stubMediaPlatform: stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}
	s := &steeringJourneySession{cujAgentSession: newCUJAgentSession(), started: make(chan string, 16), steered: make(chan string, 16)}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(func() { e.Stop() })
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	return e, p, s
}
func runMutedJob(e *Engine, kind string, mute bool, mode string) error {
	if kind == "timer" {
		return e.ExecuteTimerJob(&TimerJob{ID: "mute", SessionKey: "test:chat:user", Prompt: "scheduled", Description: "scheduled", Mute: mute, SessionMode: mode})
	}
	return e.ExecuteCronJob(&CronJob{ID: "mute", SessionKey: "test:chat:user", Prompt: "scheduled", Description: "scheduled", Mute: mute, SessionMode: mode})
}
func assertJourneyPlatform(t *testing.T, e *Engine, expected Platform) {
	t.Helper()
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	for key, state := range e.interactiveStates {
		state.mu.Lock()
		actual := state.platform
		state.mu.Unlock()
		if actual != expected {
			t.Errorf("session %s platform = %T, want real platform %T", key, actual, expected)
		}
	}
}
func sendJourneyAttachments(t *testing.T, e *Engine, label string) {
	t.Helper()
	if err := e.SendToSessionWithAttachments("test:chat:user", label, []ImageAttachment{{Data: []byte(label + " image")}}, []FileAttachment{{Data: []byte(label + " file")}}, nil, false); err != nil {
		t.Errorf("%s attachment delivery: %v", label, err)
	}
}
func TestScheduledMutedAttachments_DeliverAgentOutputAndNextTurn(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		for _, mode := range []string{"reuse", "new_per_run"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				e, p, s := newMutedJourney(t)
				done := make(chan error, 1)
				go func() { done <- runMutedJob(e, kind, true, mode) }()
				cc27Take(t, s.started)
				if got := p.getSent(); len(got) != 0 {
					t.Errorf("muted start notices: %v", got)
				}
				assertJourneyPlatform(t, e, p)
				sendJourneyAttachments(t, e, "agent text")
				if err := e.SendAudiosToSession("test:chat:user", []FileAttachment{{Data: []byte("audio"), FileName: "clip.ogg"}}); err != nil {
					t.Errorf("audio: %v", err)
				}
				if err := e.SendVideosToSession("test:chat:user", []FileAttachment{{Data: []byte("video"), FileName: "clip.mp4"}}); err != nil {
					t.Errorf("video: %v", err)
				}

				s.events <- Event{Type: EventText, Content: "agent reply"}
				s.events <- Event{Type: EventResult, Content: "agent reply", Done: true}
				if err := cc27Take(t, done); err != nil {
					t.Fatal(err)
				}
				visible := strings.Join(p.getSent(), "|")
				for _, want := range []string{"agent text", "agent reply", "audio:ogg:audio", "video:mp4:clip.mp4:video"} {
					if !strings.Contains(visible, want) {
						t.Errorf("missing %q in %q", want, visible)
					}
				}
				if strings.Contains(visible, scheduledStartNotice("scheduled")) {
					t.Errorf("muted start: %s", visible)
				}
				if len(p.images) != 1 || string(p.images[0].Data) != "agent text image" || len(p.files) != 1 || string(p.files[0].Data) != "agent text file" {
					t.Errorf("muted attachment delivery: images=%v files=%v", p.images, p.files)
				}
				if mode == "reuse" {
					assertJourneyPlatform(t, e, p)
					sendJourneyAttachments(t, e, "between turns")
				} else {
					s = &steeringJourneySession{cujAgentSession: newCUJAgentSession(), started: make(chan string, 16), steered: make(chan string, 16)}
					e.agent.(*controllableAgent).nextSession = s
				}
				e.ReceiveMessage(p, &Message{SessionKey: "test:chat:user", Platform: p.Name(), UserID: "user", Content: "next normal turn", ReplyCtx: "test:chat:user"})
				cc27Take(t, s.started)
				sendJourneyAttachments(t, e, "normal text")
				s.events <- Event{Type: EventResult, Content: "normal reply", Done: true}
				cc27WaitIdle(t, e, "test:chat:user")
				if !strings.Contains(strings.Join(p.getSent(), "|"), "normal reply") {
					t.Error("normal reply lost")
				}
				expected := 2
				if mode == "reuse" {
					expected = 3
				}
				if len(p.images) != expected || len(p.files) != expected {
					t.Errorf("next turn attachments: images=%d files=%d", len(p.images), len(p.files))
				}
			})
		}
	}
}
func TestScheduledMutedNotices_EmptyErrorAndStartupFailure(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		for _, outcome := range []string{"empty", "error", "silent", "startup failure"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				e, p, s := newMutedJourney(t)
				if outcome == "startup failure" {
					e.agent.(*controllableAgent).startSessionFn = func(context.Context, string) (AgentSession, error) { return nil, errors.New("startup failed") }
				}
				done := make(chan error, 1)
				go func() { done <- runMutedJob(e, kind, true, "reuse") }()
				if outcome != "startup failure" {
					cc27Take(t, s.started)
					event := Event{Type: EventResult, Done: true}
					if outcome == "error" {
						event = Event{Type: EventError, Error: errors.New("turn failed")}
					}
					if outcome == "silent" {
						event.Content = "NO_REPLY"
					}
					s.events <- event
				}
				if err := cc27Take(t, done); err != nil {
					t.Fatal(err)
				}
				if got := p.getSent(); len(got) != 0 {
					t.Errorf("muted system notices: %v", got)
				}
				sendJourneyAttachments(t, e, "after exit")
				if len(p.images) != 1 || len(p.files) != 1 {
					t.Error("attachments after exit lost")
				}
			})
		}
	}
}
func TestScheduledAttachments_UnmutedStartAndReply(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		t.Run(kind, func(t *testing.T) {
			e, p, s := newMutedJourney(t)
			done := make(chan error, 1)
			go func() { done <- runMutedJob(e, kind, false, "reuse") }()
			cc27Take(t, s.started)
			if got := p.getSent(); len(got) != 1 || got[0] != scheduledStartNotice("scheduled") {
				t.Errorf("normal start: %v", got)
			}
			sendJourneyAttachments(t, e, "normal")
			s.events <- Event{Type: EventResult, Content: "normal result", Done: true}
			if err := cc27Take(t, done); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(p.getSent(), "|"), "normal result") || len(p.images) != 1 || len(p.files) != 1 {
				t.Error("normal output lost")
			}
		})
	}
}
func testMutedAttachmentCommandLifecycle(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		t.Run(kind, func(t *testing.T) {
			e, p, s := newMutedJourney(t)
			dir := t.TempDir()
			var execute func() error
			var jobID func() string
			when := "24h"
			if kind == "timer" {
				store, err := NewTimerStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				scheduler := NewTimerScheduler(store)
				e.SetTimerScheduler(scheduler)
				defer scheduler.Stop()
				jobID = func() string {
					jobs := store.ListPending()
					if len(jobs) != 1 {
						t.Fatalf("jobs: %v", jobs)
					}
					return jobs[0].ID
				}
				execute = func() error { return e.ExecuteTimerJob(store.Get(jobID())) }
			} else {
				store, err := NewCronStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				scheduler := NewCronScheduler(store)
				e.SetCronScheduler(scheduler)
				defer scheduler.Stop()
				when = "0 9 * * *"
				jobID = func() string {
					jobs := store.ListBySessionKey("test:chat:user")
					if len(jobs) != 1 {
						t.Fatalf("jobs: %v", jobs)
					}
					return jobs[0].ID
				}
				execute = func() error { return e.ExecuteCronJob(store.Get(jobID())) }
			}
			send := func(content string) {
				e.ReceiveMessage(p, &Message{SessionKey: "test:chat:user", Platform: p.Name(), UserID: "user", Content: content, ReplyCtx: "test:chat:user"})
			}
			send("/" + kind + " add " + when + " attachment journey")
			id := jobID()
			send("/" + kind + " mute " + id)
			send("/" + kind + " list")
			if visible := strings.Join(p.getSent(), "|"); !strings.Contains(visible, id) || !strings.Contains(visible, "[mute]") {
				t.Fatalf("task not visibly muted: %s", visible)
			}
			p.clearSent()
			done := make(chan error, 1)
			go func() { done <- execute() }()
			cc27Take(t, s.started)
			sendJourneyAttachments(t, e, "scheduled text")
			// The CLI uses /send; exercise its actual handler during the muted turn.
			api := &APIServer{engines: map[string]*Engine{"test": e}}
			body, err := json.Marshal(SendRequest{Project: "test", SessionKey: "test:chat:user", Images: []ImageAttachment{{Data: []byte("CLI image")}}, Files: []FileAttachment{{Data: []byte("CLI file")}}})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			api.handleSend(w, httptest.NewRequest(http.MethodPost, "/send", bytes.NewReader(body)))
			if w.Code != http.StatusOK {
				t.Errorf("CLI send = %d %q", w.Code, w.Body.String())
			}
			s.events <- Event{Type: EventResult, Content: "scheduled reply", Done: true}
			if err := cc27Take(t, done); err != nil {
				t.Fatal(err)
			}
			send("next normal turn")
			cc27Take(t, s.started)
			sendJourneyAttachments(t, e, "next text")
			s.events <- Event{Type: EventResult, Content: "next reply", Done: true}
			cc27WaitIdle(t, e, "test:chat:user")
			visible := strings.Join(p.getSent(), "|")
			for _, want := range []string{"scheduled text", "scheduled reply", "next text", "next reply"} {
				if !strings.Contains(visible, want) {
					t.Errorf("missing %q in %q", want, visible)
				}
			}
			if strings.Contains(visible, scheduledStartNotice("attachment journey")) {
				t.Error("muted start leaked")
			}
			if len(p.images) != 3 || len(p.files) != 3 {
				t.Errorf("lifecycle attachments: %d images, %d files", len(p.images), len(p.files))
			}
		})
	}
}
func TestScheduledMutedShell_OutputSuppressed(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		for _, exit := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/exit=%d", kind, exit), func(t *testing.T) {
				e, p, _ := newMutedJourney(t)
				command := fmt.Sprintf("echo shell-output; exit %d", exit)
				var err error
				if kind == "timer" {
					err = e.ExecuteTimerJob(&TimerJob{ID: "shell", SessionKey: "test:chat:user", Exec: command, Mute: true})
				} else {
					err = e.ExecuteCronJob(&CronJob{ID: "shell", SessionKey: "test:chat:user", Exec: command, Mute: true})
				}
				if (err != nil) != (exit != 0) {
					t.Fatalf("shell error: %v", err)
				}
				if got := p.getSent(); len(got) != 0 {
					t.Errorf("muted shell output: %v", got)
				}
			})
		}
	}
}

func TestScheduledMutedAttachments_CardCapabilityPreserved(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		t.Run(kind, func(t *testing.T) {
			e, base, s := newMutedJourney(t)
			p := &mutedCardPlatform{base}
			e.platforms = []Platform{p}
			done := make(chan error, 1)
			go func() { done <- runMutedJob(e, kind, true, "reuse") }()
			cc27Take(t, s.started)
			e.interactiveMu.Lock()
			state := e.interactiveStates["test:chat:user"]
			e.interactiveMu.Unlock()
			state.mu.Lock()
			platform, replyCtx := state.platform, state.replyCtx
			state.mu.Unlock()
			e.replyWithCard(platform, replyCtx, &Card{})
			s.events <- Event{Type: EventResult, Content: "agent reply", Done: true}
			if err := cc27Take(t, done); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(p.getSent(), "|"), "agent card") {
				t.Error("agent card capability lost")
			}
		})
	}
}
func TestScheduledMutedAttachments_QueuedTurnDoesNotMuteNormalSuccessor(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		t.Run(kind, func(t *testing.T) {
			e, p, s := newMutedJourney(t)
			send := func(text string) {
				e.ReceiveMessage(p, &Message{SessionKey: "test:chat:user", Platform: p.Name(), UserID: "user", Content: text, ReplyCtx: "test:chat:user"})
			}
			send("first normal")
			cc27Take(t, s.started)
			if err := runMutedJob(e, kind, true, "reuse"); err != nil {
				t.Fatal(err)
			}
			send("next normal")
			s.events <- Event{Type: EventResult, Content: "first reply", Done: true}
			cc27Take(t, s.started)
			assertJourneyPlatform(t, e, p)
			sendJourneyAttachments(t, e, "muted queued text")
			s.events <- Event{Type: EventResult, Done: true}
			cc27Take(t, s.started)
			sendJourneyAttachments(t, e, "normal queued text")
			s.events <- Event{Type: EventResult, Done: true}
			cc27WaitIdle(t, e, "test:chat:user")
			visible := strings.Join(p.getSent(), "|")
			if strings.Count(visible, e.i18n.T(MsgEmptyResponse)) != 1 {
				t.Errorf("only normal empty reply should be visible: %s", visible)
			}
			for _, want := range []string{"first reply", "muted queued text", "normal queued text"} {
				if !strings.Contains(visible, want) {
					t.Errorf("missing %q in %q", want, visible)
				}
			}
			if len(p.images) != 2 || len(p.files) != 2 {
				t.Errorf("queued attachments: images=%d files=%d", len(p.images), len(p.files))
			}
		})
	}
}

func TestScheduledMutedAttachments_RestartRestoresRealPlatform(t *testing.T) {
	e, p, s := newMutedJourney(t)
	dir := t.TempDir()
	key := "test:chat:user"
	e.SetDataDir(dir)
	session := e.sessions.GetOrCreateActive(key)
	path := filepath.Join(dir, "queues", queueFileName(e.name, key))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// Load the existing durable schema, including queues written by v36.
	record := queueRecord{Project: e.name, Key: key, SessionID: session.ID, Platform: p.Name(), ReplySessionKey: key, SessionKey: key, Content: "restored scheduled input", Scheduled: true, Muted: true, InputID: "timer:restored"}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	cc27Take(t, s.started)
	assertJourneyPlatform(t, e, p)
	sendJourneyAttachments(t, e, "restored agent text")
	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: p.Name(), UserID: "user", Content: "normal after restart", ReplyCtx: key})
	s.events <- Event{Type: EventResult, Done: true}
	cc27Take(t, s.started)
	sendJourneyAttachments(t, e, "normal after restart text")
	s.events <- Event{Type: EventResult, Done: true}
	cc27WaitIdle(t, e, key)
	visible := strings.Join(p.getSent(), "|")
	if strings.Count(visible, e.i18n.T(MsgEmptyResponse)) != 1 || !strings.Contains(visible, "restored agent text") || !strings.Contains(visible, "normal after restart text") {
		t.Errorf("restart output: %s", visible)
	}
	if len(p.images) != 2 || len(p.files) != 2 {
		t.Errorf("restart attachments: images=%d files=%d", len(p.images), len(p.files))
	}
}

type mutedStreamingPlatform struct {
	*mutedAttachmentPlatform
	card *recordingStreamCard
}

func (p *mutedStreamingPlatform) CreateStreamingCard(ctx context.Context, rc any) (StreamingCard, error) {
	if err := p.Send(ctx, rc, "streaming card created"); err != nil {
		return nil, err
	}
	return p.card, nil
}
func TestScheduledMutedNotices_StreamingCardsOnlyForAgentOutput(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		for _, reply := range []string{"", "agent card reply"} {
			t.Run(kind+"/"+reply, func(t *testing.T) {
				e, base, s := newMutedJourney(t)
				card := &recordingStreamCard{}
				p := &mutedStreamingPlatform{mutedAttachmentPlatform: base, card: card}
				e.platforms = []Platform{p}
				done := make(chan error, 1)
				go func() { done <- runMutedJob(e, kind, true, "reuse") }()
				cc27Take(t, s.started)
				if got := p.getSent(); len(got) != 0 {
					t.Errorf("muted card created before agent output: %v", got)
				}
				s.events <- Event{Type: EventResult, Content: reply, Done: true}
				if err := cc27Take(t, done); err != nil {
					t.Fatal(err)
				}
				if reply == "" {
					if len(p.getSent()) != 0 || card.finalized() {
						t.Error("empty scheduled result created a completion card")
					}
				} else {
					if !card.finalized() || !strings.Contains(card.finalContent(), reply) {
						t.Errorf("agent reply card lost: %q", card.finalContent())
					}
				}
			})
		}
	}
}

func TestScheduledMutedNotices_StreamingSilentChunksAndNonterminalResult(t *testing.T) {
	for _, kind := range []string{"timer", "cron"} {
		for _, outcome := range []string{"chunked NO_REPLY", "nonterminal result", "hidden footer", "ellipsis", "hidden thinking", "hidden tool", "whitespace result", "ctx-only result"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				e, base, s := newMutedJourney(t)
				card := &recordingStreamCard{}
				p := &mutedStreamingPlatform{mutedAttachmentPlatform: base, card: card}
				e.platforms = []Platform{p}
				done := make(chan error, 1)
				go func() { done <- runMutedJob(e, kind, true, "reuse") }()
				cc27Take(t, s.started)
				switch outcome {
				case "chunked NO_REPLY":
					s.events <- Event{Type: EventText, Content: "NO_"}
					s.events <- Event{Type: EventText, Content: "REPLY"}
					s.events <- Event{Type: EventResult, Content: "NO_REPLY", Done: true}
				case "nonterminal result":
					s.events <- Event{Type: EventResult, Content: "compacting", Done: false}
					s.events <- Event{Type: EventResult, Done: true}
				case "hidden footer":
					s.events <- Event{Type: EventText, Content: "*gpt-5.5 · xhigh · out 864 in 177.7k cr 175.5k · ctx 69%*"}
					s.events <- Event{Type: EventResult, Content: "NO_REPLY", Done: true}
				case "hidden tool":
					s.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "echo hidden"}
					s.events <- Event{Type: EventResult, Done: true}
				case "whitespace result":
					s.events <- Event{Type: EventResult, Content: " \n", Done: true}
				case "ctx-only result":
					s.events <- Event{Type: EventResult, Content: "[ctx: ~20%]", Done: true}
				case "hidden thinking":
					s.events <- Event{Type: EventThinking, Content: "private thought"}
					s.events <- Event{Type: EventResult, Done: true}
				case "ellipsis":
					s.events <- Event{Type: EventText, Content: "..."}
					s.events <- Event{Type: EventResult, Content: "NO_REPLY", Done: true}
				}
				if err := cc27Take(t, done); err != nil {
					t.Fatal(err)
				}
				if len(p.getSent()) != 0 || card.finalized() {
					t.Errorf("silent result created card: %v %q", p.getSent(), card.finalContent())
				}
			})
		}
	}
}
