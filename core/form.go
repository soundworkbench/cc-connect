package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Form cards collect several parameters in one go (`cc-connect send --form`).
// A form is kept and delivered like a decision: the submitted values come
// back to the session as one message, and the card is rewritten so it can
// no longer be submitted. A form has no default and never expires.

const (
	formMaxFields  = 10
	formMaxOptions = 20
	formUnfilled   = "（未填）"
)

// FormRequest is the JSON body for POST /form and the file read by
// `cc-connect send --form`.
type FormRequest struct {
	Project    string      `json:"project,omitempty"`
	SessionKey string      `json:"session_key,omitempty"`
	Title      string      `json:"title"`
	Note       string      `json:"note,omitempty"`
	Fields     []FormField `json:"fields"`
	Submit     string      `json:"submit,omitempty"` // button label; default 提交
}

// FormField is one input of a form. The bridge names the fields itself, so
// the request only says what each one asks.
type FormField struct {
	Label       string   `json:"label"`
	Type        string   `json:"type"` // input (default), date, select, multi_select
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

// FormSpec is the persisted shape of a form card.
type FormSpec struct {
	Fields []FormField `json:"fields"`
	Submit string      `json:"submit,omitempty"`
}

// FormAnswer is one submitted value, ready to show.
type FormAnswer struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

func formFieldName(i int) string { return "f" + strconv.Itoa(i+1) }

func (r *FormRequest) validate() error {
	if strings.TrimSpace(r.Title) == "" {
		return errors.New("title is required")
	}
	if len(r.Fields) == 0 || len(r.Fields) > formMaxFields {
		return fmt.Errorf("a form needs 1 to %d fields", formMaxFields)
	}
	seen := map[string]bool{}
	for i := range r.Fields {
		f := &r.Fields[i]
		f.Label = strings.TrimSpace(f.Label)
		if f.Label == "" {
			return errors.New("every field needs a label")
		}
		if seen[f.Label] {
			return fmt.Errorf("duplicate field label %q", f.Label)
		}
		seen[f.Label] = true
		switch f.Type {
		case "":
			f.Type = "input"
		case "input", "date":
		case "select", "multi_select":
			if len(f.Options) == 0 || len(f.Options) > formMaxOptions {
				return fmt.Errorf("field %q needs 1 to %d options", f.Label, formMaxOptions)
			}
			for _, o := range f.Options {
				if strings.TrimSpace(o) == "" {
					return fmt.Errorf("field %q has an empty option", f.Label)
				}
			}
		default:
			return fmt.Errorf("field %q: unknown type %q (input, date, select, multi_select)", f.Label, f.Type)
		}
	}
	return nil
}

// formCard renders a pending form, or the submitted one.
func formCard(d *Decision) *Card {
	if d.State != decisionPending {
		cb := NewCard().Title("✅ 已提交："+d.Question, "green")
		for _, a := range d.Values {
			cb.Markdown("**" + a.Label + "**：" + a.Value)
		}
		cb.Note(decisionClock(d.ResolvedAt) + " 已提交")
		return cb.Build()
	}
	cb := NewCard().Title("需要你填写", "blue")
	cb.Markdown("**" + d.Question + "**")
	if d.Note != "" {
		cb.Markdown(d.Note)
	}
	form := CardForm{Name: "form_" + d.ID, SubmitText: d.Form.Submit, SubmitType: "primary", SubmitValue: "act:/form-submit " + d.ID}
	if form.SubmitText == "" {
		form.SubmitText = "提交"
	}
	for i, f := range d.Form.Fields {
		label := f.Label
		if f.Required {
			label += "（必填）"
		}
		field := CardFormField{Name: formFieldName(i), Label: label, Kind: f.Type, Placeholder: f.Placeholder, Required: f.Required}
		for j, o := range f.Options {
			field.Options = append(field.Options, CardSelectOption{Text: o, Value: strconv.Itoa(j + 1)})
		}
		form.Fields = append(form.Fields, field)
	}
	cb.card.Elements = append(cb.card.Elements, form)
	cb.Note("提交后作为一条消息回到对话")
	return cb.Build()
}

// formInboundText is the message the session receives for a submitted form.
func formInboundText(d *Decision) string {
	lines := []string{"【表单】" + d.Question}
	for _, a := range d.Values {
		lines = append(lines, a.Label+"："+a.Value)
	}
	return strings.Join(lines, "\n")
}

// formAnswers turns the submitted values into display text. It reports false
// when a required field is empty.
func formAnswers(spec *FormSpec, values map[string]any) ([]FormAnswer, bool) {
	var out []FormAnswer
	for i, f := range spec.Fields {
		var parts []string
		pick := func(v string) {
			v = strings.TrimSpace(v)
			if v == "" {
				return
			}
			switch f.Type {
			case "select", "multi_select":
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > len(f.Options) {
					return
				}
				v = f.Options[n-1]
			case "date":
				v, _, _ = strings.Cut(v, " ") // "2026-11-12 +0800"
			}
			parts = append(parts, v)
		}
		switch v := values[formFieldName(i)].(type) {
		case string:
			pick(v)
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok {
					pick(s)
				}
			}
		}
		if len(parts) == 0 {
			if f.Required {
				return nil, false
			}
			parts = []string{formUnfilled}
		}
		out = append(out, FormAnswer{Label: f.Label, Value: strings.Join(parts, "、")})
	}
	return out, true
}

// SendFormCard sends a form card to the session and returns its ID.
func (e *Engine) SendFormCard(req FormRequest) (string, error) {
	if e.decisions == nil {
		return "", errors.New("form cards need decision cards enabled ([decision] enabled = true)")
	}
	if err := req.validate(); err != nil {
		return "", err
	}
	d := &Decision{
		ID:         newDecisionID(),
		Project:    e.name,
		SessionKey: req.SessionKey,
		Question:   strings.TrimSpace(req.Title),
		Note:       strings.TrimSpace(req.Note),
		Form:       &FormSpec{Fields: req.Fields, Submit: strings.TrimSpace(req.Submit)},
		State:      decisionPending,
		CreatedAt:  time.Now(),
	}
	return e.postDecision(d)
}

// handleFormSubmit handles "act:/form-submit <id> <values>".
func (e *Engine) handleFormSubmit(args string) *Card {
	if e.decisions == nil {
		cardActionDropped("form", "", "decisions_disabled")
		return nil
	}
	id, raw, _ := strings.Cut(strings.TrimSpace(args), " ")
	d := e.decisions.Get(id)
	if d == nil || d.Form == nil {
		cardActionDropped("form", id, "unknown_form")
		return nil
	}
	if d.State != decisionPending {
		return formCard(d)
	}
	values := parseFormValues("form", id, raw)
	answers, ok := formAnswers(d.Form, values)
	if !ok {
		return &Card{Toast: "请把必填项填完"}
	}
	r, ok := e.decisions.ResolveForm(id, answers)
	if !ok {
		if cur := e.decisions.Get(id); cur != nil {
			return formCard(cur)
		}
		cardActionDropped("form", id, "unknown_form")
		return nil
	}
	e.wakeDecisions()
	return formCard(r)
}

// ResolveForm records a pending form's submitted values. It reports false
// when the form is unknown or already submitted. The returned value is a copy.
func (s *DecisionStore) ResolveForm(id string, answers []FormAnswer) (*Decision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil || d.Form == nil || d.State != decisionPending {
		return nil, false
	}
	d.State, d.Values, d.ResolvedAt = decisionAnswered, answers, time.Now()
	if err := s.saveLocked(); err != nil {
		return nil, false
	}
	c := *d
	return &c, true
}
