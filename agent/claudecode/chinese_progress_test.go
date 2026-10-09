package claudecode

import (
	"github.com/chenhg5/cc-connect/core"
	"testing"
)

func TestChineseProgressFallback(t *testing.T) {
	tests := []struct {
		name, tool, title string
		input             map[string]any
		want              string
	}{
		{"Chinese unchanged", "Bash", "", map[string]any{"description": "  正在检查 git 状态。  ", "command": "do not show"}, "  正在检查 git 状态。  "},
		{"English command", "Bash", "", map[string]any{"description": "Check status", "command": "API_KEY=fixture-secret git status"}, "运行命令"},
		{"Chinese title", "Read", "读取配置", map[string]any{"description": "Read config"}, "读取配置"},
		{"Input title", "Read", "", map[string]any{"title": "检查文件"}, "检查文件"},
		{"Read basename", "Read", "", map[string]any{"file_path": "/private/user/project/main.go"}, "读取文件 main.go"},
		{"Windows basename", "Read", "", map[string]any{"file_path": `C:\private\project\main.go`}, "读取文件 main.go"},
		{"Edit", "Edit", "", map[string]any{"file_path": "/private/main.go", "old_string": "secret old", "new_string": "secret new"}, "修改文件 main.go"},
		{"Write", "Write", "", map[string]any{"file_path": "/private/main.go", "content": "secret content"}, "修改文件 main.go"},
		{"Notebook", "NotebookEdit", "", map[string]any{"notebook_path": "/private/demo.ipynb", "new_source": "secret"}, "修改文件 demo.ipynb"},
		{"Grep", "Grep", "", map[string]any{"pattern": "private pattern"}, "搜索文件"},
		{"Glob", "Glob", "", map[string]any{"pattern": "private pattern"}, "搜索文件"},
		{"URL host only", "WebFetch", "", map[string]any{"url": "https://user:password@example.com:8080/private?token=secret#secret"}, "读取网页 example.com"},
		{"Bad URL", "WebFetch", "", map[string]any{"url": "private text"}, "读取网页"},
		{"Search", "WebSearch", "", map[string]any{"query": "private query"}, "联网搜索"},
		{"Agent", "Agent", "", map[string]any{"description": "Do work", "prompt": "private prompt"}, "派子代理"},
		{"Task", "Task", "", nil, "派子代理"},
		{"Skill", "Skill", "", map[string]any{"skill": "review", "args": "private args"}, "加载技能 review"},
		{"Todo", "TodoWrite", "", nil, "更新待办"},
		{"MCP", "mcp__lark_cli__search__extra", "", map[string]any{"token": "secret"}, "调用工具 lark_cli"},
		{"MCP missing tool", "mcp__lark__", "", nil, "调用工具"},
		{"MCP missing service", "mcp____search", "", nil, "调用工具"},
		{"MCP missing separator", "mcp__lark", "", nil, "调用工具"},
		{"MCP suspicious", "mcp__sk-fixture__search", "", nil, "调用工具"},
		{"Unknown", "FutureTool", "", map[string]any{"description": "private description"}, "执行操作"},
		{"Empty", "Read", "", nil, "读取文件"},
		{"Sensitive basename", "Read", "", map[string]any{"file_path": "/private/.env.production"}, "读取文件"},
		{"Suspicious basename", "Read", "", map[string]any{"file_path": "/private/sk-fixture.txt"}, "读取文件"},
		{"Opaque basename", "Read", "", map[string]any{"file_path": "/private/012345678901234567890123456789.txt"}, "读取文件"},
		{"Skill parameters", "Skill", "", map[string]any{"skill": "review --token=secret"}, "加载技能"},
		{"Control chars", "Read", "", map[string]any{"file_path": "/private/name\nsecret"}, "读取文件"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chineseToolProgress(tt.tool, tt.input, tt.title); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
	if got := chineseToolProgress("Bash", "not a map", ""); got != "运行命令" {
		t.Fatal(got)
	}
}

// Exercise the real event boundary: pre-fix code emits the full command here.
func TestClaudeEnglishToolProgressRegression(t *testing.T) {
	cs := progressSession()
	assistantBlock(cs, map[string]any{"type": "text", "text": "正在检查。"})
	assistantBlock(cs, map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"description": "Check repository", "command": "git status --private-fixture"}})
	assistantBlock(cs, map[string]any{"type": "text", "text": "已完成。"})
	cs.handleResult(map[string]any{"result": "已完成。"})
	events := drainProgressEvents(cs)
	want := []core.EventType{core.EventProgress, core.EventToolUse, core.EventText, core.EventResult}
	if len(events) != len(want) {
		t.Fatalf("events=%v", events)
	}
	for i, typ := range want {
		if events[i].Type != typ {
			t.Fatalf("event %d: %v", i, events[i])
		}
	}
	if events[1].ToolName != "Bash" || events[1].ToolInput != "运行命令" {
		t.Fatalf("unsafe/English progress: %+v", events[1])
	}
	if events[0].Content != "正在检查。" || events[2].Content != "已完成。" || events[3].Content != "已完成。" {
		t.Fatal("public/final text changed")
	}
}
