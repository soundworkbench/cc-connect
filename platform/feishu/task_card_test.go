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
		State: core.TaskCardRunning, Title: "按客户整理文件", Label: "进行中", Line: "读取文件 · 3 分钟",
		DetailsTitle: "详情", Details: []string{"读取文件", "编辑文件"}, SessionKey: "feishu:oc:u",
		Interrupted: "按客户整理文件 · 已中断",
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
	for _, want := range []string{`"title":{"content":"按客户整理文件","tag":"plain_text"}`, `"template":"blue"`,
		`"text_tag_list":[{"color":"blue","tag":"text_tag","text":{"content":"进行中","tag":"plain_text"}}]`,
		"读取文件 · 3 分钟", `"tag":"collapsible_panel"`, `"expanded":false`, "· 编辑文件"} {
		if !strings.Contains(text, want) {
			t.Errorf("live card lacks %s: %s", want, text)
		}
	}
	// One line in the body, then the folded details.
	if els := cardElements(t, text); len(els) != 2 || els[1]["tag"] != "collapsible_panel" {
		t.Errorf("live card body: %s", text)
	}
	if strings.Contains(text, `"tag":"button"`) || strings.Contains(text, `"confirm"`) {
		t.Errorf("task cards carry no button: %s", text)
	}
	if strings.Contains(text, "streaming_mode") {
		t.Errorf("task cards do not use streaming mode: %s", text)
	}
	if e, ok := store.entries["om_preview"]; !ok || e.Text != "按客户整理文件 · 已中断" || e.CardID != "card_fixture" {
		t.Fatalf("live card not recorded: %+v", store.entries)
	}

	done := core.TaskCardView{State: core.TaskCardCompleted, Title: "按客户整理文件", Label: "已完成", Line: "共 200 个文件 · 用时 5 分钟", Summary: "共 200 个文件", DetailsTitle: "详情", Details: live.Details}
	if err := p.UpdateTaskCard(context.Background(), h, done); err != nil {
		t.Fatal(err)
	}
	last := f.read()[len(f.read())-1]
	if last.method != "PUT" || last.path != "/open-apis/cardkit/v1/cards/card_fixture" {
		t.Fatalf("update=%+v", last)
	}
	settled := plainJSON(parsePreviewJSON(t, last.body["card"].(map[string]any)["data"]))
	if !strings.Contains(settled, `"template":"green"`) || !strings.Contains(settled, `"content":"已完成"`) || !strings.Contains(settled, `"content":"共 200 个文件 · 用时 5 分钟"`) ||
		strings.Contains(settled, "custom_icon") || strings.Contains(settled, spinnerGlyphs[0]) || len(cardElements(t, settled)) != 2 {
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
		State: core.TaskCardFailed, Title: "清理", Label: "失败", Line: "命令出错（退出码 1），原文在详情里 · 用时 2 分钟", Summary: "命令出错（退出码 1），原文在详情里",
		DetailsTitle: "详情", Details: []string{"运行命令"}, FullTitle: "原文", Full: raw,
	})))
	if !strings.Contains(text, `"template":"red"`) || !strings.Contains(text, `"content":"失败"`) || !strings.Contains(text, "命令出错（退出码 1），原文在详情里 · 用时 2 分钟") {
		t.Fatalf("failed card: %s", text)
	}
	for _, el := range cardElements(t, text) {
		b, _ := json.Marshal(el)
		if inPanel := el["tag"] == "collapsible_panel"; inPanel != strings.Contains(string(b), "failed with exit code 1") {
			t.Fatalf("raw error belongs in the details panel only: %s", text)
		}
	}
	if !strings.Contains(text, `"tag":"div","text":{"content":"Background command \"清理\" failed with exit code 1\n<stderr> rm: /tmp/x: denied","tag":"plain_text"`) {
		t.Fatalf("raw error not kept as plain text: %s", text)
	}
}

func cardElements(t *testing.T, text string) []map[string]any {
	t.Helper()
	var card struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(text), &card); err != nil {
		t.Fatal(err)
	}
	return card.Body.Elements
}

// Stopped and interrupted cards are grey; the text tag uses its own name for grey.
func TestEndedTaskCardGreyHeader(t *testing.T) {
	_, p := newPreviewFixture(t)
	text := plainJSON(parsePreviewJSON(t, p.taskCardJSON(core.TaskCardView{
		State: core.TaskCardInterrupted, Title: "整理纪要", Label: "已中断", Line: "用时 4 分钟",
	})))
	for _, want := range []string{`"template":"grey"`, `"color":"neutral"`, `"content":"已中断"`, `"content":"用时 4 分钟"`, `"summary":{"content":"整理纪要 · 已中断"}`} {
		if !strings.Contains(text, want) {
			t.Errorf("interrupted card lacks %s: %s", want, text)
		}
	}
	if els := cardElements(t, text); len(els) != 1 {
		t.Errorf("interrupted card body: %s", text)
	}
}
