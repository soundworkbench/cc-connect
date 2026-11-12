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

// 11-12 20:10–20:40: a subagent ran 29 minutes. Its card settled when the
// agent reported it done; the agent then reported the same task again three
// times, and each time the bridge opened a new card counting from that moment,
// so the last one read "用时不到 1 分钟". A task reported again after it ended
// opens no card and does not count as running; a new result only replaces the
// outcome on its row of the original card.
func TestTaskReportedAgainAfterItEnded(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a1", Description: "统计文档目录", Background: yes()})
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "b1", Description: "整理纪要", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a1", Status: "completed", Summary: "目录统计好了"})
	waitUntil(t, "row ended", func() bool { return lastView(env).Title == "后台任务：进行中 1/2" })

	// Reported again while its card is live: the row stays as it ended.
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a1", Description: "统计文档目录", Background: yes()})
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a1", Step: "再列一遍目录"})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "b1", Status: "completed", Summary: "8 场会的要点"})
	waitUntil(t, "card settled", func() bool { h, c := env.heldTasks(); return h == 0 && c == 0 })
	if v := lastView(env); v.State != TaskCardCompleted || rowTexts(v) != "✓ 统计文档目录 · 目录统计好了 · 用时不到 1 分钟\n✓ 整理纪要 · 8 场会的要点 · 用时不到 1 分钟" {
		t.Fatalf("settled=%+v", v)
	}

	// Reported again after its card settled: no new card, not running.
	_, before := env.p.cards()
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a1", Description: "统计文档目录", Background: yes()})
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a1", Step: "再列一遍目录", Summary: "在核对子目录"})
	time.Sleep(300 * time.Millisecond)
	if sent, up := env.p.cards(); len(sent) != 1 || len(up) != len(before) {
		t.Fatalf("reported again: cards=%d updates=%d, want 1 and %d", len(sent), len(up), len(before))
	}
	if h, c := env.heldTasks(); h != 0 || c != 0 {
		t.Fatalf("held tasks=%d cards=%d", h, c)
	}
	if title, rows := env.e.taskCards.progressRows(env.e.i18n, env.key); title != "" || rows != nil {
		t.Fatalf("progress rows=%q %q", title, rows)
	}

	// The same end again changes nothing; a new result updates the row only.
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a1", Status: "completed", Summary: "目录统计好了"})
	time.Sleep(200 * time.Millisecond)
	if _, up := env.p.cards(); len(up) != len(before) {
		t.Fatalf("same result rewrote the card: %+v", up[len(before):])
	}
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a1", Status: "completed", Summary: "共 52 个目录、318 个文件"})
	waitUntil(t, "row updated", func() bool { _, up := env.p.cards(); return len(up) == len(before)+1 })
	sent, up := env.p.cards()
	if last := up[len(up)-1]; last.handle != sent[0].handle || last.view.State != TaskCardCompleted ||
		rowTexts(last.view) != "✓ 统计文档目录 · 共 52 个目录、318 个文件 · 用时不到 1 分钟\n✓ 整理纪要 · 8 场会的要点 · 用时不到 1 分钟" {
		t.Fatalf("updated=%+v", last)
	}
	if len(sent) != 1 {
		t.Fatalf("cards=%d, want 1", len(sent))
	}
}

// An ended task with no card (it ended before the open delay) stays without
// one when reported again.
func TestShortTaskReportedAgainGetsNoCard(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "q", Description: "很快的命令", Background: yes()})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "q", Status: "completed"})
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "q", Description: "很快的命令", Background: yes()})
	time.Sleep(300 * time.Millisecond)
	if sent, _ := env.p.cards(); len(sent) != 0 {
		t.Fatalf("cards=%+v", sent)
	}
	if h, _ := env.heldTasks(); h != 0 {
		t.Fatalf("held tasks=%d", h)
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
