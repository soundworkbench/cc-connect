package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test values only: none of these is a real credential.
const (
	fakeToken    = "pat-na1-FAKE0000-1111-2222-3333-444455556666"
	fakePassword = `Fa<k>e"pa&ss\word`
)

func TestSecretNames(t *testing.T) {
	for _, ok := range []string{"hubspot-token", "ab", "web-ssh-key", "a1", strings.Repeat("a", 40)} {
		if !ValidSecretName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a", "Hubspot", "hub_spot", "hub.spot", "-token", "token-", "../x", "a b", strings.Repeat("a", 41)} {
		if ValidSecretName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestSecretSaveIsPrivateAndAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".secrets")
	if err := os.MkdirAll(dir, 0o755); err != nil { // a looser directory is tightened
		t.Fatal(err)
	}
	s := NewSecretStore(dir)
	updated, err := s.Save("hubspot-token", fakeToken)
	if err != nil || updated {
		t.Fatalf("first save: updated=%v err=%v", updated, err)
	}
	st, _ := os.Stat(dir)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o, want 700", st.Mode().Perm())
	}
	fst, _ := os.Stat(filepath.Join(dir, "hubspot-token"))
	if fst.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 600", fst.Mode().Perm())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hubspot-token")); string(b) != fakeToken {
		t.Fatalf("content = %q", b)
	}
	updated, err = s.Save("hubspot-token", "pat-na1-FAKE-second-value")
	if err != nil || !updated {
		t.Fatalf("second save: updated=%v err=%v", updated, err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
	if _, err := s.Save("../escape", fakeToken); err == nil {
		t.Fatal("a bad name was saved")
	}
}

func TestSecretListAndDelete(t *testing.T) {
	s := NewSecretStore(filepath.Join(t.TempDir(), ".secrets"))
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("empty list = %v, %v", list, err)
	}
	for _, n := range []string{"zz-key", "aa-key"} {
		if _, err := s.Save(n, fakeToken); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil || len(list) != 2 || list[0].Name != "aa-key" || list[1].Name != "zz-key" || list[0].SavedAt.IsZero() {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.Delete("aa-key"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("aa-key"); err != ErrSecretNotFound {
		t.Fatalf("second delete: %v", err)
	}
	if s.Exists("aa-key") || !s.Exists("zz-key") {
		t.Fatal("delete removed the wrong secret")
	}
}

func TestSecretRedaction(t *testing.T) {
	s := NewSecretStore(filepath.Join(t.TempDir(), ".secrets"))
	mustSave(t, s, "hubspot-token", fakeToken)
	mustSave(t, s, "site-pass", fakePassword)
	mustSave(t, s, "pin", "1234567") // too short to mask

	got := s.Redact("curl -H 'Authorization: Bearer " + fakeToken + "' (pin 1234567)")
	if strings.Contains(got, fakeToken) || !strings.Contains(got, "‹密钥:hubspot-token›") || !strings.Contains(got, "1234567") {
		t.Fatalf("Redact = %q", got)
	}

	// A text message body: the text is a JSON string inside the request JSON.
	inner, _ := json.Marshal(map[string]string{"text": "密码是 " + fakePassword + "，令牌 " + fakeToken})
	body, _ := json.Marshal(map[string]any{"receive_id": "oc_x", "msg_type": "text", "content": string(inner)})
	out, changed := s.RedactJSON(body)
	if !changed || bytes.Contains(out, []byte("FAKE0000")) || bytes.Contains(out, []byte("pa&ss")) || bytes.Contains(out, []byte(`pa\\u0026ss`)) {
		t.Fatalf("RedactJSON = %s", out)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("masked body is not JSON: %v", err)
	}
	if !strings.Contains(back["content"].(string), "‹密钥:site-pass›") {
		t.Fatalf("content = %v", back["content"])
	}
	if _, changed := s.RedactJSON([]byte(`{"text":"nothing here"}`)); changed {
		t.Fatal("a clean body was changed")
	}
}

func TestSecretMaskingWhileStreaming(t *testing.T) {
	s := NewSecretStore(filepath.Join(t.TempDir(), ".secrets"))
	mustSave(t, s, "hubspot-token", fakeToken)
	for _, partial := range []string{fakeToken[:6], fakeToken[:20], fakeToken[:len(fakeToken)-1]} {
		got := s.MaskStreaming("令牌是 " + partial)
		if got != "令牌是 ‹密钥:hubspot-token›" {
			t.Fatalf("partial %q shown as %q", partial, got)
		}
	}
	if got := s.MaskStreaming("令牌是 " + fakeToken[:12] + "…"); got != "令牌是 ‹密钥:hubspot-token›…" {
		t.Fatalf("cut preview = %q", got)
	}
	if got := s.MaskStreaming("前缀 pat-n 结束"); got != "前缀 pat-n 结束" {
		t.Fatalf("short or inner prefix masked: %q", got)
	}

	e := &Engine{secrets: s}
	mask := e.maskStreaming(func(text string) string { return strings.ToUpper(text[:1]) + text[1:] })
	if got := mask("x " + fakeToken[:10]); got != "X ‹密钥:hubspot-token›" {
		t.Fatalf("preview transform = %q", got)
	}
}

func TestSecretMaskListIsCached(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".secrets")
	s := NewSecretStore(dir)
	mustSave(t, s, "hubspot-token", fakeToken)
	if !strings.Contains(s.Redact(fakeToken), "‹密钥") {
		t.Fatal("not masked")
	}
	// Written by hand (e.g. the assistant moves a key file in): no disk read
	// until the next check, then the change is picked up.
	if err := os.WriteFile(filepath.Join(dir, "other-key"), []byte("FAKE-other-key-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Redact("FAKE-other-key-value"), "‹密钥") {
		t.Fatal("the list was read again within the check interval")
	}
	s.mu.Lock()
	s.checkedAt = time.Now().Add(-2 * secretCheckInterval)
	s.dirMod = time.Time{} // a directory change between checks
	s.mu.Unlock()
	if got := s.Redact("FAKE-other-key-value"); got != "‹密钥:other-key›" {
		t.Fatalf("new file not picked up: %q", got)
	}
	if err := s.Delete("other-key"); err != nil {
		t.Fatal(err)
	}
	if got := s.Redact("FAKE-other-key-value"); got != "FAKE-other-key-value" {
		t.Fatalf("deleted secret still masked: %q", got)
	}
}

func mustSave(t *testing.T, s *SecretStore, name, value string) {
	t.Helper()
	if _, err := s.Save(name, value); err != nil {
		t.Fatal(err)
	}
}

// ── Card flow ───────────────────────────────────────────────────

type secretPlatform struct {
	stubCardPlatform
	kinds map[string]string // session key -> owner, or "" for a group
}

func (p *secretPlatform) PrivateChatUser(_ context.Context, key string) (string, error) {
	owner, ok := p.kinds[key]
	if !ok || owner == "" {
		return "", ErrNotPrivateChat
	}
	return owner, nil
}

const (
	secretDM    = "test:oc_dm:ou_owner"
	secretGroup = "test:oc_group:ou_owner"
)

type noticeLog struct {
	mu   sync.Mutex
	sent []string
}

func (n *noticeLog) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.sent...)
}

func newSecretEngine(t *testing.T) (*Engine, *secretPlatform, *noticeLog) {
	t.Helper()
	p := &secretPlatform{
		stubCardPlatform: stubCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}},
		kinds:            map[string]string{secretDM: "ou_owner", secretGroup: ""},
	}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	e.SetSecretStore(NewSecretStore(filepath.Join(t.TempDir(), ".secrets")))
	n := &noticeLog{}
	e.secretNotify = func(key, text string) error {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.sent = append(n.sent, key+" | "+text)
		return nil
	}
	return e, p, n
}

