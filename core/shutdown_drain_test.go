package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type drainPlatform struct {
	stubPlatformEngine
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *drainPlatform) Send(ctx context.Context, r any, s string) error {
	if strings.Contains(s, "drain-final") {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.stubPlatformEngine.Send(ctx, r, s)
}

func TestEngineStop_DrainsAcceptedDeliveryBeforeCancel(t *testing.T) {
	p := &drainPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, started: make(chan struct{}), release: make(chan struct{})}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	e.launchAcceptedWork(func() { e.send(p, "ctx", "drain-final") })
	<-p.started
	done := make(chan error, 1)
	go func() { done <- e.Stop() }()
	select {
	case err := <-done:
		t.Fatalf("stopped before delivery: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if e.ctx.Err() != nil {
		t.Fatal("reply context canceled during drain")
	}
	close(p.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown stuck")
	}
	if !strings.Contains(strings.Join(p.getSent(), ""), "drain-final") {
		t.Fatal("reply lost")
	}
}

type failingUsageCardPlatform struct{ stubPlatformEngine }

func (p *failingUsageCardPlatform) ReplyCard(context.Context, any, *Card) error {
	return errors.New("card unavailable")
}
func (p *failingUsageCardPlatform) SendCard(context.Context, any, *Card) error {
	return errors.New("card unavailable")
}

func TestUsageCardFailureFallsBackToText(t *testing.T) {
	p := &failingUsageCardPlatform{stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	defer e.Stop()
	e.replyWithCard(p, "ctx", NewCard().Title("Usage", "indigo").Markdown("90% remaining").Build())
	if !strings.Contains(strings.Join(p.getSent(), ""), "90% remaining") {
		t.Fatal("usage silently lost after card error")
	}
}

type drainUsageAgent struct {
	cujAgent
	usageErr error
}

func (a *drainUsageAgent) GetUsage(context.Context) (*UsageReport, error) {
	if a.usageErr != nil {
		return nil, a.usageErr
	}
	return &UsageReport{Provider: "xAI", Plan: "SuperGrok Plus", Buckets: []UsageBucket{{Windows: []UsageWindow{{WindowSeconds: 604800, UsedPercent: 10, ResetAfterSeconds: 3600}}}}}, nil
}

func TestCUJ_RecoveryUsageThenReplySurvivesRestart(t *testing.T) {
	p := &drainPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, started: make(chan struct{}), release: make(chan struct{})}
	a := &drainUsageAgent{}
	dir := t.TempDir() + "/sessions.json"
	e := NewEngine("test", a, []Platform{p}, dir, LangEnglish)
	send := func(content string) {
		e.ReceiveMessage(p, &Message{SessionKey: "test:user", Platform: "test", MessageID: content, UserID: "user", Content: content, ReplyCtx: "ctx"})
	}
	send("/usage")
	if !strings.Contains(strings.Join(p.getSent(), ""), "90%") {
		t.Fatalf("no real allowance: %v", p.getSent())
	}
	p.clearSent()
	a.usageErr = errors.New("billing authentication expired")
	send("/usage")
	if !strings.Contains(strings.Join(p.getSent(), ""), "expired") {
		t.Fatalf("failure hidden: %v", p.getSent())
	}
	a.nextSessionEvents = []Event{{Type: EventText, Content: "drain-final"}, {Type: EventResult, Done: true, SessionID: "s"}}
	send("hello")
	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("reply did not start")
	}
	done := make(chan error, 1)
	go func() { done <- e.Stop() }()
	select {
	case <-done:
		t.Fatal("restart canceled reply")
	case <-time.After(30 * time.Millisecond):
	}
	close(p.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if !strings.Contains(strings.Join(p.getSent(), ""), "drain-final") {
		t.Fatal("final not delivered")
	}
	// Reopen the same durable session store, exactly as a service restart does.
	p2 := &stubPlatformEngine{n: "test"}
	e2 := NewEngine("test", &cujAgent{}, []Platform{p2}, dir, LangEnglish)
	defer e2.Stop()
	e2.ReceiveMessage(p2, &Message{SessionKey: "test:user", Platform: "test", MessageID: "history", UserID: "user", Content: "/history", ReplyCtx: "ctx"})
	if !strings.Contains(strings.Join(p2.getSent(), ""), "drain-final") {
		t.Fatalf("history lost on restart: %v", p2.getSent())
	}
}
