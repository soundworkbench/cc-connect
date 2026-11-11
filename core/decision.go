package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Decision cards: an assistant asks the user a multiple-choice question
// without holding its turn open. The card carries one button per option.
// A tap (or the deadline, when the card has a default) is delivered back to
// the session as an ordinary scheduled prompt, and the card is rewritten in
// place so it can no longer be used.

const (
	DefaultDecisionHours = 12

	decisionPending  = "pending"
	decisionAnswered = "answered" // user tapped an option
	decisionExpired  = "expired"  // deadline passed, default applied

	decisionRetryInterval = 30 * time.Second

	// Up to decisionButtonOptions options show as buttons; more, or a
	// multiple choice, show as a dropdown with a submit button.
	decisionButtonOptions = 6
	decisionMaxOptions    = 20
	decisionChoiceField   = "choice"
)

var decisionZone = time.FixedZone("CST", 8*3600)

// DecisionRequest is the JSON body for POST /decision and the file read by
// `cc-connect send --decision`.
type DecisionRequest struct {
	Project         string   `json:"project,omitempty"`
	SessionKey      string   `json:"session_key,omitempty"`
	Question        string   `json:"question"`
	Options         []string `json:"options"`                    // the first option is the recommended one
	Note            string   `json:"note,omitempty"`             // optional second line under the question
	NoDefault       bool     `json:"no_default,omitempty"`       // no recommended option, no deadline
	DeadlineMinutes int      `json:"deadline_minutes,omitempty"` // 0 = configured default
	// Danger marks an irreversible choice (delete, pay, send outside): red
	// buttons, a confirm dialog before the choice counts, and never a default.
	Danger bool `json:"danger,omitempty"`
	// Multi lets the person pick several options from a dropdown.
	Multi bool `json:"multi,omitempty"`
}

// Decision is one persisted decision card.
type Decision struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	SessionKey string    `json:"session_key"`
	Question   string    `json:"question"`
	Note       string    `json:"note,omitempty"`
	Options    []string  `json:"options"`
	HasDefault bool      `json:"has_default"` // Options[0] is recommended and applied at Deadline
	Deadline   time.Time `json:"deadline,omitempty"`
	MessageID  string    `json:"message_id,omitempty"`
	State      string    `json:"state"`
	Danger     bool      `json:"danger,omitempty"`
	Multi      bool      `json:"multi,omitempty"`
	Chosen     int       `json:"chosen,omitempty"`     // 1-based, valid when State != pending; the first pick when Multi
	ChosenSet  []int     `json:"chosen_set,omitempty"` // every pick, ascending, when Multi
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	Delivered  bool      `json:"delivered"` // the answer was handed to the session
	// Form, when set, makes this a form card (see form.go): Question is its
	// title, Options is empty, and Values holds what was submitted.
	Form   *FormSpec    `json:"form,omitempty"`
	Values []FormAnswer `json:"values,omitempty"`
}

func (d *Decision) validate() error {
	if strings.TrimSpace(d.Question) == "" {
		return errors.New("question is required")
	}
	if len(d.Options) < 2 {
		return errors.New("at least two options are required")
	}
	if len(d.Options) > decisionMaxOptions {
		return fmt.Errorf("at most %d options are allowed", decisionMaxOptions)
	}
	for _, o := range d.Options {
		if strings.TrimSpace(o) == "" {
			return errors.New("options must not be empty")
		}
	}
	return nil
}

// ChosenLabel returns the label of the resolved option, or the labels of all
// picked options of a multiple choice.
func (d *Decision) ChosenLabel() string {
	picks := d.ChosenSet
	if len(picks) == 0 {
		picks = []int{d.Chosen}
	}
	var labels []string
	for _, i := range picks {
		if i >= 1 && i <= len(d.Options) {
			labels = append(labels, d.Options[i-1])
		}
	}
	return strings.Join(labels, "、")
}

// usesForm reports whether the card shows a dropdown instead of buttons.
func (d *Decision) usesForm() bool {
	return d.Multi || len(d.Options) > decisionButtonOptions
}

