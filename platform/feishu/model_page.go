package feishu

import (
	"log/slog"
	"net/url"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

// configureModelPage reads model_page_url, the web page where the owner picks
// the bot's model. The /model card and the first-run welcome link to it,
// opened in the Feishu sidebar. Without a valid http(s) URL there is no link.
func (p *Platform) configureModelPage(opts map[string]any) {
	raw, _ := opts["model_page_url"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	page, err := url.Parse(raw)
	if err != nil || (page.Scheme != "http" && page.Scheme != "https") || page.Host == "" {
		slog.Warn("feishu: model_page_url ignored, want an absolute http(s) URL", "platform", p.tag())
		return
	}
	host := "applink.feishu.cn"
	if p.domain == lark.LarkBaseUrl {
		host = "applink.larksuite.com"
	}
	p.modelPageLink = "https://" + host + "/client/web_url/open?mode=sidebar-semi&url=" + url.QueryEscape(page.String())
}

// ModelPageLink implements core.ModelPageLinker: an AppLink that opens the
// model page in the Feishu sidebar, or "" when none is configured.
func (p *Platform) ModelPageLink() string { return p.modelPageLink }
