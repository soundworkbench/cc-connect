package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type previewCall struct {
	method, path string
	body         map[string]any
}

type previewFixture struct {
	mu                           sync.Mutex
	calls                        []previewCall
	latest                       string
	rejectCreate, rejectProgress bool
}

func newPreviewFixture(t *testing.T) (*previewFixture, *Platform) {
	t.Helper()
	f := &previewFixture{latest: "om_preview"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			fmt.Fprint(w, `{"code":0,"expire":7200,"tenant_access_token":"fixture"}`)
			return
		}
		c := previewCall{method: r.Method, path: r.URL.Path}
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodDelete {
			if err := json.NewDecoder(r.Body).Decode(&c.body); err != nil {
				t.Error(err)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, c)
		if f.rejectCreate && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cards") {
			fmt.Fprint(w, `{"code":230006,"msg":"fixture create rejection"}`)
			return
		}
		if f.rejectProgress && c.body["element"] != nil {
			fmt.Fprint(w, `{"code":230006,"msg":"fixture progress rejection"}`)
			return
		}
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, `{"code":0,"data":{"items":[{"message_id":%q}]}}`, f.latest)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"card_id":"card_fixture","message_id":"om_preview"}}`)
	}))
	t.Cleanup(srv.Close)
	p := &Platform{client: lark.NewClient(t.Name(), "fixture", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())), platformName: "feishu", useInteractiveCard: true, progressStyle: "compact", keepPreviewMaxAge: 120 * time.Second, finalReplyOrder: core.FinalReplyRecallFirst}
	return f, p
}

func (f *previewFixture) read() []previewCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]previewCall(nil), f.calls...)
}

func waitPreview(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("preview condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func parsePreviewJSON(t *testing.T, text any) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(text.(string)), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func previewElements(card map[string]any) []any {
	return card["body"].(map[string]any)["elements"].([]any)
}

func startFixturePreview(t *testing.T, f *previewFixture, p *Platform) *feishuPreviewHandle {
	t.Helper()
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented = true
	cfg.PushIntervalMs = 60000 // Direct tests publish at explicit times, without racing a ticker.
	handle, err := p.StartSegmentedPreview(context.Background(), replyContext{chatID: "oc"}, cfg, core.NewI18n(core.LangChinese))
	if err != nil {
		t.Fatal(err)
	}
	h := handle.(*feishuPreviewHandle)
	t.Cleanup(h.live.stop)
	return h
}

func publishFixture(t *testing.T, p *Platform, h *feishuPreviewHandle, now time.Time) {
	t.Helper()
	h.live.mu.Lock()
	defer h.live.mu.Unlock()
	if err := p.publishSegmented(context.Background(), h, now); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentedPreview_SeparateBodyProgressAndFinalCollapse(t *testing.T) {
	f, p := newPreviewFixture(t)
	h := startFixturePreview(t, f, p)
	initial := parsePreviewJSON(t, f.read()[0].body["data"])
	elements := previewElements(initial)
	bodyID := elements[0].(map[string]any)["element_id"].(string)
	progressID := elements[1].(map[string]any)["element_id"].(string)
	valid := regexp.MustCompile(`^p[0-9a-f]{18}$`)
	if !valid.MatchString(bodyID) || !valid.MatchString(progressID) || bodyID == progressID {
		t.Fatalf("invalid element IDs: %s %s", bodyID, progressID)
	}
	if elements[0].(map[string]any)["content"] != "" || elements[1].(map[string]any)["content"] != "思考中 · 0s" {
		t.Fatalf("initial elements: %v", elements)
	}
	settings := initial["config"].(map[string]any)["streaming_config"].(map[string]any)
	for _, client := range []string{"default", "android", "ios", "pc"} {
		if settings["print_frequency_ms"].(map[string]any)[client] != float64(40) || settings["print_step"].(map[string]any)[client] != float64(6) {
			t.Fatalf("wrong typing settings: %v", settings)
		}
	}
	publishFixture(t, p, h, h.live.started.Add(time.Second))
	calls := f.read()
	counter := calls[len(calls)-1]
	if !strings.HasSuffix(counter.path, "/elements/"+progressID) || parsePreviewJSON(t, counter.body["element"])["content"] != "思考中 · 1s" {
		t.Fatalf("counter rewrote body: %v", counter)
	}
	body := "```go\nfmt.Println(\"你好\")\n```\n  "
	if err := p.SetPreviewBody(context.Background(), h, body); err != nil {
		t.Fatal(err)
	}
	if err := p.AddPreviewProgress(context.Background(), h, "搜索资料"); err != nil {
		t.Fatal(err)
	}
	publishFixture(t, p, h, h.live.started.Add(2*time.Second))
	before := len(f.read())
	for i := 0; i < 4; i++ {
		if err := p.AddPreviewProgress(context.Background(), h, fmt.Sprintf("工具步骤 %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	publishFixture(t, p, h, h.live.started.Add(3*time.Second))
	for _, c := range f.read()[before:] {
		if !strings.HasSuffix(c.path, "/elements/"+progressID) {
			t.Fatalf("progress rewrote body: %s", c.path)
		}
	}
	if err := p.FinishSegmentedPreview(context.Background(), h, body); err != nil {
		t.Fatal(err)
	}
	calls = f.read()
	var streamed string
	seq := 0
	for _, c := range calls {
		if n, ok := c.body["sequence"].(float64); ok {
			if int(n) <= seq {
				t.Fatalf("sequence did not increase: %v", calls)
			}
			seq = int(n)
		}
		if strings.HasSuffix(c.path, "/content") {
			streamed = c.body["content"].(string)
			if !strings.HasSuffix(c.path, "/elements/"+bodyID+"/content") {
				t.Fatal("body ID changed")
			}
		}
	}
	last := calls[len(calls)-1]
	final := parsePreviewJSON(t, last.body["card"].(map[string]any)["data"])
	parts := previewElements(final)
	if len(parts) != 2 || parts[0].(map[string]any)["content"] != body || streamed != body || parts[0].(map[string]any)["element_id"] != bodyID {
		t.Fatalf("body moved or changed: %v", parts)
	}
	panel := parts[1].(map[string]any)
	if panel["tag"] != "collapsible_panel" || panel["expanded"] != false || panel["element_id"] != progressID || panel["header"].(map[string]any)["title"].(map[string]any)["content"] != "过程（5 步）" {
		t.Fatalf("bad process panel: %v", panel)
	}
	if strings.Contains(last.body["card"].(map[string]any)["data"].(string), "已完成") {
		t.Fatal("completion footer returned")
	}
	if !strings.HasSuffix(calls[len(calls)-2].path, "/settings") {
		t.Fatal("stream was not closed before settlement")
	}
}

func TestSegmentedPreview_NewRoundUsesFreshElementAndFallbackPreservesBody(t *testing.T) {
	for _, rejection := range []string{"none", "create", "progress"} {
		t.Run(rejection, func(t *testing.T) {
			f, p := newPreviewFixture(t)
			f.mu.Lock()
			f.rejectCreate = rejection == "create"
			f.rejectProgress = rejection == "progress"
			f.mu.Unlock()
			h := startFixturePreview(t, f, p)
			ctx := context.Background()
			if err := p.SetPreviewBody(ctx, h, "intermediate"); err != nil {
				t.Fatal(err)
			}
			publishFixture(t, p, h, time.Now())
			oldID := h.live.bodyID
			if err := p.SetPreviewBody(ctx, h, ""); err != nil {
				t.Fatal(err)
			}
			if err := p.SetPreviewBody(ctx, h, "final"); err != nil {
				t.Fatal(err)
			}
			if err := p.AddPreviewProgress(ctx, h, "执行步骤"); err != nil {
				t.Fatal(err)
			}
			publishFixture(t, p, h, time.Now())
			if oldID == h.live.bodyID {
				t.Fatal("answer round reused the old element")
			}
			if err := p.FinishSegmentedPreview(ctx, h, "final"); err != nil {
				t.Fatal(err)
			}
			calls := f.read()
			last := calls[len(calls)-1]
			var card map[string]any
			if last.body["card"] != nil {
				card = parsePreviewJSON(t, last.body["card"].(map[string]any)["data"])
			} else {
				card = parsePreviewJSON(t, last.body["content"])
			}
			if previewElements(card)[0].(map[string]any)["content"] != "final" {
				t.Fatal("fallback lost final body")
			}
		})
	}
}

func TestSegmentedPreview_EngineImmediateCounterSilentAndSwitchOff(t *testing.T) {
	for _, scenario := range []string{"enabled", "off", "disabled", "silent", "newer", "long", "multipart", "rich", "append", "thread", "group", "whitespace", "tool_round"} {
		t.Run(scenario, func(t *testing.T) {
			f, p := newPreviewFixture(t)
			s := &finalOrderSession{smokeSession: smokeSession{events: make(chan core.Event, 16)}, sent: make(chan string, 3)}
			e := core.NewEngine("segmented", &finalOrderAgent{session: s}, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
			cfg := core.DefaultStreamPreviewCfg()
			cfg.Segmented = scenario != "off"
			if scenario == "disabled" {
				cfg.Enabled = false
			}
			e.SetStreamPreviewCfg(cfg)
			display := core.DisplayCfg{Mode: "quiet", HideAgentFooter: true}
			if scenario == "rich" {
				display.CardMode = "rich"
			}
			if scenario == "append" {
				display.ProgressHistory = "append"
			}
			e.SetDisplayConfig(display)
			t.Cleanup(func() { _ = e.Stop() })
			if scenario == "newer" {
				f.mu.Lock()
				f.latest = "om_newer"
				f.mu.Unlock()
			}
			if scenario == "long" {
				p.keepPreviewMaxAge = time.Nanosecond
			}
			key := "feishu:oc:ou"
			if scenario == "thread" || scenario == "group" {
				key = "feishu:oc:root:om_root"
			}
			rc := replyContext{chatID: "oc", sessionKey: key}
			if scenario == "thread" || scenario == "group" {
				rc.messageID = "om_origin"
				rc.p2pThread = true
				p.p2pThreadIsolation = true
				if scenario == "group" {
					p.threadIsolation = true
				}
			}
			e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: "feishu", UserID: "ou", Content: "开始工作", ReplyCtx: rc})
			select {
			case <-s.sent:
			case <-time.After(5 * time.Second):
				t.Fatal("no agent prompt")
			}
			if scenario == "off" || scenario == "disabled" {
				// Let the engine's admission/acknowledgement path run without model events.
				time.Sleep(100 * time.Millisecond)
				if len(f.read()) != 0 {
					t.Fatalf("old behavior created a card before visible text: %v", f.read())
				}
			} else {
				waitPreview(t, func() bool { return len(f.read()) >= 2 })
				first := f.read()
				if first[0].path != "/open-apis/cardkit/v1/cards" {
					t.Fatalf("thinking card not created first: %v", first)
				}
				if (scenario == "thread" || scenario == "group") && (!strings.HasSuffix(first[1].path, "/om_origin/reply") || first[1].body["reply_in_thread"] != true) {
					t.Fatalf("lost thread reply: %s", first[1].path)
				}
				if scenario == "enabled" {
					waitPreview(t, func() bool {
						for _, c := range f.read() {
							if element, ok := c.body["element"].(string); ok && strings.Contains(element, "1s") {
								return true
							}
						}
						return false
					})
				}
			}
			body := "最终回答"
			if scenario == "whitespace" {
				body = "```go\nfmt.Println(1)\n```\n  "
			}
			if scenario == "silent" {
				body = "NO_REPLY"
			} else {
				s.events <- core.Event{Type: core.EventProgress, Content: "搜索资料"}
				if scenario == "tool_round" {
					s.events <- core.Event{Type: core.EventText, Content: "中途说明"}
					s.events <- core.Event{Type: core.EventToolUse, ToolName: "Bash"}
				}
				s.events <- core.Event{Type: core.EventTextDelta, Content: body}
				if scenario == "tool_round" {
					s.events <- core.Event{Type: core.EventText, Content: body}
					waitPreview(t, func() bool {
						for _, c := range f.read() {
							if strings.HasSuffix(c.path, "/content") && c.body["content"] == body {
								return true
							}
						}
						return false
					})
				}
			}
			if scenario == "multipart" {
				body = strings.Repeat("字", 4100)
			}
			s.events <- core.Event{Type: core.EventResult, Content: body, Done: true}
			waitPreview(t, func() bool { return !e.GetSessions().GetOrCreateActive(key).Busy() })
			calls := f.read()
			deletes, finals, sends := 0, 0, 0
			for _, c := range calls {
				if c.method == http.MethodDelete {
					deletes++
				}
				if c.body["card"] != nil {
					finals++
				}
				if c.method == http.MethodPost && c.body["msg_type"] != nil {
					sends++
				}
			}
			switch scenario {
			case "silent":
				if deletes != 1 || sends != 1 || finals != 0 {
					t.Fatalf("silent turn left a card: %v", calls)
				}
			case "off", "disabled": // Existing lifecycle tests assert the complete legacy flow.
			case "newer", "long":
				if deletes != 1 || sends != 2 || finals != 0 {
					t.Fatalf("recall/resend policy lost: %v", calls)
				}
			default:
				wantSends := 1
				if scenario == "multipart" {
					wantSends = 2
				}
				if deletes != 0 || finals != 1 || sends != wantSends {
					t.Fatalf("unexpected final delivery: deletes=%d finals=%d sends=%d", deletes, finals, sends)
				}
				for _, c := range calls {
					if c.body["card"] != nil {
						parts := previewElements(parsePreviewJSON(t, c.body["card"].(map[string]any)["data"]))
						wantBody := body
						if scenario == "multipart" {
							wantBody = strings.Repeat("字", 4000)
						}
						if parts[0].(map[string]any)["content"] != wantBody || len(parts) != 2 {
							t.Fatalf("body/progress lost: %v", parts)
						}
					}
				}
			}
		})
	}
}

type preparingPreviewAgent struct {
	finalOrderAgent
	started chan struct{}
	release chan struct{}
}

func (a *preparingPreviewAgent) StartSession(ctx context.Context, _ string) (core.AgentSession, error) {
	close(a.started)
	select {
	case <-a.release:
		return a.session, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestSegmentedPreview_ThinkingBeforeAgentSessionPreparation(t *testing.T) {
	f, p := newPreviewFixture(t)
	s := &finalOrderSession{smokeSession: smokeSession{events: make(chan core.Event, 4)}, sent: make(chan string, 1)}
	a := &preparingPreviewAgent{finalOrderAgent: finalOrderAgent{session: s}, started: make(chan struct{}), release: make(chan struct{})}
	e := core.NewEngine("preparation", a, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented = true
	e.SetStreamPreviewCfg(cfg)
	var once sync.Once
	release := func() { once.Do(func() { close(a.release) }) }
	t.Cleanup(func() { release(); _ = e.Stop() })
	key := "feishu:oc:ou"
	e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: "feishu", UserID: "ou", Content: "开始工作", ReplyCtx: replyContext{chatID: "oc", sessionKey: key}})
	select {
	case <-a.started:
	case <-time.After(5 * time.Second):
		t.Fatal("agent preparation not started")
	}
	calls := f.read()
	if len(calls) < 2 || calls[0].path != "/open-apis/cardkit/v1/cards" {
		t.Fatalf("preparation blocked immediate thinking card: %v", calls)
	}
	release()
	select {
	case <-s.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("no agent prompt")
	}
	s.events <- core.Event{Type: core.EventResult, Content: "NO_REPLY", Done: true}
	waitPreview(t, func() bool { return !e.GetSessions().GetOrCreateActive(key).Busy() })
}

func TestSegmentedPreview_TickerCoalescesBodyAndStopsOnFinish(t *testing.T) {
	f, p := newPreviewFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := core.DefaultStreamPreviewCfg()
	h, err := p.StartSegmentedPreview(ctx, replyContext{chatID: "oc"}, cfg, core.NewI18n(core.LangChinese))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.(*feishuPreviewHandle).live.stop)
	for _, text := range []string{"答", "答案", "答案正文"} {
		if err := p.SetPreviewBody(ctx, h, text); err != nil {
			t.Fatal(err)
		}
	}
	waitPreview(t, func() bool {
		for _, c := range f.read() {
			if c.body["content"] == "答案正文" {
				return true
			}
		}
		return false
	})
	if err := p.FinishSegmentedPreview(ctx, h, "答案正文"); err != nil {
		t.Fatal(err)
	}
	before := len(f.read())
	if err := p.SetPreviewBody(ctx, h, "late text"); err != nil {
		t.Fatal(err)
	}
	if err := p.AddPreviewProgress(ctx, h, "late progress"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if len(f.read()) != before {
		t.Fatal("late timer or event overwrote final card")
	}
}

func TestSegmentedPreview_AskUserQuestionStillUsesQuestionCard(t *testing.T) {
	f, base := newPreviewFixture(t)
	p := &interactivePlatform{Platform: base}
	s := &finalOrderSession{smokeSession: smokeSession{events: make(chan core.Event, 8)}, sent: make(chan string, 2)}
	e := core.NewEngine("questions", &finalOrderAgent{session: s}, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented = true
	e.SetStreamPreviewCfg(cfg)
	e.SetDisplayConfig(core.DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	t.Cleanup(func() { _ = e.Stop() })
	key := "feishu:oc:ou"
	rc := replyContext{chatID: "oc", sessionKey: key}
	e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: "feishu", UserID: "ou", Content: "帮我处理", ReplyCtx: rc})
	select {
	case <-s.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("no prompt")
	}
	s.events <- core.Event{Type: core.EventPermissionRequest, ToolName: "AskUserQuestion", RequestID: "question", Questions: []core.UserQuestion{{Question: "继续吗？", Options: []core.UserQuestionOption{{Label: "继续"}, {Label: "停止"}}}}}
	waitPreview(t, func() bool {
		for _, c := range f.read() {
			if c.body["msg_type"] == "interactive" && strings.Contains(fmt.Sprint(c.body["content"]), "继续吗？") {
				return true
			}
		}
		return false
	})
	e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: "feishu", UserID: "ou", Content: "1", ReplyCtx: rc})
	s.events <- core.Event{Type: core.EventTextDelta, Content: "回答后继续流式输出"}
	waitPreview(t, func() bool {
		for _, c := range f.read() {
			if strings.HasSuffix(c.path, "/content") && c.body["content"] == "回答后继续流式输出" {
				return true
			}
		}
		return false
	})
	s.events <- core.Event{Type: core.EventResult, Content: "回答后继续流式输出", Done: true}
	waitPreview(t, func() bool { return !e.GetSessions().GetOrCreateActive(key).Busy() })
	creates := 0
	for _, c := range f.read() {
		if c.method == http.MethodPost && strings.HasSuffix(c.path, "/cards") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("question broke preview ownership: %d card entities", creates)
	}
}

func startStepFixturePreview(t *testing.T, p *Platform) *feishuPreviewHandle {
	t.Helper()
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	cfg.PushIntervalMs = 60000
	handle, err := p.StartSegmentedPreview(context.Background(), replyContext{chatID: "oc"}, cfg, core.NewI18n(core.LangChinese))
	if err != nil {
		t.Fatal(err)
	}
	h := handle.(*feishuPreviewHandle)
	t.Cleanup(h.live.stop)
	return h
}

func TestSegmentedPreview_StepListLayoutAndLifecycle(t *testing.T) {
	f, p := newPreviewFixture(t)
	h := startStepFixturePreview(t, p)
	ctx := context.Background()
	start := h.live.started

	initial := previewElements(parsePreviewJSON(t, f.read()[0].body["data"]))
	if len(initial) != 3 || initial[0].(map[string]any)["content"] != "⏳ 思考中 · 0s" || initial[2].(map[string]any)["content"] != "已用 0 秒 · 工具 0 次" {
		t.Fatalf("initial layout: %v", initial)
	}
	progressID := initial[0].(map[string]any)["element_id"].(string)
	bodyID := initial[1].(map[string]any)["element_id"].(string)
	footerID := initial[2].(map[string]any)["element_id"].(string)

	h.live.mu.Lock()
	h.live.addRow(start.Add(1*time.Second), "先看第一个文件", false)
	h.live.addRow(start.Add(2*time.Second), "Read a.go", true)
	h.live.addRow(start.Add(5*time.Second), "再看第二个文件", false)
	h.live.addRow(start.Add(6*time.Second), "Read b.go", true)
	h.live.mu.Unlock()
	if err := p.SetPreviewBody(ctx, h, "正在写第三段"); err != nil {
		t.Fatal(err)
	}
	before := len(f.read())
	publishFixture(t, p, h, start.Add(8*time.Second))
	want := "✔ 先看第一个文件 · 4s\n⏳ **再看第二个文件** · 3s\n　└ Read b.go"
	var gotProgress, gotFooter, gotBody string
	for _, c := range f.read()[before:] {
		switch {
		case strings.HasSuffix(c.path, "/elements/"+progressID):
			gotProgress = parsePreviewJSON(t, c.body["element"])["content"].(string)
		case strings.HasSuffix(c.path, "/elements/"+footerID):
			gotFooter = parsePreviewJSON(t, c.body["element"])["content"].(string)
		case strings.HasSuffix(c.path, "/elements/"+bodyID+"/content"):
			gotBody = c.body["content"].(string)
		}
	}
	if gotProgress != want || gotFooter != "已用 8 秒 · 工具 2 次" || gotBody != "正在写第三段" {
		t.Fatalf("progress=%q footer=%q body=%q", gotProgress, gotFooter, gotBody)
	}

	// Elapsed time alone refreshes the list and footer, not the body.
	before = len(f.read())
	publishFixture(t, p, h, start.Add(9*time.Second))
	for _, c := range f.read()[before:] {
		if strings.HasSuffix(c.path, "/content") {
			t.Fatalf("heartbeat rewrote body: %s", c.path)
		}
	}
	if len(f.read()) != before+2 {
		t.Fatalf("want list and footer updates, got %d", len(f.read())-before)
	}

	if err := p.FinishSegmentedPreview(ctx, h, "最终回复"); err != nil {
		t.Fatal(err)
	}
	calls := f.read()
	final := previewElements(parsePreviewJSON(t, calls[len(calls)-1].body["card"].(map[string]any)["data"]))
	if len(final) != 3 || final[0].(map[string]any)["tag"] != "collapsible_panel" || final[1].(map[string]any)["content"] != "最终回复" || final[2].(map[string]any)["element_id"] != footerID {
		t.Fatalf("final layout: %v", final)
	}
	panel, _ := json.Marshal(final[0])
	if strings.Contains(string(panel), "⏳") || !strings.Contains(string(panel), "✔ 再看第二个文件") {
		t.Fatalf("open step not closed: %s", panel)
	}
}

func TestSegmentedPreview_StepListToolOnlyRowsAndCap(t *testing.T) {
	_, p := newPreviewFixture(t)
	h := startStepFixturePreview(t, p)
	v := h.live
	now := v.started
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := 0; i < 12; i++ {
		v.addRow(now.Add(time.Duration(i)*time.Second), fmt.Sprintf("读取 %d", i), true)
	}
	if v.toolCalls != 12 || len(v.rows) != 12 {
		t.Fatalf("tool-only calls should each be a step: %d calls, %d rows", v.toolCalls, len(v.rows))
	}
	lines := strings.Split(v.stepList(now.Add(12*time.Second)), "\n")
	if len(lines) != stepRowsShown+1 || lines[0] != "… +4" || !strings.HasPrefix(lines[len(lines)-1], "⏳ **读取 11**") {
		t.Fatalf("capped list: %q", lines)
	}
	v.addRow(now, "多\n行\n文本", false)
	if got := v.rows[len(v.rows)-1].text; got != "多 行 文本" {
		t.Fatalf("row text not one line: %q", got)
	}
}
