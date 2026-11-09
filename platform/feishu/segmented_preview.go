package feishu

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

var _ core.SegmentedPreviewer = (*Platform)(nil)
var _ core.PreviewToolRecorder = (*Platform)(nil)

// One turn owns the ticker and all mutations, including finalization and recall.
type segmentedPreview struct {
	mu                             sync.Mutex
	stopCh                         chan struct{}
	stopped                        bool
	cfg                            core.StreamPreviewCfg
	i18n                           *core.I18n
	started                        time.Time
	bodyID, sentBodyID, progressID string
	body, lastBody, lastProgress   string
	steps                          int
	recent                         []string
	fallback                       bool
	lastPublished                  time.Time

	// Step-list layout (cfg.StepList): the agent's step lines with done/running
	// state, the live text under them, and a footer.
	footerID, lastFooter string

	// Paced reveal (cfg.RevealCps > 0): the agent text lands in pend (own lock, so the
	// event loop never waits on a card API call); the push loop adopts it into target and
	// releases it into body at a steady rate.
	pendMu     sync.Mutex
	pend       string
	pendDirty  bool
	target     string
	shown      int
	revealAcc  float64
	lastReveal time.Time
	draining   bool

	// lastText is when the visible text last grew. While text is flowing the step
	// list keeps its dots still, so the card API calls go to the text.
	lastText time.Time
	// Model of the client typing queue (characters not yet typed at clientAt), fed by
	// each text update; the end-of-turn catch-up waits for it to run out.
	clientBacklog float64
	clientAt      time.Time
	rows          []stepRow
	toolCalls     int
}

type stepRow struct {
	text       string
	tool       string // latest tool call under this step
	toolOnly   bool   // no step line from the agent; the row is the tool call itself
	start, end time.Time
}

const stepRowsShown = 8

func previewElementID() (string, error) {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("feishu: preview element ID: %w", err)
	}
	return "p" + hex.EncodeToString(b[:]), nil
}

func (p *Platform) StartSegmentedPreview(ctx context.Context, rc any, cfg core.StreamPreviewCfg, i18n *core.I18n) (any, error) {
	if !p.useInteractiveCard {
		return nil, core.ErrNotSupported
	}
	if cfg.PushIntervalMs <= 0 {
		cfg.PushIntervalMs = 200
	}
	if cfg.TypingIntervalMs <= 0 {
		cfg.TypingIntervalMs = 40
	}
	if cfg.TypingStep <= 0 {
		cfg.TypingStep = 6
	}
	if cfg.RevealCatchupMs <= 0 {
		cfg.RevealCatchupMs = 4000
	}
	if cfg.DrainMaxMs <= 0 {
		cfg.DrainMaxMs = 8000
	}
	if cfg.DotsPauseMs <= 0 {
		cfg.DotsPauseMs = 2000
	}
	if cfg.FinishCatchupMs == 0 {
		cfg.FinishCatchupMs = 3000
	}
	if cfg.PrintStrategy != "delay" {
		cfg.PrintStrategy = "fast"
	}
	bodyID, err := previewElementID()
	if err != nil {
		return nil, err
	}
	progressID, err := previewElementID()
	if err != nil {
		return nil, err
	}
	footerID, err := previewElementID()
	if err != nil {
		return nil, err
	}
	v := &segmentedPreview{stopCh: make(chan struct{}), cfg: cfg, i18n: i18n, started: time.Now(), bodyID: bodyID, sentBodyID: bodyID, progressID: progressID, footerID: footerID}
	v.lastProgress = v.progress(v.started)
	v.lastFooter = v.footer(v.started)
	handle, err := p.SendPreviewStart(ctx, rc, v.card(false))
	if err != nil {
		return nil, err
	}
	h := handle.(*feishuPreviewHandle)
	h.live = v
	v.fallback = h.cardID == ""
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.PushIntervalMs) * time.Millisecond)
		heartbeat := time.NewTicker(time.Second)
		defer ticker.Stop()
		defer heartbeat.Stop()
		publish := func(now time.Time, thinkingOnly bool) {
			v.mu.Lock()
			defer v.mu.Unlock()
			if v.stopped || (thinkingOnly && v.body != "" && !v.cfg.StepList) {
				return
			}
			// The tick value can be seconds old when the previous frame was slow
			// (each frame is several API calls), and the two tickers deliver their
			// stale values in either order. Render with the clock at publish time.
			now = time.Now()
			if err := p.publishSegmented(ctx, h, now); err != nil {
				slog.Warn("feishu: preview frame failed", "error", err)
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-v.stopCh:
				return
			case now := <-ticker.C:
				publish(now, false)
			case now := <-heartbeat.C:
				publish(now, true)
			}
		}
	}()
	return h, nil
}

