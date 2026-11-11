package core

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func weeklyReportForm() FormRequest {
	return FormRequest{Title: "周报参数", Note: "缺的我再问", Fields: []FormField{
		{Label: "开始日期", Type: "date", Required: true},
		{Label: "对象", Type: "select", Options: []string{"全组", "只看我"}, Required: true},
		{Label: "包含", Type: "multi_select", Options: []string{"进展", "风险", "计划"}},
		{Label: "备注", Placeholder: "可以不填"},
	}}
}

func TestFormCardCollectsValuesIntoOneMessage(t *testing.T) {
	e, p, sess, store := newDecisionEngine(t, t.TempDir())
	req := weeklyReportForm()
	req.SessionKey = decKey
	id, err := e.SendFormCard(req)
	if err != nil {
		t.Fatal(err)
	}
	card := p.idCards[0]
	f := decisionFormOf(t, card)
	kinds := []string{}
	for _, fld := range f.Fields {
		kinds = append(kinds, fld.Name+":"+fld.Kind)
	}
	if card.Header.Title != "需要你填写" || strings.Join(kinds, ",") != "f1:date,f2:select,f3:multi_select,f4:input" ||
		f.Fields[0].Label != "开始日期（必填）" || !f.Fields[0].Required || f.SubmitText != "提交" || f.SubmitValue != "act:/form-submit "+id {
		t.Fatalf("form=%#v", f)
	}
	if c := e.handleCardNav(f.SubmitValue+` {"f2":"1"}`, decKey); c == nil || c.Toast != "请把必填项填完" {
		t.Fatalf("missing required: %#v", c)
	}
	c := e.handleCardNav(f.SubmitValue+` {"f1":"2026-11-12 +0800","f2":"1","f3":["3","1"]}`, decKey)
	if c == nil || !strings.Contains(c.Header.Title, "已提交：周报参数") || !strings.Contains(cardText(c), "计划、进展") {
		t.Fatalf("submitted card=%#v", c)
	}
	want := "【表单】周报参数\n开始日期：2026-11-12\n对象：全组\n包含：计划、进展\n备注：（未填）"
	if got := waitPrompts(t, sess, 1); !strings.Contains(got[0], want) {
		t.Fatalf("prompt=%q want %q", got[0], want)
	}
	// a second submit does not deliver again
	if c := e.handleCardNav(f.SubmitValue+` {"f1":"2026-11-13","f2":"2"}`, decKey); c == nil || !strings.Contains(c.Header.Title, "已提交") {
		t.Fatalf("second submit=%#v", c)
	}
	// a decision tap on a form is ignored
	if c := e.handleCardNav("act:/decision "+id+" 1", decKey); c == nil || !strings.Contains(c.Header.Title, "已提交") {
		t.Fatalf("decision tap on a form=%#v", c)
	}
	if n := len(sess.got()); n != 1 {
		t.Fatalf("prompts=%d", n)
	}
	if d := store.Get(id); d.HasDefault || !d.Deadline.IsZero() {
		t.Fatalf("a form has no default: %#v", d)
	}
}

func TestFormRequestValidation(t *testing.T) {
	e, _, _, _ := newDecisionEngine(t, t.TempDir())
	for name, mod := range map[string]func(*FormRequest){
		"no title":        func(r *FormRequest) { r.Title = " " },
		"no fields":       func(r *FormRequest) { r.Fields = nil },
		"no label":        func(r *FormRequest) { r.Fields[0].Label = "" },
		"duplicate label": func(r *FormRequest) { r.Fields[1].Label = "开始日期" },
		"bad type":        func(r *FormRequest) { r.Fields[0].Type = "slider" },
		"select no opts":  func(r *FormRequest) { r.Fields[1].Options = nil },
		"no session":      func(r *FormRequest) { r.SessionKey = "" },
	} {
		req := weeklyReportForm()
		req.SessionKey = decKey
		mod(&req)
		if _, err := e.SendFormCard(req); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestUnusableFormSubmitsAreLoggedWithAReason(t *testing.T) {
	e, _, _, _ := newDecisionEngine(t, t.TempDir())
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	for action, want := range map[string]string{
		`act:/decision-form gone {"choice":"1"}`: `msg="card: decision_form_dropped" id=gone reason=unknown_decision`,
		`act:/form-submit gone {"f1":"x"}`:       `msg="card: form_dropped" id=gone reason=unknown_form`,
		`act:/decision gone 1`:                   `msg="card: decision_dropped" id=gone reason=unknown_decision`,
	} {
		buf.Reset()
		if card := e.handleCardNav(action, decKey); card != nil {
			t.Fatalf("%s: card=%#v", action, card)
		}
		if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), want) {
			t.Fatalf("%s: log=%s", action, buf.String())
		}
	}
}
