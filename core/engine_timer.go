package core

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

func (e *Engine) SetTimerScheduler(ts *TimerScheduler) {
	e.timerScheduler = ts
}

// ExecuteTimerJob fires a one-shot timer job: resolves the platform, sends a
// notification (unless muted), and either runs a shell command or injects a
// synthetic message into the agent session.
func (e *Engine) ExecuteTimerJob(job *TimerJob) error {
	done, ok := e.trackWork(false)
	if !ok {
		return fmt.Errorf("engine is shutting down")
	}
	defer done()
	e.hooks.Emit(HookEvent{
		Event:      HookEventTimerTriggered,
		SessionKey: job.SessionKey,
		Content:    job.Prompt,
		Extra:      map[string]any{"job_id": job.ID, "job_description": job.Description},
	})

	sessionKey := job.SessionKey
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
	// Multi-workspace fallback: strip workspace prefix from session key.
	if targetPlatform == nil {
		for _, p := range e.platforms {
			needle := ":" + p.Name() + ":"
			if idx := strings.Index(sessionKey, needle); idx >= 0 {
				targetPlatform = p
				platformName = p.Name()
				sessionKey = sessionKey[idx+1:]
				break
			}
		}
	}
	if targetPlatform == nil {
		return fmt.Errorf("platform %q not found for session %q", platformName, sessionKey)
	}

	rc, ok := targetPlatform.(ReplyContextReconstructor)
	if !ok {
		return fmt.Errorf("platform %q does not support proactive messaging (timer)", platformName)
	}

	runSessionKey := sessionKey
	var replyCtx any
	var err error
	if !job.Mute {
		if resolver, ok := targetPlatform.(CronReplyTargetResolver); ok {
			resolvedSessionKey, resolvedReplyCtx, err := resolver.ResolveCronReplyTarget(sessionKey, timerRunTitle(job))
			if err != nil {
				if !errors.Is(err, ErrNotSupported) {
					return fmt.Errorf("resolve timer reply target: %w", err)
				}
			} else {
				if resolvedSessionKey != "" {
					runSessionKey = resolvedSessionKey
				}
				if resolvedReplyCtx != nil {
					replyCtx = resolvedReplyCtx
				}
			}
		}
	}
	if replyCtx == nil {
		replyCtx, err = rc.ReconstructReplyCtx(runSessionKey)
		if err != nil {
			return fmt.Errorf("reconstruct reply context: %w", err)
		}
	}

	// Notify user unless muted or silent
	if !job.Mute {
		silent := false
		if e.timerScheduler != nil {
			silent = e.timerScheduler.IsSilent(job)
		}
		if !silent {
			desc := job.Description
			if desc == "" {
				if job.IsShellJob() {
					desc = truncateStr(job.Exec, 40)
				} else {
					desc = truncateStr(job.Prompt, 40)
				}
			}
			if notice := timerStartNotice(desc); notice != "" {
				e.send(targetPlatform, replyCtx, notice)
			}
		}
	}

	if job.IsShellJob() {
		shellPlatform := targetPlatform
		if job.Mute {
			shellPlatform = &mutePlatform{targetPlatform}
		}
		return e.executeTimerShell(shellPlatform, replyCtx, job)
	}

	content := job.Prompt
	msg := &Message{
		individualReply:  strings.HasPrefix(strings.TrimSpace(job.Prompt), "/"),
		scheduledPrompt:  true,
		scheduledMute:    job.Mute,
		scheduledInputID: "timer:" + job.ID + ":" + fmt.Sprint(job.ScheduledAt.UnixNano()),
		SessionKey:       sessionKey,
		replySessionKey:  runSessionKey,
		Platform:         platformName,
		UserID:           "timer",
		UserName:         "timer",
		Content:          content,
		ReplyCtx:         replyCtx,
		ModeOverride:     job.Mode,
	}

	agent := e.agent
	sessions := e.sessions
	workspaceDir := ""

	if e.multiWorkspace {
		channelID := extractChannelID(sessionKey)
		if channelID != "" {
			workspace, _, err := e.resolveWorkspace(targetPlatform, channelID)
			if err == nil && workspace != "" {
				wsAgent, wsSessions, _, effectiveDir, err := e.workspaceContext(workspace, sessionKey)
				if err == nil {
					agent = wsAgent
					sessions = wsSessions
					workspaceDir = effectiveDir
				}
			}
		}
	}

	if job.WorkDir != "" {
		wsAgent, wsSessions, err := e.getOrCreateWorkspaceAgent(job.WorkDir)
		if err == nil {
			agent = wsAgent
			sessions = wsSessions
			workspaceDir = job.WorkDir
		} else {
			slog.Warn("timer: workspace agent creation failed, using global",
				"work_dir", job.WorkDir, "session_key", sessionKey, "error", err)
		}
	}

	if strings.HasPrefix(content, "/") {
		parts := strings.Fields(content)
		if len(parts) > 0 {
			cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
			if skill := e.skillsForAgent(agent).Resolve(cmd); skill != nil {
				content = BuildSkillInvocationPrompt(skill, parts[1:])
			}
		}
	}
	msg.Content = content

	useNewSession := false
	if e.timerScheduler != nil {
		useNewSession = e.timerScheduler.UsesNewSession(job)
	} else {
		useNewSession = job.UsesNewSessionPerRun()
	}

	if useNewSession {
		msg.SessionKey = runSessionKey
		session := sessions.NewSideSession(runSessionKey, "timer-"+job.ID)
		lockGen, locked := session.TryLock()
		if !locked {
			return fmt.Errorf("session %q is busy", runSessionKey)
		}
		iKey := fmt.Sprintf("%s#timer:%s", runSessionKey, session.ID)
		if workspaceDir != "" {
			iKey = workspaceDir + ":" + iKey
		}
		e.processInteractiveMessageWith(targetPlatform, msg, session, agent, sessions, iKey, workspaceDir, runSessionKey, lockGen)
		e.cleanupInteractiveState(iKey)
		return nil
	}

	e.assistantAdmissionMu.Lock()
	session := sessions.GetOrCreateActive(sessionKey)
	iKey := sessionKey
	if workspaceDir != "" {
		iKey = workspaceDir + ":" + sessionKey
	}
	queued, lockGen, admissionErr := e.admitScheduledPrompt(targetPlatform, msg, session, sessions, agent, iKey, workspaceDir)
	e.assistantAdmissionMu.Unlock()
	if queued || admissionErr != nil {
		return admissionErr
	}
	e.processInteractiveMessageWith(targetPlatform, msg, session, agent, sessions, iKey, workspaceDir, sessionKey, lockGen)
	return nil
}

