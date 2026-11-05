package core

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// StreamPreviewCfg controls the streaming preview behavior.
type StreamPreviewCfg struct {
	Enabled           bool     // global toggle
	DisabledPlatforms []string // platforms where streaming preview is disabled (e.g. "feishu")
	IntervalMs        int      // minimum ms between updates (default 1500)
	MinDeltaChars     int      // minimum new chars before sending an update (default 30)
	MaxChars          int      // max preview length (default 2000)
	TypingMarker      string   // shown under a preview while the turn is still running, e.g. "..."; empty = off
}

// DefaultStreamPreviewCfg returns sensible defaults.
func DefaultStreamPreviewCfg() StreamPreviewCfg {
	return StreamPreviewCfg{
		Enabled:           true,
		DisabledPlatforms: nil,
		IntervalMs:        1500,
		MinDeltaChars:     30,
		MaxChars:          2000,
	}
}

// FinalReplyOrder controls temporary preview recall relative to final delivery.
type FinalReplyOrder string

const (
	FinalReplyRecallFirst FinalReplyOrder = "recall_first"
	FinalReplySendFirst   FinalReplyOrder = "send_first"
)

// ProgressMergeCfg describes which public progress segments survive final recall.
type ProgressMergeCfg struct {
	Enabled  bool
	MinChars int
	Tables   bool
}

func DefaultProgressMergeCfg() ProgressMergeCfg {
	return ProgressMergeCfg{Enabled: true, MinChars: 400, Tables: true}
}

type ProgressMergePreference interface {
	ProgressMergeConfig() ProgressMergeCfg
}

type FinalReplyOrderPreference interface {
	FinalReplyOrder() FinalReplyOrder
}

// PlainTextSender bypasses rich rendering for final delivery recovery.
type PlainTextSender interface {
	SendPlainText(context.Context, any, string) error
}

// streamPreview manages the state and throttling of a single streaming preview.
// It accumulates text from EventText events and periodically pushes
// updates to the platform via MessageUpdater.UpdateMessage.
type streamPreview struct {
	mu sync.Mutex

	cfg       StreamPreviewCfg
	platform  Platform
	replyCtx  any
	ctx       context.Context
	transform func(string) string

	fullText               string // accumulated full text so far
	streamingText          string // latest whole preview text from streamReplace (token-level streaming)
	lastSentText           string // what was last successfully sent to the platform
	lastSentAt             time.Time
	lastSentViaUpdate      bool   // true if lastSentText was delivered via UpdateMessage (not SendPreviewStart)
	previewMsgID           any    // platform-specific ID for the preview message (returned by SendPreviewStart)
	previewStartedAt       time.Time
	markerShown            bool // the card on screen carries the typing marker, so finish must always repaint it
	pendingAcknowledgement func() // consumed by the first visible output
	degraded               bool   // if true, stop trying (platform doesn't support it or permanent error)

	timer     *time.Timer
	timerStop chan struct{} // closed when preview ends

	mergeCfg            ProgressMergeCfg
	progressI18n        *I18n
	substantiveProgress []string
	progressRows        []string
	progressSeen        map[string]struct{}
	progressFolded      int
	keepProgressCard    bool
	pendingToolProgress string     // latest coalesced progress; never answer text
	pendingStatus       CardStatus // last status set via setStatus(); applied on recovery
	// hidden marks an automated turn (timer, tell, relay...): the preview keeps
	// its state for the final reply but never creates a platform message, so
	// a turn that ends silent sends nothing and recalls nothing.
	hidden bool
}

// ToolStepKind identifies the kind of progress row shown in rich cards.
type ToolStepKind string

const (
	ToolStepKindTool     ToolStepKind = "tool"
	ToolStepKindThinking ToolStepKind = "thinking"
)

// ToolStep is one summarized progress row shown in rich progress cards.
type ToolStep struct {
	Kind     ToolStepKind // progress row kind; empty means tool for backward compatibility
	Name     string       // tool name (e.g. "Bash", "Edit")
	Summary  string       // human-readable summary shown in the card
	Result   string       // optional tool output/result summary
	Status   string       // optional tool status (e.g. completed/failed)
	ExitCode *int         // optional process exit code
	Success  *bool        // optional success flag
	Done     bool         // true once a tool result has been observed
}

// RichCardSupporter is an optional interface for platforms that can build
// native rich cards combining tool steps, markdown content, and a multi-line
// status footer.
//
// statusFooter is a pre-composed multi-line string assembled by the engine
// (typically one line per: elapsed time, model · effort · ctx, workdir).
// Pass empty string to hide the footer entirely. Lines are separated by '\n';
// the platform implementation is expected to render each line as its own
// dim-styled element so they don't visually merge with the body markdown.
//
// (Phase B refactor: previously took elapsed time.Duration; now the engine
// owns elapsed-time formatting so it can apply i18n + project-level toggles
// uniformly with the rest of the footer.)
type RichCardSupporter interface {
	BuildRichCard(status CardStatus, title string, steps []ToolStep, markdown string, streaming bool, statusFooter string) string
}

