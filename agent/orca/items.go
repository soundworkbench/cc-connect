package orca

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/chenhg5/cc-connect/core"
)

type itemRoute struct {
	Number int
	ChatID string
	Title  string
}

// itemTracker bridges Orca `needs_you` items into cc-connect's native
// AskUserQuestion / Decision Card pipeline. When [decision] is enabled in
// cc-connect, core.Engine converts EventPermissionRequest(AskUserQuestion)
// into non-blocking Feishu decision cards and later delivers the user's pick
// back through Session.Send("【决定】[#N] ..."); when [decision] is disabled,
// core.Engine uses the blocking AskQuestion card and calls RespondPermission.
// Both paths are handled seamlessly.
type itemTracker struct {
	mu             sync.Mutex
	baselined      bool
	seenEvent      map[int]string       // itemNumber -> last surfaced eventId
	pendingReq     map[string]itemRoute // requestID -> route
	itemRoutes     map[int]itemRoute    // itemNumber -> route
	questionRoutes map[string]itemRoute // normalized question -> route
}

func newItemTracker() *itemTracker {
	return &itemTracker{
		seenEvent:      make(map[int]string),
		pendingReq:     make(map[string]itemRoute),
		itemRoutes:     make(map[int]itemRoute),
		questionRoutes: make(map[string]itemRoute),
	}
}

