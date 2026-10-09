package core

import (
	"fmt"
	"strconv"
	"strings"
)

func (e *Engine) SetHeartbeatScheduler(hs *HeartbeatScheduler) {
	e.heartbeatScheduler = hs
}

// ExecuteHeartbeat runs a heartbeat check by injecting a synthetic message
// into the main session, similar to cron but designed for periodic awareness.
func (e *Engine) ExecuteHeartbeat(sessionKey, prompt string, silent bool) error {
	platformName := ""
	if idx := strings.Index(sessionKey, ":"); idx > 0 {
		platformName = sessionKey[:idx]
	}

	var targetPlatform Platform
	for _, p := range e.platforms {
		if p.Name() == platformName {
			targetPlatform = p
			break
		}
	}
	// Fallback: in multi-workspace mode the stored session key may be prefixed
	// with the workspace path (e.g. "/home/user/project:slack:C123:U456").
	// Search for a known platform name within the key and strip the prefix.
	if targetPlatform == nil {
		for _, p := range e.platforms {
			needle := ":" + p.Name() + ":"
			if idx := strings.Index(sessionKey, needle); idx >= 0 {
				targetPlatform = p
				platformName = p.Name()
				sessionKey = sessionKey[idx+1:] // strip workspace prefix
				break
			}
		}
	}
	if targetPlatform == nil {
		return fmt.Errorf("platform %q not found for session %q", platformName, sessionKey)
	}

	rc, ok := targetPlatform.(ReplyContextReconstructor)
	if !ok {
		return fmt.Errorf("platform %q does not support proactive messaging (heartbeat)", platformName)
	}

	replyCtx, err := rc.ReconstructReplyCtx(sessionKey)
	if err != nil {
		return fmt.Errorf("reconstruct reply context: %w", err)
	}

	if !silent {
		e.send(targetPlatform, replyCtx, "💓 heartbeat")
	}

	msg := &Message{
		SessionKey: sessionKey,
		Platform:   platformName,
		UserID:     "heartbeat",
		UserName:   "heartbeat",
		Content:    prompt,
		ReplyCtx:   replyCtx,
	}

	session := e.sessions.GetOrCreateActive(sessionKey)
	if !session.TryLock() {
		return fmt.Errorf("session %q is busy", sessionKey)
	}

	e.processInteractiveMessage(targetPlatform, msg, session)
	return nil
}

// ──────────────────────────────────────────────────────────────
// Heartbeat management commands
// ──────────────────────────────────────────────────────────────

func (e *Engine) cmdHeartbeat(p Platform, msg *Message, args []string) {
	if e.heartbeatScheduler == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHeartbeatNotAvailable))
		return
	}

	status := e.heartbeatScheduler.Status(e.name)
	if status == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHeartbeatNotAvailable))
		return
	}

	sub := "status"
	if len(args) > 0 {
		sub = matchSubCommand(strings.ToLower(args[0]), []string{
			"status", "pause", "stop", "resume", "start", "run", "trigger", "interval",
		})
	}

	switch sub {
	case "status", "":
		if supportsCards(p) {
			e.replyWithCard(p, msg.ReplyCtx, e.renderHeartbeatCard())
			return
		}
		e.cmdHeartbeatStatusText(p, msg, status)
	case "pause", "stop":
		e.heartbeatScheduler.Pause(e.name)
		if supportsCards(p) {
			e.replyWithCard(p, msg.ReplyCtx, e.renderHeartbeatCard())
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHeartbeatPaused))
		}
	case "resume", "start":
		e.heartbeatScheduler.Resume(e.name)
		if supportsCards(p) {
			e.replyWithCard(p, msg.ReplyCtx, e.renderHeartbeatCard())
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHeartbeatResumed))
		}
	case "run", "trigger":
		e.heartbeatScheduler.TriggerNow(e.name)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHeartbeatTriggered))
	case "interval":
		if len(args) < 2 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHeartbeatUsage))
			return
		}
		mins, err := strconv.Atoi(args[1])
		if err != nil || mins <= 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHeartbeatInvalidMins))
			return
		}
		e.heartbeatScheduler.SetInterval(e.name, mins)
		if supportsCards(p) {
			e.replyWithCard(p, msg.ReplyCtx, e.renderHeartbeatCard())
		} else {
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgHeartbeatInterval), mins))
		}
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgHeartbeatUsage))
	}
}

