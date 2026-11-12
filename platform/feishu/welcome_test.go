package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	"github.com/chenhg5/cc-connect/core"
)

// welcomeClock drives a welcomer without waiting: waitUntil jumps the clock.
type welcomeClock struct {
	mu      sync.Mutex
	now     time.Time
	waits   []time.Time
	sends   []time.Time
	fail    int // the first fail sends return an error; -1 = all
	chatted bool
	onWait  func(c *welcomeClock)
}

func bj(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, beijing)
}

func newTestWelcomer(t *testing.T, dir string, c *welcomeClock) *welcomer {
	t.Helper()
	return &welcomer{
		tag:          "feishu",
		text:         func() string { return "hi" },
		markerPath:   filepath.Join(dir, "feishu", "welcome-sent"),
		attemptsPath: filepath.Join(dir, "feishu", "welcome-attempts"),
		chatted: func() (bool, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.chatted, nil
		},
		send: func(context.Context, string) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			if _, err := os.Stat(filepath.Join(dir, "feishu", "welcome-sent")); err != nil {
				t.Error("marker must be written before the send")
			}
			c.sends = append(c.sends, c.now)
			if c.fail != 0 {
				if c.fail > 0 {
					c.fail--
				}
				return errors.New("feishu: welcome failed code=99991663")
			}
			return nil
		},
		now: func() time.Time {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.now
		},
		waitUntil: func(ctx context.Context, at time.Time) bool {
			c.mu.Lock()
			c.waits = append(c.waits, at)
			if at.After(c.now) {
				c.now = at
			}
			hook := c.onWait
			c.mu.Unlock()
			if hook != nil {
				hook(c)
			}
			return ctx.Err() == nil
		},
		jitter: func() time.Duration { return 7 * time.Minute },
	}
}

func runWelcome(t *testing.T, w *welcomer) {
	t.Helper()
	done := make(chan struct{})
	go func() { w.run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("welcome schedule did not finish")
	}
}

func TestWelcome_SendsOnceAndMarkerPreventsResend(t *testing.T) {
	dir := t.TempDir()
	c := &welcomeClock{now: bj(2026, 11, 11, 10, 0)} // Wednesday
	runWelcome(t, newTestWelcomer(t, dir, c))
	if len(c.sends) != 1 || !c.sends[0].Equal(bj(2026, 11, 11, 10, 1)) {
		t.Fatalf("sends = %v, want one at 10:01 after the start delay", c.sends)
	}
	if _, err := os.Stat(filepath.Join(dir, "feishu", "welcome-sent")); err != nil {
		t.Fatalf("marker missing after a send: %v", err)
	}
	// A restart, even the same day, sends nothing more.
	runWelcome(t, newTestWelcomer(t, dir, c))
	if len(c.sends) != 1 {
		t.Fatalf("welcome sent again: %v", c.sends)
	}
}

func TestWelcome_ExistingMarkerSkips(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "feishu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feishu", "welcome-sent"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := &welcomeClock{now: bj(2026, 11, 11, 10, 0)}
	runWelcome(t, newTestWelcomer(t, dir, c))
	if len(c.sends) != 0 || len(c.waits) != 0 {
		t.Fatalf("marker present but sends=%v waits=%v", c.sends, c.waits)
	}
}

func TestWelcome_SkipsWhenTheBotHasChatted(t *testing.T) {
	c := &welcomeClock{now: bj(2026, 11, 11, 10, 0), chatted: true}
	runWelcome(t, newTestWelcomer(t, t.TempDir(), c))
	if len(c.sends) != 0 {
		t.Fatalf("bot with a conversation got a welcome: %v", c.sends)
	}
}

// The owner may start talking while the welcome waits for the window.
func TestWelcome_RechecksConversationBeforeSending(t *testing.T) {
	c := &welcomeClock{now: bj(2026, 11, 14, 22, 0)} // Saturday night
	c.onWait = func(c *welcomeClock) { c.mu.Lock(); c.chatted = true; c.mu.Unlock() }
	runWelcome(t, newTestWelcomer(t, t.TempDir(), c))
	if len(c.sends) != 0 {
		t.Fatalf("welcome sent after the owner had started chatting: %v", c.sends)
	}
}

