package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cardButtons returns every button of a card, in order.
func cardButtons(c *Card) []CardButton {
	var out []CardButton
	for _, el := range c.Elements {
		if a, ok := el.(CardActions); ok {
			out = append(out, a.Buttons...)
		}
	}
	return out
}

func cardForm(c *Card) (CardForm, bool) {
	for _, el := range c.Elements {
		if f, ok := el.(CardForm); ok {
			return f, true
		}
	}
	return CardForm{}, false
}

func TestDecisionAndFormCardsAreShared(t *testing.T) {
	e, p, _, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "用哪个方案？", Options: []string{"甲", "乙"}})
	multi := sendTestDecision(t, e, DecisionRequest{Question: "选哪些？", Options: []string{"甲", "乙"}, Multi: true})
	form, err := e.SendFormCard(FormRequest{SessionKey: decKey, Title: "出差申请", Fields: []FormField{{Label: "地点"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range p.idCards {
		if !c.Shared {
			t.Fatalf("sent card %d is not shared: %#v", i, c)
		}
	}
	for _, c := range []*Card{
		e.handleCardNav("act:/decision "+id+" 1", decKey),
		e.handleCardNav(`act:/decision-form `+multi+` {"choice":["1","2"]}`, decKey),
		e.handleCardNav(`act:/form-submit `+form+` {"f1":"上海"}`, decKey),
	} {
		if c == nil || !c.Shared || c.Toast != "" {
			t.Fatalf("answered card is not a shared card: %#v", c)
		}
	}
	expiring := sendTestDecision(t, e, DecisionRequest{Question: "到期的", Options: []string{"A", "B"}, DeadlineMinutes: 10})
	store.mu.Lock()
	store.find(expiring).Deadline = time.Now().Add(-time.Second)
	store.mu.Unlock()
	e.wakeDecisions()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c := p.patchedCard("om_test"); c != nil {
			if !c.Shared || !strings.Contains(c.Header.Title, "已到期") {
				t.Fatalf("expired card = %#v", c)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired card was not updated")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopDecisions(t, e)
}

// stopDecisions stops the engine and waits for deliveries in flight, so
// nothing writes the decision file after the test's data directory is gone.
func stopDecisions(t *testing.T, e *Engine) {
	t.Helper()
	_ = e.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		busy := false
		e.decisionInflight.Range(func(any, any) bool { busy = true; return false })
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("decision deliveries still running after stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestVoidDecisionGreysCardCancelsDefaultAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	e, p, sess, store := newDecisionEngine(t, dir)
	id := sendTestDecision(t, e, DecisionRequest{Question: "发不发周报？", Options: []string{"发", "不发"}, DeadlineMinutes: 10})

	cardErr, err := e.VoidDecision(id)
	if err != nil || cardErr != nil {
		t.Fatalf("void: err=%v cardErr=%v", err, cardErr)
	}
	card := p.patchedCard("om_test")
	if card == nil || !card.Shared || card.Header.Color != "grey" || card.Header.Tag != "已作废" {
		t.Fatalf("voided card = %#v", card)
	}
	text := cardText(card)
	if !strings.Contains(text, "发不发周报？") || !strings.Contains(text, "已作废，不会自动处理") || strings.Contains(text, "将按推荐处理") {
		t.Fatalf("voided card text = %q", text)
	}
	btns := cardButtons(card)
	if len(btns) != 2 {
		t.Fatalf("voided card buttons = %#v", btns)
	}
	for _, b := range btns {
		if !b.Disabled || b.DisabledTip == "" {
			t.Fatalf("button still usable: %#v", b)
		}
	}

	// The deadline passes: nothing is applied or delivered.
	store.mu.Lock()
	store.find(id).Deadline = time.Now().Add(-time.Second)
	store.mu.Unlock()
	e.wakeDecisions()
	time.Sleep(300 * time.Millisecond)
	if got := sess.got(); len(got) != 0 {
		t.Fatalf("a voided card reached the session: %v", got)
	}
	if d := store.Get(id); d.State != decisionVoided {
		t.Fatalf("stored state = %q", d.State)
	}
	// A late tap shows a notice and changes nothing.
	if c := e.handleCardNav("act:/decision "+id+" 1", decKey); c == nil || c.Toast == "" {
		t.Fatalf("tap on a voided card = %#v", c)
	}
	_ = e.Stop()

	e2, _, sess2, store2 := newDecisionEngine(t, dir)
	if d := store2.Get(id); d == nil || d.State != decisionVoided {
		t.Fatalf("after restart = %#v", d)
	}
	if c := e2.handleCardNav("act:/decision "+id+" 2", decKey); c == nil || c.Toast == "" {
		t.Fatalf("tap after restart = %#v", c)
	}
	time.Sleep(300 * time.Millisecond)
	if got := sess2.got(); len(got) != 0 {
		t.Fatalf("a voided card reached the session after restart: %v", got)
	}
	// Voiding again is harmless.
	if _, err := e2.VoidDecision(id); err != nil {
		t.Fatalf("second void: %v", err)
	}
}

func TestVoidDecisionRefusesSettledAndUnknownCards(t *testing.T) {
	e, _, sess, _ := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "用哪个？", Options: []string{"甲", "乙"}})
	e.handleCardNav("act:/decision "+id+" 1", decKey)
	waitPrompts(t, sess, 1)
	time.Sleep(200 * time.Millisecond)
	if _, err := e.VoidDecision(id); err == nil || !strings.Contains(err.Error(), "answered") {
		t.Fatalf("void after answer: %v", err)
	}
	if _, err := e.VoidDecision("dmissing"); !errors.Is(err, ErrDecisionNotFound) {
		t.Fatalf("void unknown: %v", err)
	}
}

func TestVoidDropdownAndFormCards(t *testing.T) {
	e, p, sess, _ := newDecisionEngine(t, t.TempDir())
	multi := sendTestDecision(t, e, DecisionRequest{Question: "选哪些？", Options: []string{"甲", "乙"}, Multi: true})
	if _, err := e.VoidDecision(multi); err != nil {
		t.Fatal(err)
	}
	f, ok := cardForm(p.patchedCard("om_test"))
	if !ok || !f.Disabled || f.Confirm != nil {
		t.Fatalf("voided dropdown = %#v", f)
	}
	if c := e.handleCardNav(`act:/decision-form `+multi+` {"choice":["1"]}`, decKey); c == nil || c.Toast == "" {
		t.Fatalf("submit on a voided dropdown = %#v", c)
	}

	form, err := e.SendFormCard(FormRequest{SessionKey: decKey, Title: "出差申请", Fields: []FormField{{Label: "地点", Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.VoidDecision(form); err != nil {
		t.Fatal(err)
	}
	card := p.patchedCard("om_test")
	if f, ok := cardForm(card); !ok || !f.Disabled || card.Header.Tag != "已作废" || !card.Shared {
		t.Fatalf("voided form card = %#v", card)
	}
	if c := e.handleCardNav(`act:/form-submit `+form+` {"f1":"上海"}`, decKey); c == nil || c.Toast == "" {
		t.Fatalf("submit on a voided form = %#v", c)
	}
	time.Sleep(200 * time.Millisecond)
	if got := sess.got(); len(got) != 0 {
		t.Fatalf("a voided card reached the session: %v", got)
	}
}

func TestDecisionVoidAPI(t *testing.T) {
	e, _, sess, store := newDecisionEngine(t, t.TempDir())
	id := sendTestDecision(t, e, DecisionRequest{Question: "用哪个？", Options: []string{"甲", "乙"}})
	api := &APIServer{engines: map[string]*Engine{"test": e}}
	post := func(id string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(DecisionVoidRequest{ID: id})
		rec := httptest.NewRecorder()
		api.handleDecisionVoid(rec, httptest.NewRequest(http.MethodPost, "/decision/void", bytes.NewReader(body)))
		return rec
	}
	rec := post(id)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"card":"updated"`) {
		t.Fatalf("void: %d %s", rec.Code, rec.Body.String())
	}
	if d := store.Get(id); d.State != decisionVoided {
		t.Fatalf("state = %q", d.State)
	}
	if rec := post("dmissing"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d %s", rec.Code, rec.Body.String())
	}
	answered := sendTestDecision(t, e, DecisionRequest{Question: "再选", Options: []string{"甲", "乙"}})
	e.handleCardNav("act:/decision "+answered+" 2", decKey)
	if rec := post(answered); rec.Code != http.StatusConflict {
		t.Fatalf("answered id: %d %s", rec.Code, rec.Body.String())
	}
	waitPrompts(t, sess, 1)
	time.Sleep(200 * time.Millisecond)
}
