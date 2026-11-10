package feishu

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

//go:embed assets/spinner.gif
var spinnerGIF []byte

//go:embed assets/check.gif
var checkGIF []byte

//go:embed assets/blank.gif
var blankGIF []byte

// stepIconKeys are the uploaded images that fill the icon slot of a step row: the running
// spinner, a finished check, and a blank for the "N more done" line. They share one canvas
// and footprint, so every row's text starts at the same left edge.
type stepIconKeys struct{ spinner, check, blank string }

// Frames of the glyph spinner used when the GIF cannot be uploaded.
var spinnerGlyphs = []string{"◐", "◓", "◑", "◒"}

const (
	doneRowsShown = 5
	// draftMaxRunes is the longest single-line text that still counts as a progress sentence.
	// Anything longer, or with a line break, is the reply and goes to the body.
	draftMaxRunes = 60
	// stepRowSpacing is the card body's gap between elements while the step list is live
	// (body.vertical_spacing); replyTopMargin adds the rest of the gap above the reply
	// (markdown margin: top right bottom left), about 10px in all.
	stepRowSpacing      = "4px"
	replyTopMargin      = "6px 0px 0px 0px"
	spinnerRetryBackoff = 5 * time.Minute
)

var _ core.SegmentedPreviewer = (*Platform)(nil)
var _ core.PreviewToolRecorder = (*Platform)(nil)
var _ core.QuickLineSetter = (*Platform)(nil)

// One turn owns the ticker and all mutations, including finalization and recall.
type segmentedPreview struct {
	mu                             sync.Mutex
	stopCh                         chan struct{}
	stopped                        bool
	cfg                            core.StreamPreviewCfg
	i18n                           *core.I18n
	started                        time.Time
	bodyID, sentBodyID, progressID string
	doneID                         string
	body, lastBody, lastProgress   string
	// spinnerKey is the uploaded spinner image; empty means the glyph spinner,
	// whose frame is frame. checkKey and blankKey fill the same slot of finished rows
	// and of the "N more done" line; without them those rows lead with a text glyph.
	spinnerKey    string
	checkKey      string
	blankKey      string
	frame         int
	steps         int
	recent        []string
	fallback      bool
	lastPublished time.Time

	// Step-list layout (cfg.StepList): the agent's step lines with done/running
	// state, the live text under them, and a footer.
	footerID, lastFooter string

	// Paced reveal (cfg.RevealCps > 0): the agent text lands in pend (own lock, so the
	// event loop never waits on a card API call); the push loop adopts it into target and
	// releases it into body at a steady rate.
	pendMu    sync.Mutex
	pend      string
	pendDirty bool
	// pendDraft is the short single-line text being written (StepList only). It is shown
	// nowhere: not in the body, not in the running row. It becomes a finished row when a tool
	// call arrives, or the reply when the text grows or the turn ends. graduated latches once
	// the text has become the reply, until the text is reset to empty. Both are guarded by pendMu.
	pendDraft  string
	graduated  bool
	target     string
	shown      int
	revealAcc  float64
	lastReveal time.Time
	draining   bool

	// lastText is when the visible text last grew. While text is flowing the step
	// list keeps its dots still, so the card API calls go to the text.
	lastText  time.Time
	rows      []stepRow
	toolCalls int
	// quickLine is the fast model's acknowledgement sentence. It shows in the running row
	// only while the agent has produced nothing; the first real content drops it for good.
	quickLine    string
	quickDropped bool
	// draft is the adopted pendDraft; it only marks that the text so far is undecided.
	draft string
	// finishChunk is how many runes each push releases during the end-of-turn reveal
	// (CatchUpPreview); zero outside it.
	finishChunk int
	// callNs is how long the latest card API call took, in nanoseconds.
	callNs atomic.Int64

	// sent is which optional elements the card on the server holds. Empty elements
	// are left out of the card, since each one adds a blank line.
	sent previewLayout
}

// previewLayout names the optional elements of a live card; the spinner row is always there.
// done is the finished-step rows (ID and text each), body whether the reply element is present.
type previewLayout struct {
	done string
	body bool
}

func (v *segmentedPreview) layoutFor(body string) previewLayout {
	var sig strings.Builder
	for _, d := range v.doneItems() {
		sig.WriteString(d.id + "\x00" + d.text + "\x00")
	}
	return previewLayout{done: sig.String(), body: body != ""}
}

