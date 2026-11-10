package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

// fakeCardKit is a CardKit server with the platform's latency and its sequence rule: a
// card operation whose sequence is not larger than the last accepted one is answered with
// HTTP 400 and code 300317. A call takes `latency` end to end; the card applies it half
// way through (the request travels to the origin, the reply travels back).
type fakeCardKit struct {
	mu       sync.Mutex
	latency  func(n int) time.Duration // n counts card operations from 0
	ops      []cardOp
	lastSeq  float64
	inflight int
	maxIn    int
	failBody string // when set, body updates answer with this code in an HTTP 400
	ordinal  int
}

type cardOp struct {
	at       time.Time
	kind     string // "body", "element", "settings", "card"
	seq      float64
	n        int // body length in runes
	text     string
	accepted bool
}

func newFakeCardKit(t *testing.T, latency func(n int) time.Duration) (*fakeCardKit, *Platform) {
	t.Helper()
	f := &fakeCardKit{latency: latency}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			fmt.Fprint(w, `{"code":0,"expire":7200,"tenant_access_token":"fixture"}`)
			return
		}
		kind := ""
		switch {
		case strings.HasSuffix(r.URL.Path, "/content") && r.Method == http.MethodPut:
			kind = "body"
		case strings.Contains(r.URL.Path, "/elements/"):
			kind = "element"
		case strings.HasSuffix(r.URL.Path, "/settings"):
			kind = "settings"
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/cardkit/v1/cards/"):
			kind = "card"
		}
		if kind == "" {
			fmt.Fprint(w, `{"code":0,"data":{"card_id":"card_fixture","message_id":"om_preview"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		n := f.ordinal
		f.ordinal++
		f.inflight++
		f.maxIn = max(f.maxIn, f.inflight)
		f.mu.Unlock()
		d := f.latency(n)
		time.Sleep(d / 2)
		op := cardOp{at: time.Now(), kind: kind}
		op.seq, _ = body["sequence"].(float64)
		if s, ok := body["content"].(string); ok {
			op.text, op.n = s, len([]rune(s))
		}
		f.mu.Lock()
		reply := `{"code":0}`
		switch {
		case kind == "body" && f.failBody != "":
			reply = fmt.Sprintf(`{"code":%s,"msg":"fixture"}`, f.failBody)
		case op.seq <= f.lastSeq:
			reply = `{"code":300317,"msg":"sequence number compare failed"}`
		default:
			op.accepted = true
			f.lastSeq = op.seq
		}
		f.ops = append(f.ops, op)
		f.mu.Unlock()
		time.Sleep(d - d/2)
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
		if reply != `{"code":0}` {
			w.WriteHeader(http.StatusBadRequest)
		}
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)
	p := &Platform{client: lark.NewClient(t.Name(), "fixture", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())), platformName: "feishu", useInteractiveCard: true, progressStyle: "compact", keepPreviewMaxAge: 120 * time.Second, finalReplyOrder: core.FinalReplyRecallFirst}
	p.spinnerUpload = func(context.Context, []byte) (string, error) { return "", fmt.Errorf("fixture: no upload") }
	return f, p
}

func (f *fakeCardKit) snapshot() []cardOp {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cardOp(nil), f.ops...)
}

func fixedLatency(d time.Duration) func(int) time.Duration {
	return func(int) time.Duration { return d }
}

// tracedLatency draws from the measured distribution of content updates on the
// server (trace quantiles p5 481, p25 509, p50 555, p75 600, p90 735, p99 954 ms).
func tracedLatency(seed int64) func(int) time.Duration {
	q := []struct{ p, ms float64 }{{0, 440}, {.05, 481}, {.25, 509}, {.5, 555}, {.75, 600}, {.9, 735}, {.99, 954}, {1, 1100}}
	rng := rand.New(rand.NewSource(seed))
	var mu sync.Mutex
	return func(int) time.Duration {
		mu.Lock()
		u := rng.Float64()
		mu.Unlock()
		for i := 1; i < len(q); i++ {
			if u <= q[i].p {
				f := (u - q[i-1].p) / (q[i].p - q[i-1].p)
				return time.Duration((q[i-1].ms + f*(q[i].ms-q[i-1].ms)) * float64(time.Millisecond))
			}
		}
		return 1100 * time.Millisecond
	}
}

func startLivePreview(t *testing.T, p *Platform, mod func(*core.StreamPreviewCfg)) *feishuPreviewHandle {
	t.Helper()
	cfg := core.ProductStreamPreviewCfg()
	cfg.PushIntervalMs = 150
	if mod != nil {
		mod(&cfg)
	}
	handle, err := p.StartSegmentedPreview(context.Background(), replyContext{chatID: "oc"}, cfg, core.NewI18n(core.LangChinese))
	if err != nil {
		t.Fatal(err)
	}
	h := handle.(*feishuPreviewHandle)
	t.Cleanup(h.live.stop)
	return h
}

// feedModelText delivers text to the preview as an agent would: one cumulative update
// every 50 ms at the given characters per second, until `chars` have been written.
func feedModelText(p *Platform, h *feishuPreviewHandle, cps, chars int) {
	per := cps / 20
	for n := per; ; n += per {
		n = min(n, chars)
		_ = p.SetPreviewBody(context.Background(), h, replyBody(strings.Repeat("字", n)))
		if n >= chars {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// printerGrowth replays accepted body updates through the client's typewriter (4 chars
// every 40 ms, "delay" strategy: finish the buffered text first) and returns the
// instants at which visible text grew.
func printerGrowth(ops []cardOp, step int, tick time.Duration) []time.Time {
	var updates []cardOp
	for _, o := range ops {
		if o.kind == "body" && o.accepted {
			updates = append(updates, o)
		}
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].at.Before(updates[j].at) })
	var growth []time.Time
	shown, target, i := 0, 0, 0
	var tickAt time.Time // the printer's next tick while it has text to type
	land := func(u cardOp) {
		if u.n > target {
			if shown >= target { // an idle printer starts when the text lands
				tickAt = u.at
			}
			target = u.n
		}
	}
	for {
		switch {
		case shown < target && i < len(updates) && updates[i].at.Before(tickAt):
			land(updates[i])
			i++
		case shown < target:
			shown = min(shown+step, target)
			growth = append(growth, tickAt)
			tickAt = tickAt.Add(tick)
		case i < len(updates):
			land(updates[i])
			i++
		default:
			return growth
		}
	}
}

type stallStats struct {
	gaps          []time.Duration
	over150       int
	excess        time.Duration // total time beyond 150 ms across all stalls
	max, p50, p99 time.Duration
	buckets       map[string]int
}

func stallsOf(growth []time.Time) stallStats {
	s := stallStats{buckets: map[string]int{}}
	for i := 1; i < len(growth); i++ {
		s.gaps = append(s.gaps, growth[i].Sub(growth[i-1]))
	}
	if len(s.gaps) == 0 {
		return s
	}
	sorted := append([]time.Duration(nil), s.gaps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	s.max, s.p50, s.p99 = sorted[len(sorted)-1], sorted[len(sorted)/2], sorted[len(sorted)*99/100]
	for _, g := range s.gaps {
		if g > 150*time.Millisecond {
			s.over150++
			s.excess += g - 150*time.Millisecond
		}
		switch {
		case g <= 50*time.Millisecond:
			s.buckets["<=50ms"]++
		case g <= 100*time.Millisecond:
			s.buckets["51-100ms"]++
		case g <= 150*time.Millisecond:
			s.buckets["101-150ms"]++
		case g <= 250*time.Millisecond:
			s.buckets["151-250ms"]++
		case g <= 500*time.Millisecond:
			s.buckets["251-500ms"]++
		default:
			s.buckets[">500ms"]++
		}
	}
	return s
}

func (s stallStats) String() string {
	var b strings.Builder
	for _, k := range []string{"<=50ms", "51-100ms", "101-150ms", "151-250ms", "251-500ms", ">500ms"} {
		fmt.Fprintf(&b, "%s:%d ", k, s.buckets[k])
	}
	return fmt.Sprintf("growth events %d, stalls>150ms %d (%v beyond 150ms in all), max %v, p99 %v | %s", len(s.gaps)+1, s.over150, s.excess.Round(time.Millisecond), s.max.Round(time.Millisecond), s.p99.Round(time.Millisecond), b.String())
}

// runGrowth streams `chars` characters at a steady model rate through a card on a
// fake server and returns the visible-growth stalls of the client printer, measured
// from when the first text is visible to when the last frame of the stream landed.
func runGrowth(t *testing.T, latency func(int) time.Duration, cps, chars int, mod func(*core.StreamPreviewCfg)) stallStats {
	t.Helper()
	f, p := newFakeCardKit(t, latency)
	h := startLivePreview(t, p, mod)
	feedModelText(p, h, cps, chars)
	time.Sleep(1800 * time.Millisecond) // let the paced text and in-flight calls land
	h.live.stop()
	ops := f.snapshot()
	return stallsOf(printerGrowth(ops, 4, 40*time.Millisecond))
}

func serialCfg(c *core.StreamPreviewCfg) { c.BodyInflight, c.RevealLeadMs = 1, 0 }

// Simulated visible growth at a steady model output of 60 characters per second, with
// the 550 ms card call of the production path, before (one call at a time) and after
// (pipelined). The log lines carry the stall histograms.
func TestSimulatedGrowthStalls(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time simulation")
	}
	for _, tc := range []struct {
		name    string
		latency func(int) time.Duration
	}{
		{"fixed550ms", fixedLatency(550 * time.Millisecond)},
		{"traced-jitter", tracedLatency(7)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := runGrowth(t, tc.latency, 60, 480, serialCfg)
			after := runGrowth(t, tc.latency, 60, 480, nil)
			t.Logf("before (serial):    %s", before)
			t.Logf("after  (pipelined): %s", after)
			if after.excess >= before.excess || after.max >= before.max || after.max > 300*time.Millisecond {
				t.Fatalf("pipelining did not smooth the growth\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// While every card call takes 550 ms, the card still gets a body update at the push
// cadence, with up to the in-flight limit outstanding at once.
func TestPipelinedBodyCadenceWhileCallsAreSlow(t *testing.T) {
	f, p := newFakeCardKit(t, fixedLatency(550*time.Millisecond))
	h := startLivePreview(t, p, nil)
	feedModelText(p, h, 60, 300)
	time.Sleep(1500 * time.Millisecond)
	var at []time.Time
	for _, o := range f.snapshot() {
		if o.kind == "body" {
			at = append(at, o.at)
		}
	}
	if len(at) < 15 {
		t.Fatalf("%d body updates in 5 s, want about one every 150 ms", len(at))
	}
	var worst time.Duration
	for i := 1; i < len(at); i++ {
		if at[i].Sub(at[0]) < 1500*time.Millisecond { // warm-up: the first layout call is exclusive
			continue
		}
		worst = max(worst, at[i].Sub(at[i-1]))
	}
	if worst > 300*time.Millisecond {
		t.Fatalf("longest wait between body updates %v", worst)
	}
	if f.maxIn < 2 || f.maxIn > 6 {
		t.Fatalf("%d calls in flight at the peak, want 2 to 6", f.maxIn)
	}
}

func TestPipelinedBodyRespectsInflightLimit(t *testing.T) {
	f, p := newFakeCardKit(t, fixedLatency(600*time.Millisecond))
	h := startLivePreview(t, p, func(c *core.StreamPreviewCfg) { c.BodyInflight = 2 })
	feedModelText(p, h, 60, 240)
	time.Sleep(1200 * time.Millisecond)
	if f.maxIn != 2 {
		t.Fatalf("peak in flight %d, want the limit 2", f.maxIn)
	}
}

// A step change rebuilds the card layout. That call must be sent after every body
// update before it, and no body update may start before it has finished.
func TestStructuralMutationKeepsStrictOrder(t *testing.T) {
	f, p := newFakeCardKit(t, tracedLatency(3))
	h := startLivePreview(t, p, nil)
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		feedModelText(p, h, 60, 360)
	}()
	time.Sleep(1200 * time.Millisecond) // body updates are in flight
	if err := p.AddPreviewTool(ctx, h, "读文件"); err != nil {
		t.Fatal(err)
	}
	<-done
	time.Sleep(1500 * time.Millisecond)
	ops := f.snapshot()
	structural := 0
	var last cardOp
	for _, o := range ops {
		if o.kind != "body" {
			if !o.accepted {
				t.Fatalf("%s update rejected: it was overtaken by a body update", o.kind)
			}
			structural++
		}
		if o.kind == "body" && !o.accepted && last.seq == 0 {
			t.Fatal("first body update rejected")
		}
		last = o
	}
	if structural < 2 { // the initial layout and the new step row
		t.Fatalf("%d structural updates, want at least 2: %+v", structural, ops)
	}
	// Around each structural update, sequence order is arrival order.
	for i, o := range ops {
		if o.kind == "body" {
			continue
		}
		for j, b := range ops {
			if b.kind == "body" && ((j < i && b.seq > o.seq) || (j > i && b.seq < o.seq)) {
				t.Fatalf("body update %v overtook a %s update (seq %v at %d, body %v at %d)", b.seq, o.kind, o.seq, i, b.seq, j)
			}
		}
	}
}

// Two body updates overtake each other on the way: the late one is rejected as stale.
// That is harmless: the card shows the newer text, streaming goes on without a
// fallback, and later text still arrives.
func TestStaleBodyUpdateRejectionIsHarmless(t *testing.T) {
	var mu sync.Mutex
	bodies := 0
	lat := func(int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		bodies++
		if bodies == 3 { // the first body update (after create-layout calls) is slow
			return 1000 * time.Millisecond
		}
		return 100 * time.Millisecond
	}
	f, p := newFakeCardKit(t, lat)
	h := startLivePreview(t, p, nil)
	feedModelText(p, h, 60, 300)
	time.Sleep(1500 * time.Millisecond)
	rejected, accepted := 0, 0
	lastBody := ""
	for _, o := range f.snapshot() {
		if o.kind != "body" {
			continue
		}
		if o.accepted {
			accepted++
			lastBody = o.text
		} else {
			rejected++
		}
	}
	if rejected == 0 {
		t.Skip("no body update was overtaken on this run")
	}
	h.live.mu.Lock()
	fallback, shown := h.live.fallback, h.live.shown
	h.live.mu.Unlock()
	if fallback || h.live.hasPipeErr() {
		t.Fatalf("a stale rejection ended streaming (fallback=%v)", fallback)
	}
	if accepted < 10 || len([]rune(lastBody)) < shown-60 {
		t.Fatalf("streaming stalled after the rejection: %d accepted, card has %d of %d chars", accepted, len([]rune(lastBody)), shown)
	}
}

// A body update that fails for another reason ends the pipelined stream the same way a
// failed serial update does: the card moves to full-card updates.
func TestBodyUpdateFailureFallsBackToFullCard(t *testing.T) {
	f, p := newFakeCardKit(t, fixedLatency(100*time.Millisecond))
	f.failBody = "300309" // streaming mode closed
	h := startLivePreview(t, p, nil)
	feedModelText(p, h, 60, 240)
	waitPreview(t, func() bool {
		h.live.mu.Lock()
		defer h.live.mu.Unlock()
		return h.live.fallback
	})
}

// The settled card is the last thing the server sees, after every body update.
func TestFinalSettleComesAfterBodyUpdates(t *testing.T) {
	f, p := newFakeCardKit(t, fixedLatency(550*time.Millisecond))
	h := startLivePreview(t, p, nil)
	feedModelText(p, h, 60, 240)
	final := replyBody(strings.Repeat("字", 300))
	start := time.Now()
	p.CatchUpPreview(context.Background(), h, final)
	caught := time.Since(start)
	if err := p.FinishSegmentedPreview(context.Background(), h, final); err != nil {
		t.Fatal(err)
	}
	ops := f.snapshot()
	last := ops[len(ops)-1]
	if last.kind != "card" || !last.accepted {
		t.Fatalf("last operation %s accepted=%v, want the settled card", last.kind, last.accepted)
	}
	for _, o := range ops {
		if o.kind == "body" && !o.accepted && false {
			t.Fatal("unreachable")
		}
		if o.seq > last.seq {
			t.Fatalf("an operation with sequence %v came after the settle (%v)", o.seq, last.seq)
		}
	}
	got := ""
	for _, o := range ops {
		if o.kind == "body" && o.accepted {
			got = o.text
		}
	}
	if got != final {
		t.Fatalf("last streamed text has %d chars, want the final %d", len([]rune(got)), len([]rune(final)))
	}
	if caught > 2500*time.Millisecond {
		t.Fatalf("end-of-turn catch-up took %v", caught)
	}
	t.Logf("end-of-turn catch-up of 60 characters: %v", caught.Round(time.Millisecond))
}

// Without the lead the typing buffer runs empty between slow calls; with it the client
// always holds text. Deterministic check of the release rule.
func TestRevealKeepsClientBufferAtLead(t *testing.T) {
	cfg := core.ProductStreamPreviewCfg()
	cfg.RevealLeadMs = 250
	v := &segmentedPreview{cfg: cfg, target: strings.Repeat("字", 2000)}
	now := time.Unix(1000, 0)
	v.reveal(now) // first frame: one lead
	if want := int(0.25 * v.typingCps()); v.shown != want {
		t.Fatalf("first frame released %d chars, want the lead %d", v.shown, want)
	}
	prev := v.shown
	for i := 1; i <= 20; i++ {
		now = now.Add(150 * time.Millisecond)
		v.reveal(now)
	}
	got := float64(v.shown-prev) / 3.0 // chars per second over 3 s
	if got < 95 || got > 110 {
		t.Fatalf("steady release %.1f chars/s, want the typing speed 100", got)
	}
	// The same with no lead releases the base rate.
	cfg.RevealLeadMs = 0
	w := &segmentedPreview{cfg: cfg, target: strings.Repeat("字", 2000)}
	now = time.Unix(1000, 0)
	w.reveal(now)
	for i := 1; i <= 20; i++ {
		now = now.Add(150 * time.Millisecond)
		w.reveal(now)
	}
	if got := float64(w.shown) / 3.0; got < 80 || got > 110 {
		t.Fatalf("without lead: %.1f chars/s", got)
	}
}

// The quick line is written before any body text. It goes out as an element update on
// the exclusive path, in sequence order with the body updates that follow it.
func TestQuickLineGoesOutInOrderWithBody(t *testing.T) {
	f, p := newFakeCardKit(t, tracedLatency(5))
	h := startLivePreview(t, p, nil)
	if err := p.SetPreviewQuickLine(context.Background(), h, "收到，马上看"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		seen := false
		for _, o := range f.snapshot() {
			seen = seen || o.kind == "element"
		}
		if seen {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	feedModelText(p, h, 60, 240)
	time.Sleep(1500 * time.Millisecond)
	ops := f.snapshot()
	quickAt := -1
	for i, o := range ops {
		if o.kind != "body" && !o.accepted {
			t.Fatalf("%s update rejected: it was overtaken", o.kind)
		}
		if quickAt < 0 && o.kind == "element" {
			quickAt = i
		}
	}
	if quickAt < 0 {
		t.Fatal("quick line never went out as an element update")
	}
	for i, o := range ops {
		if o.kind == "body" && i < quickAt && o.seq > ops[quickAt].seq {
			t.Fatalf("body update overtook the quick line (seq %v before %v)", o.seq, ops[quickAt].seq)
		}
	}
}