// inboundText is the message the session receives once the decision is resolved.
func (d *Decision) inboundText() string {
	if d.Form != nil {
		return formInboundText(d)
	}
	if d.State == decisionExpired {
		return fmt.Sprintf("【到期默认】%s：%s（到期未回复，按推荐处理）", d.Question, d.ChosenLabel())
	}
	return fmt.Sprintf("【决定】%s：%s", d.Question, d.ChosenLabel())
}

// DecisionStore persists decisions to a JSON file.
type DecisionStore struct {
	path string
	mu   sync.Mutex
	list []*Decision
}

func NewDecisionStore(dataDir string) (*DecisionStore, error) {
	dir := filepath.Join(dataDir, "decisions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &DecisionStore{path: filepath.Join(dir, "decisions.json")}
	if data, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(data, &s.list); err != nil {
			slog.Error("decision: failed to load", "path", s.path, "error", err)
		}
	}
	return s, nil
}

// saveLocked writes the file atomically. Resolved and delivered decisions older
// than a week are dropped.
func (s *DecisionStore) saveLocked() error {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	kept := s.list[:0]
	for _, d := range s.list {
		if d.State != decisionPending && d.Delivered && d.ResolvedAt.Before(cutoff) {
			continue
		}
		kept = append(kept, d)
	}
	s.list = kept
	data, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *DecisionStore) Add(d *Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, d)
	return s.saveLocked()
}

func (s *DecisionStore) find(id string) *Decision {
	for _, d := range s.list {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// Get returns a copy of the decision.
func (s *DecisionStore) Get(id string) *Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.find(id); d != nil {
		c := *d
		return &c
	}
	return nil
}

func (s *DecisionStore) SetMessageID(id, msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.find(id); d != nil {
		d.MessageID = msgID
		return s.saveLocked()
	}
	return nil
}

// Resolve moves a pending decision to state with the 1-based option idx. It
// reports false when the decision is unknown, already resolved, or idx is out
// of range, so a double tap or a late tap is applied once. The returned value
// is a copy.
func (s *DecisionStore) Resolve(id string, idx int, state string) (*Decision, bool) {
	return s.ResolveSet(id, []int{idx}, state)
}

// ResolveSet is Resolve for one or more picks; several picks are kept only
// for a multiple-choice decision.
func (s *DecisionStore) ResolveSet(id string, picks []int, state string) (*Decision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil || d.State != decisionPending || len(picks) == 0 || (len(picks) > 1 && !d.Multi) {
		return nil, false
	}
	for _, idx := range picks {
		if idx < 1 || idx > len(d.Options) {
			return nil, false
		}
	}
	d.State, d.Chosen, d.ResolvedAt = state, picks[0], time.Now()
	if d.Multi {
		d.ChosenSet = append([]int(nil), picks...)
	}
	if err := s.saveLocked(); err != nil {
		slog.Error("decision: persist failed", "id", id, "error", err)
	}
	c := *d
	return &c, true
}

func (s *DecisionStore) SetDelivered(id string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.find(id); d != nil && d.Delivered != v {
		d.Delivered = v
		if err := s.saveLocked(); err != nil {
			slog.Error("decision: persist failed", "id", id, "error", err)
		}
	}
}

// Due returns copies of pending decisions whose default deadline has passed.
func (s *DecisionStore) Due(now time.Time) []*Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Decision
	for _, d := range s.list {
		if d.State == decisionPending && d.HasDefault && !now.Before(d.Deadline) {
			c := *d
			out = append(out, &c)
		}
	}
	return out
}

// Undelivered returns copies of resolved decisions not yet handed to the session.
func (s *DecisionStore) Undelivered() []*Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Decision
	for _, d := range s.list {
		if d.State != decisionPending && !d.Delivered {
			c := *d
			out = append(out, &c)
		}
	}
	return out
}

