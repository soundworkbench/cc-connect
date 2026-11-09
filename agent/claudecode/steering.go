package claudecode

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Input IDs are CLI UUIDs, not platform IDs. A replay is the acknowledgement;
// stdin success alone does not prove the CLI consumed a message.
type claudeInput struct {
	ack      chan struct{}
	replayed bool
	steered  bool
}

var _ core.AgentSessionSteerer = (*claudeSession)(nil)

const claudeInputTimeout = 3 * time.Second
const claudeFirstWriteTimeout = 60 * time.Second

// claudeInlineImageLimit caps base64 image bytes sent inline in one user
// message. JSON framing and text may still make the line larger than a pipe buffer.
const claudeInlineImageLimit = 48 << 10

func newInputID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("claude input UUID entropy unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (cs *claudeSession) Send(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	msg, err := cs.buildUserInput(prompt, messageID, images, files)
	if err != nil {
		return err
	}
	id := newInputID()
	msg["uuid"] = id
	cs.inputMu.Lock()
	defer cs.inputMu.Unlock()
	if cs.inputs == nil {
		cs.inputs = make(map[string]*claudeInput)
	}
	cs.inputs[id] = &claudeInput{ack: make(chan struct{})}
	cs.inputActive = true
	if err = cs.writeBoundedUser(msg); err != nil {
		delete(cs.inputs, id)
		cs.inputActive = len(cs.inputs) > 0
	}
	return err
}

