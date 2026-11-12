package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// lastView is the latest view written to the session's task cards.
func lastView(env *taskEnv) TaskCardView {
	sent, up := env.p.cards()
	if len(up) > 0 {
		return up[len(up)-1].view
	}
	if len(sent) > 0 {
		return sent[len(sent)-1].view
	}
	return TaskCardView{}
}

func rowTexts(v TaskCardView) string {
	var out []string
	for _, r := range v.Rows {
		out = append(out, markedRow(r))
	}
	return strings.Join(out, "\n")
}

func shownRows(v TaskCardView) []int {
	var out []int
	for k, r := range v.Rows {
		if r.Shown {
			out = append(out, k)
		}
	}
	return out
}

func (env *taskEnv) heldTasks() (tasks, cards int) {
	env.e.taskCards.mu.Lock()
	defer env.e.taskCards.mu.Unlock()
	return len(env.e.taskCards.tasks), len(env.e.taskCards.open)
}

// Five tasks started together appear together on one card, one row each;
// the card settles once the last of them ends, red because one failed.
func TestBatchOfTasksSharesOneCard(t *testing.T) {
	env := newTaskEnv(t)
	names := []string{"整理纪要", "统计目录", "搜索会议", "检查链接", "汇总周报"}
	for k, n := range names {
		env.task(TaskEvent{Kind: TaskStarted, TaskID: fmt.Sprint(k), Description: n, Background: yes(), Step: "读" + n})
	}
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	sent, _ := env.p.cards()
	v := sent[0].view
	want := "◐ 整理纪要 · 读整理纪要 · 不到 1 分钟\n◐ 统计目录 · 读统计目录 · 不到 1 分钟\n◐ 搜索会议 · 读搜索会议 · 不到 1 分钟\n◐ 检查链接 · 读检查链接 · 不到 1 分钟\n◐ 汇总周报 · 读汇总周报 · 不到 1 分钟"
	if v.State != TaskCardRunning || v.Title != "后台任务：进行中" || v.Label != "" || rowTexts(v) != want || len(shownRows(v)) != 5 || v.More != "" || v.Line != "" {
		t.Fatalf("batch card: %+v\nrows:\n%s", v, rowTexts(v))
	}
	if !strings.HasPrefix(v.Interrupted, "后台任务：已中断\n⊘ 整理纪要\n⊘ 统计目录") {
		t.Fatalf("interrupted text %q", v.Interrupted)
	}

	// Tasks end one by one: their rows settle, the title counts them.
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "0", Status: "completed", Summary: "纪要整理好了"})
	waitUntil(t, "first row done", func() bool { return lastView(env).Title == "后台任务：进行中 1/5" })
	if v := lastView(env); markedRow(v.Rows[0]) != "✓ 整理纪要 · 纪要整理好了 · 用时不到 1 分钟" || v.Rows[1].State != TaskCardRunning {
		t.Fatalf("after one: %s", rowTexts(v))
	}
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "1", Status: "failed", Summary: "Background command \"统计\" failed with exit code 2"})
	waitUntil(t, "second row failed", func() bool { return lastView(env).Title == "后台任务：进行中 2/5" })
	if r := lastView(env).Rows[1]; markedRow(r) != "✗ 统计目录 · 命令出错（退出码 2），原文在详情里 · 用时不到 1 分钟" || r.FullTitle != "原文" || !strings.Contains(r.Full, "exit code 2") {
		t.Fatalf("failed row: %+v", r)
	}
	if v := lastView(env); v.State != TaskCardRunning || !strings.Contains(v.Interrupted, "\n✓ 整理纪要 · 纪要整理好了 · 用时不到 1 分钟\n✗ 统计目录") {
		t.Fatalf("live card with ended rows: %+v", v)
	}
	for _, id := range []string{"2", "3", "4"} {
		env.task(TaskEvent{Kind: TaskEnded, TaskID: id, Status: "completed", Summary: "好了"})
	}
	waitUntil(t, "settled card", func() bool { return !lastView(env).State.Live() })
	_, up := env.p.cards()
	last := up[len(up)-1]
	if last.handle != sent[0].handle || last.view.State != TaskCardFailed || last.view.Label != "" || last.view.Title != "后台任务：1 项失败" {
		t.Fatalf("settled: %+v", last)
	}
	time.Sleep(100 * time.Millisecond)
	if sent, again := env.p.cards(); len(sent) != 1 || len(again) != len(up) {
		t.Fatalf("settled card changed again: sent=%d updates=%d→%d", len(sent), len(up), len(again))
	}
	if tasks, cards := env.heldTasks(); tasks != 0 || cards != 0 {
		t.Fatalf("settled card still held: tasks=%d cards=%d", tasks, cards)
	}
}

