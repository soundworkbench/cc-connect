package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func seedCatchupSession(t *testing.T, p *Platform, now time.Time) {
	t.Helper()
	store := filepath.Join(t.TempDir(), "sessions.json")
	sessions := core.NewSessionManager(store)
	sessions.GetOrCreateActive("feishu:oc_private:ou_user")
	sessions.GetOrCreateActive("feishu:oc_group:ou_user")
	sessions.GetOrCreateActive("feishu:oc_unknown:ou_user")
	sessions.GetOrCreateActive("lark:oc_other:ou_user")
	sessions.Save()
	p.catchup.sessionStore = store
	p.catchup.snapshot.Heartbeat = now.Add(-time.Minute)
	p.catchup.snapshot.PrivateChats["oc_private"] = now
	// A previously seen private chat without any existing session is ineligible.
	p.catchup.snapshot.PrivateChats["oc_no_session"] = now
}

func mockCatchupMessage(id string, now time.Time, sender string) map[string]any {
	return map[string]any{"message_id": id, "create_time": strconv.FormatInt(now.UnixMilli(), 10),
		"msg_type": "text", "sender": map[string]any{"id": "ou_user", "id_type": "open_id", "sender_type": sender},
		"body": map[string]any{"content": fmt.Sprintf(`{"text":%q}`, id)}}
}

func TestP2PCatchup_HistoryPaginationOrderAndFiltering(t *testing.T) {
	now := time.Now()
	p, got := newCatchupRegressionPlatform(t, t.TempDir(), true)
	seedCatchupSession(t, p, now)
	if _, err := p.catchup.claim("om_seen", "oc_private", now); err != nil {
		t.Fatal(err)
	}
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "catchup-test-token", "expire": 7200})
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages" {
			t.Errorf("unexpected API: %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		q := r.URL.Query()
		if q.Get("container_id") != "oc_private" || q.Get("container_id_type") != "chat" || q.Get("sort_type") != "ByCreateTimeAsc" || q.Get("page_size") != "50" {
			t.Errorf("incorrect history query: %v", q)
		}
		start, _ := strconv.ParseInt(q.Get("start_time"), 10, 64)
		if start != now.Add(-time.Minute).Unix() {
			t.Errorf("start_time = %d", start)
		}
		pages++
		items := []map[string]any{
			mockCatchupMessage("om_second", now.Add(-20*time.Second), "user"),
			mockCatchupMessage("om_bot", now.Add(-25*time.Second), "app"),
			mockCatchupMessage("om_seen", now.Add(-30*time.Second), "user"),
			mockCatchupMessage("om_too_old", now.Add(-11*time.Minute), "user"),
			mockCatchupMessage("om_future", now.Add(time.Minute), "user"),
		}
		if q.Get("page_token") == "next" {
			items = []map[string]any{mockCatchupMessage("om_first", now.Add(-40*time.Second), "user")}
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"has_more": false, "items": items}})
			return
		}
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"has_more": true, "page_token": "next", "items": items}})
	}))
	defer srv.Close()
	p.client = lark.NewClient("catchup-history", "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()))
	p.recoverP2PMessages(context.Background(), now)
	if pages != 2 {
		t.Fatalf("pages = %d, want 2", pages)
	}
	for _, id := range []string{"om_first", "om_second"} {
		select {
		case msg := <-got:
			if msg.MessageID != id {
				t.Fatalf("message = %s, want %s", msg.MessageID, id)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s", id)
		}
	}
	select {
	case msg := <-got:
		t.Fatalf("unexpected message: %+v", msg)
	default:
	}
}

func TestP2PCatchup_PermissionErrorSkipsWithOneWarning(t *testing.T) {
	for _, code := range []int{99991672, 230027} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			now := time.Now()
			p, got := newCatchupRegressionPlatform(t, t.TempDir(), true)
			seedCatchupSession(t, p, now)
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			defer slog.SetDefault(previous)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
					return
				}
				calls++
				writeJSON(t, w, map[string]any{"code": code, "msg": "Access denied: missing scope"})
			}))
			defer srv.Close()
			p.client = lark.NewClient("catchup-permission", "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()))
			ready := make(chan struct{})
			p.startP2PCatchup(ready)
			close(ready)
			// A successful persisted heartbeat proves the permission failure did not
			// terminate the startup worker. Stop joins it before reading captured logs.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				p.catchup.mu.Lock()
				heartbeat := p.catchup.snapshot.Heartbeat
				p.catchup.mu.Unlock()
				if !heartbeat.Before(now) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			p.stopP2PCatchup()
			p.catchup.mu.Lock()
			heartbeat := p.catchup.snapshot.Heartbeat
			p.catchup.mu.Unlock()
			if heartbeat.Before(now) {
				t.Fatal("permission failure prevented startup heartbeat")
			}
			if calls != 1 || strings.Count(output.String(), `"level":"WARN"`) != 1 || strings.Contains(output.String(), `"level":"ERROR"`) || !strings.Contains(output.String(), fmt.Sprintf("API code %d", code)) {
				t.Fatalf("calls=%d logs=%s", calls, output.String())
			}
			select {
			case msg := <-got:
				t.Fatalf("permission error delivered a message: %+v", msg)
			default:
			}
		})
	}
}