// nextDeadline returns the earliest pending default deadline.
func (s *DecisionStore) nextDeadline() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next time.Time
	found := false
	for _, d := range s.list {
		if d.State == decisionPending && d.HasDefault && (!found || d.Deadline.Before(next)) {
			next, found = d.Deadline, true
		}
	}
	return next, found
}

func newDecisionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "d" + hex.EncodeToString(b)
}

// ── Cards ──────────────────────────────────────────────────────

func decisionActionValue(id string, idx int) string {
	return fmt.Sprintf("act:/decision %s %d", id, idx)
}

func decisionClock(t time.Time) string {
	return t.In(decisionZone).Format("01-02 15:04")
}

func buildDecisionCard(d *Decision) *Card {
	if d.Form != nil {
		return formCard(d)
	}
	switch d.State {
	case decisionAnswered:
		cb := NewCard().Title("✅ 已决定："+d.ChosenLabel(), "green")
		cb.Markdown("**" + d.Question + "**")
		cb.Markdown("→ " + d.ChosenLabel() + "（" + decisionClock(d.ResolvedAt) + " 你已选择）")
		return cb.Build()
	case decisionExpired:
		cb := NewCard().Title("⏱ 已到期，按推荐处理："+d.ChosenLabel(), "grey")
		cb.Markdown("**" + d.Question + "**")
		cb.Markdown("→ " + d.ChosenLabel() + "（" + decisionClock(d.ResolvedAt) + " 自动处理）")
		return cb.Build()
	}

	title, color := "需要你选择", "orange"
	switch {
	case d.Danger:
		title, color = "需要你确认", "red"
	case d.HasDefault:
		title, color = "需要你决定", "blue"
	}
	cb := NewCard().Title(title, color)
	cb.Markdown("**" + d.Question + "**")
	if d.Note != "" {
		cb.Markdown(d.Note)
	}
	if d.usesForm() {
		cb.card.Elements = append(cb.card.Elements, decisionForm(d))
	} else {
		for i, opt := range d.Options {
			btn := Btn(opt, "default", decisionActionValue(d.ID, i+1))
			switch {
			case d.Danger:
				btn.Type = "danger"
				btn.Confirm = &CardConfirm{Title: "确认这个操作？", Text: "选择「" + opt + "」后会马上照做，不能撤回。"}
			case d.HasDefault && i == 0:
				btn.Text, btn.Type = opt+"（推荐）", "primary"
			}
			cb.Buttons(btn)
		}
	}
	if d.HasDefault {
		cb.Note(fmt.Sprintf("%s 前未选择，将按推荐处理：%s", decisionClock(d.Deadline), d.Options[0]))
	} else {
		cb.Note("需要你选择，不会自动处理")
	}
	return cb.Build()
}

// decisionForm is the dropdown of a decision with many options or a multiple
// choice, submitted as "act:/decision-form <id> <values>".
func decisionForm(d *Decision) CardForm {
	field := CardFormField{Name: decisionChoiceField, Kind: "select", Placeholder: "请选择", Required: true}
	if d.Multi {
		field.Kind, field.Placeholder = "multi_select", "可以选多个"
	}
	for i, opt := range d.Options {
		label := opt
		if d.HasDefault && !d.Multi && i == 0 {
			label += "（推荐）"
		}
		field.Options = append(field.Options, CardSelectOption{Text: label, Value: strconv.Itoa(i + 1)})
	}
	form := CardForm{Name: "decision_" + d.ID, Fields: []CardFormField{field}, SubmitText: "提交", SubmitType: "primary",
		SubmitValue: "act:/decision-form " + d.ID}
	if d.Danger {
		form.SubmitType = "danger"
		form.Confirm = &CardConfirm{Title: "确认这个操作？", Text: "提交后会马上照你选的做，不能撤回。"}
	}
	return form
}

// ── Engine side ────────────────────────────────────────────────

