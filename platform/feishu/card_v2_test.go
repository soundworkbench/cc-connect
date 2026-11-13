package feishu

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

// generic decodes a rendered card into plain JSON values.
func generic(t *testing.T, m any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(plainJSON(m)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// findNode returns the first object in the card for which match is true.
func findNode(node any, match func(map[string]any) bool) map[string]any {
	switch n := node.(type) {
	case map[string]any:
		if match(n) {
			return n
		}
		for _, v := range n {
			if f := findNode(v, match); f != nil {
				return f
			}
		}
	case []any:
		for _, v := range n {
			if f := findNode(v, match); f != nil {
				return f
			}
		}
	}
	return nil
}

func buttonNamed(card map[string]any, text string) map[string]any {
	return findNode(card, func(m map[string]any) bool {
		if m["tag"] != "button" {
			return false
		}
		label, _ := m["text"].(map[string]any)
		return label != nil && label["content"] == text
	})
}

// callbackValueJSON is the value Feishu sends back when the component is used.
func callbackValueJSON(t *testing.T, comp map[string]any) string {
	t.Helper()
	behaviors, _ := comp["behaviors"].([]any)
	if len(behaviors) != 1 {
		t.Fatalf("component has no callback behavior: %v", comp)
	}
	b, _ := behaviors[0].(map[string]any)
	if b["type"] != "callback" {
		t.Fatalf("behavior = %v", b)
	}
	raw, _ := json.Marshal(b["value"])
	return string(raw)
}

// A decision card as core builds it: shared, buttons, a dropdown form, a note.
func sharedDecisionCard(void bool) *core.Card {
	cb := core.NewCard().Title("需要你决定", "blue").Markdown("**用哪个方案？**")
	rec := core.Btn("甲（推荐）", "primary", "act:/decision d1 1")
	other := core.Btn("乙", "default", "act:/decision d1 2")
	if void {
		rec.Disabled, rec.DisabledTip = true, "已作废"
		other.Disabled, other.DisabledTip = true, "已作废"
	}
	cb.Buttons(rec).Buttons(other).Note("11-13 20:00 前未选择，将按推荐处理：甲")
	card := cb.Build()
	card.Elements = append(card.Elements, core.CardForm{Name: "decision_d7", SubmitText: "提交", SubmitType: "primary", SubmitValue: "act:/decision-form d7",
		Disabled: void, DisabledTip: "已作废",
		Fields: []core.CardFormField{
			{Name: "choice", Kind: "multi_select", Placeholder: "可以选多个", Required: true, Options: []core.CardSelectOption{{Text: "甲", Value: "1"}, {Text: "乙", Value: "2"}, {Text: "丙", Value: "3"}}},
			{Name: "f2", Label: "日期", Kind: "date"},
			{Name: "f3", Label: "备注", Kind: "input"},
		}})
	card.Shared = true
	if void {
		card.Header.Color, card.Header.Tag = "grey", "已作废"
	}
	return card
}

func TestSharedCardRendersAsJSON2(t *testing.T) {
	card := generic(t, renderCardMap(sharedDecisionCard(false), "feishu:oc:u"))
	text := plainJSON(card)
	if card["schema"] != "2.0" || card["elements"] != nil || !strings.Contains(text, `"update_multi":true`) {
		t.Fatalf("not a 2.0 shared card: %s", text)
	}
	for _, old := range []string{`"tag":"action"`, `"tag":"note"`, `"action_type":"form_submit"`} {
		if strings.Contains(text, old) {
			t.Errorf("card keeps 1.0 shape %s: %s", old, text)
		}
	}
	header := card["header"].(map[string]any)
	if header["template"] != "blue" || header["text_tag_list"] != nil {
		t.Fatalf("header = %v", header)
	}
	btn := buttonNamed(card, "甲（推荐）")
	if btn == nil || btn["type"] != "primary" || btn["disabled"] != nil {
		t.Fatalf("recommended button = %v", btn)
	}
	note := findNode(card, func(m map[string]any) bool {
		c, _ := m["content"].(string)
		return strings.Contains(c, "将按推荐处理")
	})
	if note == nil || note["text_size"] != "notation" || !strings.Contains(note["content"].(string), "grey") {
		t.Fatalf("note = %v", note)
	}
	submit := buttonNamed(card, "提交")
	if submit == nil || submit["form_action_type"] != "submit" || submit["name"] != "decision_d7_submit" {
		t.Fatalf("submit = %v", submit)
	}
	for _, tag := range []string{"multi_select_static", "date_picker", "input"} {
		if findNode(card, func(m map[string]any) bool { return m["tag"] == tag }) == nil {
			t.Errorf("form lacks %s: %s", tag, text)
		}
	}
}

func TestVoidedSharedCardIsGreyTaggedAndDisabled(t *testing.T) {
	card := generic(t, renderCardMap(sharedDecisionCard(true), "feishu:oc:u"))
	header := card["header"].(map[string]any)
	tags, _ := header["text_tag_list"].([]any)
	if header["template"] != "grey" || len(tags) != 1 || !strings.Contains(plainJSON(tags[0]), "已作废") {
		t.Fatalf("header = %v", header)
	}
	for _, label := range []string{"甲（推荐）", "乙", "提交"} {
		btn := buttonNamed(card, label)
		if btn == nil || btn["disabled"] != true || !strings.Contains(plainJSON(btn["disabled_tips"]), "已作废") {
			t.Fatalf("button %s = %v", label, btn)
		}
	}
	for _, tag := range []string{"multi_select_static", "date_picker", "input"} {
		f := findNode(card, func(m map[string]any) bool { return m["tag"] == tag })
		if f == nil || f["disabled"] != true {
			t.Fatalf("%s not disabled: %v", tag, f)
		}
	}
}

// Clicks and submits on the 2.0 card reach the engine as the same action
// strings as before, so the answer still arrives as "【决定】问题：选项".
func TestSharedCardCallbacksKeepTheActionFormat(t *testing.T) {
	card := generic(t, renderCardMap(sharedDecisionCard(false), "feishu:oc_test_chat:ou_allowed"))
	answered := core.NewCard().Title("✅ 已决定：乙", "green").Build()
	answered.Shared = true

	btnValue := callbackValueJSON(t, buttonNamed(card, "乙"))
	call, resp := submitForm(t, formSubmitCallback(t, btnValue, "", `{}`), answered)
	if call.action != "act:/decision d1 2" || call.sessionKey != "feishu:oc_test_chat:ou_allowed" {
		t.Fatalf("button call = %+v", call)
	}
	if resp == nil || resp.Card == nil || generic(t, resp.Card.Data)["schema"] != "2.0" {
		t.Fatalf("button reply = %+v", resp)
	}

	submitValue := callbackValueJSON(t, buttonNamed(card, "提交"))
	formValue := `{"choice": ["1", "3"], "f2": "2026-11-20 +0800", "f3": "下周"}`
	call, _ = submitForm(t, formSubmitCallback(t, submitValue, "decision_d7_submit", formValue), answered)
	if call.action != `act:/decision-form d7 {"choice":["1","3"],"f2":"2026-11-20 +0800","f3":"下周"}` {
		t.Fatalf("submit call = %q", call.action)
	}
}

// Feishu rejects a 1.0 card as the reply to a click on a 2.0 card (200830),
// so whatever the engine returns is rendered as 2.0 then; a click on a 1.0
// card keeps the 1.0 reply.
func TestClickOnJSON2CardIsAnsweredWithJSON2(t *testing.T) {
	plain := core.NewCard().Title("已删除", "green").Markdown("完成").Build()
	for name, tc := range map[string]struct {
		value string
		want  bool
	}{
		"2.0 card": {`{"action": "act:/delete-mode cancel", "card_schema": "2"}`, true},
		"1.0 card": {`{"action": "act:/delete-mode cancel"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			ip, _ := newAllowFromTestPlatform(t, "ou_allowed")
			ip.cardNavHandler = func(string, string) *core.Card { return plain }
			resp, err := ip.onCardAction(formSubmitCallback(t, tc.value, "", `{}`))
			if err != nil || resp == nil || resp.Card == nil {
				t.Fatalf("resp=%+v err=%v", resp, err)
			}
			got := generic(t, resp.Card.Data)
			if (got["schema"] == "2.0") != tc.want {
				t.Fatalf("reply schema = %v, want 2.0: %v", got["schema"], tc.want)
			}
			ip.cardActionMsgMu.Lock()
			v2 := ip.cardActionV2["feishu:oc_test_chat:ou_allowed"]
			ip.cardActionMsgMu.Unlock()
			if v2 != tc.want {
				t.Fatalf("tracked 2.0 = %v, want %v (used by a late refresh)", v2, tc.want)
			}
		})
	}
}

func TestClassicCardsStayJSON1(t *testing.T) {
	card := core.NewCard().Title("帮助", "blue").Markdown("内容").Buttons(core.PrimaryBtn("新会话", "cmd:/new")).Note("注").Build()
	got := generic(t, renderCardMap(card, ""))
	if got["schema"] != nil || got["elements"] == nil {
		t.Fatalf("classic card changed: %s", plainJSON(got))
	}
}

// A checklist card: one checker per row inside the form, a submit button and
// an agree-all button, both bound to the form and both confirmed.
func TestChecklistFormRendersCheckersAndTwoSubmits(t *testing.T) {
	form := core.CardForm{Name: "decision_d9", SubmitText: "提交勾选", SubmitType: "primary", SubmitValue: "act:/decision-form d9",
		Confirm: &core.CardConfirm{Title: "提交勾选的项？", Text: "勾选的项同意。"},
		More:    []core.CardButton{{Text: "全部同意", Type: "default", Value: "act:/decision-all d9", Confirm: &core.CardConfirm{Title: "全部同意？", Text: "同意全部 2 项。"}}},
		Fields: []core.CardFormField{
			{Name: "c1", Kind: "checker", Label: "**张三 报销 1,280 元**\n事由：交通费"},
			{Name: "c2", Kind: "checker", Label: "**李四 采购 5 万元**"},
		}}
	card := core.NewCard().Title("需要你选择", "orange").Markdown("**本批审批**").Build()
	card.Elements = append(card.Elements, form)
	card.Shared = true
	got := generic(t, renderCardMap(card, "feishu:oc_test_chat:ou_allowed"))
	c1 := findNode(got, func(m map[string]any) bool { return m["tag"] == "checker" && m["name"] == "c1" })
	if c1 == nil || c1["checked"] != false || !strings.Contains(plainJSON(c1["text"]), `"tag":"lark_md"`) || !strings.Contains(plainJSON(c1["text"]), "事由：交通费") {
		t.Fatalf("checker = %v", c1)
	}
	for label, name := range map[string]string{"提交勾选": "decision_d9_submit", "全部同意": "decision_d9_submit2"} {
		b := buttonNamed(got, label)
		if b == nil || b["form_action_type"] != "submit" || b["name"] != name || b["confirm"] == nil {
			t.Fatalf("%s = %v", label, b)
		}
	}
	formNode := findNode(got, func(m map[string]any) bool { return m["tag"] == "form" })
	if findNode(formNode, func(m map[string]any) bool { return m["tag"] == "checker" }) == nil || buttonNamed(formNode, "全部同意") == nil {
		t.Fatalf("checkers and buttons must sit inside the form: %v", formNode)
	}

	// The submit brings the ticked boxes after the action; agree-all its own action.
	reply := core.NewCard().Title("✅ 已决定：同意 1 项", "green").Build()
	call, resp := submitForm(t, formSubmitCallback(t, callbackValueJSON(t, buttonNamed(got, "提交勾选")), "decision_d9_submit", `{"c1": true, "c2": false}`), reply)
	if call.action != `act:/decision-form d9 {"c1":true,"c2":false}` {
		t.Fatalf("submit call = %q", call.action)
	}
	if resp == nil || resp.Card == nil || generic(t, resp.Card.Data)["schema"] != "2.0" {
		t.Fatalf("submit reply = %+v", resp)
	}
	call, _ = submitForm(t, formSubmitCallback(t, callbackValueJSON(t, buttonNamed(got, "全部同意")), "decision_d9_submit2", `{"c1": false, "c2": false}`), reply)
	if !strings.HasPrefix(call.action, "act:/decision-all d9") {
		t.Fatalf("agree all call = %q", call.action)
	}

	// Voided: checkers and both buttons disabled, no confirm dialog.
	form.Disabled, form.DisabledTip = true, "已作废"
	card.Elements[len(card.Elements)-1] = form
	got = generic(t, renderCardMap(card, ""))
	for _, label := range []string{"提交勾选", "全部同意"} {
		if b := buttonNamed(got, label); b["disabled"] != true || b["confirm"] != nil {
			t.Fatalf("voided %s = %v", label, b)
		}
	}
	if c := findNode(got, func(m map[string]any) bool { return m["tag"] == "checker" }); c["disabled"] != true {
		t.Fatalf("voided checker = %v", c)
	}
}