// RichCardMarkdownResolver is an optional interface for platforms that need to
// pre-process rich-card markdown before it is rendered or streamed.
//
// Feishu uses this to turn markdown image URLs into real uploaded image keys:
// intermediate streaming frames may return quickly without waiting for uploads,
// while final frames may wait briefly so the completed card can embed images.
type RichCardMarkdownResolver interface {
	ResolveRichCardMarkdown(ctx context.Context, markdown string, final bool) string
}

// MarkdownTableSplitter is an optional interface for platforms that need
// platform-specific markdown table chunking before final send.
type MarkdownTableSplitter interface {
	SplitMarkdownByTables(md string, maxTables int) []string
}

// RichCardTextStreamer is an optional interface for platforms that support
// per-element streaming text updates on rich cards (e.g. Lark/Feishu's
// cardkit-v1 streaming text update API). When implemented, the engine routes
// EventText growth through StreamRichCardText instead of full-card updates,
// giving the client a native typewriter rendering effect.
//
// Returns ErrNotSupported when the specific preview handle was created
// without a streamable card entity (fallback path); the engine then falls
// back to the standard MessageUpdater full-card update.
type RichCardTextStreamer interface {
	// StreamRichCardText pushes the latest fullText to the streaming-text
	// element of the rich card identified by previewHandle. The platform
	// implementation is responsible for serializing concurrent calls and
	// maintaining a monotonic sequence counter per handle.
	StreamRichCardText(ctx context.Context, previewHandle any, fullText string) error
}

// PreviewStarter is an optional interface for platforms that can initiate a
// streaming preview message and return a handle for subsequent updates.
type PreviewStarter interface {
	// SendPreviewStart sends the initial preview message and returns a handle
	// that can be passed to UpdateMessage for edits. Returns nil handle if
	// preview is not supported for this context.
	SendPreviewStart(ctx context.Context, replyCtx any, content string) (previewHandle any, err error)
}

// PreviewCleaner is an optional interface for platforms that need to clean up
// the preview message after a separate final response is sent (e.g. Discord deletes
// the preview and sends a fresh message).
type PreviewCleaner interface {
	DeletePreviewMessage(ctx context.Context, previewHandle any) error
}

// PreviewFinishPreference is an optional interface for platforms that want to
// keep the preview message as the final delivered message on normal completion.
type PreviewFinishPreference interface {
	KeepPreviewOnFinish() bool
}

// PreviewKeepDecider lets a platform refuse to finish one particular preview in
// place, e.g. when newer messages sit below it or the turn ran long: an edit
// raises no notification, so the reader would never see the reply.
type PreviewKeepDecider interface {
	KeepPreviewInPlace(ctx context.Context, previewHandle any, age time.Duration) bool
}

// ImmediatePreviewPreference is an optional interface for platforms that want
// an editable placeholder sent immediately before the first content delta.
type ImmediatePreviewPreference interface {
	StartPreviewImmediately() bool
}

// ThinkingProcessPreviewPreference keeps progress on a single temporary card.
type ThinkingProcessPreviewPreference interface {
	UseThinkingProcessPreview() bool
}

// markTyping appends the typing marker (e.g. "...") to a preview so a running turn
// reads differently from a finished reply. lastSentText keeps the unmarked text
// for dedupe and deltas.
func (sp *streamPreview) markTyping(text string) string {
	if sp.cfg.TypingMarker == "" || strings.HasPrefix(strings.TrimSpace(text), "{") {
		sp.markerShown = false
		return text
	}
	body := strings.TrimRight(text, " \n")
	if strings.Count(body, "```")%2 == 1 {
		body += "\n```"
	}
	sp.markerShown = true
	return body + "\n\n" + sp.cfg.TypingMarker
}

