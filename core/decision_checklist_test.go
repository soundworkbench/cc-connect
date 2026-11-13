package core

import (
	"strings"
	"testing"
	"time"
)

var checklistReq = DecisionRequest{
	Question:  "本批审批",
	Options:   []string{"张三 报销 1,280 元", "李四 采购 5 万元", "王五 出差 3 天"},
	Details:   []string{"事由：客户拜访交通费；建议同意", "事由：测试仪器；建议核对合同", ""},
	Checklist: true,
}

func TestChecklistCardRendersOneCheckBoxPerRow(t *testing.T) {
	e, p, _, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, checklistReq)
	d := store.Get(id)
	if d.HasDefault || !d.Deadline.IsZero() || !d.Multi {
		t.Fatalf("a checklist has no default: %#v", d)
	}
	card := p.idCards[0]
	if !card.Shared || card.Header.Title != "需要你选择" {
		t.Fatalf("card = %#v", card)
	}
	f, ok := cardForm(card)
	if !ok || len(f.Fields) != 3 {
		t.Fatalf("form = %#v", f)
	}
	for i, field := range f.Fields {
		if field.Kind != "checker" || field.Name != checklistField(i) || !strings.Contains(field.Label, checklistReq.Options[i]) {
			t.Fatalf("row %d = %#v", i, field)
		}
	}
	if !strings.Contains(f.Fields[1].Label, "**李四 采购 5 万元**\n事由：测试仪器；建议核对合同") || strings.Contains(f.Fields[2].Label, "\n") {
		t.Fatalf("details: %q / %q", f.Fields[1].Label, f.Fields[2].Label)
	}
	if f.SubmitText != "提交勾选" || f.Confirm == nil || f.SubmitValue != "act:/decision-form "+id {
		t.Fatalf("submit = %#v", f)
	}
	if len(f.More) != 1 || f.More[0].Text != "全部同意" || f.More[0].Confirm == nil || f.More[0].Value != "act:/decision-all "+id {
		t.Fatalf("agree all = %#v", f.More)
	}
	if !strings.Contains(cardText(card), "需要你选择，不会自动处理") {
		t.Fatalf("note: %q", cardText(card))
	}
}

func TestChecklistPartialSubmit(t *testing.T) {
	e, _, sess, _ := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, checklistReq)
	card := e.handleCardNav(`act:/decision-form `+id+` {"c1":true,"c2":false,"c3":"true"}`, decKey)
	if card == nil || card.Toast != "" || card.Header.Title != "✅ 已决定：同意 2 项" {
		t.Fatalf("answered card = %#v", card)
	}
	text := cardText(card)
	if !strings.Contains(text, "**同意（2）**\n- 张三 报销 1,280 元\n- 王五 出差 3 天") || !strings.Contains(text, "**不同意（1）**\n- 李四 采购 5 万元") {
		t.Fatalf("answered card text = %q", text)
	}
	got := waitPrompts(t, sess, 1)
	if got[0] != "【决定】本批审批：张三 报销 1,280 元、王五 出差 3 天" {
		t.Fatalf("prompt = %q", got[0])
	}
	// A second submit or 全部同意 afterwards changes nothing.
	e.handleCardNav(`act:/decision-form `+id+` {"c2":true}`, decKey)
	if c := e.handleCardNav("act:/decision-all "+id, decKey); c == nil || c.Header.Title != "✅ 已决定：同意 2 项" {
		t.Fatalf("late agree all = %#v", c)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(sess.got()); n != 1 {
		t.Fatalf("prompts = %d", n)
	}
}

func TestChecklistAgreeAll(t *testing.T) {
	e, _, sess, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, checklistReq)
	// Feishu may send the form values along with the button.
	card := e.handleCardNav(`act:/decision-all `+id+` {"c1":false}`, decKey)
	if card == nil || card.Header.Title != "✅ 已决定：全部同意" || strings.Contains(cardText(card), "不同意") {
		t.Fatalf("card = %#v", card)
	}
	got := waitPrompts(t, sess, 1)
	if got[0] != "【决定】本批审批：张三 报销 1,280 元、李四 采购 5 万元、王五 出差 3 天（全部同意）" {
		t.Fatalf("prompt = %q", got[0])
	}
	if d := store.Get(id); !d.AllAgreed || len(d.ChosenSet) != 3 {
		t.Fatalf("stored = %#v", d)
	}
	time.Sleep(200 * time.Millisecond)
}

func TestChecklistEmptySubmitOnlyShowsANotice(t *testing.T) {
	e, _, sess, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, checklistReq)
	for _, action := range []string{`act:/decision-form ` + id, `act:/decision-form ` + id + ` {"c1":false,"c2":"false"}`} {
		if c := e.handleCardNav(action, decKey); c == nil || c.Toast != checklistEmptyToast {
			t.Fatalf("%s: %#v", action, c)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if d := store.Get(id); d.State != decisionPending || len(sess.got()) != 0 {
		t.Fatalf("empty submit changed the card: %#v %v", d, sess.got())
	}
}

func TestChecklistVoid(t *testing.T) {
	e, p, sess, _ := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, checklistReq)
	if _, err := e.VoidDecision(id); err != nil {
		t.Fatal(err)
	}
	card := p.patchedCard("om_test")
	f, ok := cardForm(card)
	if !ok || !f.Disabled || f.Confirm != nil || f.More[0].Confirm != nil || card.Header.Tag != "已作废" {
		t.Fatalf("voided checklist = %#v", card)
	}
	for _, action := range []string{`act:/decision-form ` + id + ` {"c1":true}`, "act:/decision-all " + id} {
		if c := e.handleCardNav(action, decKey); c == nil || c.Toast != decisionVoidToast {
			t.Fatalf("%s on a voided checklist = %#v", action, c)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if got := sess.got(); len(got) != 0 {
		t.Fatalf("a voided checklist reached the session: %v", got)
	}
}

func TestChecklistValidationAndCheckedValues(t *testing.T) {
	e, _, _, _ := newDecisionEngine(t, t.TempDir())
	many := DecisionRequest{Question: "大批", Checklist: true}
	for i := 0; i < 30; i++ {
		many.Options = append(many.Options, "单子"+string(rune('A'+i)))
	}
	sendTestDecision(t, e, many) // more than the 20 a dropdown allows
	for name, req := range map[string]DecisionRequest{
		"details without checklist": {Question: "q", Options: []string{"a", "b"}, Details: []string{"x"}},
		"more details than options": {Question: "q", Options: []string{"a", "b"}, Details: []string{"x", "y", "z"}, Checklist: true},
	} {
		req.SessionKey = decKey
		if _, err := e.SendDecisionCard(req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for v, want := range map[any]bool{true: true, "true": true, "1": true, float64(1): true, false: false, "": false, "false": false, nil: false} {
		if checklistChecked(v) != want {
			t.Errorf("checked(%#v) != %v", v, want)
		}
	}
	if !checklistChecked(map[string]any{"checked": true}) {
		t.Error("object with checked")
	}
}
