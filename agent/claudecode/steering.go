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

// A short write cannot deliver a JSON line without its terminating newline.
// Preserve its unwritten tail so the normal queued Send can finish that same
// frame exactly once, rather than concatenate a second JSON object to it.
type claudePartialInput struct {
	id, messageID, prompt string
	tail                  []byte
}

var _ core.AgentSessionSteerer = (*claudeSession)(nil)

const claudeInputTimeout = 3 * time.Second

// claudeInlineImageLimit caps base64 image bytes sent inline in one user
// message, keeping the JSON line under the 64 KiB pipe buffer.
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
	if p := cs.partialInput; p != nil {
		if p.messageID != messageID || p.prompt != prompt {
			// The incomplete frame's owner is not resending it, so its tail will
			// never arrive. Refusing forever wedged the whole chat; instead close
			// stdin so this session ends and the next message starts fresh.
			slog.Error("claudeSession: incomplete input frame abandoned, closing session", "pending_message", p.messageID, "new_message", messageID)
			cs.partialInput = nil
			cs.stdinMu.Lock()
			_ = cs.stdin.Close()
			cs.stdinMu.Unlock()
			return errors.New("claude: previous input frame incomplete; session closed, please resend")
		}
		cs.inputs[p.id] = &claudeInput{ack: make(chan struct{})}
		cs.inputActive = true
		n, err := cs.writeBoundedBytes(p.tail)
		if err != nil {
			p.tail = p.tail[n:]
			delete(cs.inputs, p.id)
			cs.inputActive = len(cs.inputs) > 0
		} else {
			cs.partialInput = nil
		}
		return err
	}
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

// Steer admits text and inline images without attachment disk writes. Files
// retain the queued path. Stop/permission
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
	if len(files) > 0 {
		return nil, nil
	}
	// 2026-10-09: inline base64 screenshots overflowed the pipe within the 3s
	// steer deadline and left a partial frame. Large images take the queued
	// path, where buildUserInput keeps them on disk and passes the path.
	encoded := 0
	for _, img := range images {
		encoded += base64.StdEncoding.EncodedLen(len(img.Data))
	}
	if encoded > claudeInlineImageLimit {
		return nil, nil
	}
	cs.inputMu.Lock()
	if !cs.inputActive || !cs.alive.Load() || cs.partialInput != nil {
		cs.inputMu.Unlock()
		return nil, nil
	}
	// Deadline-capable pipes are required, including bounded lock acquisition.
	w, ok := cs.stdin.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		cs.inputMu.Unlock()
		return nil, nil
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
	cs.stdinMu.Unlock()
	if err != nil && n < len(data) {
		if n > 0 {
			cs.partialInput = &claudePartialInput{id: id, messageID: messageID, prompt: prompt, tail: append([]byte(nil), data[n:]...)}
		}
		delete(cs.inputs, id)
		cs.inputActive = len(cs.inputs) > 0
		cs.inputMu.Unlock()
		return nil, core.ErrSteerNotWritten
	}
	cs.inputMu.Unlock()
	if err != nil {
		return nil, core.ErrSteerWriteUncertain
	}
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
	w, ok := cs.stdin.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		cs.stdinMu.Lock()
		defer cs.stdinMu.Unlock()
		n, err := cs.stdin.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
		return n, err
	}
	deadline := time.Now().Add(claudeInputTimeout)
	for !cs.stdinMu.TryLock() {
		if time.Now().After(deadline) {
			return 0, errors.New("claude: stdin lock timeout")
		}
		select {
		case <-cs.ctx.Done():
			return 0, cs.ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer cs.stdinMu.Unlock()
	if err := w.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	defer w.SetWriteDeadline(time.Time{})
	n, err := cs.stdin.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil && n > 0 && n < len(b) {
		// A partial JSON line is now in Claude's stdin; every later message
		// would be appended to it and fail to parse, wedging the session.
		// Close stdin so the process exits and the next message starts fresh.
		slog.Error("claudeSession: partial stdin write, closing session", "written", n, "size", len(b), "error", err)
		_ = cs.stdin.Close()
		return n, fmt.Errorf("claude: partial input write (%d/%d bytes), session closed: %w", n, len(b), err)
	}
	return n, err
}