func timerRunTitle(job *TimerJob) string {
	if job == nil {
		return "timer"
	}
	if job.Description != "" {
		return job.Description
	}
	if job.IsShellJob() {
		return truncateStr(job.Exec, 40)
	}
	return truncateStr(job.Prompt, 40)
}

func (e *Engine) renderTimerCard(sessionKey string, userID string) *Card {
	if e.timerScheduler == nil {
		return e.simpleCard(e.i18n.T(MsgCardTitleTimer), "blue", e.i18n.T(MsgTimerNotAvailable))
	}

	jobs := e.timerScheduler.Store().ListPending()
	// Filter to current session
	var filtered []*TimerJob
	for _, j := range jobs {
		if j.SessionKey == sessionKey {
			filtered = append(filtered, j)
		}
	}
	if len(filtered) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleTimer), "blue", e.i18n.T(MsgTimerEmpty))
	}

	cb := NewCard().Title(e.i18n.T(MsgCardTitleTimer), "blue")
	cb.Markdown(fmt.Sprintf(e.i18n.T(MsgTimerListTitle), len(filtered)))

	for _, j := range filtered {
		desc := j.Description
		if desc == "" {
			if j.IsShellJob() {
				desc = "🖥 " + truncateStr(j.Exec, 60)
			} else {
				desc = truncateStr(j.Prompt, 60)
			}
		}
		if j.Mute {
			desc += " [mute]"
		}

		remaining := FormatTimerRemaining(j.ScheduledAt)

		var sb strings.Builder
		fmt.Fprintf(&sb, "⏰ %s\n", desc)
		sb.WriteString(e.i18n.Tf(MsgTimerIDLabel, j.ID))
		sb.WriteString(e.i18n.Tf(MsgTimerScheduledLabel, j.ScheduledAt.Format("2006-01-02 15:04"), remaining))
		sb.WriteString(e.i18n.Tf(MsgTimerBeijingTime, FormatTimerBeijingTime(j.ScheduledAt)))
		sb.WriteString("\n")
		if j.LastError != "" {
			sb.WriteString(e.i18n.Tf(MsgTimerFailedSuffix, truncateStr(j.LastError, 40)))
		}
		cb.Markdown(sb.String())

		var btns []CardButton
		if j.Mute {
			btns = append(btns, DefaultBtn(e.i18n.T(MsgTimerBtnUnmute), fmt.Sprintf("act:/timer unmute %s", j.ID)))
		} else {
			btns = append(btns, DefaultBtn(e.i18n.T(MsgTimerBtnMute), fmt.Sprintf("act:/timer mute %s", j.ID)))
		}
		btns = append(btns, DangerBtn(e.i18n.T(MsgTimerBtnDelete), fmt.Sprintf("act:/timer delete %s", j.ID)))
		cb.ButtonsEqual(btns...)
	}

	cb.Divider()
	cb.Note(e.i18n.T(MsgTimerCardHint))
	cb.Buttons(e.cardBackButton())
	return cb.Build()
}

// ──────────────────────────────────────────────────────────────
// /timer command
// ──────────────────────────────────────────────────────────────