func TestWelcome_RespectsBeijingWorkingHours(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start time.Time
		want  time.Time
	}{
		{"saturday night waits for monday", bj(2026, 11, 14, 22, 0), bj(2026, 11, 16, 9, 37)},
		{"friday evening waits for monday", bj(2026, 11, 13, 19, 45), bj(2026, 11, 16, 9, 37)},
		{"weekday early morning waits for 09:30", bj(2026, 11, 12, 8, 0), bj(2026, 11, 12, 9, 37)},
		{"start given in UTC", time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC), bj(2026, 11, 13, 9, 37)}, // 21:00 Beijing
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &welcomeClock{now: tc.start}
			runWelcome(t, newTestWelcomer(t, t.TempDir(), c))
			if len(c.sends) != 1 || !c.sends[0].Equal(tc.want) {
				t.Fatalf("sends = %v, want one at %v", c.sends, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{bj(2026, 11, 12, 9, 29), false},
		{bj(2026, 11, 12, 9, 30), true},
		{bj(2026, 11, 12, 19, 29), true},
		{bj(2026, 11, 12, 19, 30), false},
		{bj(2026, 11, 14, 12, 0), false},
		{bj(2026, 11, 15, 12, 0), false},
	} {
		if got := inWelcomeWindow(tc.at); got != tc.want {
			t.Errorf("inWelcomeWindow(%v) = %v, want %v", tc.at, got, tc.want)
		}
	}
}

func TestWelcome_RetriesAtNextWindowAndStopsAfterThreeAttempts(t *testing.T) {
	dir := t.TempDir()
	c := &welcomeClock{now: bj(2026, 11, 11, 10, 0), fail: -1} // Wednesday
	runWelcome(t, newTestWelcomer(t, dir, c))
	want := []time.Time{bj(2026, 11, 11, 10, 1), bj(2026, 11, 12, 9, 37), bj(2026, 11, 13, 9, 37)}
	if len(c.sends) != len(want) {
		t.Fatalf("sends = %v, want %v", c.sends, want)
	}
	for i := range want {
		if !c.sends[i].Equal(want[i]) {
			t.Fatalf("attempt %d at %v, want %v", i+1, c.sends[i], want[i])
		}
	}
	// Restarts never try again, and the marker stays.
	runWelcome(t, newTestWelcomer(t, dir, c))
	if len(c.sends) != 3 {
		t.Fatalf("attempts after the cap: %v", c.sends)
	}
	if _, err := os.Stat(filepath.Join(dir, "feishu", "welcome-sent")); err != nil {
		t.Fatal("marker must stay after the last failed attempt")
	}
}

func TestWelcome_FailureRemovesMarkerAndCountSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	c := &welcomeClock{now: bj(2026, 11, 11, 10, 0), fail: 1}
	ctx, cancel := context.WithCancel(context.Background())
	w := newTestWelcomer(t, dir, c)
	// The process stops while it waits for the retry window.
	c.onWait = func(c *welcomeClock) {
		c.mu.Lock()
		n := len(c.sends)
		c.mu.Unlock()
		if n == 1 {
			cancel()
		}
	}
	w.run(ctx)
	if _, err := os.Stat(filepath.Join(dir, "feishu", "welcome-sent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker must be removed after a failed send, stat err=%v", err)
	}
	c.onWait = nil
	runWelcome(t, newTestWelcomer(t, dir, c))
	if len(c.sends) != 2 {
		t.Fatalf("sends = %v, want the failed one and one retry after the restart", c.sends)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "feishu", "welcome-attempts"))
	if strings.TrimSpace(string(data)) != "2" {
		t.Fatalf("attempts = %q, want 2", data)
	}
}

func TestWelcomeOwner(t *testing.T) {
	for _, tc := range []struct{ allow, owner, reason string }{
		{"", "", "no_owner"},
		{"*", "", "no_owner"},
		{" * ", "", "no_owner"},
		{"ou_a,ou_b", "", "multiple_owners"},
		{"ou_a, *", "", "multiple_owners"},
		{"u_123", "", "owner_not_open_id"},
		{" ou_abc ", "ou_abc", ""},
		{"ou_abc,", "ou_abc", ""},
	} {
		owner, reason := welcomeOwner(tc.allow)
		if owner != tc.owner || reason != tc.reason {
			t.Errorf("welcomeOwner(%q) = %q, %q; want %q, %q", tc.allow, owner, reason, tc.owner, tc.reason)
		}
	}
}

