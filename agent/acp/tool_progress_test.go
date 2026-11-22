package acp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func toolCallEvent(t *testing.T, update string) core.Event {
	t.Helper()
	evs := mapSessionUpdate("s1", json.RawMessage(`{"sessionId":"s1","update":`+update+`}`))
	if len(evs) != 1 || evs[0].Type != core.EventToolUse {
		t.Fatalf("events = %+v, want one tool use", evs)
	}
	return evs[0]
}

func TestToolCallProgress_AntigravityShapes(t *testing.T) {
	tests := []struct {
		name       string
		update     string
		wantPublic string
		wantLabel  string // "" means no step_label
	}{
		{
			name:       "execute with toolSummary",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"1","title":"cat /srv/app/.env | grep KEY","kind":"execute","status":"pending","rawInput":{"CommandLine":"cat /srv/app/.env | grep KEY","WaitMsBeforeAsync":500,"toolSummary":"核对定时任务配置"}}`,
			wantPublic: "核对定时任务配置",
			wantLabel:  "核对定时任务配置",
		},
		{
			name:       "execute without summary keeps the generic wording",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"2","title":"git status","kind":"execute","rawInput":{"CommandLine":"git status"}}`,
			wantPublic: "运行命令",
		},
		{
			name:       "read shows only the base name",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"3","title":"/home/u/project/notes/status.md","kind":"read","rawInput":{"Path":"/home/u/project/notes/status.md"}}`,
			wantPublic: "读取文件 status.md",
			wantLabel:  "读取文件 status.md",
		},
		{
			name:       "read of a sensitive file shows no name",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"4","kind":"read","rawInput":{"Path":"/srv/app/.env","StartLine":1,"EndLine":20}}`,
			wantPublic: "读取文件",
		},
		{
			name:       "edit, delete and move map to modify",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"5","kind":"delete","rawInput":{"path":"/tmp/report.md"}}`,
			wantPublic: "修改文件 report.md",
			wantLabel:  "修改文件 report.md",
		},
		{
			name:       "search",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"6","title":"grep -r secret .","kind":"search","rawInput":{"Query":"secret"}}`,
			wantPublic: "搜索文件",
		},
		{
			name:       "fetch shows the host only",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"7","kind":"fetch","rawInput":{"Url":"https://user:pw@example.com/a?q=1"}}`,
			wantPublic: "读取网页 example.com",
			wantLabel:  "读取网页 example.com",
		},
		{
			name:       "think and other keep the operation wording",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"8","title":"plan","kind":"think"}`,
			wantPublic: "执行操作",
		},
		{
			name:       "Chinese prose title becomes the step label",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"9","title":"检查网站构建版本","kind":"execute","rawInput":{"CommandLine":"npm run build"}}`,
			wantPublic: "检查网站构建版本",
			wantLabel:  "检查网站构建版本",
		},
		{
			name:       "title that is the command is not a label",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"10","title":"echo 你好","kind":"execute","rawInput":{"CommandLine":"echo   你好"}}`,
			wantPublic: "运行命令",
		},
		{
			name:       "title with a path is not a label",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"11","title":"读取 /home/u/报告.md","kind":"read"}`,
			wantPublic: "读取文件",
		},
		{
			name:       "shell description is a label for execute",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"12","title":"ls","kind":"execute","rawInput":{"command":"ls","description":"List files"}}`,
			wantPublic: "运行命令",
			wantLabel:  "List files",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := toolCallEvent(t, tt.update)
			if got := ev.Metadata["public_tool_progress"]; got != tt.wantPublic {
				t.Errorf("public_tool_progress = %q, want %q", got, tt.wantPublic)
			}
			label, _ := ev.Metadata["step_label"].(string)
			if label != tt.wantLabel {
				t.Errorf("step_label = %q, want %q", label, tt.wantLabel)
			}
		})
	}
}

func TestToolCallProgress_LongSummaryClippedToOneLine(t *testing.T) {
	long := "第一行\n" + strings.Repeat("很长的说明", 20)
	raw, _ := json.Marshal(map[string]any{"toolSummary": long})
	ev := toolCallEvent(t, `{"sessionUpdate":"tool_call","toolCallId":"1","kind":"execute","rawInput":`+string(raw)+`}`)
	label := ev.Metadata["step_label"].(string)
	if r := []rune(label); len(r) != stepLabelMaxRunes+1 || r[len(r)-1] != '…' || r[3] != ' ' {
		t.Fatalf("step_label = %q, want one clipped line", label)
	}
}

func TestSummarizeACPToolInput_AntigravityKeys(t *testing.T) {
	if got := summarizeACPToolInput("execute", json.RawMessage(`{"CommandLine":"git status","WaitMsBeforeAsync":500,"toolSummary":"查看状态"}`)); got != "# 查看状态\ngit status" {
		t.Errorf("execute = %q", got)
	}
	if got := summarizeACPToolInput("read", json.RawMessage(`{"Path":"/tmp/a.md","StartLine":1}`)); got != "/tmp/a.md" {
		t.Errorf("read = %q", got)
	}
	// tool_call_update carries no kind; the command is still found.
	if got := summarizeACPToolInput("", json.RawMessage(`{"CommandLine":"ls"}`)); got != "ls" {
		t.Errorf("update = %q", got)
	}
}