// replacePreview edits ephemeral progress without mixing it into answer text.
func (sp *streamPreview) replacePreview(text string) bool {
	text, _ = stripTrailingSilent(text)
	sp.acknowledgeVisible(text)
	if strings.TrimSpace(text) == "" || !sp.canPreview() {
		return false
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.cancelTimerLocked()
	return sp.replacePreviewLocked(text)
}

func (sp *streamPreview) replacePreviewLocked(text string) bool {
	if sp.degraded {
		return false
	}
	if sp.transform != nil {
		text = sp.transform(text)
	}
	if text == "" {
		return false
	}
	if len([]rune(text)) > 12000 {
		text = string([]rune(text)[len([]rune(text))-12000:])
	}
	if text == sp.lastSentText {
		return true
	}
	if sp.previewMsgID == nil {
		if sp.hidden {
			return false
		}
		starter, ok := sp.platform.(PreviewStarter)
		if !ok {
			return false
		}
		handle, err := starter.SendPreviewStart(sp.ctx, sp.replyCtx, sp.markTyping(text))
		if err != nil || handle == nil {
			slog.Warn("progress preview start failed", "error", err)
			return false
		}
		sp.previewMsgID = handle
		sp.previewStartedAt = time.Now()
	} else {
		updater, ok := sp.platform.(MessageUpdater)
		if !ok {
			return false
		}
		if err := updater.UpdateMessage(sp.ctx, sp.previewMsgID, sp.markTyping(text)); err != nil {
			slog.Warn("progress preview update failed", "error", err)
			return false
		}
		sp.lastSentViaUpdate = true
	}
	sp.lastSentText = text
	sp.lastSentAt = time.Now()
	return true
}

func newStreamPreview(cfg StreamPreviewCfg, p Platform, replyCtx any, ctx context.Context, transform func(string) string) *streamPreview {
	mergeCfg := DefaultProgressMergeCfg()
	if pref, ok := p.(ProgressMergePreference); ok {
		mergeCfg = pref.ProgressMergeConfig()
	}
	return &streamPreview{
		cfg:          cfg,
		mergeCfg:     mergeCfg,
		progressI18n: NewI18n(LangChinese),
		platform:     p,
		replyCtx:     replyCtx,
		ctx:          ctx,
		transform:    transform,
		timerStop:    make(chan struct{}),
	}
}

// Immediate-preview platforms defer acknowledgement until output is visible.
func (e *Engine) acknowledgeTurnStart(p Platform, replyCtx any, sp *streamPreview, hasStreamingCard bool) {
	if hasStreamingCard {
		return
	}
	acknowledge := func() {
		if pref, ok := p.(ImmediatePreviewPreference); ok && pref.StartPreviewImmediately() {
			if sp.startPlaceholder("思考中…") {
				return
			}
		}
		if e.instantReply.Enabled {
			content := e.instantReply.Content
			if content == "" {
				content = e.i18n.T(MsgStarting)
			}
			e.send(p, replyCtx, content)
		}
	}
	if pref, ok := p.(ImmediatePreviewPreference); ok && pref.StartPreviewImmediately() {
		sp.mu.Lock()
		sp.pendingAcknowledgement = acknowledge
		sp.mu.Unlock()
		return
	}
	acknowledge()
}

func (sp *streamPreview) acknowledgeVisible(text string) {
	if sp.transform != nil {
		text = sp.transform(text)
	}
	text, _ = stripTrailingSilent(text)
	if strings.TrimSpace(text) == "" {
		return
	}
	sp.mu.Lock()
	acknowledge := sp.pendingAcknowledgement
	sp.pendingAcknowledgement = nil
	sp.mu.Unlock()
	if acknowledge != nil {
		acknowledge()
	}
}

// startPlaceholder reports whether a visible placeholder was sent. Callers can
// use it to avoid a second acknowledgement while retaining failure fallback.
func (sp *streamPreview) startPlaceholder(text string) bool {
	if sp.hidden || strings.TrimSpace(text) == "" || !sp.canPreview() {
		return false
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.degraded || sp.previewMsgID != nil {
		return sp.previewMsgID != nil
	}

	if sp.transform != nil {
		text = sp.transform(text)
	}
	if text == "" {
		return false
	}

	if starter, ok := sp.platform.(PreviewStarter); ok {
		handle, err := starter.SendPreviewStart(sp.ctx, sp.replyCtx, sp.markTyping(text))
		if err != nil {
			slog.Debug("stream preview: placeholder start failed, degrading", "error", err)
			sp.degraded = true
			return false
		}
		if handle == nil {
			return false
		}
		sp.previewMsgID = handle
		sp.previewStartedAt = time.Now()
	} else {
		if err := sp.platform.Send(sp.ctx, sp.replyCtx, text); err != nil {
			slog.Debug("stream preview: placeholder send failed", "error", err)
			sp.degraded = true
			return false
		}
		sp.previewMsgID = sp.replyCtx
	}
	sp.lastSentText = text
	sp.lastSentViaUpdate = false
	sp.lastSentAt = time.Now()
	return true
}

// canPreview returns true if the platform supports message updating and is not disabled.
// streamReplace shows text (the full preview body, e.g. progress lines plus the
// block being written) as the preview, throttled by stream_preview.interval_ms so
// token-level deltas from the agent become a card that grows every interval.
// A later replacePreview/finish/discard cancels any pending frame.
func (sp *streamPreview) streamReplace(text string) {
	text, _ = stripTrailingSilent(text)
	if strings.TrimSpace(text) == "" {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.degraded || !sp.cfg.Enabled {
		return
	}
	sp.streamingText = text
	interval := time.Duration(sp.cfg.IntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 700 * time.Millisecond
	}
	if !sp.lastSentAt.IsZero() {
		if elapsed := time.Since(sp.lastSentAt); elapsed < interval {
			if sp.timer == nil {
				sp.timer = time.AfterFunc(interval-elapsed, func() {
					sp.mu.Lock()
					defer sp.mu.Unlock()
					sp.timer = nil
					if !sp.degraded && sp.streamingText != "" {
						sp.replacePreviewLocked(sp.streamingText)
					}
				})
			}
			return
		}
	}
	sp.cancelTimerLocked()
	sp.replacePreviewLocked(text)
}

func (sp *streamPreview) canPreview() bool {
	sp.mu.Lock()
	degraded := sp.degraded
	sp.mu.Unlock()
	if degraded || !sp.cfg.Enabled {
		return false
	}
	// Check if platform is in disabled list
	platformName := sp.platform.Name()
	for _, disabled := range sp.cfg.DisabledPlatforms {
		if strings.EqualFold(disabled, platformName) {
			return false
		}
	}
	_, ok := sp.platform.(MessageUpdater)
	return ok
}

// appendText adds new text content and triggers a throttled flush if needed.
func (sp *streamPreview) appendText(text string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.degraded || !sp.cfg.Enabled {
		return
	}

	sp.fullText += text

	displayText, _ := stripTrailingSilent(sp.fullText)
	maxChars := sp.cfg.MaxChars
	if maxChars > 0 && len([]rune(displayText)) > maxChars {
		displayText = string([]rune(displayText)[:maxChars]) + "…"
	}

	delta := len([]rune(displayText)) - len([]rune(sp.lastSentText))
	elapsed := time.Since(sp.lastSentAt)
	interval := time.Duration(sp.cfg.IntervalMs) * time.Millisecond

	if delta < sp.cfg.MinDeltaChars && !sp.lastSentAt.IsZero() {
		sp.scheduleFlushLocked(interval)
		return
	}

	if elapsed < interval && !sp.lastSentAt.IsZero() {
		remaining := interval - elapsed
		sp.scheduleFlushLocked(remaining)
		return
	}

	sp.cancelTimerLocked()
	sp.flushLocked(displayText)
}

func (sp *streamPreview) scheduleFlushLocked(delay time.Duration) {
	if sp.timer != nil {
		return // already scheduled
	}
	sp.timer = time.AfterFunc(delay, func() {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		sp.timer = nil
		if sp.degraded {
			return
		}
		displayText, _ := stripTrailingSilent(sp.fullText)
		maxChars := sp.cfg.MaxChars
		if maxChars > 0 && len([]rune(displayText)) > maxChars {
			displayText = string([]rune(displayText)[:maxChars]) + "…"
		}
		sp.flushLocked(displayText)
	})
}

func (sp *streamPreview) cancelTimerLocked() {
	if sp.timer != nil {
		sp.timer.Stop()
		sp.timer = nil
	}
}

// flushLocked sends the current preview text to the platform. Must hold sp.mu.
func (sp *streamPreview) flushLocked(text string) {
	text, _ = stripTrailingSilent(text)
	if sp.transform != nil {
		text = sp.transform(text)
	}
	if text == sp.lastSentText || text == "" {
		return
	}

	updater, ok := sp.platform.(MessageUpdater)
	if !ok {
		slog.Debug("stream preview: platform does not support UpdateMessage, degrading")
		sp.degraded = true
		return
	}

	if sp.previewMsgID == nil {
		if sp.hidden {
			return
		}
		// First preview: try to send a new preview message
		if starter, ok := sp.platform.(PreviewStarter); ok {
			slog.Debug("stream preview: sending first preview via SendPreviewStart", "text_len", len(text))
			handle, err := starter.SendPreviewStart(sp.ctx, sp.replyCtx, sp.markTyping(text))
			if err != nil {
				slog.Debug("stream preview: start failed, degrading", "error", err)
				sp.degraded = true
				return
			}
			sp.previewMsgID = handle
			sp.previewStartedAt = time.Now()
		} else {
			if err := sp.platform.Send(sp.ctx, sp.replyCtx, text); err != nil {
				slog.Debug("stream preview: initial send failed", "error", err)
				sp.degraded = true
				return
			}
			sp.previewMsgID = sp.replyCtx
		}
		sp.lastSentText = text
		sp.lastSentViaUpdate = false
		sp.lastSentAt = time.Now()
		return
	}

	// Update existing preview message
	slog.Debug("stream preview: updating via UpdateMessage", "text_len", len(text))
	if err := updater.UpdateMessage(sp.ctx, sp.previewMsgID, sp.markTyping(text)); err != nil {
		slog.Debug("stream preview: update failed, degrading", "error", err)
		sp.degraded = true
		return
	}
	sp.lastSentText = text
	sp.lastSentViaUpdate = true
	sp.lastSentAt = time.Now()
}

// freeze stops the streaming preview permanently: cancels pending timers,
// updates the preview message in-place with the accumulated text, and marks
// the preview as degraded so no further updates are sent.
// Call this when a permission prompt or other interruption occurs.
func (sp *streamPreview) freeze() {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	sp.cancelTimerLocked()

	if sp.previewMsgID != nil && !sp.degraded {
		if updater, ok := sp.platform.(MessageUpdater); ok {
			text, _ := stripTrailingSilent(sp.fullText)
			maxChars := sp.cfg.MaxChars
			if maxChars > 0 && len([]rune(text)) > maxChars {
				text = string([]rune(text)[:maxChars]) + "…"
			}
			if text != "" {
				_ = updater.UpdateMessage(sp.ctx, sp.previewMsgID, text)
			}
		}
	}

	sp.degraded = true
}

// unfreeze reverses a prior freeze()+detachPreview() so the preview resumes
// streaming. After unfreeze, the next appendText() will create a NEW preview
// message (the prior handle was detached), giving the post-interruption
// output its own card instead of being buffered until end of turn.
//
// Required pairing: callers must invoke unfreeze() only after a matching
// freeze()+detachPreview() (or an equivalent sequence that left previewMsgID
// nil and degraded true). It is intended to be called once the user-visible
// interruption (permission prompt, AskUserQuestion) has been resolved and
// the agent is producing new output in the same turn.
//
// Why a new card: the old preview message is committed by freeze() as a
// permanent in-place update of the pre-interruption text. Keeping a handle
// to it would overwrite that committed message with the post-interruption
// content. Detach+re-attach on the next appendText is what the surrounding
// code (engine EventPermissionRequest handler) does today, and unfreeze()
// simply restores the streamPreview's degraded state so that flow can run
// again on the same turn.
func (sp *streamPreview) unfreeze() {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	sp.cancelTimerLocked()

	// Allow appendText() to bypass the interval/MinDeltaChars throttle on
	// the first chunk after the interruption: lastSentAt is the only gate
	// the throttle checks, and zeroing it makes that gate fall through.
	sp.lastSentAt = time.Time{}

	// Start a fresh accumulation window so the new card is a clean slate.
	// Without this, the dedupe in flushLocked (text == sp.lastSentText)
	// would suppress the first send of the new card, and the new card
	// would appear empty until enough chars accumulated to force a flush.
	sp.fullText = ""
	sp.lastSentText = ""
	sp.lastSentViaUpdate = false
	sp.pendingStatus = ""

	// previewMsgID stays nil (set by the matching detachPreview()). The
	// next flushLocked() will see nil and call SendPreviewStart, opening
	// a fresh card.
	sp.degraded = false
}

// discard removes the preview message when possible and disables further
// preview updates. Call this when the caller intends to send a separate
// non-preview message (for example after tool use or on terminal errors).
func (sp *streamPreview) discard() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.pendingAcknowledgement = nil

	sp.cancelTimerLocked()

	select {
	case <-sp.timerStop:
	default:
		close(sp.timerStop)
	}

	if sp.previewMsgID != nil {
		if cleaner, ok := sp.platform.(PreviewCleaner); ok {
			slog.Debug("stream preview discard: deleting preview")
			if err := cleaner.DeletePreviewMessage(sp.ctx, sp.previewMsgID); err != nil {
				slog.Warn("stream preview discard: recall failed", "error", err)
			}
		}
	}

	sp.previewMsgID = nil
	sp.degraded = true
}

// finish is called when the agent response is complete. It cancels any pending
// timer and optionally cleans up the preview message.
// Returns true if a preview was active and the final message was sent via preview
// (so the caller should skip sending the full response separately).
//
// `statusFooter` is an optional structured footer string (one or more lines)
// that platforms implementing StatusFooterUpdater render with small/dim
// styling separate from the body. When the platform does not implement that
// interface and statusFooter is non-empty, finish falls back to appending the
// footer inline to finalText before the regular UpdateMessage call.
func (sp *streamPreview) finish(finalText, statusFooter string) bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	sp.cancelTimerLocked()

	select {
	case <-sp.timerStop:
	default:
		close(sp.timerStop)
	}

	if sp.transform != nil {
		finalText = sp.transform(finalText)
	}
	keepPreview := false
	if pref, ok := sp.platform.(PreviewFinishPreference); ok {
		keepPreview = pref.KeepPreviewOnFinish()
	}

	// Apply the finish policy before attempting recovery of a degraded preview.
	if cleaner, ok := sp.platform.(PreviewCleaner); ok && !keepPreview && sp.previewMsgID != nil {
		slog.Debug("stream preview finish: deleting preview (PreviewCleaner)")
		if err := cleaner.DeletePreviewMessage(sp.ctx, sp.previewMsgID); err != nil {
			slog.Warn("stream preview finish: recall failed", "error", err)
		} else {
			sp.previewMsgID = nil
		}
		return false
	}

	if sp.previewMsgID == nil || sp.degraded {
		if sp.previewMsgID != nil && sp.degraded {
			// Try to recover degraded preview via UpdateMessage before falling back to delete
			if finalText != "" {
				if updater, ok := sp.platform.(MessageUpdater); ok {
					if err := updater.UpdateMessage(sp.ctx, sp.previewMsgID, finalText); err == nil {
						if sp.pendingStatus != "" {
							if statusUpdater, ok := sp.platform.(PreviewStatusUpdater); ok {
								statusUpdater.SetPreviewStatus(sp.previewMsgID, sp.pendingStatus)
							}
						}
						return true
					} else {
						slog.Debug("stream preview finish: degraded UpdateMessage failed, cleaning up", "error", err)
					}
				}
			}
			if cleaner, ok := sp.platform.(PreviewCleaner); ok {
				slog.Debug("stream preview finish: deleting stale preview (degraded)")
				_ = cleaner.DeletePreviewMessage(sp.ctx, sp.previewMsgID)
			}
		}
		slog.Debug("stream preview finish: no active preview", "hasHandle", sp.previewMsgID != nil, "degraded", sp.degraded)
		return false
	}

	updater, ok := sp.platform.(MessageUpdater)
	if !ok {
		slog.Debug("stream preview finish: no MessageUpdater")
		return false
	}

	if finalText == "" {
		slog.Debug("stream preview finish: empty final text")
		return false
	}

	// If the final text is identical to what was last sent via UpdateMessage
	// AND no status footer needs to be applied, skip the redundant API call.
	// This prevents duplicate messages on platforms (e.g. Feishu) where
	// patching with identical content may fail. We must NOT skip when a
	// statusFooter is pending — the body may match but the footer hasn't
	// been rendered yet, and dropping the call would silently lose it.
	// Only skip when lastSentViaUpdate is true — if the text was only sent
	// via SendPreviewStart (first flush), we must still call UpdateMessage
	// because it may apply different formatting (e.g. Markdown→HTML for
	// Telegram).
	if finalText == sp.lastSentText && sp.lastSentViaUpdate && statusFooter == "" && !sp.markerShown {
		slog.Debug("stream preview finish: text unchanged and no footer, skipping",
			"text_len", len(finalText))
		return true
	}

	// Try to update the preview in-place with the full final text.
	// maxChars only throttles intermediate streaming updates; at finish time
	// we always attempt a single final update regardless of length.
	slog.Debug("stream preview finish: sending final UpdateMessage",
		"text_len", len(finalText), "lastSent_len", len(sp.lastSentText),
		"same", finalText == sp.lastSentText, "viaUpdate", sp.lastSentViaUpdate,
		"footer_len", len(statusFooter))

	// Prefer the structured-footer path when the platform supports it, so the
	// footer renders with small/dim styling separate from the response body.
	if statusFooter != "" {
		if sfu, ok := sp.platform.(StatusFooterUpdater); ok {
			if err := sfu.UpdateMessageWithStatusFooter(sp.ctx, sp.previewMsgID, finalText, statusFooter); err == nil {
				slog.Debug("stream preview finish: success via UpdateMessageWithStatusFooter")
				return true
			} else {
				slog.Debug("stream preview finish: UpdateMessageWithStatusFooter failed, falling back", "error", err)
			}
		}
		// Fallback: append inline so the footer is at least visible.
		finalText = appendReplyFooter(finalText, statusFooter)
	}

	if err := updater.UpdateMessage(sp.ctx, sp.previewMsgID, finalText); err != nil {
		slog.Debug("stream preview finish: final update FAILED, cleaning up preview", "error", err)
		// Update failed (e.g. text too long for platform edit API).
		// Try to delete the stale preview so caller can send a fresh message.
		if cleaner, ok := sp.platform.(PreviewCleaner); ok {
			_ = cleaner.DeletePreviewMessage(sp.ctx, sp.previewMsgID)
		}
		return false
	}
	if sp.pendingStatus != "" {
		if statusUpdater, ok := sp.platform.(PreviewStatusUpdater); ok {
			statusUpdater.SetPreviewStatus(sp.previewMsgID, sp.pendingStatus)
		}
	}
	slog.Debug("stream preview finish: success via UpdateMessage")
	return true
}

// setStatus updates the card header status of the active preview message.
// If the preview is not yet active or is degraded, the status is saved and
// applied when the preview recovers (at finish time).
func (sp *streamPreview) setStatus(status CardStatus) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.pendingStatus = status
	if sp.previewMsgID == nil || sp.degraded {
		return
	}
	if updater, ok := sp.platform.(PreviewStatusUpdater); ok {
		updater.SetPreviewStatus(sp.previewMsgID, status)
	}
}

