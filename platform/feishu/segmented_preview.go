package feishu

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

var _ core.SegmentedPreviewer = (*Platform)(nil)

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
	bodyID, err := previewElementID()
	if err != nil {
		return nil, err
	}
	progressID, err := previewElementID()
	if err != nil {
		return nil, err
	}
	v := &segmentedPreview{stopCh: make(chan struct{}), cfg: cfg, i18n: i18n, started: time.Now(), bodyID: bodyID, sentBodyID: bodyID, progressID: progressID}
	v.lastProgress = v.progress(v.started)
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
			if v.stopped || (thinkingOnly && v.body != "") {
				return
			}
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
	v.mu.Lock()
	defer v.mu.Unlock()
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

func (v *segmentedPreview) progress(now time.Time) string {
	var lines []string
	if v.body == "" {
		lines = append(lines, v.i18n.Tf(core.MsgPreviewElapsed, int(now.Sub(v.started)/time.Second)))
	}
	if v.steps > 0 {
		lines = append(lines, v.i18n.Tf(core.MsgPreviewSteps, v.steps))
		lines = append(lines, v.recent...)
	}
	return strings.Join(lines, "\n")
}

func (v *segmentedPreview) progressElement(content string) map[string]any {
	return map[string]any{"tag": "markdown", "element_id": v.progressID, "content": content, "text_size": "notation"}
}

func (v *segmentedPreview) card(final bool) string {
	config := map[string]any{"width_mode": "fill", "summary": map[string]any{"content": v.i18n.T(core.MsgPreviewThinking)}}
	if !final && !v.fallback {
		platformValues := func(n int) map[string]int { return map[string]int{"default": n, "android": n, "ios": n, "pc": n} }
		config["streaming_mode"] = true
		config["streaming_config"] = map[string]any{"print_frequency_ms": platformValues(v.cfg.TypingIntervalMs), "print_step": platformValues(v.cfg.TypingStep), "print_strategy": "fast"}
	}
	if v.body != "" {
		config["summary"] = map[string]any{"content": string([]rune(v.body)[:min(len([]rune(v.body)), 120)])}
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

// cardKitMutation shares the sequence with the existing rich-card path, holding
// the handle lock through the HTTP request so operations cannot overtake it.
func (p *Platform) cardKitMutation(ctx context.Context, h *feishuPreviewHandle, method, suffix string, body map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sequence++
	body["sequence"] = h.sequence
	path := "/open-apis/cardkit/v1/cards/" + h.cardID + suffix
	var resp *larkcore.ApiResp
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

// Called under the turn lock, as are finalization and cancellation.
func (p *Platform) publishSegmented(ctx context.Context, h *feishuPreviewHandle, now time.Time) error {
	v := h.live
	if v.fallback && !v.lastPublished.IsZero() && now.Sub(v.lastPublished) < 700*time.Millisecond {
		return nil
	}
	progress := v.progress(now)
	if v.body == v.lastBody && progress == v.lastProgress && v.bodyID == v.sentBodyID {
		return nil
	}
	if !v.fallback {
		err := p.streamSegmentedBody(ctx, h)
		if err == nil && progress != v.lastProgress {
			err = p.replaceSegmentedElement(ctx, h, v.progressID, v.progressElement(progress))
		}
		if err == nil {
			v.lastProgress = progress
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
	v.lastBody, v.lastProgress, v.sentBodyID = v.body, progress, v.bodyID
	v.lastPublished = now
	return nil
}

func (p *Platform) FinishSegmentedPreview(ctx context.Context, handle any, body string) error {
	h, ok := handle.(*feishuPreviewHandle)
	if !ok || h.live == nil {
		return core.ErrNotSupported
	}
	v := h.live
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopLocked()
	if body != "" {
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
