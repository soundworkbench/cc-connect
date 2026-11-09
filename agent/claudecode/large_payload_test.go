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

func TestSteerAsync_LargeImageInjectsPathFrame(t *testing.T) {
	cs, ch := steerFixture(t)
	cs.workDir = t.TempDir()
	if err := cs.Send("first", "m1", nil, nil); err != nil {
		t.Fatal(err)
	}
	first := takeInput(t, ch)
	big := bytes.Repeat([]byte{0xff}, 200<<10)
	receipt, err := cs.SteerAsync("看图", "m2", []core.ImageAttachment{{MimeType: "image/jpeg", Data: big}}, nil)
	if receipt == nil || err != nil {
		t.Fatalf("large image must be injected as a path frame: receipt=%v err=%v", receipt, err)
	}
	frame := takeInput(t, ch)
	text, ok := frame["message"].(map[string]any)["content"].(string)
	if !ok || !strings.Contains(text, "看图") || !strings.Contains(text, ".cc-connect") || len(text) > 4096 {
		t.Fatalf("frame=%v", frame)
	}
	if !cs.Alive() {
		t.Fatal("stream must stay usable")
	}
	finish(cs, first["uuid"])
}

func TestSend_AfterPartialWriteCannotReuseStream(t *testing.T) {
	closed := &atomic.Bool{}
	cs, _ := steerFixture(t)
	cs.stdin = partialPipe{closed: closed}
	if err := cs.Send("old", "m-old", nil, nil); err == nil {
		t.Fatal("expected partial write failure")
	}
	if cs.Alive() || !closed.Load() {
		t.Fatal("partial write must invalidate session and close stdin")
	}
	if err := cs.Send("new", "m-new", nil, nil); err == nil {
		t.Fatal("next message must not reuse a partial stdin stream")
	}
}