func (e *Engine) cmdHeartbeatStatusText(p Platform, msg *Message, st *HeartbeatStatus) {
	stateStr, yesNo := e.heartbeatLocalizedHelpers()

	lastRunStr := ""
	if !st.LastRun.IsZero() {
		lang := e.i18n.CurrentLang()
		switch lang {
		case LangChinese, LangTraditionalChinese:
			lastRunStr = "上次执行: " + st.LastRun.Format("01-02 15:04:05") + "\n"
		case LangJapanese:
			lastRunStr = "最終実行: " + st.LastRun.Format("01-02 15:04:05") + "\n"
		default:
			lastRunStr = "Last run: " + st.LastRun.Format("01-02 15:04:05") + "\n"
		}
		if st.LastError != "" {
			lastRunStr += "⚠️ " + truncateStr(st.LastError, 80) + "\n"
		}
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgHeartbeatStatus),
		stateStr(st.Paused),
		st.IntervalMins,
		yesNo(st.OnlyWhenIdle),
		yesNo(st.Silent),
		st.RunCount,
		st.ErrorCount,
		st.SkippedBusy,
		lastRunStr,
	))
}

func (e *Engine) heartbeatLocalizedHelpers() (stateStr func(paused bool) string, yesNo func(bool) string) {
	lang := e.i18n.CurrentLang()
	switch lang {
	case LangChinese, LangTraditionalChinese:
		stateStr = func(paused bool) string {
			if paused {
				return "⏸ 已暂停"
			}
			return "▶️ 运行中"
		}
		yesNo = func(b bool) string {
			if b {
				return "是"
			}
			return "否"
		}
	case LangJapanese:
		stateStr = func(paused bool) string {
			if paused {
				return "⏸ 一時停止"
			}
			return "▶️ 実行中"
		}
		yesNo = func(b bool) string {
			if b {
				return "はい"
			}
			return "いいえ"
		}
	default:
		stateStr = func(paused bool) string {
			if paused {
				return "⏸ paused"
			}
			return "▶️ running"
		}
		yesNo = func(b bool) string {
			if b {
				return "yes"
			}
			return "no"
		}
	}
	return
}

func (e *Engine) renderHeartbeatCard() *Card {
	if e.heartbeatScheduler == nil {
		return e.simpleCard(e.i18n.T(MsgCardTitleHeartbeat), "purple", e.i18n.T(MsgHeartbeatNotAvailable))
	}
	st := e.heartbeatScheduler.Status(e.name)
	if st == nil {
		return e.simpleCard(e.i18n.T(MsgCardTitleHeartbeat), "purple", e.i18n.T(MsgHeartbeatNotAvailable))
	}

	stateStr, yesNo := e.heartbeatLocalizedHelpers()
	lang := e.i18n.CurrentLang()

	lastRunStr := ""
	if !st.LastRun.IsZero() {
		switch lang {
		case LangChinese, LangTraditionalChinese:
			lastRunStr = "上次执行: " + st.LastRun.Format("01-02 15:04:05") + "\n"
		case LangJapanese:
			lastRunStr = "最終実行: " + st.LastRun.Format("01-02 15:04:05") + "\n"
		default:
			lastRunStr = "Last run: " + st.LastRun.Format("01-02 15:04:05") + "\n"
		}
		if st.LastError != "" {
			lastRunStr += "⚠️ " + truncateStr(st.LastError, 80) + "\n"
		}
	}

	body := fmt.Sprintf(e.i18n.T(MsgHeartbeatStatus),
		stateStr(st.Paused),
		st.IntervalMins,
		yesNo(st.OnlyWhenIdle),
		yesNo(st.Silent),
		st.RunCount,
		st.ErrorCount,
		st.SkippedBusy,
		lastRunStr,
	)

	cb := NewCard().Title(e.i18n.T(MsgCardTitleHeartbeat), "purple").Markdown(body)

	var actionBtns []CardButton
	if st.Paused {
		actionBtns = append(actionBtns, PrimaryBtn("▶️ Resume", "act:/heartbeat resume"))
	} else {
		actionBtns = append(actionBtns, DefaultBtn("⏸ Pause", "act:/heartbeat pause"))
	}
	actionBtns = append(actionBtns, DefaultBtn("💓 Run Now", "act:/heartbeat run"))
	cb.Buttons(actionBtns...)

	cb.Buttons(e.cardBackButton())

	return cb.Build()
}