type stepRow struct {
	id         string // card element ID of the finished row, set when it first shows
	text       string
	tool       string // latest tool call under this step
	toolOnly   bool   // no step line from the agent; the row is the tool call itself
	start, end time.Time
}

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
		cfg.FinishCatchupMs = finishRevealMs
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
	doneID, err := previewElementID() // the "N more done" line
	if err != nil {
		return nil, err
	}
	v := &segmentedPreview{stopCh: make(chan struct{}), cfg: cfg, i18n: i18n, started: time.Now(), bodyID: bodyID, sentBodyID: bodyID, progressID: progressID, footerID: footerID, doneID: doneID}
	if cfg.StepList {
		icons := p.stepIconKeys(ctx)
		v.spinnerKey, v.checkKey, v.blankKey = icons.spinner, icons.check, icons.blank
	}
	v.sent = v.layoutFor(v.body)
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
		v.pend, v.pendDraft = v.routeText(text)
		v.pendDirty = true
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
	v.pendMu.Lock()
	body, draft := v.routeText(text)
	v.pendMu.Unlock()
	v.setDraft(draft)
	return v.setBody(body)
}

// routeText decides where streamed text shows. In step-list mode a short single line is
// undecided: it may be a progress sentence (a tool call usually follows and turns it into a
// step row) or a short reply. It is held back, shown nowhere, so the running row never flashes
// it. Longer or multi-line text is the reply and goes to the body from then on. Caller holds pendMu.
func (v *segmentedPreview) routeText(text string) (body, draft string) {
	if !v.cfg.StepList {
		return text, ""
	}
	t := strings.TrimSpace(text)
	if t == "" {
		v.graduated = false
		return text, ""
	}
	if !v.graduated && !strings.ContainsAny(t, "\r\n") && len([]rune(t)) <= draftMaxRunes {
		return "", t
	}
	v.graduated = true
	return text, ""
}

// setDraft records the undecided text. Caller holds v.mu.
func (v *segmentedPreview) setDraft(draft string) {
	v.draft = draft
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

// agentQuiet reports whether the agent has shown or held back nothing yet.
// Caller holds v.mu.
func (v *segmentedPreview) agentQuiet() bool {
	if v.body != "" || len(v.rows) > 0 || v.toolCalls > 0 || v.steps > 0 {
		return false
	}
	v.pendMu.Lock()
	defer v.pendMu.Unlock()
	return v.pend == "" && v.pendDraft == "" && v.draft == ""
}

// SetPreviewQuickLine puts the fast model's sentence into the running row of the
// card. The ticker then pushes it through the normal element update, in order with
// every other write. If the agent has already produced content it is dropped.
func (p *Platform) SetPreviewQuickLine(_ context.Context, handle any, text string) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil {
		return core.ErrNotSupported
	}
	v := h.live
	v.mu.Lock()
	defer v.mu.Unlock()
	text = stepOneLine(text, 80)
	if v.stopped || v.quickDropped || text == "" {
		return nil
	}
	if !v.agentQuiet() {
		v.quickDropped = true
		return nil
	}
	v.quickLine = text
	return nil
}

// dropQuickLine latches the sentence off once the agent shows something. Caller holds v.mu.
func (v *segmentedPreview) dropQuickLine() {
	if v.quickLine != "" && !v.agentQuiet() {
		v.quickLine, v.quickDropped = "", true
	}
}

func stepOneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return text
}