func (v *segmentedPreview) stopLocked() {
	if !v.stopped {
		v.stopped = true
		close(v.stopCh)
	}
}

func (v *segmentedPreview) stop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopLocked()
}

func (p *Platform) SetPreviewBody(_ context.Context, handle any, text string) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil {
		return core.ErrNotSupported
	}
	v := h.live
	if v.cfg.RevealCps > 0 {
		chars := []rune(text)
		if len(chars) > 4000 {
			text = string(chars[:4000])
		}
		v.pendMu.Lock()
		v.pend, v.pendDirty = text, true
		v.pendMu.Unlock()
		if core.StreamTraceOn() {
			core.StreamTrace("body", map[string]any{"n": len(chars), "wait_ms": 0})
		}
		return nil
	}
	waitFrom := time.Now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if core.StreamTraceOn() {
		core.StreamTrace("body", map[string]any{"n": len([]rune(text)), "wait_ms": float64(time.Since(waitFrom).Microseconds()) / 1000})
	}
	if v.stopped {
		return nil
	}
	// Keep live frames bounded without adding synthetic fence text to each delta.
	chars := []rune(text)
	if len(chars) > 4000 {
		text = string(chars[:4000])
	}
	return v.setBody(text)
}

func (v *segmentedPreview) setBody(text string) error {
	if !strings.HasPrefix(text, v.body) {
		id, err := previewElementID()
		if err != nil {
			return err
		}
		v.bodyID = id
	}
	v.body = text
	v.lastText = time.Now()
	return nil
}

func (p *Platform) AddPreviewProgress(_ context.Context, handle any, text string) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil {
		return core.ErrNotSupported
	}
	v := h.live
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped || strings.TrimSpace(text) == "" {
		return nil
	}
	if v.cfg.StepList {
		v.addRow(time.Now(), text, false)
		return nil
	}
	v.steps++
	chars := []rune(text)
	if len(chars) > 300 {
		text = string(chars[:300])
	}
	v.recent = append(v.recent, text)
	if len(v.recent) > 3 {
		v.recent = v.recent[len(v.recent)-3:]
	}
	return nil
}

// AddPreviewTool records a tool call under the current step (StepList only).
func (p *Platform) AddPreviewTool(_ context.Context, handle any, text string) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil {
		return core.ErrNotSupported
	}
	v := h.live
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped || strings.TrimSpace(text) == "" {
		return nil
	}
	v.addRow(time.Now(), text, true)
	return nil
}

func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return text
}

// addRow opens a new step for an agent line, or attaches a tool call to the
// open step. A tool call with no agent line before it becomes a step itself.
func (v *segmentedPreview) addRow(now time.Time, text string, tool bool) {
	text = oneLine(text, 120)
	if tool {
		v.toolCalls++
		if n := len(v.rows); n > 0 && !v.rows[n-1].toolOnly {
			v.rows[n-1].tool = text
			return
		}
	}
	v.closeRow(now)
	v.rows = append(v.rows, stepRow{text: text, toolOnly: tool, start: now})
}

func (v *segmentedPreview) closeRow(now time.Time) {
	if n := len(v.rows); n > 0 && v.rows[n-1].end.IsZero() {
		v.rows[n-1].end = now
	}
}

// secondsBetween never goes negative: a clock sample taken before a later
// event (a queued ticker value, say) reads as zero, not as a negative duration.
func secondsBetween(from, to time.Time) int {
	if d := to.Sub(from); d > 0 {
		return int(d / time.Second)
	}
	return 0
}

// runningDots shows that work is still going on: one to three dots, changing
// every second. The card shows no timers.
func runningDots(since, now time.Time) string {
	return strings.Repeat(".", 1+secondsBetween(since, now)%3)
}

