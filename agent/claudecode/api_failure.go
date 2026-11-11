package claudecode

import (
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// emit delivers one event unless the session is closing.
func (cs *claudeSession) emit(evt core.Event) {
	select {
	case cs.events <- evt:
	case <-cs.ctx.Done():
	}
}

// parseAPIRetry reads a system/api_retry frame: the CLI is about to retry a
// failed model request. Fields absent from older CLIs stay zero.
func parseAPIRetry(raw map[string]any) *core.RetryStatus {
	kind, _ := raw["error"].(string)
	return &core.RetryStatus{
		Attempt:    asInt(raw["attempt"]),
		MaxRetries: asInt(raw["max_retries"]),
		HTTPStatus: asInt(raw["error_status"]),
		Kind:       kind,
	}
}

// modelFailure recognizes the result of a turn whose model request failed for
// good: an error result that carries the API error status or code, or that
// follows an assistant message flagged with an API error. Other error results
// (max turns, budget, interrupted execution) are not model failures.
func modelFailure(raw map[string]any, apiErrorKind, content string) *core.ModelFailure {
	if failed, _ := raw["is_error"].(bool); !failed {
		return nil
	}
	if resultSubtype(raw) != "success" {
		return nil
	}
	status := asInt(raw["api_error_status"])
	code, _ := raw["api_error_code"].(string)
	if status == 0 && code == "" && apiErrorKind == "" {
		return nil
	}
	kind := apiErrorKind
	if kind == "" {
		kind = code
	}
	return &core.ModelFailure{HTTPStatus: status, Kind: kind, Detail: strings.TrimSpace(content)}
}
