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
	got := h.live.stepList(time.Now().Add(3 * time.Second))
	h.live.mu.Unlock()
	if !strings.Contains(got, "✔ 清理测试对话\n") || !strings.Contains(got, "**读取文件 main.go** .") || strings.Contains(got, "运行命令") {
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
	for name, text := range map[string]string{"list": h.live.stepList(stale), "footer": h.live.footer(stale), "progress": h.live.progress(stale)} {
		if strings.ContainsAny(text, "-0123456789秒") {
			t.Fatalf("%s shows a time: %q", name, text)
		}
	}
	if got := h.live.stepList(stale); got != "**跑检查** ." {
		t.Fatalf("running row: %q", got)
	}
}

// The final card shows every step done, the last one included.
func TestStepListFinalCardMarksLastStepDone(t *testing.T) {
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
	if strings.Contains(data, "⏳") || !strings.Contains(data, "✔ 第二步") {
		t.Fatalf("last step not marked done: %s", data)
	}
}
