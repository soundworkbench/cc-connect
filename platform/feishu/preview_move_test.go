package feishu

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestMovePreviewRepostsBelowAndFoldsOldCard(t *testing.T) {
	f, p := newPreviewFixture(t)
	store, _ := loadLiveCards(filepath.Join(t.TempDir(), "live.json"))
	p.liveCards = store
	h := startStepPreview(t, p)
	_ = p.AddPreviewTool(context.Background(), h, "读取文件")
	_ = p.SetPreviewBody(context.Background(), h, "周报第一部分：\n- 本周完成三件事")
	h.live.mu.Lock()
	h.sequence = 7
	h.live.mu.Unlock()
	before := len(f.read())
	f.mu.Lock()
	f.latest = "om_user3" // the person's message sits below the card
	f.mu.Unlock()

	if err := p.MovePreview(context.Background(), h, replyContext{chatID: "oc", messageID: "om_user3"}); err != nil {
		t.Fatal(err)
	}
	calls := f.read()[before:]
	if len(calls) != 5 || calls[0].method != "GET" {
		t.Fatalf("calls=%+v", calls)
	}
	create, reply, settings, fold := calls[1], calls[2], calls[3], calls[4]
	if create.method != "POST" || !strings.HasSuffix(create.path, "/cardkit/v1/cards") {
		t.Fatalf("create=%+v", create)
	}
	newCard := plainJSON(parsePreviewJSON(t, create.body["data"]))
	if !strings.Contains(newCard, "本周完成三件事") || !strings.Contains(newCard, "读取文件") || !strings.Contains(newCard, `"streaming_mode":true`) {
		t.Fatalf("new card lacks the turn so far: %s", newCard)
	}
	if reply.path != "/open-apis/im/v1/messages/om_user3/reply" {
		t.Fatalf("new card must reply under the latest message: %+v", reply)
	}
	if settings.method != "PATCH" || !strings.HasSuffix(settings.path, "/settings") || settings.body["sequence"] != float64(8) {
		t.Fatalf("old card stream not closed in its own sequence: %+v", settings)
	}
	folded := plainJSON(parsePreviewJSON(t, fold.body["card"].(map[string]any)["data"]))
	if fold.method != "PUT" || !strings.Contains(folded, "<font color='grey'>↓ 已移到下方</font>") || strings.Contains(folded, "本周完成三件事") {
		t.Fatalf("old card not folded to one line: %+v %s", fold, folded)
	}
	if h.sequence != 0 || h.messageID != "om_preview" {
		t.Fatalf("handle not switched to the new card: seq=%d msg=%s", h.sequence, h.messageID)
	}
	for _, c := range calls {
		if c.method == "DELETE" {
			t.Fatal("moving must not recall anything")
		}
	}
	// Later frames go to the new card only.
	_ = p.SetPreviewBody(context.Background(), h, "周报第一部分：\n- 本周完成三件事\n- 下周计划")
	publishFixture(t, p, h, h.live.started)
	last := f.read()[len(f.read())-1]
	if last.body["sequence"] != float64(1) {
		t.Fatalf("new card sequence restarts at 1: %+v", last)
	}
}

func TestMovePreviewSkipsTheLatestCard(t *testing.T) {
	f, p := newPreviewFixture(t)
	h := startStepPreview(t, p)
	before := len(f.read()) // fixture: the latest message is the card itself
	err := p.MovePreview(context.Background(), h, replyContext{chatID: "oc", messageID: "om_user3"})
	if !errors.Is(err, core.ErrPreviewIsLatest) {
		t.Fatalf("err=%v", err)
	}
	if calls := f.read()[before:]; len(calls) != 1 || calls[0].method != "GET" {
		t.Fatalf("a card already at the bottom was touched: %+v", calls)
	}
}
