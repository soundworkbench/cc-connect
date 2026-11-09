package feishu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func newSpinnerPreview(t *testing.T, upload func(context.Context, []byte) (string, error)) (*previewFixture, *Platform, *feishuPreviewHandle) {
	t.Helper()
	f, p := newPreviewFixture(t)
	p.spinnerUpload = upload
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

func okUpload(uploads *int) func(context.Context, []byte) (string, error) {
	return func(_ context.Context, data []byte) (string, error) {
		*uploads++
		if len(data) == 0 {
			return "", errors.New("empty gif")
		}
		return "img_spin", nil
	}
}

func TestSpinnerRunningRowCarriesCachedIcon(t *testing.T) {
	var uploads int
	f, p, h := newSpinnerPreview(t, okUpload(&uploads))
	if err := p.AddPreviewTool(context.Background(), h, "跑检查"); err != nil {
		t.Fatal(err)
	}
	h.live.mu.Lock()
	el := h.live.progressElement(h.live.liveLine())
	h.live.mu.Unlock()
	icon, _ := el["icon"].(map[string]any)
	if icon["tag"] != "custom_icon" || icon["img_key"] != "img_spin" || el["content"] != "跑检查" || el["tag"] != "markdown" {
		t.Fatalf("running row: %v", el)
	}
	if first := previewElements(parsePreviewJSON(t, f.read()[0].body["data"]))[1].(map[string]any); first["icon"] == nil {
		t.Fatalf("initial card lacks the icon: %v", first)
	}
	// A second turn on the same platform reuses the key.
	cfg := core.DefaultStreamPreviewCfg()
	cfg.Segmented, cfg.StepList = true, true
	h2, err := p.StartSegmentedPreview(context.Background(), replyContext{chatID: "oc"}, cfg, core.NewI18n(core.LangChinese))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h2.(*feishuPreviewHandle).live.stop)
	if uploads != 1 || h2.(*feishuPreviewHandle).live.spinnerKey != "img_spin" {
		t.Fatalf("uploads=%d", uploads)
	}
}

// With the GIF the client animates the icon, so time passing sends no step-area update.
func TestSpinnerHeartbeatSendsNoStepAreaUpdate(t *testing.T) {
	var uploads int
	f, p, h := newSpinnerPreview(t, okUpload(&uploads))
	if err := p.AddPreviewTool(context.Background(), h, "跑检查"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	publishAt(t, p, h, t0)
	base := len(f.read())
	for i := 1; i <= 6; i++ {
		publishAt(t, p, h, t0.Add(time.Duration(i)*time.Second))
	}
	if got := len(f.read()) - base; got != 0 {
		t.Fatalf("%d calls from the heartbeat, want 0", got)
	}
	// Reply text still goes out.
	_ = p.SetPreviewBody(context.Background(), h, "正文")
	publishAt(t, p, h, t0.Add(7*time.Second))
	if f.countSuffix("/content") == 0 {
		t.Fatal("reply text was not sent")
	}
}

func TestSpinnerUploadFailureFallsBackToGlyphFrames(t *testing.T) {
	calls := 0
	_, p, h := newSpinnerPreview(t, func(context.Context, []byte) (string, error) { calls++; return "", errors.New("boom") })
	if err := p.AddPreviewTool(context.Background(), h, "跑检查"); err != nil {
		t.Fatal(err)
	}
	v := h.live
	if v.spinnerKey != "" || v.progressElement("x")["icon"] != nil {
		t.Fatal("icon used although the upload failed")
	}
	t0 := v.started
	var frames []string
	for i := 0; i < 5; i++ {
		publishAt(t, p, h, t0.Add(time.Duration(i)*time.Second))
		v.mu.Lock()
		frames = append(frames, v.liveLine())
		v.mu.Unlock()
	}
	want := []string{"◐ 跑检查", "◓ 跑检查", "◑ 跑检查", "◒ 跑检查", "◐ 跑检查"}
	if strings.Join(frames, "|") != strings.Join(want, "|") {
		t.Fatalf("frames %q", frames)
	}
	// No second upload attempt inside the back-off.
	if _, err := p.StartSegmentedPreview(context.Background(), replyContext{chatID: "oc"}, core.StreamPreviewCfg{StepList: true}, core.NewI18n(core.LangChinese)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("upload attempts: %d", calls)
	}
}

func TestSpinnerDoneRowsUseCheckAndCapAtFive(t *testing.T) {
	var uploads int
	_, _, h := newSpinnerPreview(t, okUpload(&uploads))
	v := h.live
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := 0; i < 4; i++ {
		v.addRow(v.started.Add(time.Duration(i)*time.Second), fmt.Sprintf("步骤 %d", i), false)
	}
	if got := v.doneLines(); strings.Contains(got, "另有") || strings.Count(got, "<font color='green'>✓</font>") != 3 {
		t.Fatalf("three done rows: %q", got)
	}
	for i := 4; i < 9; i++ {
		v.addRow(v.started.Add(time.Duration(i)*time.Second), fmt.Sprintf("步骤 %d", i), false)
	}
	lines := strings.Split(v.doneLines(), "\n")
	if len(lines) != 6 || lines[0] != "<font color='grey'>另有 3 步已完成</font>" || lines[1] != "<font color='green'>✓</font> 步骤 3" || lines[5] != "<font color='green'>✓</font> 步骤 7" {
		t.Fatalf("capped rows: %q", lines)
	}
	if live := v.liveLine(); live != "步骤 8" {
		t.Fatalf("running row: %q", live)
	}
}