// stepList renders the steps, finished ones marked done and the open one running.
func (v *segmentedPreview) stepList(now time.Time) string {
	if len(v.rows) == 0 {
		return strings.TrimRight(v.i18n.T(core.MsgPreviewThinking), "…") + runningDots(v.started, now)
	}
	rows := v.rows
	var lines []string
	if len(rows) > stepRowsShown {
		lines = append(lines, fmt.Sprintf("… +%d", len(rows)-stepRowsShown))
		rows = rows[len(rows)-stepRowsShown:]
	}
	for _, r := range rows {
		if r.end.IsZero() {
			lines = append(lines, r.text+" "+runningDots(r.start, now))
			if r.tool != "" {
				lines = append(lines, "　└ "+r.tool)
			}
			continue
		}
		lines = append(lines, "✔ "+r.text)
	}
	return strings.Join(lines, "\n")
}

func (v *segmentedPreview) footer(now time.Time) string {
	if v.cfg.StepList {
		return ""
	}
	return v.i18n.Tf(core.MsgPreviewFooter, secondsBetween(v.started, now), v.toolCalls)
}

func (v *segmentedPreview) footerElement(content string) map[string]any {
	return map[string]any{"tag": "markdown", "element_id": v.footerID, "content": content, "text_size": "notation"}
}

func (v *segmentedPreview) progress(now time.Time) string {
	if v.cfg.StepList {
		return v.stepList(now)
	}
	var lines []string
	if v.body == "" {
		lines = append(lines, v.i18n.Tf(core.MsgPreviewElapsed, secondsBetween(v.started, now)))
	}
	if v.steps > 0 {
		lines = append(lines, v.i18n.Tf(core.MsgPreviewSteps, v.steps))
		lines = append(lines, v.recent...)
	}
	return strings.Join(lines, "\n")
}

func (v *segmentedPreview) progressElement(content string) map[string]any {
	el := map[string]any{"tag": "markdown", "element_id": v.progressID, "content": content}
	if !v.cfg.StepList {
		el["text_size"] = "notation"
	}
	return el
}

func (v *segmentedPreview) card(final bool) string {
	config := map[string]any{"width_mode": "fill", "summary": map[string]any{"content": v.i18n.T(core.MsgPreviewThinking)}}
	if !final && !v.fallback {
		platformValues := func(n int) map[string]int { return map[string]int{"default": n, "android": n, "ios": n, "pc": n} }
		config["streaming_mode"] = true
		config["streaming_config"] = map[string]any{"print_frequency_ms": platformValues(v.cfg.TypingIntervalMs), "print_step": platformValues(v.cfg.TypingStep), "print_strategy": v.cfg.PrintStrategy}
	}
	if v.body != "" {
		config["summary"] = map[string]any{"content": string([]rune(v.body)[:min(len([]rune(v.body)), 120)])}
	}
	if v.cfg.StepList {
		return v.stepCard(config, final)
	}
	elements := []map[string]any{{"tag": "markdown", "element_id": v.bodyID, "content": v.body}}
	if !final {
		elements = append(elements, v.progressElement(v.progress(time.Now())))
	} else if v.steps > 0 {
		panel := buildRichPanel(v.i18n.Tf(core.MsgPreviewProcess, v.steps), false, []map[string]any{{"tag": "markdown", "content": strings.Join(v.recent, "\n")}})
		panel["element_id"] = v.progressID
		elements = append(elements, panel)
	}
	b, err := json.Marshal(map[string]any{"schema": "2.0", "config": config, "body": map[string]any{"elements": elements}})
	if err != nil {
		slog.Error("feishu: marshal segmented preview", "error", err)
		return buildCardJSON(v.body)
	}
	return string(b)
}

// stepCard lays out the step list on top and the text being written under it. Once final, the list folds into a collapsed process panel.
func (v *segmentedPreview) stepCard(config map[string]any, final bool) string {
	now := time.Now()
	body := map[string]any{"tag": "markdown", "element_id": v.bodyID, "content": v.body}
	var elements []map[string]any
	if final {
		v.closeRow(now)
		if len(v.rows) > 0 {
			if v.body == "" {
				config["summary"] = map[string]any{"content": v.i18n.Tf(core.MsgPreviewDone, len(v.rows))}
			}
			panel := buildRichPanel(v.i18n.Tf(core.MsgPreviewDone, len(v.rows)), false, []map[string]any{{"tag": "markdown", "content": v.stepList(now)}})
			panel["element_id"] = v.progressID
			elements = append(elements, panel)
		}
	} else {
		elements = append(elements, v.progressElement(v.stepList(now)))
	}
	elements = append(elements, body)
	b, err := json.Marshal(map[string]any{"schema": "2.0", "config": config, "body": map[string]any{"elements": elements}})
	if err != nil {
		slog.Error("feishu: marshal segmented preview", "error", err)
		return buildCardJSON(v.body)
	}
	return string(b)
}

