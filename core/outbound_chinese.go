package core

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Chinese outbound: in a Chinese chat, a reply whose prose is mostly English
// (the agent tends to follow an English system event or background
// notification) is rewritten into Simplified Chinese by the quick model before
// it goes out. Code, links, paths and file names never reach the model: they
// are swapped for placeholders and put back afterwards. Any failure, a timeout,
// or a rewrite that loses a placeholder sends the original text.

const (
	outboundMinLatin       = 40               // fewer Latin letters than this is never rewritten
	outboundLatinPerHan    = 2.5              // rewritten when Latin letters exceed this many per Han character
	outboundLiveTimeout    = 5 * time.Second  // a reply the person is waiting on
	outboundLaterTimeout   = 25 * time.Second // background follow-ups, wake-ups, timers, send: nobody is watching the clock
	outboundMaxRunes       = 1500             // a longer text would not come back within the timeout
	outboundMaxTokens      = 4000
	humanEnglishMinLetters = 6
)

const outboundChinesePrompt = "把用户给出的内容改写成简体中文，给中文读者看。保持原意和 Markdown 格式（标题、列表、表格、加粗），不增不减。" +
	"⟦1⟧、⟦2⟧ 这样的占位符代表代码、链接或文件名，每个都原样保留一次，放在译文中对应的位置。" +
	"命令、参数、函数名、变量名、产品名和缩写保持英文原样。只输出改写后的正文，不要加任何说明或前缀。"