// detachPreview clears the preview message handle so that finish() won't
// delete it. Call this after freeze() when the frozen preview should remain
// visible as a permanent message (e.g. text before the first tool call).
func (sp *streamPreview) detachPreview() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.previewMsgID = nil
}

// appendSeparator inserts a paragraph break into the accumulated text without
// triggering a flush. Used in quiet mode to visually separate text segments
// that span thinking/tool boundaries without creating separate messages.
// Returns true if the separator was actually added.
func (sp *streamPreview) appendSeparator(sep string) bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.degraded || !sp.cfg.Enabled || sp.fullText == "" {
		return false
	}
	sp.fullText += sep
	return true
}

// needsDoneReaction returns true if the preview was delivered via in-place
// UpdateMessage at least once, meaning the user only received a push for the
// initial SendPreviewStart and subsequent updates were silent. In this case a
// "done" reaction can notify the user that processing has completed.
func (sp *streamPreview) needsDoneReaction() bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.previewMsgID != nil && sp.lastSentViaUpdate
}

// detachForFinal transfers cleanup ownership before a stop can discard the preview.
func (sp *streamPreview) detachForFinal() *streamPreview {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.cancelTimerLocked()
	sp.degraded = true
	final := newStreamPreview(sp.cfg, sp.platform, sp.replyCtx, sp.ctx, sp.transform)
	final.previewMsgID, final.lastSentText = sp.previewMsgID, sp.lastSentText
	final.previewStartedAt = sp.previewStartedAt
	final.markerShown = sp.markerShown
	final.keepProgressCard, final.hidden = sp.keepProgressCard, sp.hidden
	final.pendingToolProgress = sp.pendingToolProgress
	final.mergeCfg, final.progressI18n = sp.mergeCfg, sp.progressI18n
	final.substantiveProgress, sp.substantiveProgress = sp.substantiveProgress, nil
	sp.previewMsgID = nil
	return final
}

