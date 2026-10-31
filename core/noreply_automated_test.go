package core

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Unsolicited (background) turns follow the same NO_REPLY contract as
// foreground turns; history keeps the original text either way.
func TestUnsolicitedReaderNoReplyContract(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		want         []string
	}{
		{"bare", "NO_REPLY", nil},
		{"bare_spaces_lower", "  no_reply \n", nil},
		{"trailing", "后台任务完成\n\nNO_REPLY", []string{"后台任务完成"}},
		{"trailing_only_marker", "\n\n**NO_REPLY", nil},
		{"normal", "All 5 campaigns created successfully", []string{"All 5 campaigns created successfully"}},
		// Agent metadata is removed before the NO_REPLY decision, as in the
		// foreground path (cleanAgentReply).
		{"footer_after_marker", "NO_REPLY\n*claude-opus-4-8[1m] · out 788 · in 442 cw 0 cr 395.1k · ctx 40%*", nil},
		{"ctx_after_marker", "NO_REPLY\n[ctx: ~40%]", nil},
		{"text_then_ctx", "已完成\n[ctx: ~40%]", []string{"已完成"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			sess := newControllableSession("unsol-" + tc.name)
			e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
			defer e.Stop()
			e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
			iKey := "test:ch1:u1"
			session := e.sessions.GetOrCreateActive(iKey)
			state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
			e.interactiveMu.Lock()
			e.interactiveStates[iKey] = state
			e.interactiveMu.Unlock()
			e.startUnsolicitedReader(state, session, e.sessions, iKey, "")
			defer e.stopUnsolicitedReader(state)

			sess.events <- Event{Type: EventResult, Content: tc.result, Done: true}

			// History is written after the send decision, so once it holds the
			// result the reader has finished this turn.
			deadline := time.Now().Add(5 * time.Second)
			for session.HistoryLen() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			hist := session.GetHistory(0)
			if len(hist) != 1 || hist[0].Role != "assistant" || hist[0].Content != tc.result {
				t.Fatalf("history = %+v, want original %q", hist, tc.result)
			}
			if got := p.getSent(); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Fatalf("sent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveSilentReply(t *testing.T) {
	for in, want := range map[string]struct {
		out    string
		silent bool
	}{
		"NO_REPLY":          {"", true},
		" no_reply\n":       {"", true},
		"done\nNO_REPLY":    {"done", false},
		"**NO_REPLY":        {"", true},
		"hello":             {"hello", false},
		"NO_REPLY is fine.": {"NO_REPLY is fine.", false},
	} {
		out, silent := resolveSilentReply(in)
		if out != want.out || silent != want.silent {
			t.Errorf("resolveSilentReply(%q) = %q,%v want %q,%v", in, out, silent, want.out, want.silent)
		}
	}
}

func TestIsHumanTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  *Message
		want bool
	}{
		{"feishu_user", &Message{Platform: "feishu", UserID: "ou_abc"}, true},
		{"web_admin", &Message{Platform: "bridge", UserID: "web-admin"}, true},
		{"nil", nil, false},
		{"timer_tell", &Message{UserID: "timer", scheduledPrompt: true, scheduledInputID: "timer:abc"}, false},
		{"cron", &Message{UserID: "cron", scheduledPrompt: true, scheduledInputID: "cron:job"}, false},
		{"assistant_inject", &Message{UserID: "assistant", scheduledPrompt: true, scheduledInputID: "assistant:x"}, false},
		{"scheduled_unknown_kind", &Message{UserID: "ou_abc", scheduledPrompt: true, scheduledInputID: "x:1"}, false},
		{"explicit_source", &Message{Platform: "feishu", UserID: "ou_abc", Source: "github"}, false},
		{"relay_user", &Message{Platform: "feishu", UserID: "relay"}, false},
		{"relay_prefixed", &Message{Platform: "feishu", UserID: "relay:cli_123"}, false},
		{"webhook", &Message{UserID: "webhook"}, false},
		{"heartbeat", &Message{UserID: "Heartbeat"}, false},
		{"explicit_human_source", &Message{Platform: "feishu", UserID: "ou_abc", Source: "Human"}, true},
		{"batch_with_human", &Message{UserID: "timer", scheduledPrompt: true, scheduledInputID: "timer:abc", humanBatch: true}, true},
	} {
		if got := isHumanTurn(tc.msg); got != tc.want {
			t.Errorf("%s: isHumanTurn = %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsHumanQueuedTurnBatch(t *testing.T) {
	human := queuedMessage{content: "hi", userID: "ou_abc"}
	tell := queuedMessage{content: "tell", userID: "timer", scheduledPrompt: true, scheduledInputID: "timer:1"}
	relay := queuedMessage{content: "relay", userID: "relay"}
	if !isHumanQueuedTurn(human) || isHumanQueuedTurn(tell) {
		t.Fatal("single queued messages misclassified")
	}
	mixed := tell
	mixed.mergedFrom = []queuedMessage{human, tell}
	if !isHumanQueuedTurn(mixed) {
		t.Fatal("batch containing a person's message must stay human")
	}
	auto := tell
	auto.mergedFrom = []queuedMessage{relay, tell}
	if isHumanQueuedTurn(auto) {
		t.Fatal("all-automation batch must be automated")
	}
}

// runTurn drives one turn through processTurnEvents on a Feishu-like platform
// (immediate placeholder + compact thinking-process preview).
func runTurn(t *testing.T, automated bool, events ...Event) []string {
	t.Helper()
	p := &finalOrderPlatform{}
	e := newFinalOrderEngine(t, p, &stubAgent{})
	e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true})
	as := newControllableSession("auto")
	as.events = make(chan Event, len(events)+1)
	for _, ev := range events {
		as.events <- ev
	}
	state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
	e.processTurnEvents(automated, state, e.sessions.GetOrCreateActive("auto"), e.sessions, "auto", "m", time.Now(), nil, nil, "ctx")
	return p.snapshot()
}

func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestAutomatedTurnNoPreview(t *testing.T) {
	work := []Event{
		{Type: EventProgress, Content: "正在检查队列状态，读取最近的派单记录。"},
		{Type: EventToolUse, ToolName: "Bash", ToolInput: "tool status"},
		{Type: EventToolResult, ToolName: "Bash", ToolStatus: "ok"},
	}
	// The agent streams its final text and then reports the same text as the
	// result, as Claude Code does.
	final := func(text string) []Event {
		return append(append([]Event(nil), work...), Event{Type: EventText, Content: text}, Event{Type: EventResult, Content: text, Done: true})
	}
	t.Run("human_has_preview", func(t *testing.T) {
		calls := runTurn(t, false, final("答复")...)
		if countPrefix(calls, "start:") == 0 {
			t.Fatalf("human turn lost its preview: %v", calls)
		}
	})
	for _, result := range []string{"NO_REPLY", "\n**NO_REPLY", "检查完成。\nNO_REPLY"} {
		t.Run("no_preview/"+strings.ReplaceAll(result, "\n", "_"), func(t *testing.T) {
			calls := runTurn(t, true, final(result)...)
			if _, silent := resolveSilentReply(result); silent && len(calls) != 0 {
				t.Fatalf("silent automated turn touched the platform: %v", calls)
			}
			if countPrefix(calls, "start:") != 0 || countPrefix(calls, "delete:") != 0 || countPrefix(calls, "update:") != 0 {
				t.Fatalf("automated turn created or recalled a preview: %v", calls)
			}
		})
	}
	t.Run("non_silent_one_send", func(t *testing.T) {
		calls := runTurn(t, true, final("已处理：队列正常。")...)
		if len(calls) != 1 || !strings.HasPrefix(calls[0], "send:") || !strings.Contains(calls[0], "已处理：队列正常。") {
			t.Fatalf("want exactly one send, got %v", calls)
		}
	})
	t.Run("bare_result_no_events", func(t *testing.T) {
		if calls := runTurn(t, true, Event{Type: EventResult, Content: "NO_REPLY", Done: true}); len(calls) != 0 {
			t.Fatalf("want 0 platform calls, got %v", calls)
		}
	})
}

// scriptedResultSession emits a fixed event script once per turn.
type scriptedResultSession struct {
	*resultAgentSession
	script []Event
}

func (s *scriptedResultSession) Send(prompt string, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.sentPrompts = append(s.sentPrompts, prompt)
	s.sendOnce.Do(func() {
		for _, ev := range s.script {
			s.events <- ev
		}
	})
	return nil
}

// End to end through the timer path that relays use: progress and tool
// events must not create a placeholder or preview card, so a silent final has
// nothing to recall and a visible final is exactly one message.
func TestExecuteTimerJob_AutomatedTurnNoPreviewOrRecall(t *testing.T) {
	for _, result := range []string{"NO_REPLY", "role reply"} {
		t.Run(result, func(t *testing.T) {
			script := []Event{
				{Type: EventProgress, Content: "正在核对派单队列与最近的告警记录。"},
				{Type: EventToolUse, ToolName: "Bash", ToolInput: "tool status"},
				{Type: EventToolResult, ToolName: "Bash", ToolStatus: "ok"},
				{Type: EventText, Content: result},
				{Type: EventResult, Content: result, Done: true},
			}
			base := newResultAgentSession(result)
			base.events = make(chan Event, len(script))
			p := &scheduledLifecyclePlatform{lifecyclePlatform: lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "scheduled"}}}
			e := NewEngine("notice", &resultAgent{session: &scriptedResultSession{resultAgentSession: base, script: script}}, []Platform{p}, "", LangChinese)
			defer e.Stop()
			e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
			e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
			e.SetDisplayConfig(DisplayCfg{Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true})
			if err := e.ExecuteTimerJob(&TimerJob{ID: "tell", SessionKey: "scheduled:chat:user", Prompt: "tell", Description: "💬 助手 A→助手 B"}); err != nil {
				t.Fatal(err)
			}
			p.previewMu.Lock()
			starts, edits, deletes := len(p.starts), len(p.edits), len(p.deletes)
			p.previewMu.Unlock()
			if starts != 0 || edits != 0 || deletes != 0 {
				t.Fatalf("automated turn used a preview: starts=%v edits=%v recalls=%v", p.starts, p.edits, p.deletes)
			}
			sent := p.getSent()
			if result == "NO_REPLY" {
				if len(sent) != 0 {
					t.Fatalf("silent automated turn sent %v", sent)
				}
			} else if len(sent) != 1 || !strings.Contains(sent[0], "role reply") {
				t.Fatalf("want exactly one send containing the reply, got %v", sent)
			}
		})
	}
}

