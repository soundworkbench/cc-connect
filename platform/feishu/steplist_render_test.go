package feishu

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func newStepListPreview(t *testing.T) (*previewFixture, *Platform, *feishuPreviewHandle) {
	t.Helper()
	f, p := newPreviewFixture(t)
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	cfg.PushIntervalMs = 60000
	handle, err := p.StartSegmentedPreview(context.Background(), replyContext{chatID: "oc"}, cfg, core.NewI18n(core.LangChinese))
	if err != nil {
		t.Fatal(err)
	}
	h := handle.(*feishuPreviewHandle)
	t.Cleanup(h.live.stop)
	return f, p, h
}

func TestStepListToolRowsUseToolDescription(t *testing.T) {
	_, p, h := newStepListPreview(t)
	ctx := context.Background()
	for _, label := range []string{"清理测试对话", "读取文件 main.go"} {
		if err := p.AddPreviewTool(ctx, h, label); err != nil {
			t.Fatal(err)
		}
	}
	h.live.mu.Lock()
	got := h.live.doneLines() + "\n" + h.live.liveLine()
	h.live.mu.Unlock()
	if !strings.Contains(got, "✓</font> 清理测试对话\n") || !strings.Contains(got, "◐ 读取文件 main.go") || strings.Contains(got, "运行命令") {
		t.Fatalf("rows:\n%s", got)
	}
}

// The card shows no timers: a clock sample older than the step (a queued
// ticker value) still renders the running dots and no numbers.
func TestStepListShowsNoTimers(t *testing.T) {
	_, p, h := newStepListPreview(t)
	if err := p.AddPreviewTool(context.Background(), h, "跑检查"); err != nil {
		t.Fatal(err)
	}
	stale := h.live.started.Add(-5 * time.Second)
	h.live.mu.Lock()
	defer h.live.mu.Unlock()
	for name, text := range map[string]string{"list": h.live.liveLine(), "footer": h.live.footer(stale), "progress": h.live.progress(stale)} {
		if strings.ContainsAny(text, "-0123456789秒") {
			t.Fatalf("%s shows a time: %q", name, text)
		}
	}
	if got := h.live.liveLine(); got != "◐ 跑检查" {
		t.Fatalf("running row: %q", got)
	}
}

// The final card drops the steps and keeps only the reply.
func TestStepListFinalCardKeepsOnlyReply(t *testing.T) {
	f, p, h := newStepListPreview(t)
	ctx := context.Background()
	for _, label := range []string{"第一步", "第二步"} {
		if err := p.AddPreviewTool(ctx, h, label); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.FinishSegmentedPreview(ctx, h, "答复"); err != nil {
		t.Fatal(err)
	}
	calls := f.read()
	last := calls[len(calls)-1]
	data := last.body["card"].(map[string]any)["data"].(string)
	if !strings.Contains(data, "答复") || strings.Contains(data, "第二步") || strings.Contains(data, "已完成") {
		t.Fatalf("final card should hold only the reply: %s", data)
	}
}

// The step list uses the same text size as the reply under it.
func TestStepListUsesNormalTextSize(t *testing.T) {
	_, _, h := newStepListPreview(t)
	if _, ok := h.live.progressElement("x")["text_size"]; ok {
		t.Fatal("step list must not use the small notation size")
	}
}

func TestStepListLeavesOutProgressWithoutChinese(t *testing.T) {
	_, p, h := newStepListPreview(t)
	ctx := context.Background()
	if err := p.AddPreviewProgress(ctx, h, "Two changes. First, a line in the instructions."); err != nil {
		t.Fatal(err)
	}
	if err := p.AddPreviewTool(ctx, h, "修改说明书"); err != nil {
		t.Fatal(err)
	}
	h.live.mu.Lock()
	got := h.live.doneLines() + "\n" + h.live.liveLine()
	h.live.mu.Unlock()
	if strings.Contains(got, "Two changes") || !strings.Contains(got, "◐ 修改说明书") {
		t.Fatalf("rows:\n%s", got)
	}
}