// keepInPlace asks the platform whether this preview may become the final reply in place.
func (sp *streamPreview) keepInPlace() bool {
	decider, ok := sp.platform.(PreviewKeepDecider)
	if !ok {
		return true
	}
	sp.mu.Lock()
	handle, started := sp.previewMsgID, sp.previewStartedAt
	sp.mu.Unlock()
	if handle == nil {
		return true
	}
	return decider.KeepPreviewInPlace(sp.ctx, handle, time.Since(started))
}

// deliverFinal freezes progress updates before delivering the final reply.
func (sp *streamPreview) deliverFinal(body, footer string, sendFn func(Platform, any, string) error, failureText string) bool {
	// 平台要求保留预览（飞书 keep_preview_on_finish）时，把过程卡原地更新成最终回复；失败再走撤回/重发。
	// 超过单条上限（按字符计，与分块一致）时，第一块原地完成，其余块接着另发。
	if pref, ok := sp.platform.(PreviewFinishPreference); ok && pref.KeepPreviewOnFinish() && sp.keepInPlace() {
		chunks := SplitMessageCodeFenceAware(body, maxPlatformMessageLen)
		rest := chunks[1:]
		firstFooter := footer
		if len(rest) > 0 {
			firstFooter = ""
		}
		if sp.finish(chunks[0], firstFooter) {
			// 卡片已经是最终回复：交出所有权，回合末尾的 discard 不能再把它当预览撤回。
			sp.mu.Lock()
			sp.previewMsgID = nil
			sp.degraded = true
			sp.mu.Unlock()
			for i, chunk := range rest {
				if i == len(rest)-1 && footer != "" {
					chunk = appendReplyFooter(chunk, footer)
				}
				if err := sendFn(sp.platform, sp.replyCtx, chunk); err != nil {
					slog.Warn("final response: retrying failed chunk", "error", err, "chunk", i+1)
					if err := sendFn(sp.platform, sp.replyCtx, chunk); err != nil {
						slog.Error("final response: chunk retry exhausted", "error", err, "chunk", i+1)
					}
				}
			}
			return true
		}
	}
	order := FinalReplySendFirst
	if pref, ok := sp.platform.(FinalReplyOrderPreference); ok {
		order = pref.FinalReplyOrder()
	}
	return sp.deliverFinalInOrder(body, footer, sendFn, failureText, order)
}