// outboundProtected lists what never reaches the quick model. A pattern with a
// group protects only the group; the text before it is a boundary.
var outboundProtected = []*regexp.Regexp{
	regexp.MustCompile("(?s)```.*?```"),
	regexp.MustCompile("(?s)```.*$"),
	regexp.MustCompile("`[^`\n]+`"),
	regexp.MustCompile(`!?\[[^\]\n]*\]\([^)\s]+\)`),
	regexp.MustCompile(`<?(?:https?|ftp)://[^\s<>()\x{3000}-\x{303f}\x{ff00}-\x{ffef}]+>?`),
	regexp.MustCompile(`(?m)(?:^|[\s(（"'“：:，,=])((?:~|\.{1,2})?/[\w.~@+%-]+(?:/[\w.~@+%-]*)*)`),
	regexp.MustCompile(`\b[A-Za-z]:\\[^\s]+`),
	regexp.MustCompile(`\b[\w-]+(?:\.[\w-]+)*\.[A-Za-z][A-Za-z0-9]{0,7}\b`),
	regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]+\b`),
}

var outboundPlaceholder = regexp.MustCompile(`⟦(\d+)⟧`)

// maskOutbound replaces code, links, paths and file names with ⟦n⟧ and
// returns the masked text and the original spans, spans[n-1] for ⟦n⟧.
func maskOutbound(text string) (string, []string) {
	var spans []string
	for _, re := range outboundProtected {
		var b strings.Builder
		last := 0
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[0], m[1]
			if len(m) >= 4 && m[2] >= 0 {
				start, end = m[2], m[3]
			}
			spans = append(spans, text[start:end])
			b.WriteString(text[last:start])
			fmt.Fprintf(&b, "⟦%d⟧", len(spans))
			last = end
		}
		b.WriteString(text[last:])
		text = b.String()
	}
	return text, spans
}

// unmaskOutbound puts the spans back. It reports false unless every
// placeholder appears exactly once, so a rewrite that dropped or duplicated
// code is never sent.
func unmaskOutbound(text string, spans []string) (string, bool) {
	seen := make([]int, len(spans))
	ok := true
	out := outboundPlaceholder.ReplaceAllStringFunc(text, func(m string) string {
		var n int
		if _, err := fmt.Sscanf(m, "⟦%d⟧", &n); err != nil || n < 1 || n > len(spans) {
			ok = false
			return m
		}
		seen[n-1]++
		return spans[n-1]
	})
	for _, c := range seen {
		if c != 1 {
			ok = false
		}
	}
	return out, ok
}

// letterCounts counts Latin letters and Han characters in prose, with
// placeholders already removed.
func letterCounts(text string) (latin, han int) {
	for _, r := range outboundPlaceholder.ReplaceAllString(text, " ") {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
		case r < 128 && unicode.IsLetter(r):
			latin++
		}
	}
	return latin, han
}

// mostlyLatin reports whether masked prose is English enough to rewrite.
func mostlyLatin(masked string) bool {
	latin, han := letterCounts(masked)
	return latin > outboundMinLatin && float64(latin) > outboundLatinPerHan*float64(han)
}

// needsChineseRewrite reports whether text, once code, links and paths are
// set aside, is mostly English.
func needsChineseRewrite(text string) bool {
	masked, _ := maskOutbound(text)
	return mostlyLatin(masked)
}

// isEnglishMessage reports whether a person wrote in English: no Chinese at
// all and at least a few Latin letters, code and links aside.
func isEnglishMessage(text string) bool {
	masked, _ := maskOutbound(text)
	latin, han := letterCounts(masked)
	return han == 0 && latin >= humanEnglishMinLetters
}

// noteHumanLanguage remembers whether the person's latest message in the
// session was English, under every key the session's replies may use.
func (e *Engine) noteHumanLanguage(msg *Message, keys ...string) {
	content := strings.TrimSpace(msg.Content)
	if content == "" || strings.HasPrefix(content, "/") || !isHumanTurn(msg) {
		return
	}
	english := isEnglishMessage(content)
	for _, k := range keys {
		if k != "" {
			e.humanEnglish.Store(k, english)
		}
	}
}

func (e *Engine) humanWroteEnglish(sessionKey string) bool {
	v, ok := e.humanEnglish.Load(sessionKey)
	return ok && v.(bool)
}

// chineseOutbound returns text, rewritten into Simplified Chinese when the
// chat is Chinese and the text's prose is mostly English. It never fails: on
// any problem the original text is returned. live is a reply to a message the
// person just sent; it gets the short limit, everything else the long one.
func (e *Engine) chineseOutbound(sessionKey, text string, live bool) string {
	cfg := e.streamPreview.QuickLine
	if !cfg.Enabled || e.i18n.CurrentLang() != LangChinese || strings.TrimSpace(text) == "" {
		return text
	}
	masked, spans := maskOutbound(text)
	if !mostlyLatin(masked) {
		return text
	}
	reason := ""
	switch {
	case e.humanWroteEnglish(sessionKey):
		reason = "person_wrote_english"
	case utf8.RuneCountInString(masked) > outboundMaxRunes:
		reason = "too_long"
	}
	if reason != "" {
		outboundLog(sessionKey, "skipped", reason, 0)
		return text
	}
	timeout := outboundLaterTimeout
	if live {
		timeout = quickLineTimeout(cfg)
		if timeout > outboundLiveTimeout {
			timeout = outboundLiveTimeout
		}
	}
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	start := time.Now()
	maxTokens := min(outboundMaxTokens, 2*utf8.RuneCountInString(masked)+200)
	out, err := fetchQuickReply(ctx, cfg, outboundChinesePrompt, []map[string]string{{"role": "user", "content": masked}}, maxTokens)
	took := time.Since(start)
	if err != nil {
		slog.Info("outbound: chinese rewrite failed", "session", sessionKey, "error", err, "elapsed", took)
		outboundLog(sessionKey, "kept", "rewrite_failed", took)
		return text
	}
	out = strings.TrimSpace(out)
	if mostlyLatin(out) {
		outboundLog(sessionKey, "kept", "rewrite_not_chinese", took)
		return text
	}
	rewritten, ok := unmaskOutbound(out, spans)
	if !ok {
		outboundLog(sessionKey, "kept", "placeholder_lost", took)
		return text
	}
	outboundLog(sessionKey, "rewritten", "mostly_english", took)
	return rewritten
}

func outboundLog(sessionKey, result, reason string, took time.Duration) {
	slog.Info("outbound: chinese", "session", sessionKey, "result", result, "reason", reason, "elapsed", took)
	if StreamTraceOn() {
		StreamTrace("outbound_chinese", map[string]any{"session": sessionKey, "result": result, "reason": reason, "elapsed_ms": took.Milliseconds()})
	}
}