// cardKitMutation shares the sequence with the existing rich-card path, holding
// the handle lock through the HTTP request so operations cannot overtake it.
func (p *Platform) cardKitMutation(ctx context.Context, h *feishuPreviewHandle, method, suffix string, body map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sequence++
	body["sequence"] = h.sequence
	path := "/open-apis/cardkit/v1/cards/" + h.cardID + suffix
	var resp *larkcore.ApiResp
	if core.StreamTraceOn() {
		from := time.Now()
		reused := false
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }})
		defer func() {
			core.StreamTrace("api", map[string]any{"m": method, "p": suffix, "ms": float64(time.Since(from).Microseconds()) / 1000, "n": len([]rune(fmt.Sprint(body["content"]))), "reused": reused})
		}()
	}
	err := p.withFreshTenantAccessTokenRetry(ctx, "preview element", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
		var err error
		if method == http.MethodPatch {
			resp, err = client.Patch(ctx, path, body, larkcore.AccessTokenTypeTenant, options...)
		} else {
			resp, err = client.Put(ctx, path, body, larkcore.AccessTokenTypeTenant, options...)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("feishu: preview mutation: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("feishu: preview mutation: empty response")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu: preview mutation: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return fmt.Errorf("feishu: preview response: %w", err)
	}
	if result.Code != 0 {
		return classifyFeishuCardAPIError("preview mutation", result.Code, result.Msg)
	}
	return nil
}

func (p *Platform) replaceSegmentedElement(ctx context.Context, h *feishuPreviewHandle, id string, element map[string]any) error {
	b, err := json.Marshal(element)
	if err != nil {
		return fmt.Errorf("feishu: preview element: %w", err)
	}
	return p.cardKitMutation(ctx, h, http.MethodPut, "/elements/"+id, map[string]any{"element": string(b)})
}

func (p *Platform) streamSegmentedBody(ctx context.Context, h *feishuPreviewHandle) error {
	v := h.live
	if v.sentBodyID != v.bodyID || !strings.HasPrefix(v.body, v.lastBody) {
		if err := p.replaceSegmentedElement(ctx, h, v.sentBodyID, map[string]any{"tag": "markdown", "element_id": v.bodyID, "content": ""}); err != nil {
			return err
		}
		v.sentBodyID, v.lastBody = v.bodyID, ""
	}
	if v.body == v.lastBody {
		return nil
	}
	added := len([]rune(v.body)) - len([]rune(v.lastBody))
	if err := p.cardKitMutation(ctx, h, http.MethodPut, "/elements/"+v.bodyID+"/content", map[string]any{"content": v.body}); err != nil {
		return err
	}
	v.noteClientText(added, time.Now())
	v.lastBody = v.body
	return nil
}

func (p *Platform) closeSegmentedStream(ctx context.Context, h *feishuPreviewHandle) {
	if err := p.cardKitMutation(ctx, h, http.MethodPatch, "/settings", map[string]any{"settings": `{"config":{"streaming_mode":false}}`}); err != nil {
		slog.Warn("feishu: close preview stream failed; attempting replacement", "error", err)
	}
}

func (p *Platform) replaceSegmentedCard(ctx context.Context, h *feishuPreviewHandle, final bool) error {
	card := h.live.card(final)
	if h.cardID != "" {
		return p.updateCardEntity(ctx, h, card)
	}
	return p.patchCardMessage(ctx, h.messageID, card)
}

// noteClientText updates the model of the client typing queue after a text
// update of n new characters landed. With print_strategy "fast" the client shows
// the older buffered text at once, so only the new characters are left to type.
func (v *segmentedPreview) noteClientText(n int, now time.Time) {
	if v.cfg.PrintStrategy == "fast" {
		v.clientBacklog = 0
	} else if !v.clientAt.IsZero() {
		v.clientBacklog = max(0, v.clientBacklog-v.typingCps()*now.Sub(v.clientAt).Seconds())
	}
	v.clientBacklog += float64(max(n, 0))
	v.clientAt = now
}

// typingCps is the speed the client types at, in characters per second.
func (v *segmentedPreview) typingCps() float64 {
	return float64(v.cfg.TypingStep) * 1000 / float64(v.cfg.TypingIntervalMs)
}

// adoptPending moves text from the event loop into the paced target. Caller holds v.mu.
func (v *segmentedPreview) adoptPending() error {
	v.pendMu.Lock()
	text, dirty := v.pend, v.pendDirty
	v.pendDirty = false
	v.pendMu.Unlock()
	if !dirty {
		return nil
	}
	if !strings.HasPrefix(text, v.target) {
		id, err := previewElementID()
		if err != nil {
			return err
		}
		v.bodyID, v.shown, v.body, v.revealAcc = id, 0, "", 0
	}
	v.target = text
	return nil
}

// reveal releases target text into body at a steady rate: the base speed, raised
// so a backlog clears within RevealCatchupMs, never above the client typing speed.
// Upstream text that arrives in a burst therefore reaches the card as a steady stream.
// Caller holds v.mu.
func (v *segmentedPreview) reveal(now time.Time) {
	if v.cfg.RevealCps <= 0 {
		return
	}
	total := len([]rune(v.target))
	if v.shown >= total {
		v.lastReveal = now
		return
	}
	dt := now.Sub(v.lastReveal).Seconds()
	if v.lastReveal.IsZero() || dt > 1 {
		dt = float64(v.cfg.PushIntervalMs) / 1000
	}
	v.lastReveal = now
	rate := float64(v.cfg.RevealCps)
	if c := float64(total-v.shown) / (float64(v.cfg.RevealCatchupMs) / 1000); c > rate {
		rate = c
	}
	if max := v.typingCps(); rate > max {
		rate = max
	}
	v.revealAcc += rate * dt
	n := int(v.revealAcc)
	v.revealAcc -= float64(n)
	if v.shown+n > total {
		n = total - v.shown
	}
	if n <= 0 {
		return
	}
	v.shown += n
	v.body = string([]rune(v.target)[:v.shown])
	v.lastText = now
}

// textFlowing reports whether visible text grew recently.
func (v *segmentedPreview) textFlowing(now time.Time) bool {
	return !v.lastText.IsZero() && now.Sub(v.lastText) < time.Duration(v.cfg.DotsPauseMs)*time.Millisecond
}

// stripDots drops the running dots at the end of each line.
func stripDots(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, ".")
	}
	return strings.Join(lines, "\n")
}

