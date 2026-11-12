package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// The first-run welcome is one private message from a bot that has never had
// a conversation to its single owner. It is sent once per data directory,
// on a working day in Beijing working hours.

const (
	welcomeMaxAttempts = 3
	welcomeMaxJitter   = 20 * time.Minute
)

var (
	beijing = time.FixedZone("CST", 8*3600)
	// welcomeStartDelay lets the private-message catchup and the engine settle
	// before an in-window welcome goes out.
	welcomeStartDelay = time.Minute
)

// welcomer sends the first-run welcome. Its clock, wait, jitter and send are
// fields so tests can drive a whole schedule without waiting.
type welcomer struct {
	tag          string
	text         func() string
	markerPath   string
	attemptsPath string
	chatted      func() (bool, error)
	send         func(ctx context.Context, text string) error
	now          func() time.Time
	waitUntil    func(ctx context.Context, t time.Time) bool
	jitter       func() time.Duration
}

// welcomeOwner returns the single owner open_id in allow_from, or the reason
// there is none.
func welcomeOwner(allowFrom string) (string, string) {
	allowFrom = strings.TrimSpace(allowFrom)
	if allowFrom == "" || allowFrom == "*" {
		return "", "no_owner"
	}
	var ids []string
	for _, id := range strings.Split(allowFrom, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	switch {
	case len(ids) == 0 || (len(ids) == 1 && ids[0] == "*"):
		return "", "no_owner"
	case len(ids) > 1:
		return "", "multiple_owners"
	case !strings.HasPrefix(ids[0], "ou_"):
		return "", "owner_not_open_id"
	}
	return ids[0], ""
}

func (p *Platform) configureWelcome(opts map[string]any) {
	if enabled, ok := opts["welcome"].(bool); ok && !enabled {
		return
	}
	dataDir, _ := opts["cc_data_dir"].(string)
	if dataDir == "" {
		return
	}
	owner, reason := welcomeOwner(p.allowFrom)
	if owner == "" {
		slog.Info("welcome: skipped", "platform", p.tag(), "reason", reason)
		return
	}
	custom, _ := opts["welcome_message"].(string)
	custom = strings.TrimSpace(custom)
	sessionStore, _ := opts["cc_session_store"].(string)
	project, _ := opts["cc_project"].(string)
	dir := filepath.Join(dataDir, "feishu")
	p.welcome = &welcomer{
		tag:          p.tag(),
		markerPath:   filepath.Join(dir, "welcome-sent"),
		attemptsPath: filepath.Join(dir, "welcome-attempts"),
		text: func() string {
			if custom != "" {
				return custom
			}
			return p.welcomeI18n().T(core.MsgFirstRunWelcome)
		},
		chatted: func() (bool, error) { return p.hasConversation(sessionStore, project) },
		send: func(ctx context.Context, text string) error {
			return p.sendToOpenID(ctx, owner, text)
		},
		now:       time.Now,
		waitUntil: sleepUntil,
		jitter:    func() time.Duration { return rand.N(welcomeMaxJitter) },
	}
}

// SetI18n implements core.I18nReceiver.
func (p *Platform) SetI18n(i *core.I18n) { p.i18n.Store(i) }

func (p *Platform) welcomeI18n() *core.I18n {
	if i := p.i18n.Load(); i != nil {
		return i
	}
	if p.domain == lark.LarkBaseUrl {
		return core.NewI18n(core.LangEnglish)
	}
	return core.NewI18n(core.LangChinese)
}

func (p *Platform) startWelcome() {
	if p.welcome == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.welcomeCancel = cancel
	go p.welcome.run(ctx)
}

func (p *Platform) stopWelcome() {
	if p.welcomeCancel != nil {
		p.welcomeCancel()
	}
}

// hasConversation reports whether this bot has ever talked to anyone: a
// session in the project's records with history, or a confirmed private chat.
func (p *Platform) hasConversation(sessionStore, project string) (bool, error) {
	if c := p.catchup; c != nil {
		c.mu.Lock()
		n := len(c.snapshot.PrivateChats)
		c.mu.Unlock()
		if n > 0 {
			return true, nil
		}
	}
	if sessionStore == "" {
		return false, nil
	}
	paths := []string{sessionStore}
	if project != "" {
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(sessionStore), project+"_ws_????????.json"))
		if err != nil {
			return false, fmt.Errorf("find workspace session records: %w", err)
		}
		paths = append(paths, matches...)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read session records: %w", err)
		}
		var records struct {
			Sessions map[string]struct {
				History []json.RawMessage `json:"history"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(data, &records); err != nil {
			return false, fmt.Errorf("decode session records: %w", err)
		}
		for _, s := range records.Sessions {
			if len(s.History) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// sendToOpenID sends a private text message to a user by open_id.
func (p *Platform) sendToOpenID(ctx context.Context, openID, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("%s: welcome: encode: %w", p.tag(), err)
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeOpenId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(openID).
			MsgType(larkim.MsgTypeText).
			Content(string(content)).
			Build()).
		Build()
	return p.withFreshTenantAccessTokenRetry(ctx, "welcome", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
		resp, err := client.Im.Message.Create(ctx, req, options...)
		if err != nil {
			return fmt.Errorf("%s: welcome api call: %w", p.tag(), err)
		}
		if !resp.Success() {
			return fmt.Errorf("%s: welcome failed code=%d msg=%s", p.tag(), resp.Code, resp.Msg)
		}
		return nil
	})
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// inWelcomeWindow reports whether t falls on Monday to Friday, 09:30 to
// 19:30 Beijing time.
func inWelcomeWindow(t time.Time) bool {
	t = t.In(beijing)
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	return m >= 9*60+30 && m < 19*60+30
}

// nextWelcomeOpening returns the next 09:30 Beijing time on a weekday,
// strictly after t.
func nextWelcomeOpening(t time.Time) time.Time {
	t = t.In(beijing)
	day := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, beijing)
	if !day.After(t) {
		day = day.AddDate(0, 0, 1)
	}
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, 1)
	}
	return day
}

type welcomeOutcome int

const (
	welcomeDone welcomeOutcome = iota
	welcomeLater
)

func (w *welcomer) run(ctx context.Context) {
	if done, reason := w.alreadyDone(); done {
		slog.Info("welcome: skipped", "platform", w.tag, "reason", reason)
		return
	}
	now := w.now()
	at := now.Add(welcomeStartDelay)
	if !inWelcomeWindow(now) {
		at = nextWelcomeOpening(now).Add(w.jitter())
		slog.Info("welcome: scheduled", "platform", w.tag, "at", at.In(beijing).Format(time.RFC3339))
	}
	for {
		if !w.waitUntil(ctx, at) {
			return
		}
		now := w.now()
		if !inWelcomeWindow(now) {
			at = nextWelcomeOpening(now).Add(w.jitter())
			continue
		}
		if w.attempt(ctx) == welcomeDone {
			return
		}
		at = nextWelcomeOpening(w.now()).Add(w.jitter())
		slog.Info("welcome: scheduled", "platform", w.tag, "at", at.In(beijing).Format(time.RFC3339))
	}
}

// alreadyDone reports whether the welcome is no longer due before anything is
// scheduled.
func (w *welcomer) alreadyDone() (bool, string) {
	if _, err := os.Stat(w.markerPath); err == nil {
		return true, "already_sent"
	}
	if w.attempts() >= welcomeMaxAttempts {
		return true, "attempts_exhausted"
	}
	chatted, err := w.chatted()
	if err != nil {
		slog.Warn("welcome: cannot read conversation records", "platform", w.tag, "error", err)
		return true, "records_unreadable"
	}
	if chatted {
		return true, "has_conversation"
	}
	return false, ""
}

// attempt sends the welcome once. The marker is written before the send, so
// a crash during the send never leads to a second welcome.
func (w *welcomer) attempt(ctx context.Context) welcomeOutcome {
	if done, reason := w.alreadyDone(); done {
		slog.Info("welcome: skipped", "platform", w.tag, "reason", reason)
		return welcomeDone
	}
	if err := os.MkdirAll(filepath.Dir(w.markerPath), 0o755); err != nil {
		slog.Warn("welcome: cannot create data directory", "platform", w.tag, "error", err)
		return welcomeDone
	}
	f, err := os.OpenFile(w.markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		// Another project on the same bot sent it.
		slog.Info("welcome: skipped", "platform", w.tag, "reason", "already_sent")
		return welcomeDone
	}
	if err != nil {
		slog.Warn("welcome: cannot write marker", "platform", w.tag, "error", err)
		return welcomeDone
	}
	_, _ = fmt.Fprintf(f, "sent %s\n", w.now().In(beijing).Format(time.RFC3339))
	_ = f.Close()

	n := w.attempts() + 1
	if err := os.WriteFile(w.attemptsPath, []byte(strconv.Itoa(n)+"\n"), 0o644); err != nil {
		slog.Warn("welcome: cannot record attempt", "platform", w.tag, "error", err)
		return welcomeDone
	}
	err = w.send(ctx, w.text())
	if err == nil {
		slog.Info("welcome: sent", "platform", w.tag, "attempt", n)
		return welcomeDone
	}
	if n >= welcomeMaxAttempts {
		slog.Warn("welcome: send failed; giving up", "platform", w.tag, "attempt", n, "error", err)
		return welcomeDone
	}
	slog.Warn("welcome: send failed; retrying at the next window", "platform", w.tag, "attempt", n, "error", err)
	if rmErr := os.Remove(w.markerPath); rmErr != nil {
		slog.Warn("welcome: cannot remove marker; not retrying", "platform", w.tag, "error", rmErr)
		return welcomeDone
	}
	return welcomeLater
}

func (w *welcomer) attempts() int {
	data, err := os.ReadFile(w.attemptsPath)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}