func (e *Engine) cmdTimer(p Platform, msg *Message, args []string) {
	if e.timerScheduler == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerNotAvailable))
		return
	}

	if len(args) == 0 {
		if !supportsCards(p) {
			e.cmdTimerList(p, msg)
			return
		}
		e.replyWithCard(p, msg.ReplyCtx, e.renderTimerCard(msg.SessionKey, msg.UserID))
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{
		"add", "addexec", "list", "del", "delete", "rm", "remove", "mute", "unmute",
	})
	switch sub {
	case "add":
		e.cmdTimerAdd(p, msg, args[1:])
	case "addexec":
		e.cmdTimerAddExec(p, msg, args[1:])
	case "list":
		e.cmdTimerList(p, msg)
	case "del", "delete", "rm", "remove":
		e.cmdTimerDel(p, msg, args[1:])
	case "mute":
		e.cmdTimerMute(p, msg, args[1:], true)
	case "unmute":
		e.cmdTimerMute(p, msg, args[1:], false)
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerUsage))
	}
}

func (e *Engine) cmdTimerAdd(p Platform, msg *Message, args []string) {
	// /timer add <delay_or_time> <prompt...>
	if len(args) < 2 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerAddUsage))
		return
	}

	fireAt, err := ParseDelayOrTime(args[0])
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
		return
	}

	prompt := strings.Join(args[1:], " ")

	job := &TimerJob{
		ID:          GenerateTimerID(),
		Project:     e.name,
		SessionKey:  msg.SessionKey,
		ScheduledAt: fireAt,
		Prompt:      prompt,
		CreatedAt:   time.Now(),
	}

	if err := e.timerScheduler.AddJob(job); err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
		return
	}

	remaining := FormatTimerRemaining(fireAt)
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTimerAdded), job.ID, remaining, truncateStr(prompt, 60))+"\n"+e.i18n.Tf(MsgTimerBeijingTime, FormatTimerBeijingTime(fireAt)))
}

func (e *Engine) cmdTimerAddExec(p Platform, msg *Message, args []string) {
	if !e.isAdmin(msg.UserID) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAdminRequired), "/timer addexec"))
		return
	}

	// /timer addexec <delay_or_time> <shell command...>
	if len(args) < 2 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerAddExecUsage))
		return
	}

	fireAt, err := ParseDelayOrTime(args[0])
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
		return
	}

	shellCmd := strings.Join(args[1:], " ")

	job := &TimerJob{
		ID:          GenerateTimerID(),
		Project:     e.name,
		SessionKey:  msg.SessionKey,
		ScheduledAt: fireAt,
		Exec:        shellCmd,
		CreatedAt:   time.Now(),
	}

	if err := e.timerScheduler.AddJob(job); err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
		return
	}

	remaining := FormatTimerRemaining(fireAt)
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTimerAddedExec), job.ID, remaining, truncateStr(shellCmd, 60))+"\n"+e.i18n.Tf(MsgTimerBeijingTime, FormatTimerBeijingTime(fireAt)))
}

func (e *Engine) cmdTimerList(p Platform, msg *Message) {
	jobs := e.timerScheduler.Store().ListPending()
	// Filter to current session
	var filtered []*TimerJob
	for _, j := range jobs {
		if j.SessionKey == msg.SessionKey {
			filtered = append(filtered, j)
		}
	}
	if len(filtered) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerEmpty))
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, e.i18n.T(MsgTimerListTitle), len(filtered))
	sb.WriteString("\n")

	for i, j := range filtered {
		if i > 0 {
			sb.WriteString("\n")
		}
		desc := j.Description
		if desc == "" {
			if j.IsShellJob() {
				desc = "🖥 " + truncateStr(j.Exec, 60)
			} else {
				desc = truncateStr(j.Prompt, 60)
			}
		}
		if j.Mute {
			desc += " [mute]"
		}
		remaining := FormatTimerRemaining(j.ScheduledAt)
		fmt.Fprintf(&sb, "⏰ %s (%s)\n", desc, remaining)
		fmt.Fprintf(&sb, "ID: %s\n", j.ID)
		fmt.Fprintf(&sb, "%s\n", e.i18n.Tf(MsgTimerBeijingTime, FormatTimerBeijingTime(j.ScheduledAt)))
	}

	fmt.Fprintf(&sb, "\n%s", e.i18n.T(MsgTimerListFooter))
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdTimerDel(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerDelUsage))
		return
	}
	id := args[0]
	if !e.timerScheduler.RemoveJob(id) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerNotFound))
		return
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTimerDeleted), id))
}

func (e *Engine) cmdTimerMute(p Platform, msg *Message, args []string, mute bool) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerMuteUsage))
		return
	}
	id := args[0]
	if !e.timerScheduler.SetMute(id, mute) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTimerNotFound))
		return
	}
	if mute {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTimerMuted), id))
	} else {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgTimerUnmuted), id))
	}
}
