package core

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

func (e *Engine) SetCronScheduler(cs *CronScheduler) {
	e.cronScheduler = cs
}

// ExecuteCronJob runs a cron job by injecting a synthetic message into the engine.
// It finds the platform that owns the session key, reconstructs a reply context,
// and processes the message as if the user sent it.
func (e *Engine) ExecuteCronJob(job *CronJob) error {
	done, ok := e.trackWork(false)
	if !ok {
		return fmt.Errorf("engine is shutting down")
	}
	defer done()
	e.hooks.Emit(HookEvent{
		Event:      HookEventCronTriggered,
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
		return fmt.Errorf("platform %q does not support proactive messaging (cron)", platformName)
	}

	runSessionKey := sessionKey
	var replyCtx any
	var err error
	if !job.Mute {
		if resolver, ok := targetPlatform.(CronReplyTargetResolver); ok {
			resolvedSessionKey, resolvedReplyCtx, err := resolver.ResolveCronReplyTarget(sessionKey, cronRunTitle(job))
			if err != nil {
				if !errors.Is(err, ErrNotSupported) {
					return fmt.Errorf("resolve cron reply target: %w", err)
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

	// Notify user that a cron job is executing (unless silent/muted)
	// Note: this notification uses targetPlatform directly, not the tracking wrapper,
	// so it won't count as a "meaningful delivery" for empty response detection.
	if !job.Mute {
		silent := false
		if e.cronScheduler != nil {
			silent = e.cronScheduler.IsSilent(job)
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
			e.send(targetPlatform, replyCtx, scheduledStartNotice(desc))
		}
	}

	if job.IsShellJob() {
		shellPlatform := targetPlatform
		if job.Mute {
			shellPlatform = &mutePlatform{targetPlatform}
		}
		return e.executeCronShell(shellPlatform, replyCtx, job)
	}

	content := job.Prompt
	if strings.HasPrefix(content, "/") {
		parts := strings.Fields(content)
		if len(parts) > 0 {
			cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
			if skill := e.skills.Resolve(cmd); skill != nil {
				content = BuildSkillInvocationPrompt(skill, parts[1:])
			}
		}
	}

	msg := &Message{
		individualReply:  strings.HasPrefix(strings.TrimSpace(job.Prompt), "/"),
		scheduledPrompt:  true,
		scheduledMute:    job.Mute,
		scheduledInputID: "cron:" + job.ID + ":" + fmt.Sprint(time.Now().UnixNano()),
		SessionKey:       sessionKey,
		replySessionKey:  runSessionKey,
		Platform:         platformName,
		UserID:           "cron",
		UserName:         "cron",
		Content:          content,
		ReplyCtx:         replyCtx,
		ModeOverride:     job.Mode,
	}

	// Resolve workspace-specific agent and sessions for multi-workspace mode.
	// Priority: job.WorkDir (explicit) > workspace binding > global agent fallback.
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
			slog.Warn("cron: workspace agent creation failed, using global",
				"work_dir", job.WorkDir, "session_key", sessionKey, "error", err)
		}
	}

	useNewSession := false
	if e.cronScheduler != nil {
		useNewSession = e.cronScheduler.UsesNewSession(job)
	} else {
		useNewSession = job.UsesNewSessionPerRun()
	}

	if useNewSession {
		msg.SessionKey = runSessionKey
		session := sessions.NewSideSession(runSessionKey, "cron-"+job.ID)
		if !session.TryLock() {
			return fmt.Errorf("session %q is busy", runSessionKey)
		}
		iKey := fmt.Sprintf("%s#cron:%s", runSessionKey, session.ID)
		if workspaceDir != "" {
			iKey = workspaceDir + ":" + iKey
		}
		prevHistLen := session.HistoryLen()
		e.processInteractiveMessageWith(targetPlatform, msg, session, agent, sessions, iKey, workspaceDir, runSessionKey)
		e.cleanupInteractiveState(iKey)
		// Empty-response detection via session history delta: processInteractiveMessageWith
		// always adds a "user" entry (prevHistLen+1), then an "assistant" entry on success
		// (prevHistLen+2). This approach correctly detects empty responses across all
		// delivery modes (plain text, cards, rich cards, DingTalk AI streaming) because
		// AddHistory("assistant",...) is called before any platform-specific rendering path.
		if !job.Mute && session.HistoryLen() < prevHistLen+2 {
			return fmt.Errorf("cron job %q produced an empty response", job.ID)
		}
		return nil
	}

	e.assistantAdmissionMu.Lock()
	session := sessions.GetOrCreateActive(sessionKey)
	iKey := sessionKey
	if workspaceDir != "" {
		iKey = workspaceDir + ":" + sessionKey
	}
	queued, admissionErr := e.admitScheduledPrompt(targetPlatform, msg, session, sessions, agent, iKey, workspaceDir)
	e.assistantAdmissionMu.Unlock()
	if queued || admissionErr != nil {
		return admissionErr
	}
	prevHistLen := session.HistoryLen()
	e.processInteractiveMessageWith(targetPlatform, msg, session, agent, sessions, iKey, workspaceDir, sessionKey)
	// Same empty-response detection as the useNewSession path above.
	if !job.Mute && session.HistoryLen() < prevHistLen+2 {
		return fmt.Errorf("cron job %q produced an empty response", job.ID)
	}
	return nil
}

func cronRunTitle(job *CronJob) string {
	if job == nil {
		return "cron"
	}
	if desc := strings.TrimSpace(job.Description); desc != "" {
		return truncateStr(desc, 60)
	}
	if job.IsShellJob() {
		if cmd := strings.TrimSpace(job.Exec); cmd != "" {
			return truncateStr(cmd, 60)
		}
		return "cron"
	}
	if prompt := strings.TrimSpace(job.Prompt); prompt != "" {
		return truncateStr(prompt, 60)
	}
	return "cron"
}

func (e *Engine) renderCronCard(sessionKey string, userID string) *Card {
	if e.cronScheduler == nil {
		return e.simpleCard(e.i18n.T(MsgCardTitleCron), "orange", e.i18n.T(MsgCronNotAvailable))
	}

	jobs := e.cronScheduler.Store().ListBySessionKey(sessionKey)
	if len(jobs) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleCron), "orange", e.i18n.T(MsgCronEmpty))
	}

	lang := e.i18n.CurrentLang()
	now := time.Now()

	cb := NewCard().Title(e.i18n.T(MsgCardTitleCron), "orange")
	cb.Markdown(fmt.Sprintf(e.i18n.T(MsgCronListTitle), len(jobs)))

	for _, j := range jobs {
		status := "✅"
		if !j.Enabled {
			status = "⏸"
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

		human := CronExprToHuman(j.CronExpr, lang)

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%s %s\n", status, desc))
		sb.WriteString(e.i18n.Tf(MsgCronIDLabel, j.ID))
		sb.WriteString(e.i18n.Tf(MsgCronScheduleLabel, human, j.CronExpr))
		nextRun := e.cronScheduler.NextRun(j.ID)
		if !nextRun.IsZero() {
			fmtStr := cronTimeFormat(nextRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronNextRunLabel, nextRun.Format(fmtStr)))
		}
		if !j.LastRun.IsZero() {
			fmtStr := cronTimeFormat(j.LastRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronLastRunLabel, j.LastRun.Format(fmtStr)))
			if j.LastError != "" {
				sb.WriteString(e.i18n.Tf(MsgCronFailedSuffix, truncateStr(j.LastError, 40)))
			}
			sb.WriteString("\n")
		}
		cb.Markdown(sb.String())

		var btns []CardButton
		if j.Enabled {
			btns = append(btns, DefaultBtn(e.i18n.T(MsgCronBtnDisable), fmt.Sprintf("act:/cron disable %s", j.ID)))
		} else {
			btns = append(btns, PrimaryBtn(e.i18n.T(MsgCronBtnEnable), fmt.Sprintf("act:/cron enable %s", j.ID)))
		}
		if j.Mute {
			btns = append(btns, DefaultBtn(e.i18n.T(MsgCronBtnUnmute), fmt.Sprintf("act:/cron unmute %s", j.ID)))
		} else {
			btns = append(btns, DefaultBtn(e.i18n.T(MsgCronBtnMute), fmt.Sprintf("act:/cron mute %s", j.ID)))
		}
		btns = append(btns, DangerBtn(e.i18n.T(MsgCronBtnDelete), fmt.Sprintf("act:/cron delete %s", j.ID)))
		cb.ButtonsEqual(btns...)
	}

	cb.Divider()
	cb.Note(e.i18n.T(MsgCronCardHint))
	cb.Buttons(e.cardBackButton())
	return cb.Build()
}

