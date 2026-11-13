package feishu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func startStepPreview(t *testing.T, p *Platform) *feishuPreviewHandle {
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

func TestPreviewNoticeReplacesRunningStep(t *testing.T) {
	_, p := newPreviewFixture(t)
	h := startStepPreview(t, p)
	if err := p.AddPreviewTool(context.Background(), h, "运行命令"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetPreviewNotice(context.Background(), h, "模型服务连不上，正在重试（第 2 次）"); err != nil {
		t.Fatal(err)
	}
	h.live.mu.Lock()
	line := h.live.liveLine()
	h.live.mu.Unlock()
	if !strings.Contains(line, "模型服务连不上，正在重试（第 2 次）") || strings.Contains(line, "运行命令") {
		t.Fatalf("live line=%q", line)
	}
	_ = p.SetPreviewNotice(context.Background(), h, "")
	h.live.mu.Lock()
	line = h.live.liveLine()
	h.live.mu.Unlock()
	if !strings.Contains(line, "运行命令") {
		t.Fatalf("step not restored: %q", line)
	}
}

// With no quick line the running row reads "思考中"; the waiting line the core
// sets each minute goes out as an update of that row, and the next minute's
// line as another.
func TestWaitingLineUpdatesTheRunningRow(t *testing.T) {
	f, p := newPreviewFixture(t)
	h := startStepPreview(t, p)
	put := func(text string) {
		t.Helper()
		_ = p.SetPreviewNotice(context.Background(), h, text)
		h.live.mu.Lock()
		err := p.publishSegmented(context.Background(), h, time.Now())
		h.live.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		calls := f.read()
		last := calls[len(calls)-1]
		if last.method != "PUT" || !strings.HasSuffix(last.path, "/elements/"+h.live.progressID) {
			t.Fatalf("last call %s %s, want the running row", last.method, last.path)
		}
		if got := parsePreviewJSON(t, last.body["element"])["content"].(string); !strings.Contains(got, text) || strings.Contains(got, "思考中") {
			t.Fatalf("running row=%q, want %q", got, text)
		}
	}
	put("仍在处理 · 模型响应较慢，已等待 2 分钟")
	put("仍在处理 · 模型响应较慢，已等待 3 分钟")
}

// plainJSON renders v as JSON without escaping <, > and &, as card markup is read.
func plainJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return b.String()
}

func lastCardPut(t *testing.T, f *previewFixture) map[string]any {
	t.Helper()
	calls := f.read()
	for i := len(calls) - 1; i >= 0; i-- {
		if c := calls[i]; c.method == "PUT" && strings.HasSuffix(c.path, "/cards/card_fixture") {
			return parsePreviewJSON(t, c.body["card"].(map[string]any)["data"])
		}
	}
	t.Fatal("no full card update")
	return nil
}

func TestFailPreviewKeepsReplyAndAddsRetryButton(t *testing.T) {
	dir := t.TempDir()
	f, p := newPreviewFixture(t)
	store, err := loadLiveCards(filepath.Join(dir, "live.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.liveCards = store
	h := startStepPreview(t, p)
	if _, ok := store.entries[h.messageID]; !ok {
		t.Fatal("live card not recorded")
	}
	_ = p.SetPreviewBody(context.Background(), h, "先列出第一部分：\n- 甲\n- 乙")
	card := core.FailureCard{Title: "这次没做成：模型服务暂时连不上", ButtonLabel: "重试", Action: "act:/retry tok", SessionKey: "feishu:oc:u"}
	if err := p.FailPreview(context.Background(), h, card); err != nil {
		t.Fatal(err)
	}
	if !h.live.stopped {
		t.Fatal("the push loop must stop with the failure")
	}
	if _, ok := store.entries[h.messageID]; ok {
		t.Fatal("failed card still recorded as live")
	}
	text := plainJSON(lastCardPut(t, f))
	for _, want := range []string{"先列出第一部分", "<font color='red'>这次没做成：模型服务暂时连不上</font>", `"action":"act:/retry tok"`, `"session_key":"feishu:oc:u"`, `"type":"callback"`} {
		if !strings.Contains(text, want) {
			t.Errorf("failure card lacks %s: %s", want, text)
		}
	}
	if strings.Contains(text, "streaming_mode") {
		t.Errorf("failure card must not stream: %s", text)
	}

	card.Note, card.Action, card.ButtonLabel = "已重新发送", "", ""
	if err := p.FailPreview(context.Background(), h, card); err != nil {
		t.Fatal(err)
	}
	if text := plainJSON(lastCardPut(t, f)); strings.Contains(text, `"tag":"button"`) || !strings.Contains(text, "已重新发送") {
		t.Fatalf("clicked card: %s", text)
	}
}

func TestFinishedPreviewLeavesLiveCardStore(t *testing.T) {
	_, p := newPreviewFixture(t)
	store, _ := loadLiveCards(filepath.Join(t.TempDir(), "live.json"))
	p.liveCards = store
	h := startStepPreview(t, p)
	if err := p.FinishSegmentedPreview(context.Background(), h, "答复"); err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 0 {
		t.Fatalf("entries=%v", store.entries)
	}
	data, _ := os.ReadFile(store.path)
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("file=%s", data)
	}
}

func TestRestartMarksLeftoverCardsInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feishu", "live.json")
	if err := writeFileAtomic(path, []liveCardEntry{
		{MessageID: "om_old", CardID: "card_old", Text: "已中断，请重发", Since: time.Now()},
		{MessageID: "om_inline", Text: "已中断", Since: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	f, p := newPreviewFixture(t)
	store, err := loadLiveCards(path)
	if err != nil {
		t.Fatal(err)
	}
	p.liveCards = store
	p.settleLeftoverCards(context.Background())

	var settings, put, patch bool
	for _, c := range f.read() {
		switch {
		case c.method == "PATCH" && c.path == "/open-apis/cardkit/v1/cards/card_old/settings":
			settings = true
		case c.method == "PUT" && c.path == "/open-apis/cardkit/v1/cards/card_old":
			put = true
			if seq, _ := c.body["sequence"].(float64); seq < float64(time.Now().Add(-time.Minute).Unix()) {
				t.Errorf("sequence %v may not exceed the old process's", seq)
			}
			if data := c.body["card"].(map[string]any)["data"].(string); !strings.Contains(data, "已中断，请重发") || strings.Contains(data, "streaming_mode") {
				t.Errorf("interrupted card=%s", data)
			}
		case c.method == "PATCH" && c.path == "/open-apis/im/v1/messages/om_inline":
			patch = true
		}
	}
	if !settings || !put || !patch {
		t.Fatalf("settings=%v put=%v patch=%v calls=%+v", settings, put, patch, f.read())
	}
	if len(store.entries) != 0 || len(store.takeLeftover()) != 0 {
		t.Fatalf("leftovers kept: %v", store.entries)
	}
}
