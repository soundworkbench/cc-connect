package core

import (
	"testing"
	"time"
)

// 11-12 20:42: a done row read "✓ 统计文档目录 · 统计文档目录任务已完成 · 用时不到 1 分钟".
// An outcome that only restates the name and the state is left out; one that
// starts with the name keeps what follows it.
func TestOutcomeAfterName(t *testing.T) {
	for _, tc := range []struct{ text, name, want string }{
		{"统计文档目录任务已完成", "统计文档目录", ""},
		{"统计文档目录（任务）已完成。", "统计文档目录", ""},
		{"统计文档目录完成", "统计文档目录", ""},
		{"统计文档目录", "统计文档目录", ""},
		{"统计文档目录 done", "统计文档目录", ""},
		{"Run echo command", "Run echo command", ""},
		{"任务已完成", "统计文档目录", ""},
		{"已完成", "统计文档目录", ""},
		{"无", "统计文档目录", ""},
		{"统计文档目录：共 52 个目录、318 个文件", "统计文档目录", "共 52 个目录、318 个文件"},
		{"统计文档目录完成，共 52 个目录", "统计文档目录", "完成，共 52 个目录"},
		{"整理好了 8 场会的要点", "整理纪要", "整理好了 8 场会的要点"},
		{"完成度 80%", "统计", "完成度 80%"},
		{"", "统计文档目录", ""},
	} {
		if got := outcomeAfterName(tc.text, tc.name); got != tc.want {
			t.Errorf("outcomeAfterName(%q, %q)=%q, want %q", tc.text, tc.name, got, tc.want)
		}
	}
}

func TestDoneRowDropsAnOutcomeThatRepeatsTheName(t *testing.T) {
	env := newTaskEnv(t)
	for _, id := range []string{"a", "b", "c"} {
		env.task(TaskEvent{Kind: TaskStarted, TaskID: id, Description: map[string]string{"a": "统计文档目录", "b": "整理纪要", "c": "检查链接"}[id], Background: yes()})
	}
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "completed", Summary: "统计文档目录任务已完成"})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "b", Status: "completed", Summary: "整理纪要：8 场会的要点"})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "c", Status: "completed", Summary: "Background command \"检查链接\" completed (exit code 0)"})
	waitUntil(t, "settled", func() bool { return lastView(env).State == TaskCardCompleted })
	v := lastView(env)
	want := []string{"✓ 统计文档目录 · 用时不到 1 分钟", "✓ 整理纪要 · 8 场会的要点 · 用时不到 1 分钟", "✓ 检查链接 · 用时不到 1 分钟"}
	for k, r := range v.Rows {
		if got := markedRow(r); got != want[k] || r.Full != "" {
			t.Errorf("row %d=%q full=%q, want %q", k, got, r.Full, want[k])
		}
	}
}

func TestOneTaskCardDropsAnOutcomeThatRepeatsTheName(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "统计文档目录", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "completed", Summary: "统计文档目录（任务）已完成"})
	v := settledView(t, env)
	if v.Title != "后台任务：统计文档目录" || v.Label != "已完成" || v.Line != "用时不到 1 分钟" || v.Summary != "" || v.Full != "" {
		t.Fatalf("settled=%+v", v)
	}

	env = newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "统计文档目录", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "completed", Summary: "统计文档目录：共 52 个目录"})
	if v := settledView(t, env); v.Line != "共 52 个目录 · 用时不到 1 分钟" || v.Summary != "共 52 个目录" {
		t.Fatalf("settled=%+v", v)
	}
}

