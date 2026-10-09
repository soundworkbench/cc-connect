package feishu

import (
	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"strings"
	"testing"
)

func TestFinalReplyOrderFactory(t *testing.T) {
	for _, name := range []string{"feishu", "lark"} {
		for _, tc := range []struct {
			name     string
			value    any
			supplied bool
			want     core.FinalReplyOrder
			invalid  bool
		}{
			{name: "default", want: core.FinalReplyRecallFirst},
			{name: "recall_first", value: "recall_first", supplied: true, want: core.FinalReplyRecallFirst},
			{name: "send_first", value: "send_first", supplied: true, want: core.FinalReplySendFirst},
			{name: "unknown", value: "last_first", supplied: true, invalid: true},
			{name: "empty", value: "", supplied: true, invalid: true},
			{name: "type", value: 1, supplied: true, invalid: true},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				opts := map[string]any{"app_id": "fixture", "app_secret": "fixture"}
				if tc.supplied {
					opts["final_reply_order"] = tc.value
				}
				platform, err := newPlatform(name, lark.FeishuBaseUrl, opts)
				if tc.invalid {
					if err == nil || !strings.Contains(err.Error(), "final_reply_order") {
						t.Fatalf("expected config error, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := platform.(core.FinalReplyOrderPreference).FinalReplyOrder(); got != tc.want {
					t.Fatalf("got %q want %q", got, tc.want)
				}
			})
		}
	}
	if got := (&Platform{}).FinalReplyOrder(); got != core.FinalReplyRecallFirst {
		t.Fatalf("zero platform order %q", got)
	}
}
