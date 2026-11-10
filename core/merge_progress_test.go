package core

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMergeProgressFinalRegression(t *testing.T) {
	for _, order := range []FinalReplyOrder{FinalReplyRecallFirst, FinalReplySendFirst} {
		for _, tc := range []struct {
			name, progress, final string
			merge                 bool
		}{
			{"long", strings.Repeat("文", 400), "完成", true},
			{"boundary", strings.Repeat("文", 399), "完成", false},
			{"short", "先查代码，再验证。", "完成", false},
			{"table", "| 项目 | 结果 |\n|---|---|\n| A | B |", "完成", true},
			{"spaced_table", "| A | B |\n| --- | --- |", "完成", true},
			{"contained", strings.Repeat("文", 400), strings.Repeat("文", 400) + "\n完成", false},
		} {
			t.Run(string(order)+"/"+tc.name, func(t *testing.T) {
				p := &finalOrderPlatform{order: order}
				sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, context.Background(), nil)
				sp.startPlaceholder("思考中")
				sp.appendProgressEvent(Event{Type: EventProgress}, tc.progress)
				if !sp.deliverFinal(tc.final, "footer", func(_ Platform, _ any, text string) error { p.record("final:" + text); return nil }, "失败") {
					t.Fatal("delivery failed")
				}
				calls := p.snapshot()
				var sent string
				var sendIndex, deleteIndex int
				for i, c := range calls {
					if strings.HasPrefix(c, "final:") {
						sent += strings.TrimPrefix(c, "final:")
						sendIndex = i
					}
					if strings.HasPrefix(c, "delete:") {
						deleteIndex = i
					}
				}
				if strings.Contains(sent, "（中途内容）") != tc.merge {
					t.Fatalf("merge=%v sent=%q", tc.merge, sent)
				}
				if tc.merge && !strings.Contains(sent, tc.progress) {
					t.Fatal("lost progress")
				}
				if strings.Count(sent, tc.progress) != 1 && (tc.merge || tc.name == "contained") {
					t.Fatal("duplicated progress")
				}
				if !strings.HasSuffix(sent, "*footer*") {
					t.Fatal("lost footer")
				}
				if (order == FinalReplyRecallFirst) != (deleteIndex < sendIndex) {
					t.Fatal(calls)
				}
			})
		}
	}
}

type progressMergePlatform struct {
	finalOrderPlatform
	cfg ProgressMergeCfg
}

func (p *progressMergePlatform) ProgressMergeConfig() ProgressMergeCfg { return p.cfg }

func TestMergeProgressRetentionAndPolicy(t *testing.T) {
	for _, order := range []FinalReplyOrder{FinalReplyRecallFirst, FinalReplySendFirst} {
		for _, mode := range []string{"folded", "short_many", "disabled", "custom_threshold", "no_tables", "partial", "private", "detached"} {
			t.Run(string(order)+"/"+mode, func(t *testing.T) {
				cfg := DefaultProgressMergeCfg()
				if mode == "disabled" {
					cfg.Enabled = false
				}
				if mode == "custom_threshold" {
					cfg.MinChars = 5
				}
				if mode == "no_tables" {
					cfg.Tables = false
				}
				p := &progressMergePlatform{finalOrderPlatform: finalOrderPlatform{order: order}, cfg: cfg}
				sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, context.Background(), nil)
				sp.startPlaceholder("思考中")
				text := strings.Repeat("🙂", 400)
				body := "完成"
				want := true
				switch mode {
				case "short_many":
					text = "先查代码。"
					want = false
				case "disabled":
					want = false
				case "custom_threshold":
					text = "有五个字符"
				case "no_tables":
					text = "| A | B |\n|---|---|"
					want = false
				case "partial":
					body = text + "\n完成"
				case "private":
					want = false
				}
				event := Event{Type: EventProgress, ProgressID: "main"}
				if mode == "private" {
					event.Type = EventThinking
				}
				sp.appendProgressEvent(event, text)
				if mode == "folded" || mode == "short_many" {
					for i := 0; i < 1000; i++ {
						sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: fmt.Sprint(i)}, "先查代码，再验证行为。")
					}
					if mode == "folded" && sp.progressFolded == 0 {
						t.Fatal("fixture did not fold")
					}
				}
				if mode == "partial" {
					sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "second"}, "| A | B |\n| --- | --- |")
					sp.appendProgressEvent(Event{Type: EventProgress, ProgressID: "duplicate"}, text)
				}
				if mode == "detached" {
					sp = sp.detachForFinal()
				}
				var sent string
				if !sp.deliverFinal(body, "", func(_ Platform, _ any, text string) error { sent += text; return nil }, "失败") {
					t.Fatal("send failed")
				}
				if strings.Contains(sent, "（中途内容）") != want {
					t.Fatal(sent)
				}
				if want && !strings.Contains(sent, text) {
					t.Fatal("lost segment")
				}
				if mode == "partial" && (strings.Count(sent, text) != 1 || !strings.Contains(sent, "| --- |")) {
					t.Fatal("partial dedup")
				}
			})
		}
	}
}

