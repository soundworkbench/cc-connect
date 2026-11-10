package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func sampleCard(t *testing.T) []*larkim.Message {
	t.Helper()
	data, err := os.ReadFile("testdata/sample-merge-forward.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample struct {
		Data struct {
			Items []*larkim.Message `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &sample); err != nil {
		t.Fatal(err)
	}
	return sample.Data.Items
}

func TestInteractiveCard_DisplayFieldsAndPlaceholder(t *testing.T) {
	content := `{"header":{"title":{"tag":"plain_text","content":"标题"}},"elements":[{"tag":"div","text":{"tag":"lark_md","content":"正文"}},{"tag":"column_set","columns":[{"tag":"column","elements":[{"tag":"markdown","content":"列"}]}]},{"tag":"note","elements":[{"tag":"text","text":"备注"}]},{"tag":"button","text":{"tag":"plain_text","content":"按钮"},"value":{"content":"callback-secret"}},{"tag":"img","image_key":"img_test","alt":{"tag":"plain_text","content":"说明"}},{"tag":"text","text":"请升级至最新版本客户端，以查看内容"}]}`
	text, keys := parseInteractiveCard(content)
	if text != "标题\n正文\n列\n备注\n按钮\n说明" || !reflect.DeepEqual(keys, []string{"img_test"}) {
		t.Fatalf("text=%q keys=%v", text, keys)
	}
	wrapped, _ := json.Marshal(map[string]string{"json_card": content})
	if got := extractInteractiveCardText(string(wrapped)); got != text {
		t.Fatalf("wrapper: %q", got)
	}
	for _, content := range []string{`{"title":"旧标题","elements":[[{"tag":"text","text":"旧正文"}]]}`, `{"header":{"title":{"content":"旧标题"}},"body":{"tag":"body","property":{"elements":[{"tag":"markdown","property":{"content":"旧正文"}}]}}}`} {
		if got := extractInteractiveCardText(content); got != "旧标题\n旧正文" {
			t.Fatalf("legacy/schema2: %q", got)
		}
	}
	if got := extractInteractiveCardText(`{"elements":[{"tag":"text","text":"请升级至最新版本客户端，以查看内容"}]}`); got != "[interactive card]" {
		t.Fatalf("placeholder=%q", got)
	}
}

func TestInteractiveCard_SampleDownloadFallback(t *testing.T) {
	items := sampleCard(t)
	content := *items[1].Body.Content
	root, sub := *items[0].MessageId, *items[1].MessageId
	_, keys := parseInteractiveCard(content)
	for _, mode := range []string{"fail", "sub", "root", "direct"} {
		t.Run(mode, func(t *testing.T) {
			var calls []string
			rootID := root
			if mode == "direct" {
				rootID = sub
			}
			text, images := interactiveCardContent(content, sub, rootID, "Alice", func(id, key string) ([]byte, string, error) {
				if key != keys[0] {
					t.Fatalf("key=%s", key)
				}
				calls = append(calls, id)
				if mode == "sub" || (mode == "root" && id == root) {
					return []byte("image-bytes"), "image/png", nil
				}
				return nil, "", errors.New("234003 File not in msg. secret-token")
			})
			wantCalls := []string{sub}
			if mode == "fail" || mode == "root" {
				wantCalls = append(wantCalls, root)
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("calls=%v", calls)
			}
			if mode == "fail" || mode == "direct" {
				want := "[卡片图片无法下载：飞书限制跨应用卡片图片｜发送方 Alice ｜image_key=" + keys[0] + "]"
				if text != want || len(images) != 0 {
					t.Fatalf("text=%q images=%v", text, images)
				}
			} else if text != "" || len(images) != 1 || string(images[0].Data) != "image-bytes" || images[0].MimeType != "image/png" {
				t.Fatalf("text=%q images=%v", text, images)
			}
		})
	}
}

// Exercise the real SDK boundary and both incoming dispatch branches. This
// catches missing wiring that pure parser tests would not detect.
func TestInteractiveCard_IncomingDirectAndNestedForward(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "restricted", true: "downloadable"}[success], func(t *testing.T) {
			var calls []string
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					_, _ = w.Write([]byte(`{"code":0,"expire":7200,"tenant_access_token":"mock-token"}`))
					return
				}
				if strings.Contains(r.URL.Path, "/resources/") {
					if r.URL.Query().Get("type") != "image" {
						t.Errorf("type=%s", r.URL.RawQuery)
					}
					// Upstream's chunked downloader probes with a Range request
					// before a plain GET; count one call per download attempt.
					if success || r.Header.Get("Range") == "" {
						calls = append(calls, r.URL.Path)
					}
					if success {
						w.Header().Set("Content-Type", "image/png")
						_, _ = w.Write([]byte("image-bytes"))
						return
					}
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"code":234003,"msg":"File not in msg."}`))
					return
				}
				t.Errorf("unexpected API %s", r.URL.Path)
				_, _ = w.Write([]byte(`{"code":1}`))
			}))
			defer srv.Close()
			var got *core.Message
			p := &Platform{platformName: "feishu", domain: srv.URL, appID: "mock-app", appSecret: "mock-secret", fetchResourceToken: func(context.Context) (string, error) { return "mock-token", nil }, client: lark.NewClient("mock-app", "mock-secret", lark.WithOpenBaseUrl(srv.URL)), handler: func(_ core.Platform, msg *core.Message) { got = msg }}
			items := sampleCard(t)
			sub, root := *items[1].MessageId, *items[0].MessageId
			p.dispatchMessage(context.Background(), "interactive", *items[1].Body.Content, nil, sub, "session", "", "", replyContext{}, "", 123)
			if got == nil || got.MessageID != sub || got.UserMessageTimeMs != 123 {
				t.Fatalf("dispatch=%+v", got)
			}
			if success {
				if len(got.Images) != 1 || string(got.Images[0].Data) != "image-bytes" {
					t.Fatalf("images=%v", got.Images)
				}
			} else if !strings.Contains(got.Content, "image_key=img_v3_") || strings.Contains(got.Content, "请升级") {
				t.Fatalf("content=%q", got.Content)
			}
			mu.Lock()
			calls = nil
			mu.Unlock()
			mid := "nested"
			typ := "merge_forward"
			nested := &larkim.Message{MessageId: &mid, MsgType: &typ}
			var sb strings.Builder
			var images []core.ImageAttachment
			var files []core.FileAttachment
			p.formatMergeForwardTree(root, root, map[string][]*larkim.Message{root: {nested}, mid: {items[1]}}, map[string]string{*items[1].Sender.Id: "Alice"}, &sb, &images, &files, 0)
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if !success {
				want = 2
				if (len(calls) != 2 || !strings.Contains(calls[1], "/"+root+"/resources/")) || !strings.Contains(sb.String(), "发送方 Alice") {
					t.Fatalf("calls=%v text=%s", calls, sb.String())
				}
			}
			if len(calls) != want || strings.Contains(sb.String(), "[interactive message]") || strings.Contains(sb.String(), "请升级") {
				t.Fatalf("calls=%v text=%s", calls, sb.String())
			}
			if success && len(images) != 1 {
				t.Fatalf("images=%v", images)
			}
		})
	}
}

func TestInteractiveCard_NativeAndNestedImages(t *testing.T) {
	content := `{"elements":[{"tag":"img","img_key":"native"},{"tag":"list","property":{"items":[{"elements":[{"tag":"img","property":{"img_key":"nested"}}]}]}},{"tag":"img","image_key":"native"},{"tag":"button","text":{"content":"label"},"value":{"tag":"img","img_key":"callback"}}]}`
	text, keys := parseInteractiveCard(content)
	if text != "label" || !reflect.DeepEqual(keys, []string{"native", "nested"}) {
		t.Fatalf("text=%q keys=%v", text, keys)
	}
}