// ──────────────────────────────────────────────────────────────
// /cron command
// ──────────────────────────────────────────────────────────────

func (e *Engine) cmdCron(p Platform, msg *Message, args []string) {
	if e.cronScheduler == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronNotAvailable))
		return
	}

	if len(args) == 0 {
		if !supportsCards(p) {
			e.cmdCronList(p, msg)
			return
		}
		e.replyWithCard(p, msg.ReplyCtx, e.renderCronCard(msg.SessionKey, msg.UserID))
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{
		"add", "addexec", "list", "exec", "run", "trigger", "del", "delete", "rm", "remove", "enable", "disable", "mute", "unmute", "setup",
	})
	switch sub {
	case "add":
		e.cmdCronAdd(p, msg, args[1:])
	case "addexec":
		e.cmdCronAddExec(p, msg, args[1:])
	case "list":
		e.cmdCronList(p, msg)
	case "exec", "run", "trigger":
		e.cmdCronExec(p, msg, args[1:])
	case "del", "delete", "rm", "remove":
		e.cmdCronDel(p, msg, args[1:])
	case "enable":
		e.cmdCronToggle(p, msg, args[1:], true)
	case "disable":
		e.cmdCronToggle(p, msg, args[1:], false)
	case "mute":
		e.cmdCronMute(p, msg, args[1:], true)
	case "unmute":
		e.cmdCronMute(p, msg, args[1:], false)
	case "setup":
		e.cmdCronSetup(p, msg)
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronUsage))
	}
}