func TestP2PCatchup_RetentionBoundAtomicStateAndConcurrentClaim(t *testing.T) {
	p, _ := newCatchupRegressionPlatform(t, t.TempDir(), true)
	now := time.Now()
	p.catchup.snapshot.Processed["expired"] = now.Add(-p2pDedupRetention - time.Second)
	for i := 0; i < p2pMaxMessageIDs+5; i++ {
		p.catchup.snapshot.Processed[fmt.Sprintf("old-%05d", i)] = now.Add(-time.Minute).Add(time.Duration(i))
	}
	p.catchup.heartbeat(now)
	loaded, err := loadP2PCatchup(p.catchup.path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.snapshot.Processed) != p2pMaxMessageIDs {
		t.Fatalf("retained %d IDs", len(loaded.snapshot.Processed))
	}
	if _, ok := loaded.snapshot.Processed["expired"]; ok {
		t.Fatal("expired ID retained")
	}
	var wg sync.WaitGroup
	accepted := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			duplicate, err := loaded.claim("same-id", "oc_private", now)
			if err != nil {
				t.Error(err)
			}
			accepted <- !duplicate
		}()
	}
	wg.Wait()
	close(accepted)
	count := 0
	for ok := range accepted {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("accepted %d concurrent duplicates", count)
	}
	data, err := os.ReadFile(loaded.path)
	if err != nil {
		t.Fatal(err)
	}
	var state p2pCatchupSnapshot
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Processed["same-id"]; !ok {
		t.Fatal("accepted ID missing from disk")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(loaded.path), ".p2p-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary state files left behind: %v, %v", files, err)
	}
	info, err := os.Stat(loaded.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode %o", info.Mode().Perm())
	}
}

func TestP2PCatchup_FirstRunAndConnectionReadiness(t *testing.T) {
	p, _ := newCatchupRegressionPlatform(t, t.TempDir(), true)
	ready := make(chan struct{})
	p.startP2PCatchup(ready)
	// Stop before a connection exists; no history request or heartbeat may run.
	p.stopP2PCatchup()
	if _, err := os.Stat(p.catchup.path); !os.IsNotExist(err) {
		t.Fatalf("state created before connection: %v", err)
	}
	p.startP2PCatchup(ready)
	close(ready)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.catchup.mu.Lock()
		heartbeat := p.catchup.snapshot.Heartbeat
		p.catchup.mu.Unlock()
		if !heartbeat.IsZero() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	p.stopP2PCatchup()
	loaded, err := loadP2PCatchup(p.catchup.path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.snapshot.Heartbeat.IsZero() {
		t.Fatal("first connection did not establish baseline")
	}
}

func TestP2PCatchup_ClampsHistoryToTenMinutes(t *testing.T) {
	p, _ := newCatchupRegressionPlatform(t, t.TempDir(), true)
	now := time.Now()
	seedCatchupSession(t, p, now)
	p.catchup.snapshot.Heartbeat = now.Add(-time.Hour)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		calls++
		if r.URL.Query().Get("start_time") != strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10) {
			t.Errorf("unbounded recovery query: %s", r.URL.RawQuery)
		}
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"has_more": false, "items": []*larkim.Message{}}})
	}))
	defer srv.Close()
	p.client = lark.NewClient("catchup-clamp", "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()))
	p.recoverP2PMessages(context.Background(), now)
	if calls != 1 {
		t.Fatalf("history calls=%d", calls)
	}
}

