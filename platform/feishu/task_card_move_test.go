package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// A task card moves by posting it again below, inside the topic it belongs
// to, and folding the old card to one grey line; nothing is recalled.
func TestMoveTaskCardRepostsInTopicAndFoldsOldCard(t *testing.T) {
	f, p := newPreviewFixture(t)
	live := core.TaskCardView{
		State: core.TaskCardRunning, Title: "后台任务：进行中 1/2", DetailsTitle: "详情", SessionKey: "feishu:oc:root:om_root",
		Rows:  []core.TaskCardRow{{State: core.TaskCardRunning, Mark: "◐", Text: "任务甲 · 3 分钟", Shown: true}, {State: core.TaskCardCompleted, Mark: "✅", Text: "任务乙 · 用时 1 分钟", Shown: true}},
		Moved: "↓ 已移到下方",
	}
	h, err := p.SendTaskCard(context.Background(), replyContext{chatID: "oc"}, live)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.read())
	rc := replyContext{chatID: "oc", messageID: "om_user7", sessionKey: "feishu:oc:root:om_root", inThread: true}
	moved, err := p.MoveTaskCard(context.Background(), h, rc, live)
	if err != nil {
		t.Fatal(err)
	}
	if moved == h {
		t.Fatal("move returned the old handle")
	}
	calls := f.read()[before:]
	if len(calls) != 3 {
		t.Fatalf("calls=%+v", calls)
	}
	create, reply, fold := calls[0], calls[1], calls[2]
	if create.method != "POST" || !strings.HasSuffix(create.path, "/cardkit/v1/cards") ||
		!strings.Contains(plainJSON(parsePreviewJSON(t, create.body["data"])), "任务甲 · 3 分钟") {
		t.Fatalf("new card=%+v", create)
	}
	if reply.path != "/open-apis/im/v1/messages/om_user7/reply" || reply.body["reply_in_thread"] != true {
		t.Fatalf("new card must reply inside the topic: %+v", reply)
	}
	folded := plainJSON(parsePreviewJSON(t, fold.body["card"].(map[string]any)["data"]))
	if fold.method != "PUT" || fold.path != "/open-apis/cardkit/v1/cards/card_fixture" ||
		!strings.Contains(folded, "<font color='grey'>↓ 已移到下方</font>") || strings.Contains(folded, "任务甲") {
		t.Fatalf("old card not folded: %+v %s", fold, folded)
	}
	for _, c := range f.read() {
		if c.method == "DELETE" {
			t.Fatal("moving must not recall anything")
		}
	}
}
