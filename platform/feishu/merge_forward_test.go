package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

// A forwarded bot card, image, file and rich-text post: the agent must get the
// card's own text (not the downgraded placeholder) and every resource, which
// Feishu only serves under the outer forward's message ID.
func TestParseMergeForward_CardTextAndResources(t *testing.T) {
	const root = "om_forward_root"
	const cardKey = "img_card_body"
	pngBytes := "\x89PNG\r\n\x1a\nforwarded-image"

	card, err := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": "日报"}},
		"body": map[string]any{"elements": []any{
			map[string]any{"tag": "markdown", "content": "今天完成了合并转发修复"},
			map[string]any{"tag": "img", "img_key": cardKey},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// What Feishu returns for the same card without card_msg_content_type.
	downgraded := `{"title":null,"elements":[[{"tag":"img","image_key":"img_upgrade_placeholder"},{"tag":"text","text":"请升级至最新版本客户端，以查看内容"},{"tag":"text","text":""}]]}`

	item := func(id, typ, senderID, senderType, content string) map[string]any {
		m := map[string]any{
			"message_id":  id,
			"msg_type":    typ,
			"create_time": "1795310040000",
			"sender":      map[string]any{"id": senderID, "id_type": "open_id", "sender_type": senderType},
			"body":        map[string]any{"content": content},
		}
		if id != root {
			m["upper_message_id"] = root
		}
		return m
	}

	var mu sync.Mutex
	var resourceIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(r.URL.Path, "/resources/") {
			w.Header().Set("Content-Type", "application/json")
		}
		switch {
		case strings.Contains(r.URL.Path, "tenant_access_token"):
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "tenant-token"})
		case r.URL.Path == "/open-apis/im/v1/messages/"+root:
			cardContent := downgraded
			if r.URL.Query().Get("card_msg_content_type") == "user_card_content" {
				cardContent = string(card)
			}
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"items": []map[string]any{
				item(root, "merge_forward", "ou_jason", "user", "Merged and Forwarded Message"),
				item("om_sub_card", "interactive", "cli_peer", "app", cardContent),
				item("om_sub_image", "image", "ou_alice", "user", `{"image_key":"img_plain"}`),
				item("om_sub_file", "file", "ou_alice", "user", `{"file_key":"file_doc","file_name":"report.pdf"}`),
				item("om_sub_post", "post", "ou_alice", "user", `{"title":"","content":[[{"tag":"text","text":"见图"},{"tag":"img","image_key":"img_post"}]]}`),
			}}})
		case strings.Contains(r.URL.Path, "/resources/"):
			msgID := strings.Split(strings.TrimPrefix(r.URL.Path, "/open-apis/im/v1/messages/"), "/")[0]
			resourceIDs = append(resourceIDs, msgID+" "+r.URL.Query().Get("type"))
			if r.URL.Query().Get("type") == "image" && r.Header.Get("Range") != "" {
				t.Errorf("image download sent Range %q", r.Header.Get("Range"))
			}
			if msgID != root {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":234003,"msg":"File not in msg."}`))
				return
			}
			_, _ = w.Write([]byte(pngBytes))
		default:
			t.Errorf("unexpected API %s", r.URL.String())
			writeJSON(t, w, map[string]any{"code": 1})
		}
	}))
	defer srv.Close()

	p := &Platform{
		platformName:         "feishu",
		domain:               srv.URL,
		peerBots:             map[string]string{"cli_peer": "伙伴助理"},
		resourceDownloadHTTP: &http.Client{Timeout: 10 * time.Second},
		client:               lark.NewClient("mock-app", "mock-secret", lark.WithOpenBaseUrl(srv.URL)),
	}
	p.fetchResourceToken = func(_ context.Context) (string, error) { return "tenant-token", nil }
	p.userNameCache.Store("ou_alice", "Alice")

	text, images, files := p.parseMergeForward(root)

	for _, want := range []string{"伙伴助理:", "日报", "今天完成了合并转发修复", "Alice: [image]", "[file: report.pdf]", "见图"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"请升级", "download failed", "无法下载"} {
		if strings.Contains(text, bad) {
			t.Errorf("text contains %q:\n%s", bad, text)
		}
	}
	if len(images) != 3 {
		t.Errorf("images = %d, want card, image and post images", len(images))
	}
	for _, img := range images {
		if string(img.Data) != pngBytes || img.MimeType != "image/png" {
			t.Errorf("image = %q (%s)", img.Data, img.MimeType)
		}
	}
	if len(files) != 1 || files[0].FileName != "report.pdf" || string(files[0].Data) != pngBytes {
		t.Errorf("files = %+v", files)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(resourceIDs) != 4 {
		t.Errorf("resource requests = %v, want one per resource", resourceIDs)
	}
	for _, r := range resourceIDs {
		if !strings.HasPrefix(r, root+" ") {
			t.Errorf("resource fetched under %q, want root forward %s", r, root)
		}
	}
}