// Steer admits text and inline images; files and large images are saved to
// disk and injected as a text frame with their paths. Stop/permission
// handling remains owned by Engine. No interrupt or auto-resend is performed.
func (cs *claudeSession) Steer(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (bool, error) {
	receipt, err := cs.SteerAsync(prompt, messageID, images, files)
	if err != nil || receipt == nil {
		return false, err
	}
	select {
	case <-receipt.Ack:
		return true, nil
	case <-receipt.End:
		return false, errors.New("claude: process ended before acknowledgement")
	case <-cs.ctx.Done():
		return false, cs.ctx.Err()
	}
}

// Only the write has a deadline. A tool can run much longer than three seconds.
// Claude queues complete stdin frames itself, including across native results.
func (cs *claudeSession) SteerAsync(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (*core.SteerReceipt, error) {
	// 2026-10-09: inline base64 screenshots overflowed the pipe within the 3s
	// steer deadline and left a partial frame. Large images take the queued
	// path, where buildUserInput keeps them on disk and passes the path.
	encoded := 0
	for _, img := range images {
		encoded += base64.StdEncoding.EncodedLen(len(img.Data))
	}
	onDisk := len(files) > 0 || encoded > claudeInlineImageLimit
	// Send can hold inputMu for the entire first-write timeout. Engine calls
	// steering while holding lifecycle locks, so busy input must take the queue.
	if !cs.inputMu.TryLock() {
		return nil, nil
	}
	if !cs.inputActive || !cs.alive.Load() {
		cs.inputMu.Unlock()
		return nil, nil
	}
	// Deadline-capable pipes are required, including bounded lock acquisition.
	w, ok := cs.stdin.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		cs.inputMu.Unlock()
		return nil, nil
	}
	// Files and large images go to disk; only their paths travel over stdin.
	if onDisk {
		prompt = cs.attachmentTextFrame(prompt, messageID, images, files)
		images = nil
	}
	deadline := time.Now().Add(claudeInputTimeout)
	for !cs.stdinMu.TryLock() {
		if time.Now().After(deadline) {
			cs.inputMu.Unlock()
			return nil, nil
		}
		select {
		case <-cs.ctx.Done():
			cs.inputMu.Unlock()
			return nil, nil
		case <-time.After(time.Millisecond):
		}
	}
	if err := w.SetWriteDeadline(deadline); err != nil {
		cs.stdinMu.Unlock()
		cs.inputMu.Unlock()
		return nil, core.ErrSteerNotWritten
	}
	id := newInputID()
	input := &claudeInput{ack: make(chan struct{}), steered: true}
	cs.inputs[id] = input
	var content any = prompt
	if len(images) > 0 {
		parts := make([]map[string]any, 0, len(images)+1)
		for _, img := range images {
			mime := img.MimeType
			if mime == "" {
				mime = "image/png"
			}
			parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": base64.StdEncoding.EncodeToString(img.Data)}})
		}
		if prompt != "" {
			parts = append(parts, map[string]any{"type": "text", "text": prompt})
		}
		content = parts
	}
	data, _ := json.Marshal(map[string]any{"type": "user", "uuid": id, "message": map[string]any{"role": "user", "content": content}})
	data = append(data, '\n')
	n, err := cs.stdin.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	_ = w.SetWriteDeadline(time.Time{})
	if err != nil {
		delete(cs.inputs, id)
		cs.inputActive = len(cs.inputs) > 0
		failure := cs.invalidateInput(n, len(data), err)
		cs.stdinMu.Unlock()
		cs.inputMu.Unlock()
		return nil, errors.Join(core.ErrSteerWriteUncertain, failure)
	}
	cs.stdinMu.Unlock()
	cs.inputMu.Unlock()
	slog.Info("steered message into running turn", "session", cs.CurrentSessionID(), "size", len(data))
	return &core.SteerReceipt{Ack: input.ack, End: cs.done}, nil
}

func (cs *claudeSession) acknowledgeInput(raw map[string]any) {
	if raw["isReplay"] != true || raw["parent_tool_use_id"] != nil {
		return
	}
	id, _ := raw["uuid"].(string)
	cs.inputMu.Lock()
	defer cs.inputMu.Unlock()
	if p := cs.inputs[id]; p != nil && !p.replayed {
		p.replayed = true
		close(p.ack)
	}
}

// A boundary-race input can be consumed by the next CLI result. Keep the same
// Engine lifecycle open until every admitted UUID is accounted for; never ask
// Engine to send that message again or wait for a result already coalesced by CLI.
func (cs *claudeSession) completeInputs(raw map[string]any) bool {
	if raw["parent_tool_use_id"] != nil {
		return true
	}
	cs.inputMu.Lock()
	defer cs.inputMu.Unlock()
	ids := []string{}
	if list, ok := raw["user_message_uuids"].([]any); ok {
		for _, v := range list {
			if id, ok := v.(string); ok {
				ids = append(ids, id)
			}
		}
	}
	if id, ok := raw["user_message_uuid"].(string); ok && id != "" {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if p := cs.inputs[id]; p != nil {
			// The terminal association is stronger evidence than a missing replay.
			if !p.replayed {
				p.replayed = true
				close(p.ack)
			}
			delete(cs.inputs, id)
		}
	}
	if len(ids) == 0 {
		// Older CLI results can finish the one ordinary Send, but cannot prove a
		// steered input was consumed. Leave uncertain steers pending, never resend.
		for id, p := range cs.inputs {
			if !p.steered {
				delete(cs.inputs, id)
			}
		}
	}
	cs.inputActive = len(cs.inputs) > 0
	return cs.inputActive
}

// resultPrecedesInputs reports whether a result belongs to a turn the CLI
// started before it read our input: it names none of the pending inputs, and
// no pending input has been replayed yet. The CLI replays a user message when
// it starts that turn, so a result seen first closes someone else's turn (for
// example one triggered by a background task notification).
func (cs *claudeSession) resultPrecedesInputs(raw map[string]any) bool {
	cs.inputMu.Lock()
	defer cs.inputMu.Unlock()
	if len(cs.inputs) == 0 {
		return false
	}
	for _, p := range cs.inputs {
		if p.replayed {
			return false
		}
	}
	if id, _ := raw["user_message_uuid"].(string); id != "" && cs.inputs[id] != nil {
		return false
	}
	if list, ok := raw["user_message_uuids"].([]any); ok {
		for _, v := range list {
			if id, _ := v.(string); id != "" && cs.inputs[id] != nil {
				return false
			}
		}
	}
	return true
}

// Ordinary Send is bounded too, so it cannot hold inputMu indefinitely and
// prevent steering or completion from acquiring it. Test buffers use writeJSON.
func (cs *claudeSession) writeBoundedUser(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = cs.writeBoundedBytes(append(b, '\n'))
	return err
}

func (cs *claudeSession) writeBoundedBytes(b []byte) (int, error) {
	return cs.writeInputBytes(b, true)
}

func (cs *claudeSession) writeInputBytes(b []byte, userInput bool) (int, error) {
	cs.stdinMu.Lock()
	defer cs.stdinMu.Unlock()
	if !cs.alive.Load() {
		return 0, errors.New("claude: session process is not running")
	}
	timeout := claudeInputTimeout
	if userInput && !cs.firstWriteDone {
		timeout = cs.firstWriteTimeout
		if timeout <= 0 {
			timeout = claudeFirstWriteTimeout
		}
	}
	if w, ok := cs.stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := w.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return 0, cs.invalidateInput(0, len(b), err)
		}
		defer func() {
			if err := w.SetWriteDeadline(time.Time{}); err != nil && cs.inputFailure.Load() == nil {
				slog.Warn("claudeSession: reset stdin deadline", "session", cs.CurrentSessionID(), "error", err)
			}
		}()
	}
	n, err := cs.stdin.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return n, cs.invalidateInput(n, len(b), err)
	}
	if userInput {
		cs.firstWriteDone = true
	}
	return n, nil
}