func TestP2PCatchup_LiveMessagesWaitForHistory(t *testing.T) {
	now := time.Now()
	p, got := newCatchupRegressionPlatform(t, t.TempDir(), true)
	seedCatchupSession(t, p, now)
	requestStarted := make(chan struct{})
	releaseHistory := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "tenant_access_token": "test-token", "expire": 7200})
			return
		}
		close(requestStarted)
		<-releaseHistory
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"has_more": false, "items": []map[string]any{
			mockCatchupMessage("om_history", now.Add(-30*time.Second), "user"),
		}}})
	}))
	defer srv.Close()
	p.client = lark.NewClient("catchup-live", "test-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()))
	ready := make(chan struct{})
	p.startP2PCatchup(ready)
	close(ready)
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("history did not start")
	}
	// A retransmission overlaps the historical response and a new live message
	// arrives while fetching. Admission must remain ordered and deduplicated.
	if err := p.onMessage(context.Background(), catchupRegressionEvent("om_history", "p2p", now.Add(-30*time.Second))); err != nil {
		t.Fatal(err)
	}
	live := catchupRegressionEvent("om_live", "p2p", now)
	if err := p.onMessage(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	// The buffer must own the event rather than borrowing the SDK's pointers.
	*live.Event.Message.Content = `{"text":"mutated SDK buffer"}`
	select {
	case msg := <-got:
		t.Fatalf("live traffic overtook history: %+v", msg)
	default:
	}
	close(releaseHistory)
	for _, id := range []string{"om_history", "om_live"} {
		select {
		case msg := <-got:
			if msg.MessageID != id {
				t.Fatalf("message=%s, want %s", msg.MessageID, id)
			}
			if id == "om_live" && msg.Content != "message during restart" {
				t.Fatalf("buffered content mutated: %s", msg.Content)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s", id)
		}
	}
	p.stopP2PCatchup()
	select {
	case msg := <-got:
		t.Fatalf("duplicate delivery: %+v", msg)
	default:
	}
}

func TestP2PCatchup_DefaultEnabledAndCorruptStatePreserved(t *testing.T) {
	opts := map[string]any{"app_id": "catchup-default", "app_secret": "test-secret", "allow_from": "*", "enable_feishu_card": false, "cc_data_dir": t.TempDir()}
	platform, err := newPlatform("feishu", lark.FeishuBaseUrl, opts)
	if err != nil {
		t.Fatal(err)
	}
	p := platform.(*Platform)
	if p.catchup == nil {
		t.Fatal("catchup should default to enabled")
	}
	p.catchup.heartbeat(time.Now())
	if err := os.WriteFile(p.catchup.path, []byte("corrupt state"), 0600); err != nil {
		t.Fatal(err)
	}
	platform, err = newPlatform("feishu", lark.FeishuBaseUrl, opts)
	if err != nil {
		t.Fatalf("corrupt state prevented platform construction: %v", err)
	}
	if platform.(*Platform).catchup != nil {
		t.Fatal("must preserve old-message filtering when durable dedup cannot be loaded")
	}
	data, err := os.ReadFile(p.catchup.path)
	if err != nil || string(data) != "corrupt state" {
		t.Fatalf("corrupt state was overwritten: %s, %v", data, err)
	}
}