func TestSecretCardOnlyInPrivateChat(t *testing.T) {
	e, p, _ := newSecretEngine(t)
	_, err := e.SendSecretCard(SecretRequest{SessionKey: secretGroup, Name: "hubspot-token"})
	if err == nil || !strings.Contains(err.Error(), "private chat") {
		t.Fatalf("group: err = %v", err)
	}
	if len(p.sentCards) != 0 {
		t.Fatal("a card was sent to a group")
	}
	if _, err := e.SendSecretCard(SecretRequest{SessionKey: secretDM, Name: "HubSpot"}); err == nil {
		t.Fatal("a bad name was accepted")
	}

	replaces, err := e.SendSecretCard(SecretRequest{SessionKey: secretDM, Name: "hubspot-token", Hint: "  用来读取\n商机数据 ",
		URL: "https://example.invalid/secrets/hubspot-token"})
	if err != nil || replaces {
		t.Fatalf("send: replaces=%v err=%v", replaces, err)
	}
	if len(p.sentCards) != 1 {
		t.Fatalf("cards sent = %d", len(p.sentCards))
	}
	c := p.sentCards[0]
	txt := cardText(c)
	for _, want := range []string{"填写密钥", "hubspot-token", "用来读取 商机数据", "不进聊天记录", "超过 1000 字或带换行", "[网页填写](https://example.invalid/secrets/hubspot-token)"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("card lacks %q:\n%s", want, txt)
		}
	}
	if !c.Shared {
		t.Fatal("the card is not a shared (2.0) card")
	}
	form := findForm(t, c)
	f := form.Fields[0]
	if len(form.Fields) != 1 || !f.Password || f.MaxLength != 1000 || f.Kind != "input" || form.SubmitText != "保存" ||
		form.SubmitValue != "act:/secret-submit hubspot-token" || form.Owner != "ou_owner" {
		t.Fatalf("form = %+v", form)
	}

	if _, err := e.secrets.Save("hubspot-token", fakeToken); err != nil {
		t.Fatal(err)
	}
	replaces, err = e.SendSecretCard(SecretRequest{SessionKey: secretDM, Name: "hubspot-token"})
	if err != nil || !replaces || !strings.Contains(cardText(p.sentCards[1]), "替换旧的") {
		t.Fatalf("resend: replaces=%v err=%v", replaces, err)
	}
	if strings.Contains(cardText(p.sentCards[1]), "网页填写](") {
		t.Fatal("a card without --url links a page")
	}
}