func (e *Engine) cmdCronAdd(p Platform, msg *Message, args []string) {
	// /cron add <min> <hour> <day> <month> <weekday> <prompt...>
	if len(args) < 6 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronAddUsage))
		return
	}

	cronExpr := strings.Join(args[:5], " ")
	prompt := strings.Join(args[5:], " ")

	job := &CronJob{
		ID:         GenerateCronID(),
		Project:    e.name,
		SessionKey: msg.SessionKey,
		CronExpr:   cronExpr,
		Prompt:     prompt,
		Enabled:    true,
		CreatedAt:  time.Now(),
	}

	if err := e.cronScheduler.AddJob(job); err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
		return
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronAdded), job.ID, cronExpr, truncateStr(prompt, 60)))
}

func (e *Engine) cmdCronAddExec(p Platform, msg *Message, args []string) {
	if !e.isAdmin(msg.UserID) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAdminRequired), "/cron addexec"))
		return
	}

	// /cron addexec <min> <hour> <day> <month> <weekday> <shell command...>
	if len(args) < 6 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronAddExecUsage))
		return
	}

	cronExpr := strings.Join(args[:5], " ")
	shellCmd := strings.Join(args[5:], " ")

	job := &CronJob{
		ID:         GenerateCronID(),
		Project:    e.name,
		SessionKey: msg.SessionKey,
		CronExpr:   cronExpr,
		Exec:       shellCmd,
		Enabled:    true,
		CreatedAt:  time.Now(),
	}

	if err := e.cronScheduler.AddJob(job); err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
		return
	}

	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronAddedExec), job.ID, cronExpr, truncateStr(shellCmd, 60)))
}

