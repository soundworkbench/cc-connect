package claudecode

import (
	"bytes"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// 2026-10-09: a Feishu screenshot (~400 KiB base64) overflowed the stdin pipe,
// the bounded write timed out mid-line and every later message failed to parse.
func TestBuildUserInput_LargeImageIsPathOnly(t *testing.T) {
	cs := &claudeSession{workDir: t.TempDir()}
	cs.alive.Store(true)
	big := bytes.Repeat([]byte{0xff}, 200<<10)
	msg, err := cs.buildUserInput("看图", "m1", []core.ImageAttachment{{MimeType: "image/jpeg", Data: big}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parts := msg["message"].(map[string]any)["content"].([]map[string]any)
	for _, p := range parts {
		if p["type"] == "image" {
			t.Fatal("large image must not be inlined as base64")
		}
	}
	text := parts[len(parts)-1]["text"].(string)
	if !strings.Contains(text, "please read them with the Read tool") || !strings.Contains(text, ".jpg") {
		t.Fatalf("missing saved image path in text: %q", text)
	}
}

func TestBuildUserInput_SmallImageStillInline(t *testing.T) {
	cs := &claudeSession{workDir: t.TempDir()}
	cs.alive.Store(true)
	msg, err := cs.buildUserInput("看图", "m1", []core.ImageAttachment{{MimeType: "image/png", Data: []byte("tiny")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parts := msg["message"].(map[string]any)["content"].([]map[string]any)
	if parts[0]["type"] != "image" {
		t.Fatal("small image should stay inline")
	}
}

type partialPipe struct{ closed *atomic.Bool }

func (p partialPipe) Write(b []byte) (int, error)      { return len(b) / 2, errors.New("i/o timeout") }
func (p partialPipe) Close() error                     { p.closed.Store(true); return nil }
func (p partialPipe) SetWriteDeadline(time.Time) error { return nil }

func TestWriteBoundedBytes_PartialWriteClosesSession(t *testing.T) {
	closed := &atomic.Bool{}
	cs, _ := steerFixture(t)
	cs.stdin = partialPipe{closed: closed}
	n, err := cs.writeBoundedBytes([]byte(strings.Repeat("x", 100)))
	if err == nil || n != 50 {
		t.Fatalf("want partial error, got n=%d err=%v", n, err)
	}
	if !closed.Load() {
		t.Fatal("stdin must be closed after a partial write so the next message starts fresh")
	}
}

func TestSteerAsync_LargeImageFallsBackToQueue(t *testing.T) {
	cs, ch := steerFixture(t)
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	first := takeInput(t, ch)
	big := bytes.Repeat([]byte{0xff}, 200<<10)
	receipt, err := cs.SteerAsync("看图", "m2", []core.ImageAttachment{{MimeType: "image/jpeg", Data: big}}, nil)
	if receipt != nil || err != nil {
		t.Fatalf("large image must not be steered inline: receipt=%v err=%v", receipt, err)
	}
	if cs.partialInput != nil {
		t.Fatal("no partial frame may be left behind")
	}
	finish(cs, first["uuid"])
}

func TestSend_AbandonedPartialFrameClosesSession(t *testing.T) {
	closed := &atomic.Bool{}
	cs, _ := steerFixture(t)
	cs.partialInput = &claudePartialInput{id: "x", messageID: "old", prompt: "old", tail: []byte("}\n")}
	cs.stdin = partialPipe{closed: closed}
	err := cs.Send("new", "m-new", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "session closed") {
		t.Fatalf("want session-closed error, got %v", err)
	}
	if !closed.Load() || cs.partialInput != nil {
		t.Fatal("abandoned partial frame must close stdin and clear state")
	}
}
