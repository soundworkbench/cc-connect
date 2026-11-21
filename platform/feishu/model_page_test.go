package feishu

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

const testModelPage = "https://workpartner.example.test/_wp/model"

func newModelPagePlatform(t *testing.T, extra map[string]any) *Platform {
	t.Helper()
	opts := map[string]any{"app_id": "cli_test", "app_secret": "secret", "allow_from": "ou_owner", "cc_data_dir": t.TempDir(), "p2p_catchup": false}
	for k, v := range extra {
		opts[k] = v
	}
	pl, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pl.(core.ModelPageLinker); !ok {
		t.Fatal("feishu platform must implement core.ModelPageLinker")
	}
	return platformOf(pl)
}

func TestModelPage_SidebarAppLink(t *testing.T) {
	p := newModelPagePlatform(t, map[string]any{"model_page_url": " " + testModelPage + " "})
	link, err := url.Parse(p.ModelPageLink())
	if err != nil {
		t.Fatal(err)
	}
	if link.Host != "applink.feishu.cn" || link.Path != "/client/web_url/open" {
		t.Fatalf("link = %s", link)
	}
	if q := link.Query(); q.Get("mode") != "sidebar-semi" || q.Get("url") != testModelPage {
		t.Fatalf("query = %v", q)
	}
}

func TestModelPage_MissingOrInvalidURLHasNoLink(t *testing.T) {
	for _, raw := range []any{nil, "", "  ", "/_wp/model", "ftp://host/_wp/model", "https://"} {
		extra := map[string]any{}
		if raw != nil {
			extra["model_page_url"] = raw
		}
		if got := newModelPagePlatform(t, extra).ModelPageLink(); got != "" {
			t.Fatalf("model_page_url %v: link = %q, want none", raw, got)
		}
	}
}

func TestWelcomeContent_ModelButtonOnlyWithModelPage(t *testing.T) {
	text := "你好"
	msgType, content, err := newModelPagePlatform(t, nil).welcomeContent(text)
	if err != nil || msgType != "text" || !strings.Contains(content, text) {
		t.Fatalf("without page: %q %q %v", msgType, content, err)
	}

	p := newModelPagePlatform(t, map[string]any{"model_page_url": testModelPage})
	msgType, content, err = p.welcomeContent(text)
	if err != nil || msgType != "interactive" {
		t.Fatalf("with page: %q %v", msgType, err)
	}
	var card struct {
		Elements []struct {
			Tag     string           `json:"tag"`
			Content string           `json:"content"`
			Actions []map[string]any `json:"actions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatal(err)
	}
	if len(card.Elements) != 2 || card.Elements[0].Content != text || len(card.Elements[1].Actions) != 1 {
		t.Fatalf("card = %s", content)
	}
	btn := card.Elements[1].Actions[0]
	if btn["url"] != p.ModelPageLink() || btn["value"] != nil || btn["text"].(map[string]any)["content"] != "模型" {
		t.Fatalf("button = %v", btn)
	}
}

func TestRenderCardMap_LinkButtonOpensURL(t *testing.T) {
	card := core.NewCard().Buttons(core.LinkBtn("模型", "https://x.test/a"), core.DefaultBtn("返回", "nav:/help")).Build()
	actions := renderCardMap(card, "feishu:oc_1:ou_1")["elements"].([]map[string]any)[0]["actions"].([]map[string]any)
	if actions[0]["url"] != "https://x.test/a" || actions[0]["value"] != nil {
		t.Fatalf("link button = %v", actions[0])
	}
	if actions[1]["url"] != nil || actions[1]["value"] == nil {
		t.Fatalf("callback button = %v", actions[1])
	}

	compact := core.NewCard().Compact().Buttons(core.LinkBtn("模型", "https://x.test/a")).Build()
	btn := renderCardMap(compact, "")["body"].(map[string]any)["elements"].([]map[string]any)[0]
	behaviors := btn["behaviors"].([]map[string]any)
	if len(behaviors) != 1 || behaviors[0]["type"] != "open_url" || behaviors[0]["default_url"] != "https://x.test/a" {
		t.Fatalf("compact link button = %v", btn)
	}
}
