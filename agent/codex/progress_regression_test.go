package codex

import (
	"context"
	"github.com/chenhg5/cc-connect/core"
	"testing"
)

func TestCodexInterimProgressFinalSequence(t *testing.T) {
	for _, messageType := range []string{"message", "agent_message"} {
		t.Run(messageType, func(t *testing.T) {
			s := &codexSession{ctx: context.Background(), events: make(chan core.Event, 16)}
			s.handleItemCompleted(map[string]any{"item": map[string]any{"type": messageType, "content": []any{map[string]any{"type": "output_text", "text": "我先审回执。"}}}})
			s.handleItemCompleted(map[string]any{"item": map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "PRIVATE_THINKING"}}}})
			s.handleItemStarted(map[string]any{"item": map[string]any{"type": "command_execution", "id": "cmd-1", "command": "git log --oneline /private"}})
			s.handleItemCompleted(map[string]any{"item": map[string]any{"type": messageType, "text": "最终回复"}})
			s.flushPendingAsText()
			want := []core.EventType{core.EventThinking, core.EventProgress, core.EventToolUse, core.EventText}
			for i, typ := range want {
				e := <-s.events
				if e.Type != typ {
					t.Fatalf("event %d = %+v, want %v", i, e, typ)
				}
				if typ == core.EventProgress && e.Content != "我先审回执。" {
					t.Fatal(e)
				}
				if typ == core.EventText && e.Content != "最终回复" {
					t.Fatal(e)
				}
			}
			if len(s.events) != 0 || len(s.pendingMsgs) != 0 {
				t.Fatal("repeated final or pending progress")
			}
		})
	}
}

func TestCodexCommandOnlySafeProgress(t *testing.T) {
	for _, command := range []string{"git log --oneline /private", "TOKEN=fixture git log"} {
		t.Run(command, func(t *testing.T) {
			want := "运行命令 · git log"
			if command[0] == 'T' {
				want = "运行命令"
			}
			s := &codexSession{ctx: context.Background(), events: make(chan core.Event, 4)}
			s.handleItemStarted(map[string]any{"item": map[string]any{"type": "command_execution", "command": command}})
			e := <-s.events
			if e.Type != core.EventToolUse || e.Metadata["public_tool_progress"] != want {
				t.Fatal(e)
			}
			a := &appServerSession{events: make(chan core.Event, 4)}
			a.handleItemStarted(map[string]any{"type": "commandExecution", "command": command})
			e = <-a.events
			if e.Type != core.EventToolUse || e.Metadata["public_tool_progress"] != want {
				t.Fatal(e)
			}
		})
	}
}

func TestAppServerInterimProgressFinalSequence(t *testing.T) {
	s := &appServerSession{currentTurn: "t", events: make(chan core.Event, 8)}
	s.handleItemCompleted(map[string]any{"type": "agentMessage", "text": "我先审回执。"})
	s.handleItemStarted(map[string]any{"type": "commandExecution", "command": "make test"})
	s.handleItemCompleted(map[string]any{"type": "agentMessage", "text": "最终回复"})
	s.finishTurnID("t", nil)
	for _, typ := range []core.EventType{core.EventProgress, core.EventToolUse, core.EventText, core.EventResult} {
		e := <-s.events
		if e.Type != typ {
			t.Fatal(e)
		}
		if typ == core.EventText && e.Content != "最终回复" {
			t.Fatal(e)
		}
	}
	if len(s.events) != 0 {
		t.Fatal("duplicate final")
	}
}

func TestCodexSafeCommandSummary(t *testing.T) {
	cases := map[string]string{
		"sudo -S echo private-argument":                                       "echo",
		"/usr/bin/python3.11 /private/script.py":                              "python3.11",
		"sudo -s echo private-argument":                                       "echo",
		"env -S 'echo private' private-argument":                              "",
		"sudo --unknown=value private-argument":                               "",
		"timeout -s TERM -k 2s 10s git log":                                   "git log",
		"env --chdir=/private -u PRIVATE git log":                             "git log",
		"VAR='private value' env /usr/bin/git log":                            "git log",
		"git log --format='%s' /private":                                      "git log",
		"make test --user private":                                           "make test",
		"my-tool --arg private":                                               "my-tool",
		"VAR=value sudo -n timeout 10s env LANG=C /usr/bin/git log --oneline": "git log",
		"sudo -u private timeout --signal=TERM 1.5m env -i VAR=value git log": "git log",
		"git -C /private log --oneline":                                       "git log",
		"git -C '/private dir' log":                                           "git log",
		"printf '%s' private":                                                 "printf",
		"echo private":                                                        "echo",
		"git --author private log":                                            "git",
		"git '/private' log":                                                  "git",
		"git log; echo private":                                               "git log",
		"git log | cat":                                                       "git log",
		"bash -lc 'git log'":                                                  "bash",
		"'private' git log":                                                   "",
		"git log --token fixture":                                             "",
		"SECRET=value git log":                                                "",
		"git log --api-key fixture":                                           "",
		"echo Bearer fixture":                                                 "",
		"git log --password fixture":                                          "",
		"$(private) git log":                                                  "",
		"":                                                                    "",
	}
	for command, suffix := range cases {
		t.Run(command, func(t *testing.T) {
			want := "运行命令"
			if suffix != "" {
				want += " · " + suffix
			}
			if got := codexCommandProgress(command); got != want {
				t.Fatalf("got %q want %q", got, want)
			}
		})
	}
}
