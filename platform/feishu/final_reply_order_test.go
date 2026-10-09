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

func TestProgressMergeFactory(t *testing.T) {
	for _, name := range []string{"feishu", "lark"} {
		for _, tc := range []struct {
			name    string
			options map[string]any
			want    core.ProgressMergeCfg
			invalid bool
		}{
			{name: "default", want: core.DefaultProgressMergeCfg()},
			{name: "off", options: map[string]any{"merge_progress_into_final": false}, want: core.ProgressMergeCfg{MinChars: 400, Tables: true}},
			{name: "custom", options: map[string]any{"merge_progress_min_chars": int64(600), "merge_progress_tables": false}, want: core.ProgressMergeCfg{Enabled: true, MinChars: 600}},
			{name: "bad_bool", options: map[string]any{"merge_progress_into_final": "true"}, invalid: true},
			{name: "bad_tables", options: map[string]any{"merge_progress_tables": 1}, invalid: true},
			{name: "zero", options: map[string]any{"merge_progress_min_chars": 0}, invalid: true},
			{name: "negative", options: map[string]any{"merge_progress_min_chars": -1}, invalid: true},
			{name: "fraction", options: map[string]any{"merge_progress_min_chars": 400.5}, invalid: true},
			{name: "string", options: map[string]any{"merge_progress_min_chars": "400"}, invalid: true},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				opts := map[string]any{"app_id": "fixture", "app_secret": "fixture"}
				for k, v := range tc.options {
					opts[k] = v
				}
				p, err := newPlatform(name, lark.FeishuBaseUrl, opts)
				if tc.invalid {
					if err == nil || !strings.Contains(err.Error(), "merge_progress") {
						t.Fatalf("want config error, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := p.(core.ProgressMergePreference).ProgressMergeConfig(); got != tc.want {
					t.Fatalf("got %+v want %+v", got, tc.want)
				}
			})
		}
	}
	if got := (&Platform{}).ProgressMergeConfig(); got != core.DefaultProgressMergeCfg() {
		t.Fatal(got)
	}
}
