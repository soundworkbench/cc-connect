package fleet_acceptance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

// Opt-in regression through the real Engine + Claude adapter + installed CLI.
// Uses a newly fabricated diagnostic transcript, never a business session.
// Tools, hooks and MCP are disabled; no historical worker can be resumed.
// Authentication and proxy come from the caller's environment, never the repo.
func TestFleetLiveClaudeOrphanResume(t *testing.T) {
	if os.Getenv("CLAUDE_RESUME_LIVE") != "1" {
		t.Skip("explicit isolated Claude resume acceptance only")
	}
	model := os.Getenv("CLAUDE_RESUME_MODEL")
	if model == "" {
		t.Fatal("CLAUDE_RESUME_MODEL must be explicit")
	}
	work := t.TempDir()
	sid, transcript := orphanResumeFixture(t, work, model)
	a, err := core.CreateAgent("claudecode", map[string]any{
		"work_dir": work, "cc_data_dir": t.TempDir(), "model": model, "reasoning_effort": "high", "mode": "default",
		"cmd":           []string{"claude", "-p", "--tools=", "--setting-sources=", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--settings", `{"disableAllHooks":true}`},
		"system_prompt": "Isolated bridge regression. Reply only with the requested marker. Never resume or launch a task. No tools may run.",
	})
	if err != nil {
		t.Fatalf("agent: %s", safeError(err))
	}
	p := &capture{final: make(chan string, 32)}
	e := core.NewEngine("resume-live", a, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	e.SetStreamPreviewCfg(core.DefaultStreamPreviewCfg())
	e.SetDisplayConfig(core.DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	e.SetReplyFooterEnabled(false)
	defer e.Stop()
	const key = "capture:resume-health"
	session := e.GetSessions().GetOrCreateActive(key)
	session.SetAgentSessionID(sid, a.Name())
	e.GetSessions().Save()
	// Both the formerly broken first turn and a following turn must have exactly
	// one final answer, with no empty/unsolicited answer mixed into history.
	for i, marker := range []string{"BRIDGE_RESUME_OK", "BRIDGE_NEXT_OK"} {
		e.ReceiveMessage(p, &core.Message{SessionKey: key, Platform: p.Name(), UserID: "health", MessageID: fmt.Sprint(i), Content: "Reply exactly " + marker + ". Do not resume any task.", ReplyCtx: "capture"})
		select {
		case text := <-p.final:
			if strings.TrimSpace(text) != marker {
				t.Fatalf("turn %d wrong final: %s", i, safe(text))
			}
		case <-time.After(90 * time.Second):
			t.Fatal("real resumed turn timed out")
		}
		deadline := time.Now().Add(5 * time.Second)
		for session.Busy() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if session.Busy() {
			t.Fatal("turn remained busy after final reply")
		}
	}
	if session.GetAgentSessionID() != sid {
		t.Fatal("resume silently fell back to a new session")
	}
	history := session.GetHistory(0)
	var answers []string
	for _, h := range history {
		if h.Role == "assistant" {
			answers = append(answers, h.Content)
		}
	}
	if len(answers) != 2 || answers[0] != "BRIDGE_RESUME_OK" || answers[1] != "BRIDGE_NEXT_OK" {
		t.Fatal("history contains missing/duplicate/unsolicited answers")
	}
	select {
	case <-p.final:
		t.Fatal("extra reply after completed turns")
	case <-time.After(200 * time.Millisecond):
	}
	// Ensure this was the orphan-notification path, not merely a new-session
	// smoke test which can pass while resume remains broken.
	f, err := os.Open(transcript)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	notification := false
	highReply := false
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 4<<20)
	for scan.Scan() {
		var r map[string]any
		if json.Unmarshal(scan.Bytes(), &r) != nil {
			continue
		}
		origin, _ := r["origin"].(map[string]any)
		if origin["kind"] == "task-notification" {
			notification = true
		}
		message, _ := r["message"].(map[string]any)
		if r["type"] == "assistant" && r["effort"] == "high" && message["model"] == model {
			highReply = true
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if !notification || !highReply {
		t.Fatalf("resume path unverified: notification=%v high=%v", notification, highReply)
	}
	p.mu.Lock()
	starts, recalls := p.starts, p.recalls
	p.mu.Unlock()
	if starts != 2 || recalls != 2 {
		t.Fatalf("preview lifecycle starts=%d recalls=%d", starts, recalls)
	}
	t.Log("CLAUDE_RESUME_ACCEPTANCE orphan_notification=true same_session=true first_final=BRIDGE_RESUME_OK next_final=BRIDGE_NEXT_OK history_clean=true effort=high previews=2/2")
}

func orphanResumeFixture(t *testing.T, work, model string) (string, string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(work, "-"))
	// Require a fresh directory; cleanup may never touch an existing transcript.
	if err := os.MkdirAll(filepath.Dir(project), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(project); err != nil {
			t.Error(err)
		}
	})
	sid, aid := uuid.NewString(), "a"+strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	tid := "toolu_isolated_fixture"
	transcript := filepath.Join(project, sid+".jsonl")
	output := filepath.Join(project, sid, "subagents", "agent-"+aid+".jsonl")
	rows := []map[string]any{}
	var parent any
	add := func(kind string, message map[string]any, extra map[string]any) string {
		id := uuid.NewString()
		r := map[string]any{"type": kind, "message": message, "uuid": id, "parentUuid": parent, "isSidechain": false, "userType": "external", "entrypoint": "sdk-cli", "cwd": work, "sessionId": sid, "version": "2.1.273", "gitBranch": "HEAD", "timestamp": time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)}
		for k, v := range extra {
			r[k] = v
		}
		rows = append(rows, r)
		parent = id
		return id
	}
	assistant := func(id, stop string, content []any) map[string]any {
		return map[string]any{"role": "assistant", "model": model, "id": id, "type": "message", "stop_reason": stop, "content": content, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}
	}
	add("user", map[string]any{"role": "user", "content": "Isolated diagnostic history fixture. No work should be resumed."}, nil)
	aa := add("assistant", assistant("msg_fixture1", "tool_use", []any{map[string]any{"type": "tool_use", "id": tid, "name": "Agent", "input": map[string]any{"description": "isolated fixture", "prompt": "No work.", "subagent_type": "general-purpose", "run_in_background": true}}}), nil)
	add("user", map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": tid, "content": "Diagnostic async fixture. No real task is running."}}}, map[string]any{"sourceToolAssistantUUID": aa, "toolUseResult": map[string]any{"isAsync": true, "status": "async_launched", "agentId": aid, "description": "isolated fixture", "resolvedModel": model, "prompt": "No work.", "outputFile": output, "canReadOutputFile": true}})
	add("assistant", assistant("msg_fixture2", "end_turn", []any{map[string]any{"type": "text", "text": "FIXTURE_READY"}}), nil)
	var data []byte
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, b...), '\n')
	}
	if err := os.WriteFile(transcript, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Old inert subagent transcript, deliberately no .meta.json, so the CLI
	// reports stopped instead of automatically restarting a real task.
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rows[0])
	if err := os.WriteFile(output, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(output, old, old); err != nil {
		t.Fatal(err)
	}
	return sid, transcript
}