func (e *Engine) cmdCronList(p Platform, msg *Message) {
	jobs := e.cronScheduler.Store().ListByProject(e.name)
	if len(jobs) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronEmpty))
		return
	}

	lang := e.i18n.CurrentLang()
	now := time.Now()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(e.i18n.T(MsgCronListTitle), len(jobs)))
	sb.WriteString("\n")
	sb.WriteString("\n")

	for i, j := range jobs {
		if i > 0 {
			sb.WriteString("\n")
		}

		status := "✅"
		if !j.Enabled {
			status = "⏸"
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
		sb.WriteString(fmt.Sprintf("%s %s\n", status, desc))

		sb.WriteString(fmt.Sprintf("ID: %s\n", j.ID))

		human := CronExprToHuman(j.CronExpr, lang)
		sb.WriteString(e.i18n.Tf(MsgCronScheduleLabel, human, j.CronExpr))

		nextRun := e.cronScheduler.NextRun(j.ID)
		if !nextRun.IsZero() {
			fmtStr := cronTimeFormat(nextRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronNextRunLabel, nextRun.Format(fmtStr)))
		}

		if !j.LastRun.IsZero() {
			fmtStr := cronTimeFormat(j.LastRun, now)
			sb.WriteString(e.i18n.Tf(MsgCronLastRunLabel, j.LastRun.Format(fmtStr)))
			if j.LastError != "" {
				sb.WriteString(fmt.Sprintf(" (failed: %s)", truncateStr(j.LastError, 40)))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n%s", e.i18n.T(MsgCronListFooter)))
	e.reply(p, msg.ReplyCtx, sb.String())
}

func (e *Engine) cmdCronExec(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronExecUsage))
		return
	}
	id := args[0]
	job := e.cronScheduler.store.Get(id)
	if job == nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronNotFound), id))
		return
	}
	if job.IsShellJob() && !e.isAdmin(msg.UserID) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAdminRequired), "/cron exec"))
		return
	}
	if err := e.cronScheduler.RunJobNow(id); err != nil {
		switch {
		case errors.Is(err, ErrCronJobNotFound):
			e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronNotFound), id))
		case errors.Is(err, ErrCronProjectNotFound):
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronProjectUnavailable))
		default:
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
		}
		return
	}
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronTriggered), id))
}

func (e *Engine) cmdCronDel(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronDelUsage))
		return
	}
	id := args[0]
	if e.cronScheduler.RemoveJob(id) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronDeleted), id))
	} else {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronNotFound), id))
	}
}

func (e *Engine) cmdCronToggle(p Platform, msg *Message, args []string, enable bool) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronDelUsage))
		return
	}
	id := args[0]
	var err error
	if enable {
		err = e.cronScheduler.EnableJob(id)
	} else {
		err = e.cronScheduler.DisableJob(id)
	}
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
		return
	}
	if enable {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronEnabled), id))
	} else {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronDisabled), id))
	}
}

func (e *Engine) cmdCronMute(p Platform, msg *Message, args []string, mute bool) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCronDelUsage))
		return
	}
	id := args[0]
	if !e.cronScheduler.Store().SetMute(id, mute) {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronNotFound), id))
		return
	}
	if mute {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronMuted), id))
	} else {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronUnmuted), id))
	}
}

func (e *Engine) cmdCronSetup(p Platform, msg *Message) {
	result, baseName, err := e.setupMemoryFile()
	switch result {
	case setupNative:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSetupNative))
	case setupNoMemory:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgRelaySetupNoMemory))
	case setupExists:
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgRelaySetupExists), baseName))
	case setupError:
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgError, err))
	case setupOK:
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgCronSetupOK), baseName))
	}
}