func TestConfigureWelcome_OwnerAndSwitch(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]any
		want bool
	}{
		{"single owner", map[string]any{"allow_from": "ou_owner"}, true},
		{"everyone", map[string]any{"allow_from": "*"}, false},
		{"several owners", map[string]any{"allow_from": "ou_a,ou_b"}, false},
		{"turned off", map[string]any{"allow_from": "ou_owner", "welcome": false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts["app_id"], tc.opts["app_secret"], tc.opts["cc_data_dir"] = "cli_test", "secret", t.TempDir()
			tc.opts["p2p_catchup"] = false
			pl, err := New(tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			p := platformOf(pl)
			if got := p.welcome != nil; got != tc.want {
				t.Fatalf("welcome configured = %v, want %v", got, tc.want)
			}
		})
	}
}

func platformOf(pl core.Platform) *Platform {
	switch v := pl.(type) {
	case *interactivePlatform:
		return v.Platform
	case *Platform:
		return v
	}
	return nil
}

func TestConfigureWelcome_TextFollowsLanguageOrOption(t *testing.T) {
	pl, err := New(map[string]any{"app_id": "cli_test", "app_secret": "secret", "allow_from": "ou_owner", "cc_data_dir": t.TempDir(), "p2p_catchup": false})
	if err != nil {
		t.Fatal(err)
	}
	p := platformOf(pl)
	if got := p.welcome.text(); !strings.HasPrefix(got, "你好，我是你的工作伙伴") {
		t.Fatalf("default text without an engine language = %q", got)
	}
	p.SetI18n(core.NewI18n(core.LangJapanese))
	if got := p.welcome.text(); !strings.HasPrefix(got, "こんにちは") {
		t.Fatalf("Japanese text = %q", got)
	}
	pl, err = New(map[string]any{"app_id": "cli_test", "app_secret": "secret", "allow_from": "ou_owner", "cc_data_dir": t.TempDir(), "p2p_catchup": false, "welcome_message": "  欢迎！  "})
	if err != nil {
		t.Fatal(err)
	}
	if got := platformOf(pl).welcome.text(); got != "欢迎！" {
		t.Fatalf("custom text = %q", got)
	}
}

func TestHasConversation_SessionHistoryOrPrivateChat(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "proj.json")
	p := &Platform{platformName: "feishu"}
	if chatted, err := p.hasConversation(store, "proj"); err != nil || chatted {
		t.Fatalf("no records: chatted=%v err=%v", chatted, err)
	}
	write := func(path string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(store, map[string]any{"sessions": map[string]any{"s1": map[string]any{"id": "s1", "history": []any{}}}})
	if chatted, _ := p.hasConversation(store, "proj"); chatted {
		t.Fatal("a session without history is not a conversation")
	}
	write(filepath.Join(dir, "proj_ws_0123abcd.json"), map[string]any{"sessions": map[string]any{"s2": map[string]any{"history": []any{map[string]any{"role": "user", "content": "x"}}}}})
	if chatted, _ := p.hasConversation(store, "proj"); !chatted {
		t.Fatal("history in a workspace store counts as a conversation")
	}
	p2 := &Platform{platformName: "feishu", catchup: &p2pCatchup{snapshot: p2pCatchupSnapshot{PrivateChats: map[string]time.Time{"oc_1": time.Now()}}}}
	if chatted, _ := p2.hasConversation(filepath.Join(t.TempDir(), "none.json"), "proj"); !chatted {
		t.Fatal("a confirmed private chat counts as a conversation")
	}
}

func TestSendToOpenID_UsesOpenIDReceiver(t *testing.T) {
	var gotType, gotID, gotMsgType, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "welcome-test-token", "expire": 7200})
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages" || r.Method != http.MethodPost {
			t.Errorf("unexpected API: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		gotType = r.URL.Query().Get("receive_id_type")
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ReceiveID string `json:"receive_id"`
			MsgType   string `json:"msg_type"`
			Content   string `json:"content"`
		}
		_ = json.Unmarshal(body, &req)
		var content struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal([]byte(req.Content), &content)
		gotID, gotMsgType, gotText = req.ReceiveID, req.MsgType, content.Text
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "om_welcome"}})
	}))
	defer srv.Close()
	p := &Platform{platformName: "feishu"}
	p.client = lark.NewClient("cli_welcome", "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()))
	text := core.NewI18n(core.LangChinese).T(core.MsgFirstRunWelcome)
	if err := p.sendToOpenID(context.Background(), "ou_owner", text); err != nil {
		t.Fatal(err)
	}
	if gotType != "open_id" || gotID != "ou_owner" || gotMsgType != "text" || gotText != text {
		t.Fatalf("request: type=%q id=%q msg_type=%q text=%q", gotType, gotID, gotMsgType, gotText)
	}
}
