package feishu

import (
	"context"
	"encoding/json"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSteerReactionReceivedThenDone(t *testing.T) {
	for _, consumed := range []bool{true, false} {
		t.Run(map[bool]string{true: "ack", false: "exit"}[consumed], func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					w.Write([]byte(`{"code":0,"expire":7200,"tenant_access_token":"fixture"}`))
					return
				}
				if !strings.Contains(r.URL.Path, "/messages/om_supplement/reactions") {
					t.Errorf("wrong message path: %s", r.URL.Path)
				}
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodDelete {
					calls = append(calls, "remove")
				} else {
					var body struct {
						ReactionType struct {
							EmojiType string `json:"emoji_type"`
						} `json:"reaction_type"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					calls = append(calls, body.ReactionType.EmojiType)
				}
				w.Write([]byte(`{"code":0,"data":{"reaction_id":"reaction-1"}}`))
			}))
			defer srv.Close()
			p := &Platform{client: lark.NewClient("steer-reaction-test", "fixture", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())), reactionEmoji: "OnIt"}
			done := p.StartSteerReaction(context.Background(), replyContext{messageID: "om_supplement"})
			done(consumed)
			done(consumed)
			want := []string{"OnIt", "remove"}
			if consumed {
				want = []string{"OnIt", "DONE", "remove"}
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("%v != %v", calls, want)
			}
		})
	}
}