// SetDecisionStore enables decision cards on this engine and starts the
// deadline loop. defaultHours is used when a request has no deadline.
func (e *Engine) SetDecisionStore(s *DecisionStore, defaultHours int) {
	if defaultHours <= 0 {
		defaultHours = DefaultDecisionHours
	}
	e.decisions = s
	e.decisionDefaultHours = defaultHours
	e.decisionWake = make(chan struct{}, 1)
	go e.decisionLoop()
}

// SendDecisionCard sends a decision card to the session and returns its ID.
func (e *Engine) SendDecisionCard(req DecisionRequest) (string, error) {
	if e.decisions == nil {
		return "", errors.New("decision cards are not enabled")
	}
	d := &Decision{
		ID:         newDecisionID(),
		Project:    e.name,
		SessionKey: req.SessionKey,
		Question:   strings.TrimSpace(req.Question),
		Note:       strings.TrimSpace(req.Note),
		Options:    req.Options,
		HasDefault: !req.NoDefault && !req.Danger && !req.Multi,
		Danger:     req.Danger,
		Multi:      req.Multi,
		State:      decisionPending,
		CreatedAt:  time.Now(),
	}
	if err := d.validate(); err != nil {
		return "", err
	}
	if d.HasDefault {
		mins := req.DeadlineMinutes
		if mins <= 0 {
			mins = e.decisionDefaultHours * 60
		}
		d.Deadline = d.CreatedAt.Add(time.Duration(mins) * time.Minute)
	}

	return e.postDecision(d)
}

// postDecision sends a decision or form card and stores it.
func (e *Engine) postDecision(d *Decision) (string, error) {
	if d.SessionKey == "" {
		return "", errors.New("session_key is required")
	}
	_, p, replyCtx, err := e.resolveOutboundSessionTarget(d.SessionKey, false)
	if err != nil {
		return "", err
	}
	idSender, ok := p.(CardIDSender)
	if !ok {
		return "", fmt.Errorf("platform %s: %w", p.Name(), ErrNotSupported)
	}
	if err := e.waitOutgoing(p); err != nil {
		return "", err
	}
	msgID, err := idSender.SendCardID(e.ctx, replyCtx, e.renderCardForPlatform(p, buildDecisionCard(d)))
	if err != nil {
		return "", err
	}
	d.MessageID = msgID
	if err := e.decisions.Add(d); err != nil {
		return "", err
	}
	e.wakeDecisions()
	return d.ID, nil
}

func (e *Engine) wakeDecisions() {
	select {
	case e.decisionWake <- struct{}{}:
	default:
	}
}

// handleDecisionTap handles "act:/decision <id> <n>" and returns the card that
// replaces the tapped one.
func (e *Engine) handleDecisionTap(args string) *Card {
	if e.decisions == nil {
		cardActionDropped("decision", "", "decisions_disabled")
		return nil
	}
	f := strings.Fields(args)
	if len(f) != 2 {
		cardActionDropped("decision", "", "bad_arguments")
		return nil
	}
	idx, err := strconv.Atoi(f[1])
	if err != nil {
		cardActionDropped("decision", f[0], "bad_arguments")
		return nil
	}
	if d, ok := e.decisions.Resolve(f[0], idx, decisionAnswered); ok {
		e.wakeDecisions() // delivery runs in the loop so it is retried if the session is not ready
		return buildDecisionCard(d)
	}
	if d := e.decisions.Get(f[0]); d != nil {
		return buildDecisionCard(d) // already answered or expired: show how it ended
	}
	cardActionDropped("decision", f[0], "unknown_decision")
	return nil
}

// cardActionDropped logs a decision or form callback that changes nothing.
func cardActionDropped(kind, id, reason string) {
	slog.Info("card: "+kind+"_dropped", "id", id, "reason", reason)
}

// parseFormValues reads the form values a platform appends to a form submit.
func parseFormValues(kind, id, raw string) map[string]any {
	var values map[string]any
	if raw == "" {
		return nil // every field left empty
	}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		cardActionDropped(kind, id, "form_values_unreadable")
	}
	return values
}