func findForm(t *testing.T, c *Card) CardForm {
	t.Helper()
	for _, el := range c.Elements {
		if f, ok := el.(CardForm); ok {
			return f
		}
	}
	t.Fatal("card has no form")
	return CardForm{}
}

func submitSecret(e *Engine, name, value string) *Card {
	raw, _ := json.Marshal(map[string]string{secretField: value})
	return e.handleCardNav("act:/secret-submit "+name+" "+string(raw), secretDM)
}

func TestSecretSubmitSavesAndNeverShowsTheValue(t *testing.T) {
	logs := captureCoreLogs(t)
	e, _, notices := newSecretEngine(t)
	done := submitSecret(e, "hubspot-token", "  "+fakeToken+"  ")
	if done == nil || done.Toast != "" || done.Header == nil || done.Header.Title != "✅ 已保存：hubspot-token" || done.Header.Color != "green" || !done.Shared {
		t.Fatalf("saved card = %+v", done)
	}
	if b, _ := os.ReadFile(filepath.Join(e.secrets.Dir(), "hubspot-token")); string(b) != fakeToken {
		t.Fatalf("file = %q", b)
	}
	again := submitSecret(e, "hubspot-token", "pat-na1-FAKE-replacement-value")
	if again == nil || again.Header.Title != "✅ 已更新：hubspot-token" {
		t.Fatalf("second save card = %+v", again)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(notices.all()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := notices.all()
	if len(got) != 2 || !strings.HasPrefix(got[0], secretDM+" | ") ||
		!strings.Contains(got[0], "用户已保存密钥 hubspot-token") || !strings.Contains(got[0], "$(cat "+e.secrets.Dir()+"/hubspot-token)") ||
		!strings.Contains(got[1], "用户已更新密钥 hubspot-token") {
		t.Fatalf("notices = %q", got)
	}
	everything := cardText(done) + cardText(again) + strings.Join(got, "\n") + logs.String()
	for _, v := range []string{fakeToken, "FAKE-replacement"} {
		if strings.Contains(everything, v) {
			t.Fatalf("the value %q showed up:\n%s", v, everything)
		}
	}
	if !strings.Contains(logs.String(), "secret: saved") {
		t.Fatalf("save not logged: %s", logs.String())
	}
}

func TestSecretNoticeUsesHomePath(t *testing.T) {
	e := &Engine{secrets: NewSecretStore(DefaultSecretDir())}
	if got := e.secretNotice("hubspot-token", false); got != "【密钥已保存】用户已保存密钥 hubspot-token，命令里用 $(cat ~/.secrets/hubspot-token) 读取，不要显示内容。" {
		t.Fatalf("notice = %q", got)
	}
}

func TestSecretSubmitRejectsWhatTheBoxCannotHold(t *testing.T) {
	logs := captureCoreLogs(t)
	e, _, notices := newSecretEngine(t)
	pem := "-----BEGIN OPENSSH PRIVATE KEY----- FAKEb3BlbnNzaC1rZXktdjEAAAAA -----END OPENSSH PRIVATE KEY-----"
	for name, v := range map[string]string{
		"too long":   strings.Repeat("F", 1001),
		"line break": "FAKE-line-one\nFAKE-line-two",
		"flattened":  pem,
	} {
		c := submitSecret(e, "web-ssh-key", v)
		if c == nil || c.Toast != secretTooLong {
			t.Fatalf("%s: got %+v", name, c)
		}
	}
	if c := submitSecret(e, "web-ssh-key", "   "); c == nil || c.Toast != secretEmptyText {
		t.Fatalf("empty: got %+v", c)
	}
	if e.secrets.Exists("web-ssh-key") || len(notices.all()) != 0 {
		t.Fatal("a rejected value was saved or announced")
	}
	if c := submitSecret(e, "web-ssh-key", strings.Repeat("F", 1000)); c == nil || c.Toast != "" {
		t.Fatalf("1000 characters should fit: %+v", c)
	}
	if strings.Contains(logs.String(), "FAKEb3Bl") || strings.Contains(logs.String(), "FAKE-line") {
		t.Fatalf("a value reached the log: %s", logs.String())
	}
}

func TestSecretSubmitFailureLogsNoValue(t *testing.T) {
	logs := captureCoreLogs(t)
	e, _, _ := newSecretEngine(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.SetSecretStore(NewSecretStore(filepath.Join(blocker, ".secrets"))) // cannot be created
	c := submitSecret(e, "hubspot-token", fakeToken)
	if c == nil || c.Toast != secretSaveFail {
		t.Fatalf("got %+v", c)
	}
	// A broken values object is dropped and logged by name only.
	if c := e.handleCardNav(`act:/secret-submit hubspot-token {"secret":"`+fakeToken, secretDM); c == nil || c.Toast != secretEmptyText {
		t.Fatalf("broken values: %+v", c)
	}
	out := logs.String()
	if !strings.Contains(out, "secret: save failed") || strings.Contains(out, "FAKE0000") {
		t.Fatalf("log = %s", out)
	}
	if got := CardActionForLog(`act:/secret-submit hubspot-token {"secret":"` + fakeToken + `"}`); got != "act:/secret-submit hubspot-token fields=secret" {
		t.Fatalf("CardActionForLog = %q", got)
	}
}

func captureCoreLogs(t *testing.T) *lockedLog {
	t.Helper()
	buf := &lockedLog{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

// lockedLog lets goroutines started by the test log while it reads.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