func (v *segmentedPreview) fullyShown() bool {
	return v.cfg.RevealCps <= 0 || v.shown >= len([]rune(v.target))
}

// Called under the turn lock, as are finalization and cancellation.
func (p *Platform) publishSegmented(ctx context.Context, h *feishuPreviewHandle, now time.Time) error {
	v := h.live
	if v.cfg.RevealCps > 0 {
		if err := v.adoptPending(); err != nil {
			return err
		}
		v.reveal(now)
	}
	if v.fallback && !v.lastPublished.IsZero() && now.Sub(v.lastPublished) < 700*time.Millisecond {
		return nil
	}
	progress := v.progress(now)
	if v.cfg.StepList && progress != v.lastProgress && v.textFlowing(now) && stripDots(progress) == stripDots(v.lastProgress) {
		progress = v.lastProgress // only the dots moved; the text goes first
	}
	footer := v.lastFooter
	if v.cfg.StepList {
		footer = v.footer(now)
	}
	if v.body == v.lastBody && progress == v.lastProgress && footer == v.lastFooter && v.bodyID == v.sentBodyID {
		return nil
	}
	if !v.fallback {
		err := p.streamSegmentedBody(ctx, h)
		if err == nil && progress != v.lastProgress {
			err = p.replaceSegmentedElement(ctx, h, v.progressID, v.progressElement(progress))
		}
		if err == nil && footer != v.lastFooter {
			err = p.replaceSegmentedElement(ctx, h, v.footerID, v.footerElement(footer))
		}
		if err == nil {
			v.lastProgress, v.lastFooter = progress, footer
			v.lastPublished = now
			return nil
		}
		slog.Warn("feishu: preview streaming failed; using full card updates", "error", err)
		p.closeSegmentedStream(ctx, h)
		v.fallback = true
	}
	if err := p.replaceSegmentedCard(ctx, h, false); err != nil {
		return err
	}
	v.lastBody, v.lastProgress, v.lastFooter, v.sentBodyID = v.body, progress, footer, v.bodyID
	v.lastPublished = now
	return nil
}