// A task started while a card is live joins it instead of opening another.
func TestTaskJoinsTheLiveCard(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "任务甲", Background: yes(), Step: "读文件"})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	sent, _ := env.p.cards()
	if v := sent[0].view; v.Title != "后台任务：任务甲" || v.Line != "读文件 · 不到 1 分钟" || len(v.Rows) != 0 {
		t.Fatalf("single card: %+v", v)
	}
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "b", Description: "任务乙", Background: yes()})
	waitUntil(t, "joined", func() bool { return len(lastView(env).Rows) == 2 })
	_, up := env.p.cards()
	if v := up[len(up)-1]; v.handle != sent[0].handle || v.view.Title != "后台任务：进行中" || rowTexts(v.view) != "◐ 任务甲 · 读文件 · 不到 1 分钟\n⏳ 任务乙 · 不到 1 分钟" {
		t.Fatalf("joined card: %+v", v)
	}
	if sent, _ := env.p.cards(); len(sent) != 1 {
		t.Fatalf("cards=%d, want one", len(sent))
	}
}

// Once every task of a card has ended the card is final; the next task opens
// a new card.
func TestSettledCardThenNewTaskOpensANewCard(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "任务甲", Background: yes()})
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "b", Description: "任务乙", Background: yes()})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "a", Status: "completed", Summary: "甲好了"})
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "b", Status: "completed", Summary: "乙好了"})
	waitUntil(t, "settled", func() bool { return lastView(env).State == TaskCardCompleted })
	if v := lastView(env); v.Title != "后台任务：已完成" || rowTexts(v) != "✓ 任务甲 · 甲好了 · 用时不到 1 分钟\n✓ 任务乙 · 乙好了 · 用时不到 1 分钟" {
		t.Fatalf("settled: %+v", v)
	}
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "c", Description: "任务丙", Background: yes()})
	waitUntil(t, "new card", func() bool { sent, _ := env.p.cards(); return len(sent) == 2 })
	sent, _ := env.p.cards()
	if sent[1].handle == sent[0].handle || sent[1].view.Title != "后台任务：任务丙" || len(sent[1].view.Rows) != 0 {
		t.Fatalf("new card: %+v", sent[1])
	}
}

// Beyond eight tasks the body keeps eight rows, running ones first; the rest
// sit in the details with a note saying how many.
func TestMoreThanEightTasks(t *testing.T) {
	env := newTaskEnv(t)
	for k := range 10 {
		env.task(TaskEvent{Kind: TaskStarted, TaskID: fmt.Sprint(k), Description: fmt.Sprintf("任务%d", k), Background: yes()})
	}
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	v := lastView(env)
	if v.Title != "后台任务：进行中" || len(v.Rows) != 10 || fmt.Sprint(shownRows(v)) != "[0 1 2 3 4 5 6 7]" || v.More != "另有 2 项，见详情" {
		t.Fatalf("ten tasks: shown=%v more=%q %+v", shownRows(v), v.More, v)
	}
	for _, id := range []string{"0", "1", "2"} {
		env.task(TaskEvent{Kind: TaskEnded, TaskID: id, Status: "completed", Summary: "好了"})
	}
	waitUntil(t, "three done", func() bool { return lastView(env).Title == "后台任务：进行中 3/10" })
	if v := lastView(env); fmt.Sprint(shownRows(v)) != "[0 3 4 5 6 7 8 9]" || v.More != "另有 2 项，见详情" {
		t.Fatalf("after three: shown=%v more=%q", shownRows(v), v.More)
	}
}

