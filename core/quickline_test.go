package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchQuickLineTakesFirstTextBlockAndReadsKeyAtCallTime(t *testing.T) {
	var gotKey, gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotPrompt = string(b[:n])
		w.Write([]byte(`{"content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"收到，我先查一下。"},{"type":"text","text":"later"}]}`))
	}))
	defer srv.Close()
	kf := filepath.Join(t.TempDir(), "k")
	cfg := QuickLineCfg{Enabled: true, BaseURL: srv.URL + "/", Model: "m", KeyFile: kf}
	if _, err := fetchQuickLine(context.Background(), cfg, "hi", nil); err == nil {
		t.Fatal("missing key file must fail")
	}
	cfg.Effort = ""
	os.WriteFile(kf, []byte("dummy\n"), 0o600)
	hist := []HistoryEntry{{Role: "user", Content: "前一句"}, {Role: "assistant", Content: "前答"}}
	line, err := fetchQuickLine(context.Background(), cfg, "新消息", hist)
	if err != nil || line != "收到，我先查一下。" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if gotKey != "dummy" || !strings.Contains(gotPrompt, "新消息") || !strings.Contains(gotPrompt, "前一句") {
		t.Fatal("request missing key or prompt parts")
	}
}
