package claudecode

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Steer admits only text. Attachments retain the existing queued path so disk
// writes and large payloads never block busy-session admission. Stop/permission
// handling remains owned by Engine. No interrupt or auto-resend is performed.
func (cs *claudeSession) Steer(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (bool, error) {
	if len(images) > 0 || len(files) > 0 {
		return false, nil
	}
	cs.inputMu.Lock()
	if !cs.inputActive || !cs.alive.Load() {
		cs.inputMu.Unlock()
		return false, nil
	}
	// Deadline-capable pipes are required, including bounded lock acquisition.
	w, ok := cs.stdin.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		cs.inputMu.Unlock()
		return false, nil
	}
	deadline := time.Now().Add(claudeInputTimeout)
	for !cs.stdinMu.TryLock() {
		if time.Now().After(deadline) {
			cs.inputMu.Unlock()
			return false, nil
		}
		select {
		case <-cs.ctx.Done():
			cs.inputMu.Unlock()
			return false, nil
		case <-time.After(time.Millisecond):
		}
	}
	if err := w.SetWriteDeadline(deadline); err != nil {
		cs.stdinMu.Unlock()
		cs.inputMu.Unlock()
		return false, nil
	}
	id := newInputID()
	input := &claudeInput{ack: make(chan struct{}), steered: true}
	cs.inputs[id] = input
	data, _ := json.Marshal(map[string]any{"type": "user", "uuid": id, "message": map[string]any{"role": "user", "content": prompt}})
	data = append(data, '\n')
	n, err := cs.stdin.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	_ = w.SetWriteDeadline(time.Time{})
	cs.stdinMu.Unlock()
	if err != nil && n == 0 {
		delete(cs.inputs, id)
		cs.inputActive = len(cs.inputs) > 0
		cs.inputMu.Unlock()
		return false, nil
	}
	cs.inputMu.Unlock()
	if err != nil {
		return false, errors.New("claude: input write unconfirmed; not replaying")
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-input.ack:
		return true, nil
	case <-cs.ctx.Done():
		return false, errors.New("claude: session ended before input acknowledgement")
	case <-timer.C:
		return false, errors.New("claude: input acknowledgement timed out; not replaying")
	}
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
	w, ok := cs.stdin.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		return cs.writeJSON(v)
	}
	deadline := time.Now().Add(claudeInputTimeout)
	for !cs.stdinMu.TryLock() {
		if time.Now().After(deadline) {
			return errors.New("claude: stdin lock timeout")
		}
		select {
		case <-cs.ctx.Done():
			return cs.ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer cs.stdinMu.Unlock()
	if err := w.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer w.SetWriteDeadline(time.Time{})
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := cs.stdin.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}