func TestMergeProgressMultipartRecovery(t *testing.T) {
	for _, order := range []FinalReplyOrder{FinalReplyRecallFirst, FinalReplySendFirst} {
		p := &finalOrderPlatform{order: order}
		sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, context.Background(), nil)
		sp.startPlaceholder("思考中")
		progress := "```text\n" + strings.Repeat("x", maxPlatformMessageLen+500) + "\n```"
		sp.appendProgressEvent(Event{Type: EventProgress}, progress)
		body := "（中途内容）\n\n" + progress + "\n\n完成"
		expected := SplitMessageCodeFenceAware(body, maxPlatformMessageLen)
		expected[len(expected)-1] = appendReplyFooter(expected[len(expected)-1], "footer")
		var delivered []string
		calls := 0
		ok := sp.deliverFinal("完成", "footer", func(_ Platform, _ any, text string) error {
			calls++
			if calls == 2 || calls == 3 {
				return fmt.Errorf("fixture rejected")
			}
			delivered = append(delivered, text)
			return nil
		}, "失败")
		if !ok {
			t.Fatal("plain recovery failed")
		}
		for _, c := range p.snapshot() {
			if strings.HasPrefix(c, "plain:") {
				delivered = append(delivered, strings.TrimPrefix(c, "plain:"))
			}
		}
		if !reflect.DeepEqual(delivered, expected) {
			t.Fatalf("chunks differ: got %d want %d", len(delivered), len(expected))
		}
	}
}

func TestMergeProgressPartialFallbackPreservesSuffix(t *testing.T) {
	for _, order := range []FinalReplyOrder{FinalReplyRecallFirst, FinalReplySendFirst} {
		for _, failFrom := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d", order, failFrom), func(t *testing.T) {
				p := &finalOrderPlatform{order: order, failSends: 2, plainFailures: 2, plainFailFrom: failFrom}
				e := newFinalOrderEngine(t, p, &stubAgent{})
				sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, e.ctx, nil)
				sp.startPlaceholder("思考中")
				progress := strings.Repeat("x", maxPlatformMessageLen*2+100)
				sp.appendProgressEvent(Event{Type: EventProgress}, progress)
				chunks := SplitMessageCodeFenceAware("（中途内容）\n\n"+progress+"\n\n完成", maxPlatformMessageLen)
				if sp.deliverFinal("完成", "footer", e.sendWithError, "失败") {
					t.Fatal("expected failed fallback")
				}
				var recovered []string
				deletes := 0
				for _, call := range p.snapshot() {
					if strings.HasPrefix(call, "delete:") {
						deletes++
					}
					if strings.HasPrefix(call, "update:") {
						recovered = append(recovered, strings.TrimPrefix(call, "update:"))
					}
					if strings.HasPrefix(call, "start:") && call != "start:思考中" {
						recovered = append(recovered, strings.TrimPrefix(call, "start:"))
					}
				}
				if order == FinalReplySendFirst && deletes != 0 {
					t.Fatal("recalled recovery card")
				}
				var want []string
				for i := failFrom - 1; i < len(chunks); i++ {
					text := chunks[i]
					if i == len(chunks)-1 {
						text = appendReplyFooter(text, "footer")
					}
					want = append(want, appendReplyFooter(text, "失败"))
				}
				if !reflect.DeepEqual(recovered, want) {
					t.Fatalf("got %d recovery segments want %d", len(recovered), len(want))
				}
			})
		}
	}
}

func TestMergeProgressEngineResult(t *testing.T) {
	for _, order := range []FinalReplyOrder{FinalReplyRecallFirst, FinalReplySendFirst} {
		p := &finalOrderPlatform{order: order}
		e := newFinalOrderEngine(t, p, &stubAgent{})
		as := newControllableSession("merge")
		state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
		progress := strings.Repeat("文", 400)
		as.events <- Event{Type: EventProgress, Content: progress}
		as.events <- Event{Type: EventToolUse, ToolName: "Read", ToolInput: "PRIVATE_ARGS"}
		as.events <- Event{Type: EventResult, Content: "完成", Done: true}
		e.processInteractiveEvents(state, e.sessions.GetOrCreateActive("merge"), e.sessions, "merge", "m", time.Now(), nil, nil, "ctx", 0)
		var sent string
		for _, c := range p.snapshot() {
			if strings.HasPrefix(c, "send:") {
				sent += c
			}
		}
		if !strings.Contains(sent, "（中途内容）") || !strings.Contains(sent, progress) || !strings.Contains(sent, "完成") || strings.Contains(sent, "PRIVATE_ARGS") {
			t.Fatal(sent)
		}
	}
}

func TestMergeProgressPublicToolSummary(t *testing.T) {
	for _, order := range []FinalReplyOrder{FinalReplyRecallFirst, FinalReplySendFirst} {
		for _, long := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", order, long), func(t *testing.T) {
				p := &finalOrderPlatform{order: order}
				sp := newStreamPreview(DefaultStreamPreviewCfg(), p, nil, context.Background(), nil)
				sp.startPlaceholder("思考中")
				summary := "先查文件。"
				if long {
					summary = strings.Repeat("公开摘要", 100)
				}
				event := Event{Type: EventToolUse, ToolName: "Read", ToolInput: strings.Repeat("PRIVATE_TOOL_ARGUMENT", 100), Metadata: map[string]any{"public_tool_progress": summary}}
				sp.appendProgressEvent(event, publicToolProgress(event))
				var sent string
				if !sp.deliverFinal("完成", "", func(_ Platform, _ any, text string) error { sent += text; return nil }, "失败") {
					t.Fatal("send failed")
				}
				if strings.Contains(sent, "（中途内容）") != long {
					t.Fatal("wrong substantive tool summary classification")
				}
				if long && !strings.Contains(sent, summary) {
					t.Fatal("lost public summary")
				}
				if strings.Contains(sent, "PRIVATE_TOOL_ARGUMENT") {
					t.Fatal("exposed private tool argument")
				}
			})
		}
	}
}
