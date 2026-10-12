package core

import (
	"reflect"
	"strings"
	"testing"
)

func TestScheduledStartNoticePreservesStatusAndChatIcons(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want string
	}{
		{"success", "✅ 已完成", "✅ 已完成"},
		{"failure", "❌ 失败", "❌ 失败"},
		{"chat", "💬 说明", "💬 说明"},
		{"success icon", "✅", "✅"},
		{"failure icon", "❌", "❌"},
		{"chat icon", "💬", "💬"},
		{"plain text", "Daily summary", "⏰ Daily summary"},
		{"empty", "", "⏰ "},
		{"leading space", " ✅ spaced", "⏰  ✅ spaced"},
		{"embedded status", "提醒 ✅", "⏰ 提醒 ✅"},
		{"spaced chat", " 💬 说明", "⏰  💬 说明"},
		{"embedded chat", "说明 💬", "⏰ 说明 💬"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scheduledStartNotice(tt.desc); got != tt.want {
				t.Fatalf("%q: got %q want %q", tt.desc, got, tt.want)
			}
		})
	}
}

func TestScheduledStartNoticeCronAndTimerDelivery(t *testing.T) {
	for _, timer := range []bool{false, true} {
		for _, tt := range []struct {
			desc string
			want string
		}{
			{"✅ 已完成", "✅ 已完成"},
			{"❌ 失败", "❌ 失败"},
			{"💬 说明", "💬 说明"},
			{"Daily summary", "⏰ Daily summary"},
			{"", "⏰ work"},
		} {
			p := &scheduledTestPlatform{&steerReactionPlatform{stubPlatformEngine: stubPlatformEngine{n: "scheduled"}}}
			e := NewEngine("notice", &resultAgent{session: newResultAgentSession("final")}, []Platform{p}, "", LangEnglish)
			e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
			var err error
			if timer {
				err = e.ExecuteTimerJob(&TimerJob{ID: "t", SessionKey: "scheduled:chat:user", Prompt: "work", Description: tt.desc})
			} else {
				err = e.ExecuteCronJob(&CronJob{ID: "c", SessionKey: "scheduled:chat:user", Prompt: "work", Description: tt.desc})
			}
			e.cancel()
			if err != nil {
				t.Fatal(err)
			}
			sent := p.getSent()
			want := []string{tt.want, "final"}
			if timer && strings.HasPrefix(tt.desc, "💬") {
				want = []string{"final"}
			}
			if !reflect.DeepEqual(sent, want) {
				t.Fatalf("timer=%v desc=%q sent=%v", timer, tt.desc, sent)
			}
		}
	}
}

type scheduledLifecyclePlatform struct{ lifecyclePlatform }

func (p *scheduledLifecyclePlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }

func TestExecuteTimerJob_RoleRelaySilentAndVisibleReplies(t *testing.T) {
	for _, result := range []string{"NO_REPLY", "role reply"} {
		t.Run(result, func(t *testing.T) {
			p := &scheduledLifecyclePlatform{lifecyclePlatform: lifecyclePlatform{stubPlatformEngine: stubPlatformEngine{n: "scheduled"}}}
			e := NewEngine("notice", &resultAgent{session: newResultAgentSession(result)}, []Platform{p}, "", LangChinese)
			defer e.Stop()
			e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
			e.SetStreamPreviewCfg(DefaultStreamPreviewCfg())
			e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
			if err := e.ExecuteTimerJob(&TimerJob{ID: "relay", SessionKey: "scheduled:chat:user", Prompt: "relay", Description: "💬 助手 C→助手 B"}); err != nil {
				t.Fatal(err)
			}
			if result == "NO_REPLY" {
				if len(p.getSent()) != 0 || len(p.starts) != 0 || len(p.edits) != 0 || len(p.deletes) != 0 {
					t.Fatalf("silent relay left a footprint: sent=%v starts=%v edits=%v recalls=%v", p.getSent(), p.starts, p.edits, p.deletes)
				}
			} else if got := p.getSent(); len(got) != 1 || got[0] != "role reply" {
				t.Fatalf("role reply missing or start notice leaked: %v", got)
			}
		})
	}
}