func (it *itemTracker) baseline(ctx context.Context, client *orcaClient, botID string) {
	items, err := client.listNeedsYouItems(ctx, botID)
	if err != nil {
		slog.Debug("orca: needs-you baseline skipped", "bot", botID, "error", err)
		return
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	it.baselined = true
	for _, item := range items {
		it.seenEvent[item.Number] = itemEventID(&item)
		it.itemRoutes[item.Number] = itemRoute{
			Number: item.Number,
			ChatID: itemChatID(&item),
			Title:  item.Title,
		}
	}
}

func (it *itemTracker) poll(ctx context.Context, client *orcaClient, botID string, emit func(core.Event)) {
	summaries, err := client.listNeedsYouItems(ctx, botID)
	if err != nil {
		slog.Debug("orca: poll needs-you failed", "bot", botID, "error", err)
		return
	}

	it.mu.Lock()
	if !it.baselined {
		it.baselined = true
		for _, item := range summaries {
			it.seenEvent[item.Number] = itemEventID(&item)
			it.itemRoutes[item.Number] = itemRoute{
				Number: item.Number,
				ChatID: itemChatID(&item),
				Title:  item.Title,
			}
		}
		it.mu.Unlock()
		return
	}
	var toFetch []orcaItem
	for _, s := range summaries {
		evID := itemEventID(&s)
		if prev, ok := it.seenEvent[s.Number]; ok && prev == evID {
			continue
		}
		toFetch = append(toFetch, s)
	}
	it.mu.Unlock()

	for _, summary := range toFetch {
		detail, err := client.showItem(ctx, summary.Number)
		if err != nil {
			slog.Warn("orca: show needs-you item failed", "item", summary.Number, "error", err)
			continue
		}
		if detail.Status != "needs_you" {
			continue
		}
		evID := itemEventID(detail)
		questions := buildUserQuestions(detail)

		it.mu.Lock()
		if prev, ok := it.seenEvent[detail.Number]; ok && prev == evID {
			it.mu.Unlock()
			continue
		}
		it.seenEvent[detail.Number] = evID
		route := itemRoute{
			Number: detail.Number,
			ChatID: itemChatID(detail),
			Title:  detail.Title,
		}
		it.itemRoutes[detail.Number] = route
		if len(questions) == 0 {
			it.mu.Unlock()
			continue
		}
		reqID := fmt.Sprintf("orca-item:%d:%s", detail.Number, evID)
		it.pendingReq[reqID] = route
		for _, q := range questions {
			it.questionRoutes[strings.TrimSpace(q.Question)] = route
		}
		it.mu.Unlock()

		emit(core.Event{
			Type:      core.EventPermissionRequest,
			RequestID: reqID,
			ToolName:  "AskUserQuestion",
			ToolInput: questions[0].Question,
			Questions: questions,
		})
	}
}

func (it *itemTracker) consumePending(requestID string) (itemRoute, bool) {
	it.mu.Lock()
	defer it.mu.Unlock()
	r, ok := it.pendingReq[requestID]
	if ok {
		delete(it.pendingReq, requestID)
	}
	return r, ok
}

var decisionItemTagRe = regexp.MustCompile(`^【(?:决定|到期默认)】\s*\[#(\d+)\]`)

// matchDecisionPrompt inspects an inbound prompt delivered by cc-connect's
// Decision Card engine ("【决定】[#12] 问题：选项") so Session.Send can route
// it directly to Orca Item #12 with `--item 12 --type handoff`.
func (it *itemTracker) matchDecisionPrompt(prompt string) (itemRoute, bool) {
	trimmed := strings.TrimSpace(prompt)
	if m := decisionItemTagRe.FindStringSubmatch(trimmed); len(m) == 2 {
		if num, err := strconv.Atoi(m[1]); err == nil && num > 0 {
			it.mu.Lock()
			r, ok := it.itemRoutes[num]
			it.mu.Unlock()
			if ok {
				return r, true
			}
			return itemRoute{Number: num}, true
		}
	}
	// Fallback match by exact stored question prefix.
	for _, prefix := range []string{"【决定】", "【到期默认】"} {
		if strings.HasPrefix(trimmed, prefix) {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
			if q, _, ok := strings.Cut(rest, "："); ok {
				it.mu.Lock()
				r, found := it.questionRoutes[strings.TrimSpace(q)]
				it.mu.Unlock()
				if found {
					return r, true
				}
			}
		}
	}
	return itemRoute{}, false
}

func itemEventID(item *orcaItem) string {
	if item == nil {
		return ""
	}
	if item.Question != nil && strings.TrimSpace(item.Question.EventID) != "" {
		return strings.TrimSpace(item.Question.EventID)
	}
	if strings.TrimSpace(item.LastEventID) != "" {
		return strings.TrimSpace(item.LastEventID)
	}
	return fmt.Sprintf("v%d", item.Version)
}

func itemChatID(item *orcaItem) string {
	if item != nil && item.Chat != nil {
		return strings.TrimSpace(item.Chat.ID)
	}
	return ""
}

func buildUserQuestions(item *orcaItem) []core.UserQuestion {
	if item == nil || item.Question == nil || len(item.Question.Decisions) == 0 {
		return nil
	}
	header := fmt.Sprintf("#%d", item.Number)
	if t := strings.TrimSpace(item.Title); t != "" {
		header = fmt.Sprintf("#%d %s", item.Number, oneLineClip(t, 40))
	}
	gist := strings.TrimSpace(item.Question.Gist)
	if gist == "" {
		gist = strings.TrimSpace(item.Question.Text)
	}

	var out []core.UserQuestion
	for idx, d := range item.Question.Decisions {
		if len(d.Replies) == 0 {
			continue
		}
		qText := strings.TrimSpace(d.Question)
		if qText == "" {
			qText = strings.TrimSpace(item.Title)
		}
		if qText == "" {
			qText = fmt.Sprintf("请确认事项 #%d", item.Number)
		}
		// Tag with [#N] so both the reader and cc-connect decision return path
		// know which Orca Item this question belongs to.
		taggedQuestion := fmt.Sprintf("[#%d] %s", item.Number, qText)
		if idx == 0 && gist != "" && !strings.Contains(qText, gist) {
			taggedQuestion = fmt.Sprintf("[#%d] %s（%s）", item.Number, qText, oneLineClip(gist, 80))
		}

		opts := make([]core.UserQuestionOption, 0, len(d.Replies))
		for _, r := range d.Replies {
			label := strings.TrimSpace(r.Label)
			if label == "" {
				continue
			}
			desc := ""
			if r.Recommended {
				desc = "推荐"
			}
			opts = append(opts, core.UserQuestionOption{
				Label:       label,
				Description: desc,
			})
		}
		if len(opts) == 0 {
			continue
		}
		out = append(out, core.UserQuestion{
			Header:   header,
			Question: taggedQuestion,
			Options:  opts,
		})
	}
	return out
}
