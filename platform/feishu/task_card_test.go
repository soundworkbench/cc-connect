package feishu

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestTaskCardLiveAndSettled(t *testing.T) {
	f, p := newPreviewFixture(t)
	store, _ := loadLiveCards(filepath.Join(t.TempDir(), "live.json"))
	p.liveCards = store
	live := core.TaskCardView{
		State: core.TaskCardRunning, Title: "后台任务 · 正在进行：按客户整理文件", Status: "读取文件", Elapsed: "已跑 3 分钟", Recent: "最近进展：正在读第 3 个客户",
		Hint:         "在后台进行，不影响聊天，你可以接着跟我说话；做完我会告诉你。要停的话直接跟我说。",
		DetailsTitle: "详情", Details: []string{"读取文件", "编辑文件"}, SessionKey: "feishu:oc:u",
		Interrupted: "后台任务 · 已中断：按客户整理文件",
	}
	h, err := p.SendTaskCard(context.Background(), replyContext{chatID: "oc"}, live)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.read()
	created := calls[0]
	if created.method != "POST" || !strings.HasSuffix(created.path, "/cardkit/v1/cards") {
		t.Fatalf("first call=%+v", created)
	}
	text := plainJSON(parsePreviewJSON(t, created.body["data"]))
	for _, want := range []string{"**后台任务 · 正在进行：按客户整理文件**", "读取文件", "已跑 3 分钟", "最近进展：正在读第 3 个客户", `"tag":"collapsible_panel"`, "· 编辑文件",
		"<font color='grey'>在后台进行，不影响聊天，你可以接着跟我说话；做完我会告诉你。要停的话直接跟我说。</font>"} {
		if !strings.Contains(text, want) {
			t.Errorf("live card lacks %s: %s", want, text)
		}
	}
	if strings.Contains(text, `"tag":"button"`) || strings.Contains(text, `"confirm"`) {
		t.Errorf("task cards carry no button: %s", text)
	}
	if strings.Index(text, "最近进展") > strings.Index(text, "在后台进行") {
		t.Errorf("the hint goes under the latest progress: %s", text)
	}
	if strings.Contains(text, "streaming_mode") {
		t.Errorf("task cards do not use streaming mode: %s", text)
	}
	if e, ok := store.entries["om_preview"]; !ok || e.Text != "后台任务 · 已中断：按客户整理文件" || e.CardID != "card_fixture" {
		t.Fatalf("live card not recorded: %+v", store.entries)
	}

	done := core.TaskCardView{State: core.TaskCardCompleted, Title: "按客户整理文件", Status: "后台任务 · ✓ 已完成：按客户整理文件", Summary: "共 200 个文件", DetailsTitle: "详情", Details: live.Details}
	if err := p.UpdateTaskCard(context.Background(), h, done); err != nil {
		t.Fatal(err)
	}
	last := f.read()[len(f.read())-1]
	if last.method != "PUT" || last.path != "/open-apis/cardkit/v1/cards/card_fixture" {
		t.Fatalf("update=%+v", last)
	}
	settled := plainJSON(parsePreviewJSON(t, last.body["card"].(map[string]any)["data"]))
	if !strings.Contains(settled, "<font color='green'>后台任务 · ✓ 已完成：按客户整理文件</font>") || !strings.Contains(settled, "共 200 个文件") ||
		strings.Contains(settled, "在后台进行") || strings.Contains(settled, "已跑") || !strings.Contains(settled, "collapsible_panel") {
		t.Fatalf("settled card: %s", settled)
	}
	if len(store.entries) != 0 {
		t.Fatalf("settled card still recorded: %+v", store.entries)
	}
}

// A failed card shows one Chinese line; the raw error sits in the details
// panel as plain text.
func TestFailedTaskCardKeepsTheRawErrorInDetails(t *testing.T) {
	_, p := newPreviewFixture(t)
	raw := "Background command \"清理\" failed with exit code 1\n<stderr> rm: /tmp/x: denied"
	text := plainJSON(parsePreviewJSON(t, p.taskCardJSON(core.TaskCardView{
		State: core.TaskCardFailed, Status: "后台任务 · 没做成：清理", Summary: "命令出错结束（退出码 1），原始信息在“详情”里。",
		DetailsTitle: "详情", Details: []string{"运行命令"}, FullTitle: "原文", Full: raw,
	})))
	if !strings.Contains(text, "<font color='red'>后台任务 · 没做成：清理</font>") || !strings.Contains(text, "命令出错结束（退出码 1）") {
		t.Fatalf("failed card: %s", text)
	}
	var card struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(text), &card); err != nil {
		t.Fatal(err)
	}
	for _, el := range card.Body.Elements {
		b, _ := json.Marshal(el)
		if inPanel := el["tag"] == "collapsible_panel"; inPanel != strings.Contains(string(b), "failed with exit code 1") {
			t.Fatalf("raw error belongs in the details panel only: %s", text)
		}
	}
	if !strings.Contains(text, `"tag":"div","text":{"content":"Background command \"清理\" failed with exit code 1\n<stderr> rm: /tmp/x: denied","tag":"plain_text"`) {
		t.Fatalf("raw error not kept as plain text: %s", text)
	}
}
