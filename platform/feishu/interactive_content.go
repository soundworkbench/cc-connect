package feishu

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// Only traverse display fields: action values, URLs and callback payloads are
// not message text. Arrays cover both legacy row arrays and modern columns.
func parseInteractiveCard(content string) (string, []string) {
	var wrapper struct {
		JSONCard string `json:"json_card"`
		UserDSL  string `json:"user_dsl"`
	}
	if json.Unmarshal([]byte(content), &wrapper) == nil {
		if wrapper.JSONCard != "" {
			content = wrapper.JSONCard
		} else if wrapper.UserDSL != "" {
			var dsl map[string]any
			if json.Unmarshal([]byte(wrapper.UserDSL), &dsl) == nil && dsl != nil {
				content = wrapper.UserDSL
			}
		}
	}
	var card map[string]any
	if json.Unmarshal([]byte(content), &card) != nil {
		return "[interactive card]", nil
	}
	var parts, keys []string
	seen := map[string]bool{}
	add := func(text string) {
		text = strings.TrimSpace(text)
		switch text {
		case "", "请升级至最新版本客户端，以查看内容", "请升级至最新版本客户端以查看内容", "请升级至最新版本客户端，以查看内容。", "Please upgrade to the latest version of the client to view the content":
			return
		}
		parts = append(parts, text)
	}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case string:
			add(v)
		case []any:
			for _, child := range v {
				walk(child)
			}
		case map[string]any:
			tag, _ := v["tag"].(string)
			// Retain the existing formatting for schema 2 code, tables and lists.
			switch tag {
			case "code_block", "code_span", "table", "list", "hr":
				raw, _ := json.Marshal(v)
				var formatted []string
				extractCardElements([]json.RawMessage{raw}, &formatted)
				for _, text := range formatted {
					add(text)
				}
				return
			}
			for _, field := range []string{"header", "title", "text", "content", "alt", "body", "elements", "columns", "fields", "actions", "property"} {
				if child, ok := v[field]; ok {
					walk(child)
				}
			}
		}
	}
	// Collect images separately so formatted list/table/code text paths do not
	// prevent nested display images from reaching the agent.
	var walkImages func(any)
	walkImages = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, child := range v {
				walkImages(child)
			}
		case map[string]any:
			tag, _ := v["tag"].(string)
			if tag == "img" || tag == "image" {
				key, _ := v["image_key"].(string)
				if key == "" {
					key, _ = v["img_key"].(string)
				}
				if prop, ok := v["property"].(map[string]any); ok && key == "" {
					key, _ = prop["image_key"].(string)
					if key == "" {
						key, _ = prop["img_key"].(string)
					}
				}
				if key != "" && !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			}
			var fields []string
			for field := range v {
				// Callback/action values are not card display resources.
				if field != "value" && field != "behaviors" && field != "config" {
					fields = append(fields, field)
				}
			}
			sort.Strings(fields)
			for _, field := range fields {
				walkImages(v[field])
			}
		}
	}
	walkImages(card)
	walk(card)
	if len(parts) == 0 && len(keys) == 0 {
		return "[interactive card]", nil
	}
	return strings.Join(parts, "\n"), keys
}

// The root remains the outermost forward, including through nested forwards.
// Identical IDs (direct cards) and duplicate image keys are attempted once.
func interactiveCardContent(content, messageID, rootID, sender string, download func(string, string) ([]byte, string, error)) (string, []core.ImageAttachment) {
	text, keys := parseInteractiveCard(content)
	var images []core.ImageAttachment
	for _, key := range keys {
		data, mime, err := download(messageID, key)
		if err != nil && rootID != "" && rootID != messageID {
			data, mime, err = download(rootID, key)
		}
		if err == nil {
			images = append(images, core.ImageAttachment{MimeType: mime, Data: data})
		} else {
			if sender == "" {
				sender = "unknown"
			}
			// Do not log SDK errors here: transport errors can include credentials.
			placeholder := core.NewI18n(core.LangChinese).Tf(core.MsgFeishuCardImageUnavailable, sender, key)
			if text != "" {
				text += "\n"
			}
			text += placeholder
		}
	}
	return text, images
}