func (sp *streamPreview) deliverFinalInOrder(body, footer string, sendFn func(Platform, any, string) error, failureText string, order FinalReplyOrder) bool {
	sp.acknowledgeVisible(body)
	sp.mu.Lock()
	sp.cancelTimerLocked()
	sp.degraded = true
	mergedBody := sp.mergeProgressLocked(body)
	mergedProgress := mergedBody != body
	body = mergedBody
	handle, progress := sp.previewMsgID, sp.lastSentText
	if (sp.keepProgressCard || len(sp.progressRows) > 0) && sp.pendingToolProgress != "" {
		progress = sp.pendingToolProgress
	}
	sp.previewMsgID = nil
	select {
	case <-sp.timerStop:
	default:
		close(sp.timerStop)
	}
	sp.mu.Unlock()
	recalled := false
	if order == FinalReplyRecallFirst && handle != nil && !sp.keepProgressCard {
		if cleaner, ok := sp.platform.(PreviewCleaner); ok {
			if err := cleaner.DeletePreviewMessage(sp.ctx, handle); err != nil {
				slog.Warn("final response: recall failed", "error", err)
			} else {
				recalled = true
			}
		}
	}
	chunks := SplitMessageCodeFenceAware(body, maxPlatformMessageLen)
	if strings.TrimSpace(body) == "" && strings.TrimSpace(footer) == "" {
		chunks = nil // Duplicate suppression has no new message to deliver.
	}
delivery:
	for i, chunk := range chunks {
		if i == len(chunks)-1 && footer != "" {
			if sender, ok := sp.platform.(StatusFooterSender); ok {
				if err := sender.SendWithStatusFooter(sp.ctx, sp.replyCtx, chunk, footer); err == nil {
					continue
				} else {
					slog.Warn("final response: structured footer send failed", "error", err)
				}
			}
			chunk = appendReplyFooter(chunk, footer)
		}
		if err := sendFn(sp.platform, sp.replyCtx, chunk); err == nil {
			continue
		} else {
			slog.Warn("final response: retrying failed chunk", "error", err, "chunk", i)
		}
		// Retry only the failed chunk; never resend the delivered prefix.
		if err := sendFn(sp.platform, sp.replyCtx, chunk); err == nil {
			continue
		} else {
			slog.Error("final response: chunk retry exhausted", "error", err, "chunk", i)
		}
		if order == FinalReplyRecallFirst || mergedProgress {
			unsent := append([]string(nil), chunks[i:]...)
			if footer != "" {
				unsent[len(unsent)-1] = appendReplyFooter(unsent[len(unsent)-1], footer)
			}
			if sender, ok := sp.platform.(PlainTextSender); ok {
				recovered := true
				for index, text := range unsent {
					if err := sender.SendPlainText(sp.ctx, sp.replyCtx, text); err != nil {
						slog.Warn("final response: plain-text fallback failed; retrying", "error", err)
						if err = sender.SendPlainText(sp.ctx, sp.replyCtx, text); err != nil {
							slog.Error("final response: plain-text fallback retry failed", "error", err)
							recovered = false
							unsent = unsent[index:]
							break
						}
					}
				}
				if recovered {
					break delivery
				}
			}
			// Recover each remaining segment independently to respect platform size limits.
			for index, text := range unsent {
				recoveryText := appendReplyFooter(text, failureText)
				if index == 0 && !recalled && handle != nil {
					if updater, ok := sp.platform.(MessageUpdater); ok {
						if err := updater.UpdateMessage(sp.ctx, handle, recoveryText); err == nil {
							continue
						} else {
							slog.Error("final response: recovery card update failed", "error", err)
						}
					}
				}
				if starter, ok := sp.platform.(PreviewStarter); ok {
					if _, err := starter.SendPreviewStart(sp.ctx, sp.replyCtx, recoveryText); err != nil {
						slog.Error("final response: recovery card creation failed", "error", err)
					}
				}
			}
			return false
		}
		if handle != nil {
			if updater, ok := sp.platform.(MessageUpdater); ok {
				text := appendReplyFooter(progress, failureText)
				if sp.transform != nil {
					text = sp.transform(text)
				}
				if status, ok := sp.platform.(PreviewStatusUpdater); ok {
					status.SetPreviewStatus(handle, CardStatusError)
				}
				if err := updater.UpdateMessage(sp.ctx, handle, text); err != nil {
					slog.Warn("final response: failure notice update failed", "error", err)
				}
			}
		}
		return false
	}
	if handle != nil && sp.keepProgressCard {
		if status, ok := sp.platform.(PreviewStatusUpdater); ok {
			status.SetPreviewStatus(handle, CardStatusDone)
		}
		if updater, ok := sp.platform.(MessageUpdater); ok {
			text := progress
			if sp.transform != nil {
				text = sp.transform(text)
			}
			if err := updater.UpdateMessage(sp.ctx, handle, text); err != nil {
				slog.Warn("final response: retained progress update failed", "error", err)
			}
		}
	}
	if handle != nil && !sp.keepProgressCard && order != FinalReplyRecallFirst {
		if cleaner, ok := sp.platform.(PreviewCleaner); ok {
			if err := cleaner.DeletePreviewMessage(sp.ctx, handle); err != nil {
				slog.Warn("final response: recall failed", "error", err)
			}
		}
	}
	return true
}
