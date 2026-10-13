package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCmdTimer_RequiresOffsetAndShowsBeijingTime(t *testing.T) {
	for _, sub := range []string{"add", "addexec"} {
		t.Run(sub, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
			t.Cleanup(func() { _ = e.Stop() })
			e.SetAdminFrom("user")
			store, err := NewTimerStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			e.SetTimerScheduler(NewTimerScheduler(store))
			msg := &Message{SessionKey: "test:chat:user", UserID: "user", ReplyCtx: context.Background()}
			e.cmdTimer(p, msg, []string{sub, "2099-10-13T20:45", "check"})
			if sent := p.getSent(); len(sent) != 1 || !strings.Contains(sent[0], "--at 必须带时区偏移") {
				t.Fatalf("missing timezone rejection: %v", sent)
			}
			p.clearSent()
			e.cmdTimer(p, msg, []string{"list"})
			if sent := p.getSent(); len(sent) != 1 || strings.Contains(sent[0], "check") {
				t.Fatalf("rejected timer leaked into list: %v", sent)
			}
			p.clearSent()
			e.cmdTimer(p, msg, []string{sub, "2099-10-13T20:45Z", "check"})
			if sent := p.getSent(); len(sent) != 1 || !strings.Contains(sent[0], "北京时间: 10-14 04:45") {
				t.Fatalf("missing Beijing creation time: %v", sent)
			}
			p.clearSent()
			e.cmdTimer(p, msg, []string{"list"})
			if sent := p.getSent(); len(sent) != 1 || !strings.Contains(sent[0], "北京时间: 10-14 04:45") {
				t.Fatalf("missing Beijing list time: %v", sent)
			}
			card := e.renderTimerCard(msg.SessionKey, msg.UserID)
			for _, elem := range card.Elements {
				if md, ok := elem.(CardMarkdown); ok && strings.Contains(md.Content, "check") {
					if !strings.Contains(md.Content, "北京时间: 10-14 04:45") {
						t.Fatalf("missing Beijing card time: %q", md.Content)
					}
					return
				}
			}
			t.Fatal("timer missing from card")
		})
	}
}

func TestAgentSystemPrompt_TimerRequiresOffset(t *testing.T) {
	prompt := AgentSystemPrompt()
	for _, want := range []string{"--at requires an explicit timezone offset", "For Beijing time, use +08:00", "use --delay instead of guessing", `--at "2026-05-16T09:00+08:00"`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing instruction %q", want)
		}
	}
	if strings.Contains(prompt, "Absolute times without timezone") {
		t.Fatal("old local-time instruction remains")
	}
}

func TestTimerLocalization_OffsetHelpAndBeijingLabel(t *testing.T) {
	for _, lang := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
		i18n := NewI18n(lang)
		for _, key := range []MsgKey{MsgTimerUsage, MsgTimerAddUsage, MsgTimerTimezoneRequired} {
			text := i18n.T(key)
			if !strings.Contains(text, "+08:00") || !strings.Contains(text, "--delay") {
				t.Errorf("%s/%s: %q", lang, key, text)
			}
		}
		if text := i18n.Tf(MsgTimerBeijingTime, "10-14 04:45"); !strings.Contains(text, "10-14 04:45") {
			t.Errorf("%s: %q", lang, text)
		}
	}
}

func TestHandleTimerAdd_RequiresExplicitTimezone(t *testing.T) {
	for _, tc := range []struct {
		at    string
		valid bool
	}{
		{"2099-10-13T22:45", false}, {"2099-10-13T22:45:00", false},
		{"2099-10-13 22:45", false}, {"2099-10-13 22:45:00", false},
		{"2099-10-13T22:45+08:00", true}, {"2099-10-13T22:45:00+08:00", true},
		{"2099-10-13T22:45+09:00", true}, {"2099-10-13T22:45:00+09:00", true},
		{"2099-10-13T22:45Z", true}, {"2099-10-13T22:45:00Z", true},
	} {
		t.Run(tc.at, func(t *testing.T) {
			store, err := NewTimerStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			api := &APIServer{timer: NewTimerScheduler(store)}
			t.Cleanup(api.timer.Stop)
			body, err := json.Marshal(TimerAddRequest{Project: "test", SessionKey: "test:chat:user", Delay: tc.at, Prompt: "check"})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			api.handleTimerAdd(rec, httptest.NewRequest(http.MethodPost, "/timer/add", strings.NewReader(string(body))))
			if !tc.valid {
				want := "--at 必须带时区偏移，例如 2026-10-13T22:45+08:00（北京）或 +09:00（东京）；也可用 --delay"
				if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want || len(store.List()) != 0 {
					t.Fatalf("status=%d body=%q jobs=%d", rec.Code, rec.Body.String(), len(store.List()))
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var job TimerJob
			if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
				t.Fatal(err)
			}
			layout := time.RFC3339
			if tc.at[16] != ':' {
				layout = "2006-01-02T15:04Z07:00"
			}
			want, err := time.Parse(layout, tc.at)
			if err != nil || !job.ScheduledAt.Equal(want) || len(store.List()) != 1 {
				t.Fatalf("scheduled_at=%s want=%s err=%v", job.ScheduledAt, want, err)
			}
		})
	}
}