// addRow opens a new step for an agent line, or attaches a tool call to the
// open step. A tool call with no agent line before it becomes a step itself.
func (v *segmentedPreview) addRow(now time.Time, text string, tool bool) {
	text = stepOneLine(text, 120)
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

// stepIconKeys uploads the step-row images once per platform and caches their keys. After a
// failed upload it returns empty keys (glyph spinner, text check) and tries again only after a back-off.
func (p *Platform) stepIconKeys(ctx context.Context) stepIconKeys {
	p.spinnerMu.Lock()
	defer p.spinnerMu.Unlock()
	if p.stepIcons.spinner != "" {
		return p.stepIcons
	}
	if !p.spinnerFailed.IsZero() && time.Since(p.spinnerFailed) < spinnerRetryBackoff {
		return stepIconKeys{}
	}
	upload := p.spinnerUpload
	if upload == nil {
		upload = p.uploadImageKey
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var keys stepIconKeys
	var err error
	for _, img := range []struct {
		data []byte
		key  *string
	}{{spinnerGIF, &keys.spinner}, {checkGIF, &keys.check}, {blankGIF, &keys.blank}} {
		if *img.key, err = upload(ctx, img.data); err != nil || *img.key == "" {
			slog.Warn("feishu: step icon upload failed; using glyphs", "error", err)
			p.spinnerFailed = time.Now()
			return stepIconKeys{}
		}
	}
	p.stepIcons = keys
	return keys
}

type doneItem struct {
	id, text string
	summary  bool
}

// doneItems lists what the finished steps show: the last few rows, led by an "N more" line
// when older ones dropped off. Row IDs are assigned on first show and stay with the row.
func (v *segmentedPreview) doneItems() []doneItem {
	if !v.cfg.StepList {
		return nil
	}
	var idx []int
	for i, r := range v.rows {
		if !r.end.IsZero() {
			idx = append(idx, i)
		}
	}
	var items []doneItem
	if len(idx) > doneRowsShown {
		items = append(items, doneItem{id: v.doneID, summary: true, text: fmt.Sprintf("<font color='grey'>另有 %d 步已完成</font>", len(idx)-doneRowsShown)})
		idx = idx[len(idx)-doneRowsShown:]
	}
	for _, i := range idx {
		if v.rows[i].id == "" {
			id, err := previewElementID()
			if err != nil {
				id = fmt.Sprintf("pdone%013d", i)
			}
			v.rows[i].id = id
		}
		items = append(items, doneItem{id: v.rows[i].id, text: v.rows[i].text})
	}
	return items
}

// doneElements renders the finished steps as one markdown element each. With the check image
// each row carries it as its icon, in the slot the running row's spinner uses; without it the
// row leads with a text check.
func (v *segmentedPreview) doneElements() []map[string]any {
	var els []map[string]any
	for _, d := range v.doneItems() {
		el := map[string]any{"tag": "markdown", "element_id": d.id, "content": d.text}
		switch {
		case v.checkKey == "" && !d.summary:
			el["content"] = "<font color='green'>\u2713</font> " + d.text
		case v.checkKey != "":
			key := v.checkKey
			if d.summary {
				key = v.blankKey
			}
			el["icon"] = map[string]any{"tag": "custom_icon", "img_key": key}
		}
		els = append(els, el)
	}
	return els
}

// doneLines is the finished steps as plain text, one line each.
func (v *segmentedPreview) doneLines() string {
	var lines []string
	for _, d := range v.doneItems() {
		if d.summary {
			lines = append(lines, d.text)
		} else {
			lines = append(lines, "<font color='green'>\u2713</font> "+d.text)
		}
	}
	return strings.Join(lines, "\n")
}

// liveLine renders the open step. With the spinner image the client draws the
// spinner as the element's icon; otherwise a glyph leads the text.
func (v *segmentedPreview) liveLine() string {
	text := strings.TrimRight(v.i18n.T(core.MsgPreviewThinking), "…")
	tool := ""
	if n := len(v.rows); n > 0 && v.rows[n-1].end.IsZero() {
		text, tool = v.rows[n-1].text, v.rows[n-1].tool
	}
	if v.quickLine != "" && v.body == "" && len(v.rows) == 0 {
		text = v.quickLine + "\n<font color='grey'>" + text + "</font>"
	}
	if v.spinnerKey == "" {
		text = spinnerGlyphs[v.frame%len(spinnerGlyphs)] + " " + text
	}
	if tool != "" {
		text += "\n<font color='grey'>\u2514 " + tool + "</font>"
	}
	return text
}

// advanceFrame moves the glyph spinner one frame per second, except while the
// reply text is growing, so the card calls go to the text.
func (v *segmentedPreview) advanceFrame(now time.Time) {
	if v.spinnerKey == "" && !v.textFlowing(now) {
		v.frame = secondsBetween(v.started, now) % len(spinnerGlyphs)
	}
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
		return v.liveLine()
	}
	var lines []string
	if v.body == "" {
		if v.quickLine != "" && v.steps == 0 {
			lines = append(lines, v.quickLine)
		}
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
	} else if v.spinnerKey != "" {
		el["icon"] = map[string]any{"tag": "custom_icon", "img_key": v.spinnerKey}
	}
	return el
}

func (v *segmentedPreview) card(final bool) string {
	return v.cardWith(final, v.body, v.layoutFor(v.body))
}

// cardWith renders the card with body as the reply text and the optional elements lay names.
func (v *segmentedPreview) cardWith(final bool, body string, lay previewLayout) string {
	config := map[string]any{"width_mode": "default", "summary": map[string]any{"content": v.i18n.T(core.MsgPreviewThinking)}}
	if !final && !v.fallback {
		platformValues := func(n int) map[string]int { return map[string]int{"default": n, "android": n, "ios": n, "pc": n} }
		config["streaming_mode"] = true
		config["streaming_config"] = map[string]any{"print_frequency_ms": platformValues(v.cfg.TypingIntervalMs), "print_step": platformValues(v.cfg.TypingStep), "print_strategy": v.cfg.PrintStrategy}
	}
	if body != "" {
		config["summary"] = map[string]any{"content": string([]rune(body)[:min(len([]rune(body)), 120)])}
	}
	if v.cfg.StepList {
		return v.stepCard(config, final, body, lay)
	}
	var elements []map[string]any
	if final || lay.body {
		elements = append(elements, map[string]any{"tag": "markdown", "element_id": v.bodyID, "content": body})
	}
	if !final {
		elements = append(elements, v.progressElement(v.progress(time.Now())))
	} else if v.steps > 0 {
		panel := buildRichPanel(v.i18n.Tf(core.MsgPreviewProcess, v.steps), []map[string]any{{"tag": "markdown", "content": strings.Join(v.recent, "\n")}})
		panel["element_id"] = v.progressID
		elements = append(elements, panel)
	}
	b, err := json.Marshal(map[string]any{"schema": "2.0", "config": config, "body": map[string]any{"elements": elements}})
	if err != nil {
		slog.Error("feishu: marshal segmented preview", "error", err)
		return buildCardJSON(body)
	}
	return string(b)
}

// stepCard lays out the step list on top and the text being written under it. Once final, the list folds into a collapsed process panel.
func (v *segmentedPreview) stepCard(config map[string]any, final bool, text string, lay previewLayout) string {
	now := time.Now()
	body := map[string]any{"tag": "markdown", "element_id": v.bodyID, "content": text}
	var elements []map[string]any
	cardBody := map[string]any{}
	if final {
		// Settled cards stay quiet: the reply alone, or a single grey "done" line
		// when the reply goes out as its own message.
		v.closeRow(now)
		if text == "" || text == v.i18n.T(core.MsgPreviewDoneBare) {
			done := v.i18n.T(core.MsgPreviewDoneBare)
			config["summary"] = map[string]any{"content": done}
			body["content"] = "<font color='grey'>" + done + "</font>"
			body["text_size"] = "notation"
		}
	} else {
		elements = append(elements, v.doneElements()...)
		elements = append(elements, v.progressElement(v.liveLine()))
		// Rows sit close like a list; the reply keeps a normal gap above it.
		cardBody["vertical_spacing"] = stepRowSpacing
		body["margin"] = replyTopMargin
	}
	if final || lay.body {
		elements = append(elements, body)
	}
	cardBody["elements"] = elements
	b, err := json.Marshal(map[string]any{"schema": "2.0", "config": config, "body": cardBody})
	if err != nil {
		slog.Error("feishu: marshal segmented preview", "error", err)
		return buildCardJSON(text)
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
	callFrom := time.Now()
	if h.live != nil {
		defer func() { h.live.callNs.Store(int64(time.Since(callFrom))) }()
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

// ensureLayout updates the card structure with one full update when an element has
// to appear or go away, so element-level updates always find their element.
func (p *Platform) ensureLayout(ctx context.Context, h *feishuPreviewHandle) error {
	v := h.live
	want := v.layoutFor(v.body)
	if want == v.sent {
		return nil
	}
	card := v.cardWith(false, "", want)
	if err := p.updateCardEntity(ctx, h, card); err != nil {
		return err
	}
	v.sent, v.sentBodyID, v.lastBody = want, v.bodyID, ""
	v.lastProgress = v.progress(time.Now())
	return nil
}

func (p *Platform) streamSegmentedBody(ctx context.Context, h *feishuPreviewHandle) error {
	v := h.live
	if err := p.ensureLayout(ctx, h); err != nil {
		return err
	}
	if v.sentBodyID != v.bodyID || !strings.HasPrefix(v.body, v.lastBody) {
		if err := p.replaceSegmentedElement(ctx, h, v.sentBodyID, map[string]any{"tag": "markdown", "element_id": v.bodyID, "content": ""}); err != nil {
			return err
		}
		v.sentBodyID, v.lastBody = v.bodyID, ""
	}
	if v.body == v.lastBody {
		return nil
	}
	if err := p.cardKitMutation(ctx, h, http.MethodPut, "/elements/"+v.bodyID+"/content", map[string]any{"content": v.body}); err != nil {
		return err
	}
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

// typingCps is the speed the client types at, in characters per second.
func (v *segmentedPreview) typingCps() float64 {
	return float64(v.cfg.TypingStep) * 1000 / float64(v.cfg.TypingIntervalMs)
}

// adoptPending moves text from the event loop into the paced target. Caller holds v.mu.
func (v *segmentedPreview) adoptPending() error {
	v.pendMu.Lock()
	text, draft, dirty := v.pend, v.pendDraft, v.pendDirty
	v.pendDirty = false
	v.pendMu.Unlock()
	if !dirty {
		return nil
	}
	v.setDraft(draft)
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
	if v.finishChunk > 0 {
		v.lastReveal = now
		v.shown += min(v.finishChunk, total-v.shown)
		v.body = string([]rune(v.target)[:v.shown])
		v.lastText = now
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
	v.dropQuickLine()
	if v.fallback && !v.lastPublished.IsZero() && now.Sub(v.lastPublished) < 700*time.Millisecond {
		return nil
	}
	if v.cfg.StepList {
		v.advanceFrame(now)
	}
	progress := v.progress(now)
	footer := v.lastFooter
	if v.cfg.StepList {
		footer = v.footer(now)
	}
	if v.body == v.lastBody && progress == v.lastProgress && footer == v.lastFooter && v.bodyID == v.sentBodyID && v.layoutFor(v.body) == v.sent {
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
	v.sent = v.layoutFor(v.body)
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
	v.pendDraft, v.draft = "", ""
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

// draftPending reports whether progress-line text has been streamed and not yet turned into the reply.
func (v *segmentedPreview) draftPending() bool {
	v.pendMu.Lock()
	defer v.pendMu.Unlock()
	return v.draft != "" || v.pendDraft != ""
}

func (v *segmentedPreview) pendDirtyNow() bool {
	v.pendMu.Lock()
	defer v.pendMu.Unlock()
	return v.pendDirty
}

// finishRevealMs is the longest the end-of-turn reveal takes.
const finishRevealMs = 1000

// CatchUpPreview brings the card up to the final reply before it settles or is recalled:
// the unrevealed rest goes out in steady steps that end within FinishCatchupMs (at most
// finishRevealMs) of the call, instead of one jump, and without waiting for the client to
// type it. It does nothing when the reply does not continue the text on the card, or when
// the card already shows it all.
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
	// A reply that is still just a progress line is held back, not in the body; the card is
	// about to settle, so it is not typed into the body first.
	if v.cfg.StepList && v.body == "" && v.draftPending() {
		v.mu.Unlock()
		return
	}
	v.stopLocked()
	v.pendMu.Lock()
	v.pend, v.pendDirty = "", false
	v.pendDraft, v.draft = "", ""
	v.pendMu.Unlock()
	start := time.Now()
	budget := min(time.Duration(v.cfg.FinishCatchupMs), finishRevealMs) * time.Millisecond
	steps := 0
	if final != v.body && v.cfg.RevealCps <= 0 {
		v.target, v.body, v.shown = final, final, len([]rune(final))
		if err := p.streamSegmentedBody(ctx, h); err != nil {
			slog.Warn("feishu: catch-up update failed", "error", err)
		}
	} else if final != v.body {
		v.target = final
		total := len([]rune(final))
		step := time.Duration(min(v.cfg.PushIntervalMs, 150)) * time.Millisecond
		for v.shown < total && ctx.Err() == nil {
			// Even steps that fit the time left, given how long one card call takes.
			per := max(step, time.Duration(v.callNs.Load()))
			n := max(1, int((budget-time.Since(start)+per/2)/per))
			v.finishChunk = (total - v.shown + n - 1) / n
			tick := time.Now()
			if err := p.publishSegmented(ctx, h, tick); err != nil {
				slog.Warn("feishu: catch-up update failed", "error", err)
				break
			}
			steps++
			if v.shown >= total {
				break
			}
			v.mu.Unlock()
			time.Sleep(step - time.Since(tick)) // a card call that outlasted the step needs no pause
			v.mu.Lock()
		}
		v.finishChunk = 0
		if v.shown < total || v.body != v.lastBody {
			v.shown, v.body = total, final
			if err := p.streamSegmentedBody(ctx, h); err != nil {
				slog.Warn("feishu: catch-up flush failed", "error", err)
			}
		}
	}
	v.mu.Unlock()
	if core.StreamTraceOn() {
		core.StreamTrace("catchup", map[string]any{"wait_ms": time.Since(start).Milliseconds(), "steps": steps})
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
	v.pendDraft, v.draft = "", ""
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
			v.pendDraft, v.draft = "", ""
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