// A closing agent session cuts off the running rows only; finished rows keep
// their outcome and the card turns grey.
func TestManyTaskCardInterrupted(t *testing.T) {
	env := newTaskEnv(t)
	for k, n := range []string{"任务甲", "任务乙", "任务丙"} {
		env.task(TaskEvent{Kind: TaskStarted, TaskID: fmt.Sprint(k), Description: n, Background: yes()})
	}
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	env.task(TaskEvent{Kind: TaskEnded, TaskID: "0", Status: "completed", Summary: "甲好了"})
	waitUntil(t, "one done", func() bool { return lastView(env).Title == "后台任务：进行中 1/3" })
	if got := lastView(env).Interrupted; got != "后台任务：已中断\n✓ 任务甲 · 甲好了 · 用时不到 1 分钟\n⊘ 任务乙\n⊘ 任务丙" {
		t.Fatalf("restart text %q", got)
	}
	env.e.cleanupInteractiveState(env.key)
	waitUntil(t, "interrupted", func() bool { return lastView(env).State == TaskCardInterrupted })
	if v := lastView(env); v.Title != "后台任务：已中断" || rowTexts(v) != "✓ 任务甲 · 甲好了 · 用时不到 1 分钟\n⊘ 任务乙 · 用时不到 1 分钟\n⊘ 任务丙 · 用时不到 1 分钟" {
		t.Fatalf("interrupted card: %+v", v)
	}
	if tasks, cards := env.heldTasks(); tasks != 0 || cards != 0 {
		t.Fatalf("interrupted card still held: tasks=%d cards=%d", tasks, cards)
	}
}

// The settled header: green when all completed, red when any failed, else
// grey and named by the interruption or the stop.
func TestManyTasksSettledHeader(t *testing.T) {
	i := NewI18n(LangChinese)
	now := time.Now()
	for _, tc := range []struct {
		states []TaskCardState
		state  TaskCardState
		label  string
	}{
		{[]TaskCardState{TaskCardCompleted, TaskCardCompleted}, TaskCardCompleted, "已完成"},
		{[]TaskCardState{TaskCardCompleted, TaskCardFailed, TaskCardFailed}, TaskCardFailed, "2 项失败"},
		{[]TaskCardState{TaskCardFailed, TaskCardStopped}, TaskCardFailed, "1 项失败"},
		{[]TaskCardState{TaskCardCompleted, TaskCardInterrupted, TaskCardStopped}, TaskCardInterrupted, "已中断"},
		{[]TaskCardState{TaskCardCompleted, TaskCardStopped}, TaskCardStopped, "已停止"},
	} {
		var tasks []*cardTask
		for k, s := range tc.states {
			tasks = append(tasks, &cardTask{desc: fmt.Sprintf("任务%d", k), start: now, ended: true, endedAt: now, state: s})
		}
		v := manyTasksView(i, "s", tasks)
		if v.State != tc.state || v.Title != "后台任务："+tc.label || v.Label != "" {
			t.Errorf("%v: state=%s title=%q label=%q, want %s %q", tc.states, v.State, v.Title, v.Label, tc.state, tc.label)
		}
	}
}

// Two tasks on one card leave the restart record one by one.
func TestCardTasksLeaveTheResumeRecordOneByOne(t *testing.T) {
	env := newTriageEnv(t)
	dir := t.TempDir()
	env.e.dataDir = dir
	env.e.openInflight()
	env.e.taskCards.openDelay = 0
	env.firstTurn("已在后台整理。")
	env.backgroundTask("t1", "整理纪要", "读纪要")
	state := env.backgroundTask("t2", "统计目录", "列目录")
	path := inflightPath(dir, env.e.name)
	waitUntil(t, "both recorded", func() bool {
		rec := readInflight(t, path).Sessions[env.key]
		return rec != nil && len(rec.Tasks) == 2
	})
	env.e.handleTaskEvent(state, env.key, &TaskEvent{Kind: TaskEnded, TaskID: "t1", Status: "completed", Summary: "好了"})
	waitUntil(t, "ended task left", func() bool {
		rec := readInflight(t, path).Sessions[env.key]
		return rec != nil && len(rec.Tasks) == 1 && rec.Tasks["t2"] == "统计目录"
	})
	env.e.taskCards.mu.Lock()
	open := env.e.taskCards.open[env.key]
	env.e.taskCards.mu.Unlock()
	if open == nil || len(open.tasks) != 2 {
		t.Fatalf("card should stay live with both rows: %+v", open)
	}
}