// 11-12 20:10–20:40: a subagent ran 29 minutes. Its first card settled when
// the agent reported it done; the agent then reported the same task again
// three times, and each time the bridge counted from that moment, so the
// last card read "用时不到 1 分钟". A task reported again keeps its first start.
func TestTaskReportedAgainKeepsItsFirstStart(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a1", Description: "统计文档目录", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a1", Status: "completed", Summary: "目录统计好了"})
	waitUntil(t, "first card settled", func() bool { h, c := env.heldTasks(); return h == 0 && c == 0 })
	// Pretend the first report was 28 minutes ago.
	m := env.e.taskCards
	m.mu.Lock()
	m.started[env.key+"\x00a1"] = time.Now().Add(-28 * time.Minute)
	m.mu.Unlock()

	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a1", Description: "统计文档目录", Background: yes()})
	waitUntil(t, "second card", func() bool { sent, _ := env.p.cards(); return len(sent) == 2 })
	if sent, _ := env.p.cards(); sent[1].view.Line != "28 分钟" {
		t.Fatalf("second card opens with %q", sent[1].view.Line)
	}
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a1", Status: "completed", Summary: "目录统计好了，共 52 个"})
	sent, _ := env.p.cards()
	waitUntil(t, "second card settled", func() bool {
		_, up := env.p.cards()
		last := up[len(up)-1]
		return last.handle == sent[1].handle && !last.view.State.Live()
	})
	if v := lastView(env); v.Line != "目录统计好了，共 52 个 · 用时 28 分钟" {
		t.Fatalf("settled=%+v", v)
	}
}

// The agent's own run time on the end report wins over a later first sight.
func TestEndReportRunTimeSetsTheStart(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a1", Description: "检查版式", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a1", Status: "completed", Summary: "版式没问题", Elapsed: 29*time.Minute + 10*time.Second})
	if v := settledView(t, env); v.Line != "版式没问题 · 用时 29 分钟" {
		t.Fatalf("settled=%+v", v)
	}
}

// A task first heard of mid-run, with no run time, has no real start: its
// card and the progress answer leave the time out rather than count from
// when the bridge heard of it.
func TestTaskWithoutAKnownStartShowsNoTime(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskUpdated, TaskID: "a1", Description: "检查版式", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	if sent, _ := env.p.cards(); sent[0].view.Line != "进行中" {
		t.Fatalf("open line=%q", sent[0].view.Line)
	}
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a1", Step: "读幻灯片"})
	waitUntil(t, "step", func() bool { return lastView(env).Line == "读幻灯片" })
	if title, rows := env.e.taskCards.progressRows(env.e.i18n, env.key); title != "后台任务：进行中" || len(rows) != 1 || rows[0] != "◐ 检查版式 · 读幻灯片" {
		t.Fatalf("progress rows=%q %q", title, rows)
	}
	for _, tc := range []struct {
		task triageTask
		want string
	}{
		{triageTask{title: "检查版式", step: "读幻灯片"}, "后台任务「检查版式」：读幻灯片。"},
		{triageTask{title: "检查版式"}, "后台任务「检查版式」：进行中。"},
		{triageTask{title: "检查版式", step: "读幻灯片", elapsed: "3 分钟"}, "后台任务「检查版式」：读幻灯片 · 3 分钟。"},
	} {
		if got := env.e.progressAnswer(triageView{tasks: []triageTask{tc.task}}, nil); got != tc.want+"\n"+env.e.i18n.T(MsgTriageSeeCards) {
			t.Errorf("progress answer=%q, want %q", got, tc.want)
		}
	}
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a1", Status: "completed", Summary: "版式没问题"})
	if v := settledView(t, env); v.Line != "版式没问题" {
		t.Fatalf("settled=%+v", v)
	}
	// A run time on a later report gives the start.
	env2 := newTaskEnv(t)
	env2.task(TaskEvent{Kind: TaskUpdated, TaskID: "a1", Description: "检查版式", Background: yes()})
	env2.task(TaskEvent{Kind: TaskProgress, TaskID: "a1", Step: "读幻灯片", Elapsed: 4 * time.Minute})
	waitUntil(t, "card", func() bool { sent, _ := env2.p.cards(); return len(sent) == 1 })
	waitUntil(t, "time", func() bool { return lastView(env2).Line == "读幻灯片 · 4 分钟" })
}