// handleDecisionForm handles "act:/decision-form <id> <values>", the submit of
// a decision's dropdown, and returns the card that replaces it.
func (e *Engine) handleDecisionForm(args string) *Card {
	if e.decisions == nil {
		cardActionDropped("decision_form", "", "decisions_disabled")
		return nil
	}
	id, raw, _ := strings.Cut(strings.TrimSpace(args), " ")
	values := parseFormValues("decision_form", id, raw)
	var picks []int
	seen := map[int]bool{}
	add := func(v any) {
		if s, ok := v.(string); ok {
			if n, err := strconv.Atoi(s); err == nil && !seen[n] {
				seen[n] = true
				picks = append(picks, n)
			}
		}
	}
	switch v := values[decisionChoiceField].(type) {
	case []any:
		for _, x := range v {
			add(x)
		}
	default:
		add(v)
	}
	sort.Ints(picks)
	if d, ok := e.decisions.ResolveSet(id, picks, decisionAnswered); ok {
		e.wakeDecisions()
		return buildDecisionCard(d)
	}
	d := e.decisions.Get(id)
	if d == nil {
		cardActionDropped("decision_form", id, "unknown_decision")
		return nil
	}
	if d.State == decisionPending {
		return &Card{Toast: "请先选择"}
	}
	return buildDecisionCard(d)
}

func (e *Engine) decisionLoop() {
	for {
		e.expireDecisions()
		wait := decisionRetryInterval
		if next, ok := e.decisions.nextDeadline(); ok {
			if d := time.Until(next); d < wait {
				wait = d
			}
		}
		if wait < 0 {
			wait = 0
		}
		select {
		case <-e.ctx.Done():
			return
		case <-e.decisionWake:
		case <-time.After(wait):
		}
	}
}

// expireDecisions applies defaults that are due, then delivers every resolved
// decision that has not reached the session yet. A failed delivery stays
// undelivered and is retried on the next pass.
func (e *Engine) expireDecisions() {
	for _, d := range e.decisions.Due(time.Now()) {
		if d.Project != e.name {
			continue
		}
		if r, ok := e.decisions.Resolve(d.ID, 1, decisionExpired); ok {
			e.patchDecisionCard(r)
		}
	}
	for _, d := range e.decisions.Undelivered() {
		if d.Project != e.name {
			continue
		}
		if _, busy := e.decisionInflight.LoadOrStore(d.ID, true); busy {
			continue
		}
		// A turn can run for minutes, so deliver off the loop. The decision is
		// marked delivered before the turn starts (at most once across restarts)
		// and unmarked again if the session could not take it.
		e.decisions.SetDelivered(d.ID, true)
		go func(d *Decision) {
			defer e.decisionInflight.Delete(d.ID)
			if err := e.deliverDecision(d); err != nil {
				slog.Warn("decision: delivery failed, will retry", "id", d.ID, "error", err)
				e.decisions.SetDelivered(d.ID, false)
			}
		}(d)
	}
}

func (e *Engine) patchDecisionCard(d *Decision) {
	if d.MessageID == "" {
		return
	}
	_, p, _, err := e.resolveOutboundSessionTarget(d.SessionKey, false)
	if err != nil {
		slog.Warn("decision: card update skipped", "id", d.ID, "error", err)
		return
	}
	patcher, ok := p.(CardPatcher)
	if !ok {
		return
	}
	if err := patcher.PatchCard(e.ctx, d.MessageID, d.SessionKey, e.renderCardForPlatform(p, buildDecisionCard(d))); err != nil {
		slog.Warn("decision: card update failed", "id", d.ID, "error", err)
	}
}

// deliverDecision hands the answer to the session as a scheduled prompt.
func (e *Engine) deliverDecision(d *Decision) error {
	silent := true
	return e.ExecuteTimerJob(&TimerJob{
		ID:          "decision-" + d.ID,
		Project:     d.Project,
		SessionKey:  d.SessionKey,
		ScheduledAt: d.ResolvedAt,
		Prompt:      d.inboundText(),
		Description: "decision " + d.ID,
		Silent:      &silent,
	})
}