// Several running tasks are answered as their card: its title, then one line per task.
func TestProgressAnswerListsEveryRunningTask(t *testing.T) {
	env := newTriageEnv(t)
	env.firstTurn("已在后台整理。")
	env.backgroundTask("t1", "整理纪要", "读纪要")
	env.backgroundTask("t2", "统计目录", "列目录")
	env.backgroundTask("t3", "搜索会议", "查日历")
	before := len(env.p.stubPlatformEngine.getSent())
	env.send("进度")
	if env.reachesAgent() {
		t.Fatal("progress question woke the agent")
	}
	got := env.p.stubPlatformEngine.getSent()[before]
	want := "后台任务：进行中\n◐ 整理纪要 · 读纪要 · 不到 1 分钟\n◐ 统计目录 · 列目录 · 不到 1 分钟\n◐ 搜索会议 · 查日历 · 不到 1 分钟\n进展看上面的任务卡，做完我会告诉你。"
	if !strings.Contains(got, want) {
		t.Fatalf("answer:\n%s\nwant:\n%s", got, want)
	}
}

// A task shows the waiting sign until anything comes from it; a command's
// launch, or a subagent's first tool call, is enough to show it running.
func TestWaitingRowUntilTheTaskReports(t *testing.T) {
	env := newTaskEnv(t)
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "a", Description: "子代理", Background: yes()})
	env.task(TaskEvent{Kind: TaskStarted, TaskID: "b", Description: "跑测试", Background: yes(), Step: "运行命令", StepGeneric: true})
	waitUntil(t, "card", func() bool { sent, _ := env.p.cards(); return len(sent) == 1 })
	v := lastView(env)
	if rowTexts(v) != "⏳ 子代理 · 不到 1 分钟\n◐ 跑测试 · 不到 1 分钟" || !v.Rows[0].Waiting || v.Rows[1].Waiting || v.Title != "后台任务：进行中" {
		t.Fatalf("rows:\n%s\n%+v", rowTexts(v), v.Rows)
	}
	env.task(TaskEvent{Kind: TaskProgress, TaskID: "a", Step: "读取文件", StepGeneric: true})
	waitUntil(t, "running", func() bool { return !lastView(env).Rows[0].Waiting })
	if got := rowTexts(lastView(env)); got != "◐ 子代理 · 不到 1 分钟\n◐ 跑测试 · 不到 1 分钟" {
		t.Fatalf("after first step:\n%s", got)
	}
}

// With a task ended on the card, the progress answer counts it as the card
// does and keeps its row.
func TestProgressAnswerCountsLikeTheCard(t *testing.T) {
	env := newTriageEnv(t)
	env.e.taskCards.openDelay = 0
	env.firstTurn("已在后台整理。")
	state := env.backgroundTask("t1", "整理纪要", "读纪要")
	env.backgroundTask("t2", "统计目录", "列目录")
	env.backgroundTask("t3", "搜索会议", "查日历")
	waitUntil(t, "on the card", func() bool {
		env.e.taskCards.mu.Lock()
		defer env.e.taskCards.mu.Unlock()
		c := env.e.taskCards.open[env.key]
		return c != nil && len(c.tasks) == 3
	})
	env.e.handleTaskEvent(state, env.key, &TaskEvent{Kind: TaskEnded, TaskID: "t1", Status: "completed", Summary: "纪要整理好了"})
	before := len(env.p.stubPlatformEngine.getSent())
	env.send("进度")
	if env.reachesAgent() {
		t.Fatal("progress question woke the agent")
	}
	got := env.p.stubPlatformEngine.getSent()[before]
	want := "后台任务：进行中 1/3\n✓ 整理纪要 · 纪要整理好了 · 用时不到 1 分钟\n◐ 统计目录 · 列目录 · 不到 1 分钟\n◐ 搜索会议 · 查日历 · 不到 1 分钟\n"
	if !strings.Contains(got, want) {
		t.Fatalf("answer:\n%s\nwant:\n%s", got, want)
	}
}

// A batch cut off by a restart is named in the resume prompt up to ten tasks;
// the rest are counted.
func TestResumeNamesTenTasksAndCountsTheRest(t *testing.T) {
	e := NewEngine("p", &stubAgent{}, nil, "", LangChinese)
	t.Cleanup(func() { _ = e.Stop() })
	rec := &inflightSession{Tasks: map[string]string{}}
	for k := range 12 {
		rec.Tasks[fmt.Sprint(k)] = fmt.Sprintf("任务%02d", k)
	}
	items := e.interruptedItems(rec)
	if len(items) != 11 || items[0] != "后台任务「任务00」" || items[9] != "后台任务「任务09」" || items[10] != "另有 2 个后台任务" {
		t.Fatalf("items=%q", items)
	}
}