// cardStylePlatform is a text-preview platform whose progress_style is
// "card" (compact progress writer enabled), like Feishu progress_style=card.
type cardStylePlatform struct{ lifecyclePlatform }

func (p *cardStylePlatform) ProgressStyle() string { return "card" }

// plainPlatform has no preview or update support, like Feishu with cards off.
type plainPlatform struct{ stubPlatformEngine }

// Non-compact paths (thinking-process preview off): an automated turn must
// not flush text segments into side messages or send thinking/tool messages.
// The whole text A + B arrives as one final message; a silent turn sends and
// recalls nothing.
func TestAutomatedTurnNonCompactPaths(t *testing.T) {
	platforms := map[string]func() (Platform, *stubPlatformEngine, *lifecyclePlatform){
		"legacy": func() (Platform, *stubPlatformEngine, *lifecyclePlatform) {
			p := &lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "legacy"}, textPreview: true}
			return p, &p.stubPlatformEngine, p
		},
		"card": func() (Platform, *stubPlatformEngine, *lifecyclePlatform) {
			p := &cardStylePlatform{lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "card"}, textPreview: true}}
			return p, &p.stubPlatformEngine, &p.lifecyclePlatform
		},
		"cards_off": func() (Platform, *stubPlatformEngine, *lifecyclePlatform) {
			p := &plainPlatform{stubPlatformEngine{n: "plain"}}
			return p, &p.stubPlatformEngine, nil
		},
	}
	displays := map[string]DisplayCfg{
		"quiet_thinking": {Mode: "quiet", ThinkingMessages: true, HideAgentFooter: true},
		"quiet_tools":    {Mode: "quiet", ToolMessages: true, HideAgentFooter: true},
		"compact":        {Mode: "compact", HideAgentFooter: true},
		"all_on":         {Mode: "quiet", ThinkingMessages: true, ToolMessages: true, HideAgentFooter: true},
	}
	const a, b = "第一段：重要结论 A。", "第二段：B。"
	work := []Event{
		{Type: EventThinking, Content: "想一想"},
		{Type: EventToolUse, ToolName: "Bash", ToolInput: "tool status"},
		{Type: EventToolResult, ToolName: "Bash", ToolResult: "ok", ToolStatus: "ok"},
	}
	run := func(t *testing.T, mk func() (Platform, *stubPlatformEngine, *lifecyclePlatform), d DisplayCfg, automated bool, events []Event) ([]string, []string) {
		t.Helper()
		p, sent, lp := mk()
		e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangChinese)
		defer e.Stop()
		e.SetDisplayConfig(d)
		e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
		as := newControllableSession("np")
		as.events = make(chan Event, len(events)+1)
		for _, ev := range events {
			as.events <- ev
		}
		state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
		e.processTurnEvents(automated, state, e.sessions.GetOrCreateActive("np"), e.sessions, "np", "m", time.Now(), nil, nil, "ctx")
		var preview []string
		if lp != nil {
			lp.previewMu.Lock()
			preview = append(append(append(preview, lp.starts...), lp.edits...), lp.deletes...)
			lp.previewMu.Unlock()
		}
		return sent.getSent(), preview
	}
	for pn, mk := range platforms {
		for dn, d := range displays {
			t.Run(pn+"/"+dn, func(t *testing.T) {
				visible := append(append([]Event{{Type: EventText, Content: a}}, work...),
					Event{Type: EventText, Content: b}, Event{Type: EventResult, Content: b, Done: true})
				sent, preview := run(t, mk, d, true, visible)
				if len(sent) != 1 || !strings.Contains(sent[0], a) || !strings.Contains(sent[0], b) || len(preview) != 0 {
					t.Fatalf("automated visible turn: want 1 send with A and B and no preview, got sent=%q preview=%q", sent, preview)
				}
				silent := append(append([]Event(nil), work...),
					Event{Type: EventText, Content: "NO_REPLY"}, Event{Type: EventResult, Content: "NO_REPLY", Done: true})
				if sent, preview := run(t, mk, d, true, silent); len(sent) != 0 || len(preview) != 0 {
					t.Fatalf("automated silent turn: want 0 sends and 0 preview/recall, got sent=%q preview=%q", sent, preview)
				}
				// Human turns keep the old behavior: A is shown somewhere.
				sent, preview = run(t, mk, d, false, visible)
				if !strings.Contains(strings.Join(append(sent, preview...), "|"), a) {
					t.Fatalf("human turn lost A: sent=%q preview=%q", sent, preview)
				}
			})
		}
	}
}
