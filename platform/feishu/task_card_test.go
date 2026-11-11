package feishu

import (
	"context"
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
		State: core.TaskCardRunning, Title: "按客户整理文件", Status: "读取文件", Elapsed: "已跑 3 分钟", Recent: "最近进展：正在读第 3 个客户",
		DetailsTitle: "详情", Details: []string{"读取文件", "编辑文件"},
		StopLabel: "停止", StopConfirmTitle: "停止这个后台任务？", StopConfirmText: "任务会马上停下。", StopAction: "act:/taskstop tok", SessionKey: "feishu:oc:u",
		Interrupted: "已中断：按客户整理文件",
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
	for _, want := range []string{"**按客户整理文件**", "读取文件", "已跑 3 分钟", "最近进展：正在读第 3 个客户", `"tag":"collapsible_panel"`, "· 编辑文件",
		`"type":"danger"`, `"confirm"`, "停止这个后台任务？", `"action":"act:/taskstop tok"`, `"session_key":"feishu:oc:u"`} {
		if !strings.Contains(text, want) {
			t.Errorf("live card lacks %s: %s", want, text)
		}
	}
	if strings.Contains(text, "streaming_mode") {
		t.Errorf("task cards do not use streaming mode: %s", text)
	}
	if e, ok := store.entries["om_preview"]; !ok || e.Text != "已中断：按客户整理文件" || e.CardID != "card_fixture" {
		t.Fatalf("live card not recorded: %+v", store.entries)
	}

	done := core.TaskCardView{State: core.TaskCardCompleted, Title: "按客户整理文件", Status: "✓ 已完成：按客户整理文件", Summary: "共 200 个文件", DetailsTitle: "详情", Details: live.Details}
	if err := p.UpdateTaskCard(context.Background(), h, done); err != nil {
		t.Fatal(err)
	}
	last := f.read()[len(f.read())-1]
	if last.method != "PUT" || last.path != "/open-apis/cardkit/v1/cards/card_fixture" {
		t.Fatalf("update=%+v", last)
	}
	settled := plainJSON(parsePreviewJSON(t, last.body["card"].(map[string]any)["data"]))
	if !strings.Contains(settled, "<font color='green'>✓ 已完成：按客户整理文件</font>") || !strings.Contains(settled, "共 200 个文件") ||
		strings.Contains(settled, `"tag":"button"`) || strings.Contains(settled, "已跑") || !strings.Contains(settled, "collapsible_panel") {
		t.Fatalf("settled card: %s", settled)
	}
	if len(store.entries) != 0 {
		t.Fatalf("settled card still recorded: %+v", store.entries)
	}
}
