package feishu

import (
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// ownerNameWait bounds the name lookup inside a card callback, which must
// answer within three seconds. A slower lookup still fills the name cache.
var ownerNameWait = 1500 * time.Millisecond

// cardOwner is the person a card's buttons serve: the user part of a
// per-user session key. Shared and thread sessions have no single owner.
func (p *Platform) cardOwner(sessionKey string) string {
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) != 3 || parts[0] != p.tag() || isThreadSessionKey(sessionKey) {
		return ""
	}
	return parts[2]
}

// notYourButton is the toast for a click on someone else's card.
func (p *Platform) notYourButton(owner string) *callback.CardActionTriggerResponse {
	done := make(chan string, 1)
	go func() { done <- p.resolveUserName(owner) }()
	text := "这个按钮不是给你的"
	select {
	case name := <-done:
		if name != "" && name != owner {
			text = "这个按钮是给 " + name + " 的"
		}
	case <-time.After(ownerNameWait):
	}
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "info", Content: text}}
}
