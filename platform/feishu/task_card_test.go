package feishu

import (
	"context"
	"encoding/json"
	"fmt"
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

// A card of several tasks: its state in the title and no tag, one row per
// task on the body, the overflow note, and every task's row and steps in the
// folded details, with a failed task's raw error as plain text.
func TestManyTaskCardRows(t *testing.T) {
	f, p := newPreviewFixture(t)
	store, _ := loadLiveCards(filepath.Join(t.TempDir(), "live.json"))
	p.liveCards = store
	live := core.TaskCardView{
		State: core.TaskCardRunning, Title: "后台任务：进行中 1/3", DetailsTitle: "详情", SessionKey: "feishu:oc:u",
		Rows: []core.TaskCardRow{
			{State: core.TaskCardCompleted, Mark: "✓", Text: "整理纪要 · 纪要整理好了 · 用时 2 分钟", Shown: true, Details: []string{"读纪要"}},
			{State: core.TaskCardFailed, Mark: "✗", Text: "清理目录 · 命令出错（退出码 1），原文在详情里 · 用时 1 分钟", Shown: true, Details: []string{"删除临时文件"}, FullTitle: "原文", Full: "rm: /tmp/x: denied"},
			{State: core.TaskCardRunning, Mark: "◐", Text: "统计目录 · 列目录 · 3 分钟", Details: []string{"读目录", "列目录"}},
		},
		More:        "另有 1 项，见详情",
		Interrupted: "后台任务：已中断\n✓ 整理纪要 · 纪要整理好了 · 用时 2 分钟\n✗ 清理目录\n⊘ 统计目录",
	}
	h, err := p.SendTaskCard(context.Background(), replyContext{chatID: "oc"}, live)
	if err != nil {
		t.Fatal(err)
	}
	text := plainJSON(parsePreviewJSON(t, f.read()[0].body["data"]))
	els := cardElements(t, text)
	if len(els) != 4 || rowSign(t, els[0]) != "✓" || rowText(t, els[0]) != "整理纪要 · 纪要整理好了 · 用时 2 分钟" ||
		rowSign(t, els[1]) != "✗" || rowText(t, els[1]) != "清理目录 · 命令出错（退出码 1），原文在详情里 · 用时 1 分钟" ||
		!strings.Contains(els[2]["content"].(string), "另有 1 项，见详情") || els[3]["tag"] != "collapsible_panel" {
		t.Fatalf("body: %s", text)
	}
	panel, _ := json.Marshal(els[3])
	for _, want := range []string{"◐ 统计目录 · 列目录 · 3 分钟\\n· 读目录\\n· 列目录", "✗ 清理目录", "· 删除临时文件", `"content":"rm: /tmp/x: denied","tag":"plain_text"`, `"expanded":false`} {
		if !strings.Contains(string(panel), want) {
			t.Errorf("details lack %s: %s", want, panel)
		}
	}
	for _, want := range []string{`"title":{"content":"后台任务：进行中 1/3","tag":"plain_text"}`, `"template":"blue"`, `"summary":{"content":"后台任务：进行中 1/3"}`} {
		if !strings.Contains(text, want) {
			t.Errorf("card lacks %s: %s", want, text)
		}
	}
	if strings.Contains(text, "text_tag") {
		t.Errorf("the title carries the state; no tag: %s", text)
	}
	if e := store.entries["om_preview"]; e.Text != live.Interrupted {
		t.Fatalf("restart text %q", e.Text)
	}

	// A task ending changes what a restart leaves; settling clears the record.
	live.Interrupted = "后台任务：已中断\n✓ 整理纪要 · 纪要整理好了 · 用时 2 分钟\n✗ 清理目录\n✓ 统计目录"
	if err := p.UpdateTaskCard(context.Background(), h, live); err != nil {
		t.Fatal(err)
	}
	if e := store.entries["om_preview"]; e.Text != live.Interrupted {
		t.Fatalf("restart text not updated: %q", e.Text)
	}
	done := live
	done.State, done.Title = core.TaskCardFailed, "后台任务：1 项失败"
	if err := p.UpdateTaskCard(context.Background(), h, done); err != nil {
		t.Fatal(err)
	}
	settled := plainJSON(parsePreviewJSON(t, f.read()[len(f.read())-1].body["card"].(map[string]any)["data"]))
	if !strings.Contains(settled, `"template":"red"`) || !strings.Contains(settled, `"title":{"content":"后台任务：1 项失败"`) || strings.Contains(settled, "text_tag") || len(store.entries) != 0 {
		t.Fatalf("settled: %s (live entries %d)", settled, len(store.entries))
	}
}

// A running row leads with the spinner image the card of one task uses, at a
// line icon's size; a waiting row with its hourglass. Without the image yet,
// a running row leads with the spinner glyph. Each row's sign sits in a
// column of its own width, so the texts line up.
func TestManyTaskCardSigns(t *testing.T) {
	_, p := newPreviewFixture(t)
	v := core.TaskCardView{State: core.TaskCardRunning, Title: "后台任务：进行中", DetailsTitle: "详情", Rows: []core.TaskCardRow{
		{State: core.TaskCardRunning, Mark: "◐", Text: "整理纪要 · 读纪要 · 2 分钟", Shown: true},
		{State: core.TaskCardRunning, Waiting: true, Mark: "⏳", Text: "统计目录 · 2 分钟", Shown: true},
	}}
	els := cardElements(t, plainJSON(parsePreviewJSON(t, p.taskCardJSON(v))))
	if rowSign(t, els[0]) != spinnerGlyphs[0] || rowSign(t, els[1]) != "⏳" {
		t.Fatalf("glyph signs: %v", els)
	}
	p.stepIcons = stepIconKeys{spinner: "img_spinner"}
	els = cardElements(t, plainJSON(parsePreviewJSON(t, p.taskCardJSON(v))))
	cols := els[0]["columns"].([]any)
	sign := cols[0].(map[string]any)["elements"].([]any)[0].(map[string]any)
	if els[0]["tag"] != "column_set" || els[0]["flex_mode"] != "none" || cols[0].(map[string]any)["width"] != "20px" ||
		sign["tag"] != "img" || sign["img_key"] != "img_spinner" || sign["size"] != "16px 16px" || sign["preview"] != false {
		t.Fatalf("running row: %v", els[0])
	}
	if rowSign(t, els[1]) != "⏳" || rowText(t, els[1]) != "统计目录 · 2 分钟" {
		t.Fatalf("waiting row: %v", els[1])
	}
}

// rowSign and rowText read a body row's two columns.
func rowSign(t *testing.T, el map[string]any) string {
	t.Helper()
	return rowColumn(t, el, 0)
}

func rowText(t *testing.T, el map[string]any) string {
	t.Helper()
	return rowColumn(t, el, 1)
}

func rowColumn(t *testing.T, el map[string]any, k int) string {
	t.Helper()
	cols, _ := el["columns"].([]any)
	if el["tag"] != "column_set" || len(cols) != 2 {
		t.Fatalf("not a row: %v", el)
	}
	inner := cols[k].(map[string]any)["elements"].([]any)[0].(map[string]any)
	content, _ := inner["content"].(string)
	return content
}

// A card with many long rows drops the steps from its details to stay within
// Feishu's card size; the rows stay.
func TestManyTaskCardStaysWithinSize(t *testing.T) {
	_, p := newPreviewFixture(t)
	p.stepIcons = stepIconKeys{spinner: "img_spinner"}
	v := core.TaskCardView{State: core.TaskCardRunning, Title: "后台任务：进行中", DetailsTitle: "详情"}
	for k := range 40 {
		v.Rows = append(v.Rows, core.TaskCardRow{State: core.TaskCardRunning, Mark: "◐", Text: fmt.Sprintf("任务%d · 读文件 · 3 分钟", k), Shown: k < 8,
			Details: []string{strings.Repeat("很长的一步", 12), strings.Repeat("又一步", 20), strings.Repeat("再一步", 20), strings.Repeat("第四步", 20), strings.Repeat("第五步", 20)}})
	}
	card := p.taskCardJSON(v)
	if len(card) > maxRichCardJSONBytes || !strings.Contains(card, "任务39 · 读文件") || strings.Contains(card, "很长的一步") {
		t.Fatalf("card %d bytes", len(card))
	}
}

// A restart leaves a card of several tasks as grey lines, one per task.
func TestGreyLinesForSeveralTasks(t *testing.T) {
	text := plainJSON(parsePreviewJSON(t, greyLineCardJSON("后台任务：已中断\n✓ 整理纪要 · 好了 · 用时 2 分钟\n⊘ 统计目录")))
	for _, want := range []string{`"summary":{"content":"后台任务：已中断"}`,
		`<font color='grey'>后台任务：已中断</font>\n<font color='grey'>✓ 整理纪要 · 好了 · 用时 2 分钟</font>\n<font color='grey'>⊘ 统计目录</font>`} {
		if !strings.Contains(text, want) {
			t.Errorf("grey card lacks %s: %s", want, text)
		}
	}
}
