package core

import "testing"

func TestScheduledStartNoticePreservesStatusIcon(t *testing.T) {
	for desc, want := range map[string]string{"✅ 已完成": "✅ 已完成", "❌ 失败": "❌ 失败", "✅": "✅", "❌": "❌", "Daily summary": "⏰ Daily summary", "": "⏰ ", " ✅ spaced": "⏰  ✅ spaced", "提醒 ✅": "⏰ 提醒 ✅"} {
		if got := scheduledStartNotice(desc); got != want {
			t.Fatalf("%q: got %q want %q", desc, got, want)
		}
	}
}

func TestScheduledStartNoticeCronAndTimerDelivery(t *testing.T) {
	for _, timer := range []bool{false, true} {
		for _, desc := range []string{"✅ 已完成", "❌ 失败", "Daily summary"} {
			p := &scheduledTestPlatform{&steerReactionPlatform{stubPlatformEngine: stubPlatformEngine{n: "scheduled"}}}
			e := NewEngine("notice", &resultAgent{session: newResultAgentSession("final")}, []Platform{p}, "", LangEnglish)
			e.SetQueueBatchConfig(QueueBatchConfig{Enabled: false})
			var err error
			if timer {
				err = e.ExecuteTimerJob(&TimerJob{ID: "t", SessionKey: "scheduled:chat:user", Prompt: "work", Description: desc})
			} else {
				err = e.ExecuteCronJob(&CronJob{ID: "c", SessionKey: "scheduled:chat:user", Prompt: "work", Description: desc})
			}
			e.cancel()
			if err != nil {
				t.Fatal(err)
			}
			sent := p.getSent()
			if len(sent) != 2 || sent[0] != scheduledStartNotice(desc) || sent[1] != "final" {
				t.Fatalf("timer=%v desc=%q sent=%v", timer, desc, sent)
			}
		}
	}
}
