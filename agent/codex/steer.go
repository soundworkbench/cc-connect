package codex

import (
	"errors"
	"fmt"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const appServerSteerTimeout = 5 * time.Second

// Steer appends input to an existing turn and checks the server acknowledgement.
// Transport failures are deliberately not converted into queueable rejections.
func (s *appServerSession) Steer(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (bool, error) {
	s.stateMu.Lock()
	turnID := s.currentTurn
	s.stateMu.Unlock()
	if turnID == "" || !s.alive.Load() {
		return false, nil
	}
	if len(files) > 0 {
		prompt = core.AppendFileRefs(prompt, core.SaveFilesToDisk(s.GetWorkDir(), messageID, files))
	}
	prompt, paths, err := s.stageImages(prompt, images)
	if err != nil {
		return false, err
	}
	input := []map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}}
	for _, path := range paths {
		input = append(input, map[string]any{"type": "localImage", "path": path})
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	err = s.requestWithTimeout("turn/steer", map[string]any{
		"threadId": s.CurrentSessionID(), "expectedTurnId": turnID, "input": input,
	}, &response, appServerSteerTimeout)
	if err != nil {
		var rejected *rpcError
		if errors.As(err, &rejected) {
			// A server rejection (including completion racing this call) means
			// this input was not accepted; the engine can safely queue it.
			return false, nil
		}
		return false, fmt.Errorf("codex steering delivery unconfirmed: %w", err)
	}
	if response.TurnID != turnID {
		return false, fmt.Errorf("codex steering acknowledgement has unexpected turn id")
	}
	return true, nil
}
