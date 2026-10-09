package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const billingFixture = `{"config":{"creditUsagePercent":10,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-05T20:18:50Z","end":"2026-09-12T20:18:50Z"}},"subscription_tier":"SuperGrok Plus"}`

func TestGrokUsageUsesActualBillingPeriod(t *testing.T) {
	var b grokBilling
	if err := json.Unmarshal([]byte(billingFixture), &b); err != nil {
		t.Fatal(err)
	}
	r, err := grokUsageReport(b, time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	w := r.Buckets[0].Windows[0]
	if w.UsedPercent != 10 || w.WindowSeconds != 604800 || w.ResetAtUnix != time.Date(2026, 9, 12, 20, 18, 50, 0, time.UTC).Unix() || r.Plan != "SuperGrok Plus" {
		t.Fatalf("bad report: %+v %+v", r, w)
	}
	// A month must not be silently relabeled as a seven-day quota.
	b.Config.Period.Start = "2026-09-01T00:00:00Z"
	b.Config.Period.End = "2026-10-01T00:00:00Z"
	r, err = grokUsageReport(b, time.Now())
	if err != nil || r.Buckets[0].Windows[0].WindowSeconds != 30*86400 {
		t.Fatalf("monthly period: %+v %v", r, err)
	}
}

func TestGrokUsageMissingIsNotZero(t *testing.T) {
	for _, raw := range []string{`{}`, `{"config":{}}`, strings.Replace(billingFixture, `"creditUsagePercent":10,`, "", 1), strings.Replace(billingFixture, "2026-09-12T20:18:50Z", "invalid", 1)} {
		var b grokBilling
		_ = json.Unmarshal([]byte(raw), &b)
		if r, e := grokUsageReport(b, time.Now()); e == nil || r != nil {
			t.Fatalf("accepted incomplete billing: %s", raw)
		}
	}
	var b grokBilling
	_ = json.Unmarshal([]byte(strings.Replace(billingFixture, ":10", ":0", 1)), &b)
	if r, e := grokUsageReport(b, time.Now()); e != nil || r.Buckets[0].Windows[0].UsedPercent != 0 {
		t.Fatalf("explicit zero rejected: %v", e)
	}
}

func TestGrokBillingProtocolNeverSendsInference(t *testing.T) {
	var sent bytes.Buffer
	response := "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":" + billingFixture + "}\n"
	b, err := readGrokBilling(context.Background(), &sent, strings.NewReader(response))
	if err != nil || b.Config == nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent.String(), `"_x.ai/billing"`) || strings.Contains(sent.String(), "prompt") || strings.Contains(sent.String(), "completions") {
		t.Fatalf("wrong protocol: %s", sent.String())
	}
}

func TestGrokBillingFailureIsVisibleAndRedacted(t *testing.T) {
	var sent bytes.Buffer
	_, err := readGrokBilling(context.Background(), &sent, strings.NewReader("{\"id\":1,\"error\":{\"code\":-32000,\"message\":\"auth expired\",\"data\":\"private-token\"}}\n"))
	if err == nil || !strings.Contains(err.Error(), "authentication") || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("bad error: %v", err)
	}
	_, err = readGrokBilling(context.Background(), &sent, strings.NewReader(""))
	if err == nil {
		t.Fatal("empty client output treated as success")
	}
}

func TestGrokBillingCancellation(t *testing.T) {
	script := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 20\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := fetchGrokBilling(ctx, script)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("cancellation failed: %v", err)
	}
}

func TestGrokUsageLive(t *testing.T) {
	cmd := os.Getenv("CC_TEST_GROK_USAGE_CMD")
	if cmd == "" {
		t.Skip("explicit live verification only")
	}
	a := &Agent{usageCmd: cmd, model: "xai/grok-4.6"}
	r, err := a.GetUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Buckets) != 1 || r.Buckets[0].Windows[0].ResetAtUnix == 0 {
		t.Fatalf("incomplete live report: %+v", r)
	}
	t.Logf("provider=%s plan=%s used=%d reset=%d", r.Provider, r.Plan, r.Buckets[0].Windows[0].UsedPercent, r.Buckets[0].Windows[0].ResetAtUnix)
}