// drain keeps the push loop running until the paced text has been released, for at
// most DrainMaxMs. It applies when the final text continues what was streamed.
func (v *segmentedPreview) drain(final string) {
	v.mu.Lock()
	if v.stopped || v.fallback || !strings.HasPrefix(final, v.body) || !strings.HasPrefix(final, v.target) {
		v.mu.Unlock()
		return
	}
	v.pendMu.Lock()
	v.pend, v.pendDirty = final, true
	v.pendMu.Unlock()
	v.mu.Unlock()
	deadline := time.Now().Add(time.Duration(v.cfg.DrainMaxMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		v.mu.Lock()
		done := v.stopped || (!v.pendDirtyNow() && v.fullyShown())
		v.mu.Unlock()
		if done {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (v *segmentedPreview) pendDirtyNow() bool {
	v.pendMu.Lock()
	defer v.pendMu.Unlock()
	return v.pendDirty
}

// CatchUpPreview brings the card up to the final reply before it is recalled:
// the rest of the text goes out in one update, then the client gets time to type it,
// up to FinishCatchupMs. It does nothing when the reply does not continue the text
// on the card, or when the card is already complete and typed.
func (p *Platform) CatchUpPreview(ctx context.Context, handle any, final string) {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil || h.cardID == "" {
		return
	}
	v := h.live
	if v.cfg.FinishCatchupMs < 0 {
		return
	}
	if chars := []rune(final); len(chars) > 4000 {
		final = string(chars[:4000])
	}
	v.mu.Lock()
	if v.stopped || v.fallback || final == "" || !strings.HasPrefix(final, v.body) {
		v.mu.Unlock()
		return
	}
	v.stopLocked()
	if final != v.body {
		v.pendMu.Lock()
		v.pend, v.pendDirty = "", false
		v.pendMu.Unlock()
		v.target, v.body, v.shown = final, final, len([]rune(final))
		if err := p.streamSegmentedBody(ctx, h); err != nil {
			slog.Warn("feishu: catch-up update failed", "error", err)
			v.mu.Unlock()
			return
		}
	}
	remaining := v.clientBacklog - v.typingCps()*time.Since(v.clientAt).Seconds()
	wait := time.Duration(remaining / v.typingCps() * float64(time.Second))
	if limit := time.Duration(v.cfg.FinishCatchupMs) * time.Millisecond; wait > limit {
		wait = limit
	}
	v.mu.Unlock()
	if core.StreamTraceOn() {
		core.StreamTrace("catchup", map[string]any{"wait_ms": wait.Milliseconds()})
	}
	if wait < 50*time.Millisecond {
		return
	}
	select {
	case <-time.After(wait):
	case <-ctx.Done():
	}
}

// CollapsePreview settles the card to its step summary, a collapsed "done · N steps"
// panel, with no reply text. Nothing is recalled; the reply follows as a new message.
func (p *Platform) CollapsePreview(ctx context.Context, handle any) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil {
		return core.ErrNotSupported
	}
	v := h.live
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopLocked()
	v.pendMu.Lock()
	v.pend, v.pendDirty = "", false
	v.pendMu.Unlock()
	v.target, v.shown, v.body = "", 0, ""
	if len(v.rows) == 0 {
		v.body = v.i18n.T(core.MsgPreviewDoneBare)
	}
	if !v.fallback {
		p.closeSegmentedStream(ctx, h)
	}
	return p.replaceSegmentedCard(ctx, h, true)
}

func (p *Platform) FinishSegmentedPreview(ctx context.Context, handle any, body string) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil {
		return core.ErrNotSupported
	}
	v := h.live
	if v.cfg.RevealCps > 0 && body != "" {
		v.drain(body)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopLocked()
	if v.cfg.RevealCps > 0 {
		if body != "" {
			v.pendMu.Lock()
			v.pend, v.pendDirty = body, true
			v.pendMu.Unlock()
		}
		if err := v.adoptPending(); err != nil {
			return err
		}
		v.shown, v.body = len([]rune(v.target)), v.target
	} else if body != "" {
		if err := v.setBody(body); err != nil {
			return err
		}
	}
	if !v.fallback {
		if err := p.streamSegmentedBody(ctx, h); err != nil {
			slog.Warn("feishu: final preview stream failed; attempting replacement", "error", err)
		}
		p.closeSegmentedStream(ctx, h)
	}
	return p.replaceSegmentedCard(ctx, h, true)
}
