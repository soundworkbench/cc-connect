package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readInflight(t *testing.T, path string) inflightFile {
	t.Helper()
	var f inflightFile
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestInflightRecordSurvivesOnlyAnUnfinishedRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inflight", "p.json")
	ctx, cancel := context.WithCancel(context.Background())
	s, left := openInflightStore(ctx, path)
	if len(left.Sessions) != 0 {
		t.Fatalf("fresh start left %+v", left)
	}
	s.turnStart("k1", "帮我按客户整理 200 个文件，然后出一张汇总表")
	s.turnStart("k2", "查一下日志")
	s.turnEnd("k2")
	s.taskOpen("k1", "t1", "按客户整理文件")
	s.taskOpen("k1", "t2", "导出汇总表")
	s.taskEnd("k1", "t2")
	cancel() // shutdown: what ends now stays recorded
	s.turnEnd("k1")
	s.taskEnd("k1", "t1")

	s2, left := openInflightStore(context.Background(), path)
	rec := left.Sessions["k1"]
	if len(left.Sessions) != 1 || rec == nil || rec.Turn == nil || !strings.HasPrefix(rec.Turn.Request, "帮我按客户整理") || rec.Turn.Resume ||
		len(rec.Tasks) != 1 || rec.Tasks["t1"] != "按客户整理文件" || time.Since(left.AliveAt) > time.Minute {
		t.Fatalf("left=%+v rec=%+v", left, rec)
	}
	// Each leftover is handled once: the new process starts an empty record.
	if f := readInflight(t, path); len(f.Sessions) != 0 {
		t.Fatalf("record not reset: %+v", f)
	}
	s2.turnStart("k1", resumeMarker+"上次在 10:00 因重启中断")
	if f := readInflight(t, path); f.Sessions["k1"] == nil || !f.Sessions["k1"].Turn.Resume {
		t.Fatalf("resume turn not marked: %+v", f)
	}
	var nilStore *inflightStore
	nilStore.turnStart("k", "x") // no data dir: no record, no panic
}

type resumeEnv struct {
	p   *scheduledTestPlatform
	s   *scheduledTestSession
	e   *Engine
	key string
}

