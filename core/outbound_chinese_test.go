package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const englishReport = "I merged the pull request and deployed the gateway. The health check passed and the new image is running on all containers."

func TestNeedsChineseRewrite(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{englishReport, true},
		{"Done.", false}, // too few letters
		{"已经部署好了，网关健康检查通过，新镜像在所有容器上运行。", false},
		{"已合入并部署：\n```\ngit fetch origin && git checkout --detach origin/main && docker compose up -d --force-recreate gateway\n```", false},
		{"见 https://github.com/soundworkbench/cc-connect/pull/123/files?diff=split 和 `bin/upgrade-and-check.sh sw13-abcdef`", false},
		{"日志在 /srv/xiaoman/partner-dev/runs/cc-connect-ux3/bin/cc-connect-linux-amd64 ，改了 core/outbound_chinese.go 和 platform/feishu/segmented_preview.go", false},
		{"部署好了。I merged the pull request and deployed the gateway, all good now.", true},
	}
	for _, c := range cases {
		if got := needsChineseRewrite(c.text); got != c.want {
			t.Errorf("needsChineseRewrite(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestMaskKeepsCodeLinksPathsAndFileNames(t *testing.T) {
	text := "Run `make build` then open [the PR](https://github.com/a/b/pull/1), see https://x.example/y and /srv/app/run.sh; edit main.go and CC_SESSION_KEY.\n```go\nfmt.Println(\"hi\")\n```"
	masked, spans := maskOutbound(text)
	for _, keep := range []string{"`make build`", "[the PR](https://github.com/a/b/pull/1)", "https://x.example/y", "/srv/app/run.sh", "main.go", "CC_SESSION_KEY", "```go\nfmt.Println(\"hi\")\n```"} {
		found := false
		for _, s := range spans {
			if s == keep {
				found = true
			}
		}
		if !found || strings.Contains(masked, keep) {
			t.Errorf("%q not masked: masked=%q spans=%q", keep, masked, spans)
		}
	}
	back, ok := unmaskOutbound(masked, spans)
	if !ok || back != text {
		t.Fatalf("round trip: ok=%v back=%q", ok, back)
	}
	if _, ok := unmaskOutbound(strings.Replace(masked, "⟦1⟧", "", 1), spans); ok {
		t.Fatal("a lost placeholder must be refused")
	}
	if _, ok := unmaskOutbound(masked+"⟦1⟧", spans); ok {
		t.Fatal("a duplicated placeholder must be refused")
	}
}

func TestIsEnglishMessage(t *testing.T) {
	for text, want := range map[string]bool{
		"please reply in English": true,
		"ok":                      false,
		"更新进度":                    false,
		"看下这个报错 error: connection refused at line 3": false,
	} {
		if got := isEnglishMessage(text); got != want {
			t.Errorf("isEnglishMessage(%q) = %v, want %v", text, got, want)
		}
	}
}

// outboundEngine is an engine with a quick model that answers with reply after delay.
func outboundEngine(t *testing.T, lang Language, delay time.Duration, reply func(user string) string) (*Engine, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		time.Sleep(delay)
		out := reply(body.Messages[0].Content)
		if out == "" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}]}`, out)
	}))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEngine("p", &stubAgent{}, nil, "", lang)
	cfg := DefaultStreamPreviewCfg()
	cfg.QuickLine = QuickLineCfg{Enabled: true, BaseURL: srv.URL, Model: "m", KeyFile: keyFile, TimeoutMs: 8000}
	e.SetStreamPreviewCfg(cfg)
	t.Cleanup(func() { _ = e.Stop() })
	return e, calls
}

func TestChineseOutboundRewritesAndKeepsCode(t *testing.T) {
	text := englishReport + " Binary: `/srv/out/cc-connect` (see https://github.com/a/b/pull/7)."
	e, _ := outboundEngine(t, LangChinese, 0, func(user string) string {
		if strings.Contains(user, "/srv/out/cc-connect") || strings.Contains(user, "https://") {
			return "" // code and links must not reach the model
		}
		return "我已合入拉取请求并部署了网关，健康检查通过，新镜像已在所有容器上运行。二进制：⟦1⟧（见 ⟦2⟧）。"
	})
	got := e.chineseOutbound("s", text, true)
	want := "我已合入拉取请求并部署了网关，健康检查通过，新镜像已在所有容器上运行。二进制：`/srv/out/cc-connect`（见 https://github.com/a/b/pull/7）。"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestChineseOutboundKeepsOriginalWhenItCannotRewrite(t *testing.T) {
	t.Run("gateway error", func(t *testing.T) {
		e, calls := outboundEngine(t, LangChinese, 0, func(string) string { return "" })
		if got := e.chineseOutbound("s", englishReport, true); got != englishReport || calls.Load() != 1 {
			t.Fatalf("got %q calls=%d", got, calls.Load())
		}
	})
	t.Run("placeholder lost", func(t *testing.T) {
		e, _ := outboundEngine(t, LangChinese, 0, func(string) string { return "已部署，镜像在运行。" })
		text := englishReport + " See `make build`."
		if got := e.chineseOutbound("s", text, true); got != text {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("still English", func(t *testing.T) {
		e, _ := outboundEngine(t, LangChinese, 0, func(string) string { return englishReport })
		if got := e.chineseOutbound("s", englishReport, true); got != englishReport {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		e, _ := outboundEngine(t, LangChinese, 2*time.Second, func(string) string { return "已部署。" })
		cfg := e.streamPreview.QuickLine
		cfg.TimeoutMs = 300
		e.streamPreview.QuickLine = cfg
		start := time.Now()
		if got := e.chineseOutbound("s", englishReport, true); got != englishReport {
			t.Fatalf("got %q", got)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("waited %v past the timeout", took)
		}
	})
}

func TestChineseOutboundLeavesAloneWhatItShould(t *testing.T) {
	e, calls := outboundEngine(t, LangChinese, 0, func(string) string { return "已部署。" })
	for _, text := range []string{"已经部署好了。", "Done.", "```\n" + englishReport + "\n```"} {
		if got := e.chineseOutbound("s", text, true); got != text {
			t.Fatalf("%q rewritten to %q", text, got)
		}
	}
	// The person asked in English: the reply stays English.
	e.noteHumanLanguage(&Message{Content: "please answer in English from now on", UserID: "u"}, "s")
	if got := e.chineseOutbound("s", englishReport, true); got != englishReport {
		t.Fatalf("English chat rewritten: %q", got)
	}
	// A Chinese message switches it back.
	e.noteHumanLanguage(&Message{Content: "好的，接着做", UserID: "u"}, "s")
	if got := e.chineseOutbound("s", englishReport, true); got != "已部署。" {
		t.Fatalf("got %q", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("quick model calls = %d, want 1", calls.Load())
	}
	// An English-language bridge never rewrites.
	en, enCalls := outboundEngine(t, LangEnglish, 0, func(string) string { return "已部署。" })
	if got := en.chineseOutbound("s", englishReport, true); got != englishReport || enCalls.Load() != 0 {
		t.Fatalf("English bridge: %q calls=%d", got, enCalls.Load())
	}
}

// A turn's English reply reaches the chat in Chinese.
func TestTurnReplyIsSentInChinese(t *testing.T) {
	env := newTriageEnv(t)
	env.rewrite.Store("我已合入拉取请求并部署了网关，健康检查通过。")
	env.firstTurn(englishReport)
	sent := env.p.stubPlatformEngine.getSent()
	if last := sent[len(sent)-1]; last != "我已合入拉取请求并部署了网关，健康检查通过。" {
		t.Fatalf("last sent = %q (all %q)", last, sent)
	}
	// History keeps the agent's own words.
	h := env.e.sessions.GetOrCreateActive(env.key).GetHistory(1)
	if len(h) != 1 || h[0].Content != englishReport {
		t.Fatalf("history = %+v", h)
	}
	// After an English message the reply goes out as written.
	before := env.rewrites.Load()
	env.send("please summarize the deploy in English")
	if !env.reachesAgent() {
		t.Fatal("message did not reach the agent")
	}
	env.finishTurn(englishReport)
	sent = env.p.stubPlatformEngine.getSent()
	if last := sent[len(sent)-1]; last != englishReport || env.rewrites.Load() != before {
		t.Fatalf("English chat: last sent = %q, rewrites %d→%d", last, before, env.rewrites.Load())
	}
}

// A slow quick line still turns a background report into Chinese; a reply the
// person is waiting on gives up after the short limit and goes out as written.
func TestChineseOutboundWaitsLongerOffTheLiveReply(t *testing.T) {
	const slow = outboundLiveTimeout + time.Second // the quick line took 8 s and more on 11-12
	t.Run("background follow-up", func(t *testing.T) {
		t.Parallel()
		e, _ := outboundEngine(t, LangChinese, slow, func(string) string { return "已部署。" })
		if got := e.chineseOutbound("s", englishReport, false); got != "已部署。" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("live reply", func(t *testing.T) {
		t.Parallel()
		e, _ := outboundEngine(t, LangChinese, slow, func(string) string { return "已部署。" })
		start := time.Now()
		if got := e.chineseOutbound("s", englishReport, true); got != englishReport {
			t.Fatalf("got %q", got)
		}
		if took := time.Since(start); took > outboundLiveTimeout+500*time.Millisecond {
			t.Fatalf("live reply waited %v", took)
		}
	})
	if outboundLaterTimeout < 20*time.Second {
		t.Fatalf("background limit = %v", outboundLaterTimeout)
	}
}
