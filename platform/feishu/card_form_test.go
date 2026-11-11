package feishu

import (
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestDangerButtonAndFormRender(t *testing.T) {
	btn := core.Btn("删除", "danger", "act:/decision d1 1")
	btn.Confirm = &core.CardConfirm{Title: "确认这个操作？", Text: "选择「删除」后会马上照做。"}
	card := core.NewCard().Title("需要你确认", "red").Buttons(btn).Build()
	card.Elements = append(card.Elements, core.CardForm{Name: "decision_d2", SubmitText: "提交", SubmitType: "danger", SubmitValue: "act:/decision-form d2",
		Confirm: &core.CardConfirm{Title: "确认？", Text: "提交后照做。"},
		Fields: []core.CardFormField{{Name: "choice", Kind: "multi_select", Placeholder: "可以选多个", Required: true,
			Options: []core.CardSelectOption{{Text: "甲", Value: "1"}, {Text: "乙", Value: "2"}}}}})
	text := plainJSON(renderCardMap(card, "feishu:oc:u"))
	for _, want := range []string{`"confirm":{"text":{"content":"选择「删除」后会马上照做。"`, `"name":"decision_d2","tag":"form"`, `"tag":"multi_select_static"`,
		`"name":"choice"`, `"required":true`, `"action_type":"form_submit"`, `"name":"decision_d2_submit"`, `"action":"act:/decision-form d2"`, `"session_key":"feishu:oc:u"`} {
		if !strings.Contains(text, want) {
			t.Errorf("card lacks %s: %s", want, text)
		}
	}
	// The card is JSON 1.0 (no schema), where only action_type binds a button
	// to the form submit; form_action_type is the 2.0 key and leaves the
	// button doing nothing.
	if strings.Contains(text, `"schema"`) || strings.Contains(text, "form_action_type") {
		t.Errorf("submit button is not bound the JSON 1.0 way: %s", text)
	}
}

func TestFormSubmitCarriesValuesToTheAction(t *testing.T) {
	ip, _ := newAllowFromTestPlatform(t, "ou_allowed")
	got := make(chan string, 1)
	ip.cardNavHandler = func(action, _ string) *core.Card { got <- action; return nil }
	ev := cardActionEvent("ou_allowed", "act:/decision-form d2")
	ev.Event.Action.FormValue = map[string]any{"choice": []any{"1", "2"}}
	if _, err := ip.onCardAction(ev); err != nil {
		t.Fatal(err)
	}
	if a := <-got; a != `act:/decision-form d2 {"choice":["1","2"]}` {
		t.Fatalf("action=%q", a)
	}
}