func newResumeEnv(t *testing.T, dataDir string) *resumeEnv {
	t.Helper()
	env := &resumeEnv{key: "scheduled:chat:user"}
	env.p = &scheduledTestPlatform{&steerReactionPlatform{stubPlatformEngine: stubPlatformEngine{n: "scheduled"}, reactions: make(chan string, 16)}}
	env.s = &scheduledTestSession{steeringJourneySession: &steeringJourneySession{cujAgentSession: newCUJAgentSession(), started: make(chan string, 16), steered: make(chan string, 16)}, modes: make(chan string, 16), ids: make(chan string, 16)}
	env.e = NewEngine("scheduled", &controllableAgent{nextSession: env.s}, []Platform{env.p}, filepath.Join(dataDir, "sessions.json"), LangChinese)
	env.e.dataDir = dataDir
	env.e.SetDisplayConfig(DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	env.e.openInflight()
	env.e.markPlatformReady(env.p) // as Start does; the resume pass itself is called by the tests
	env.e.queueBatch.Hold = 0      // automated prompts start at once instead of waiting to be batched
	t.Cleanup(env.e.cancel)
	return env
}

func (env *resumeEnv) noPrompt(t *testing.T) {
	t.Helper()
	select {
	case got := <-env.s.started:
		t.Fatalf("unexpected turn: %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRestartResumesTheCutOffTurnOnce(t *testing.T) {
	dir := t.TempDir()
	// First process: a turn and a background task are running when it dies.
	a := newResumeEnv(t, dir)
	a.e.ReceiveMessage(a.p, &Message{SessionKey: a.key, Platform: a.p.Name(), MessageID: "u1", UserID: "user", Content: "把三张付款单逐张批掉", ReplyCtx: a.key})
	cc27Take(t, a.s.started)
	a.e.inflight.taskOpen(a.key, "t1", "核对越南运费")
	a.e.cancel()

	// Second process: one internal turn names what was cut off.
	b := newResumeEnv(t, dir)
	clock := b.e.inflightLeft.AliveAt.In(decisionZone).Format("15:04")
	b.e.resumeInterrupted(time.Now())
	got := cc27Take(t, b.s.started)
	for _, want := range []string{resumeMarker, "上次在 " + clock + " 因重启中断", "请求「把三张付款单逐张批掉」", "后台任务「核对越南运费」", "请核对状态后续做，并用一句话告诉用户已接着做。"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt %q lacks %q", got, want)
		}
	}
	b.e.resumeInterrupted(time.Now()) // at most once
	b.noPrompt(t)

	// The resume turn is itself cut off: the third process only tells the person.
	b.e.cancel()
	c := newResumeEnv(t, dir)
	c.e.resumeInterrupted(time.Now())
	c.noPrompt(t)
	sent := c.p.getSent()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "上次 ") || !strings.HasSuffix(sent[0], " 的任务被中断，需要的话说一声我再继续。") {
		t.Fatalf("sent=%q", sent)
	}
}

func TestRestartAfterSixHoursOnlyTellsThePerson(t *testing.T) {
	dir := t.TempDir()
	path := inflightPath(dir, "scheduled")
	cut := time.Date(2026, 11, 12, 2, 5, 0, 0, time.UTC) // 10:05 Beijing
	data, _ := json.Marshal(inflightFile{AliveAt: cut, Sessions: map[string]*inflightSession{
		"scheduled:chat:user": {Turn: &inflightTurn{Request: "整理周报", Started: cut.Add(-time.Minute)}},
	}})
	if err := AtomicWriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	env := newResumeEnv(t, dir)
	env.e.resumeInterrupted(cut.Add(6*time.Hour + time.Minute))
	env.noPrompt(t)
	if sent := env.p.getSent(); len(sent) != 1 || sent[0] != "上次 10:05 的任务被中断，需要的话说一声我再继续。" {
		t.Fatalf("sent=%q", sent)
	}
}

func TestFinishedWorkLeavesNothingToResume(t *testing.T) {
	dir := t.TempDir()
	a := newResumeEnv(t, dir)
	a.e.ReceiveMessage(a.p, &Message{SessionKey: a.key, Platform: a.p.Name(), MessageID: "u1", UserID: "user", Content: "查一下日志", ReplyCtx: a.key})
	cc27Take(t, a.s.started)
	a.s.events <- Event{Type: EventResult, Content: "查好了", Done: true}
	cc27WaitIdle(t, a.e, a.key)
	a.e.cancel()
	b := newResumeEnv(t, dir)
	if len(b.e.inflightLeft.Sessions) != 0 {
		t.Fatalf("finished turn left %+v", b.e.inflightLeft.Sessions)
	}
}

// A background task's card puts the task in the record until it ends.
func TestTaskCardsAreRecordedWhileOpen(t *testing.T) {
	env := newTriageEnv(t)
	dir := t.TempDir()
	env.e.dataDir = dir
	env.e.openInflight()
	env.e.taskCards.openDelay = 0
	env.firstTurn("已在后台整理。")
	env.backgroundTask("t1", "按客户整理 200 个文件", "读取文件")
	path := inflightPath(dir, env.e.name)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if rec := readInflight(t, path).Sessions[env.key]; rec != nil && rec.Tasks["t1"] == "按客户整理 200 个文件" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task not recorded: %+v", readInflight(t, path))
		}
		time.Sleep(10 * time.Millisecond)
	}
	env.e.interactiveMu.Lock()
	state := env.e.interactiveStates[env.key]
	env.e.interactiveMu.Unlock()
	env.e.handleTaskEvent(state, env.key, &TaskEvent{Kind: TaskEnded, TaskID: "t1", Status: "completed", Summary: "整理完成"})
	deadline = time.Now().Add(10 * time.Second)
	for readInflight(t, path).Sessions[env.key] != nil {
		if time.Now().After(deadline) {
			t.Fatalf("ended task still recorded: %+v", readInflight(t, path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
