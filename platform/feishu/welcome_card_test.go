package feishu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

var welcomeButtons = []string{"看看我今天的日程", "帮我写一封英文邮件", "你能帮我做什么？"}

func TestWelcomeCardRendersGreetingAndThreeButtons(t *testing.T) {
	p := &Platform{platformName: "feishu"}
	w := p.newWelcomer("ou_owner", "", t.TempDir(), func() (bool, error) { return false, nil })
	got := generic(t, renderCardMap(w.card(""), ""))
	if got["schema"] != "2.0" || !strings.Contains(plainJSON(got), `"update_multi":true`) {
		t.Fatalf("not a 2.0 card: %s", plainJSON(got))
	}
	if !strings.Contains(plainJSON(got), "你好，我是你的工作伙伴") {
		t.Fatalf("greeting missing: %s", plainJSON(got))
	}
	for _, text := range welcomeButtons {
		b := buttonNamed(got, text)
		if b == nil || b["disabled"] != nil || !strings.Contains(callbackValueJSON(t, b), `"action":"welcome:`+text+`"`) {
			t.Fatalf("button %s = %v", text, b)
		}
	}
	// welcome_message replaces the greeting; the buttons stay.
	w = p.newWelcomer("ou_owner", "  欢迎加入！  ", t.TempDir(), nil)
	got = generic(t, renderCardMap(w.card(""), ""))
	if !strings.Contains(plainJSON(got), "欢迎加入！") || buttonNamed(got, welcomeButtons[0]) == nil {
		t.Fatalf("custom greeting card: %s", plainJSON(got))
	}
}

func welcomeTapPlatform(t *testing.T, dir string) (*interactivePlatform, chan *core.Message) {
	t.Helper()
	ip, msgs := newAllowFromTestPlatform(t, "ou_allowed,ou_other")
	ip.welcome = ip.newWelcomer("ou_allowed", "", dir, nil)
	return ip, msgs
}

func TestWelcomeTapBecomesTheOwnersMessageOnce(t *testing.T) {
	dir := t.TempDir()
	ip, msgs := welcomeTapPlatform(t, dir)
	resp, err := ip.onCardAction(cardActionEvent("ou_allowed", "welcome:帮我写一封英文邮件"))
	if err != nil || resp == nil || resp.Card == nil {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	msg := waitAnyMessage(t, msgs, time.Second, "welcome tap")
	if msg.Content != "帮我写一封英文邮件" || msg.UserID != "ou_allowed" || msg.SessionKey != "feishu:oc_test_chat:ou_allowed" {
		t.Fatalf("message = %+v", msg)
	}
	card := generic(t, resp.Card.Data)
	if card["schema"] != "2.0" || !strings.Contains(plainJSON(card), "已发送：帮我写一封英文邮件") {
		t.Fatalf("tapped card = %s", plainJSON(card))
	}
	for _, text := range welcomeButtons {
		if b := buttonNamed(card, text); b == nil || b["disabled"] != true {
			t.Fatalf("button %s still usable: %v", text, b)
		}
	}

	// Another tap, on the same or another button, only shows a notice.
	resp, _ = ip.onCardAction(cardActionEvent("ou_allowed", "welcome:看看我今天的日程"))
	if resp == nil || resp.Toast == nil || resp.Card != nil {
		t.Fatalf("second tap = %+v", resp)
	}
	waitNoMessage(t, msgs, 200*time.Millisecond, "second tap")

	// Also after a restart.
	ip2, msgs2 := welcomeTapPlatform(t, dir)
	resp, _ = ip2.onCardAction(cardActionEvent("ou_allowed", "welcome:你能帮我做什么？"))
	if resp == nil || resp.Toast == nil {
		t.Fatalf("tap after restart = %+v", resp)
	}
	waitNoMessage(t, msgs2, 200*time.Millisecond, "tap after restart")
}

func TestWelcomeTapOnlyCountsForTheOwner(t *testing.T) {
	ip, msgs := welcomeTapPlatform(t, t.TempDir())
	resp, _ := ip.onCardAction(cardActionEvent("ou_other", "welcome:看看我今天的日程"))
	if resp == nil || resp.Toast == nil || resp.Card != nil {
		t.Fatalf("tap by someone else = %+v", resp)
	}
	waitNoMessage(t, msgs, 200*time.Millisecond, "tap by someone else")
	// The owner can still use the card.
	if resp, _ := ip.onCardAction(cardActionEvent("ou_allowed", "welcome:看看我今天的日程")); resp == nil || resp.Card == nil {
		t.Fatalf("owner tap after another's = %+v", resp)
	}
	waitAnyMessage(t, msgs, time.Second, "owner tap")
	// An unknown text is not sent on.
	if resp, _ := ip.onCardAction(cardActionEvent("ou_allowed", "welcome:删除所有文件")); resp != nil {
		t.Fatalf("unknown welcome text = %+v", resp)
	}
}

func TestWelcomeFallsBackToTextWhenTheCardFails(t *testing.T) {
	for name, cardErr := range map[string]error{"card sent": nil, "card failed": errors.New("feishu: welcome failed code=230001")} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := &welcomeClock{now: bj(2026, 11, 12, 10, 0)}
			w := newTestWelcomer(t, dir, c)
			cards := 0
			w.card = func(string) *core.Card { return welcomeCard("hi", welcomeButtons, "") }
			w.sendCard = func(context.Context, *core.Card) error { cards++; return cardErr }
			runWelcome(t, w)
			texts := len(c.sends)
			if cards != 1 || (cardErr == nil) != (texts == 0) || (cardErr != nil && texts != 1) {
				t.Fatalf("cards=%d texts=%d", cards, texts)
			}
			if w.attempts() != 1 {
				t.Fatalf("attempts = %d, want one attempt for card and fallback together", w.attempts())
			}
		})
	}
}
