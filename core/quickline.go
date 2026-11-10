package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// QuickLineCfg configures the one-sentence acknowledgement a fast model writes
// into the streaming card while the main agent is still thinking.
type QuickLineCfg struct {
	Enabled      bool
	BaseURL      string // Anthropic-compatible gateway; the call is POST {BaseURL}/v1/messages
	Model        string // sent as is
	KeyFile      string // read at call time; the value is never logged
	Effort       string // sent as output_config.effort; empty = field not sent
	TimeoutMs    int    // default 5000
	SystemPrompt string // empty = defaultQuickLinePrompt
}

const (
	defaultQuickLineTimeoutMs = 5000
	quickLineMaxTokens        = 80
	quickLineHistoryTurns     = 4
	quickLineTurnRunes        = 200
	quickLineMaxRunes         = 60
)

const defaultQuickLinePrompt = "你是用户的工作助理，现在用第一人称回复用户刚发来的消息。只写一句中文，不超过 40 个字：说明你收到了，并说你先去做什么。" +
	"不要声称已经有任何结果，不要实质性地回答问题，不要提到其他助理或模型，不要加引号或前缀。"

// QuickLineSetter is implemented by platforms whose preview card can show the
// acknowledgement. It must apply only while the main agent has produced nothing
// visible and must go through the card's own update path.
type QuickLineSetter interface {
	SetPreviewQuickLine(ctx context.Context, previewHandle any, text string) error
}

// QuickLineReader is implemented by platforms that keep the sentence on the card. The
// final reply leads with it, so it also shows when the reply goes out as a new message.
type QuickLineReader interface {
	PreviewQuickLine(previewHandle any) string
}

// startQuickLine begins the quick call for a turn whose card is already up.
// It never blocks: the call and the card write run in their own goroutine.
func (e *Engine) startQuickLine(sp *streamPreview, userMsg string, history []HistoryEntry) {
	cfg := e.streamPreview.QuickLine
	if !cfg.Enabled || sp == nil || sp.segmented == nil || sp.hidden || strings.TrimSpace(userMsg) == "" {
		return
	}
	if _, ok := sp.segmented.(QuickLineSetter); !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(e.ctx, quickLineTimeout(cfg))
		defer cancel()
		line, err := fetchQuickLine(ctx, cfg, userMsg, history)
		if err != nil {
			slog.Info("quick line: skipped", "error", err)
			return
		}
		sp.setQuickLine(line)
	}()
}

func quickLineTimeout(cfg QuickLineCfg) time.Duration {
	if cfg.TimeoutMs <= 0 {
		return defaultQuickLineTimeoutMs * time.Millisecond
	}
	return time.Duration(cfg.TimeoutMs) * time.Millisecond
}

// setQuickLine hands the sentence to the platform card, which refuses it when real
// content has already started.
func (sp *streamPreview) setQuickLine(text string) {
	sp.mu.Lock()
	handle, degraded := sp.previewMsgID, sp.degraded
	sp.mu.Unlock()
	setter, ok := sp.segmented.(QuickLineSetter)
	if !ok || handle == nil || degraded {
		return
	}
	if sp.transform != nil {
		text = sp.transform(text)
	}
	if err := setter.SetPreviewQuickLine(sp.ctx, handle, text); err != nil {
		slog.Info("quick line: card write skipped", "error", err)
	}
}

func clipRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// quickLineMessages builds the prompt: the last few turns, then the new message.
func quickLineMessages(userMsg string, history []HistoryEntry) []map[string]string {
	if n := len(history); n > quickLineHistoryTurns {
		history = history[n-quickLineHistoryTurns:]
	}
	var ctxLines []string
	for _, h := range history {
		if strings.TrimSpace(h.Content) == "" {
			continue
		}
		who := "用户"
		if h.Role != "user" {
			who = "助理"
		}
		ctxLines = append(ctxLines, who+"："+clipRunes(h.Content, quickLineTurnRunes))
	}
	content := "用户刚发来的消息：\n" + clipRunes(userMsg, 800)
	if len(ctxLines) > 0 {
		content = "最近的对话：\n" + strings.Join(ctxLines, "\n") + "\n\n" + content
	}
	return []map[string]string{{"role": "user", "content": content}}
}

// fetchQuickLine asks the gateway for the sentence. The key file is read on every call.
func fetchQuickLine(ctx context.Context, cfg QuickLineCfg, userMsg string, history []HistoryEntry) (string, error) {
	key, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return "", fmt.Errorf("read key file: %w", err)
	}
	token := strings.TrimSpace(string(key))
	if token == "" {
		return "", fmt.Errorf("key file is empty")
	}
	system := cfg.SystemPrompt
	if strings.TrimSpace(system) == "" {
		system = defaultQuickLinePrompt
	}
	payload := map[string]any{
		"model":      cfg.Model,
		"max_tokens": quickLineMaxTokens,
		"system":     system,
		"messages":   quickLineMessages(userMsg, history),
	}
	if cfg.Effort != "" {
		payload["output_config"] = map[string]any{"effort": cfg.Effort}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", token)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway HTTP %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	for _, c := range out.Content {
		if c.Type != "text" {
			continue
		}
		line := strings.TrimSpace(strings.Join(strings.Fields(c.Text), " "))
		if line == "" {
			return "", fmt.Errorf("empty text block")
		}
		if r := []rune(line); len(r) > quickLineMaxRunes {
			line = string(r[:quickLineMaxRunes]) + "…"
		}
		return line, nil
	}
	return "", fmt.Errorf("no text block")
}
