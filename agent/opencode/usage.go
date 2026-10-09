package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Use Grok's billing extension, not inference rate-limit headers. The official
// client owns OAuth refresh/locking; this adapter never reads or writes tokens.
type grokBilling struct {
	Config *struct {
		Used   *float64 `json:"creditUsagePercent"`
		Period *struct {
			Type  string `json:"type"`
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"currentPeriod"`
	} `json:"config"`
	Tier string `json:"subscription_tier"`
}

func (a *Agent) GetUsage(ctx context.Context) (*core.UsageReport, error) {
	a.mu.RLock()
	cmd, model := a.usageCmd, a.model
	a.mu.RUnlock()
	if !strings.HasPrefix(model, "xai/") {
		return nil, fmt.Errorf("opencode: subscription usage is unavailable for model %q", model)
	}
	if cmd == "" {
		return nil, fmt.Errorf("opencode: usage_cmd must point to the authenticated Grok client")
	}
	billing, err := fetchGrokBilling(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return grokUsageReport(billing, time.Now())
}

func fetchGrokBilling(ctx context.Context, binary string) (grokBilling, error) {
	ctx, cancel := context.WithTimeout(ctx, 9*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "agent", "--no-leader", "stdio")
	// Billing has no project/session. Never load hooks from the live worktree.
	cmd.Dir = "/tmp"
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return grokBilling{}, fmt.Errorf("grok billing stdin: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return grokBilling{}, fmt.Errorf("grok billing stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return grokBilling{}, fmt.Errorf("grok billing start: %w", err)
	}
	defer func() { _ = in.Close(); cancel(); _ = cmd.Wait() }()
	return readGrokBilling(ctx, in, out)
}

func readGrokBilling(ctx context.Context, in io.Writer, out io.Reader) (grokBilling, error) {
	enc := json.NewEncoder(in)
	send := func(id int, method string, params any) error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}
	if err := send(1, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "cc-connect-usage", "version": "1"},
	}); err != nil {
		return grokBilling{}, fmt.Errorf("grok billing initialize: %w", err)
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	initialized := false
	for scanner.Scan() {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &reply) != nil {
			continue
		}
		if reply.ID != 1 && reply.ID != 2 {
			continue
		}
		if reply.Error != nil {
			// Raw upstream diagnostics may contain private account identifiers.
			detail := strings.ToLower(reply.Error.Message + string(reply.Error.Data))
			kind := "upstream request failed"
			if strings.Contains(detail, "auth") || strings.Contains(detail, "login") || strings.Contains(detail, "expired") {
				kind = "authentication failed; run grok login to renew authorization"
			}
			return grokBilling{}, fmt.Errorf("grok billing: %s (RPC %d)", kind, reply.Error.Code)
		}
		if reply.ID == 1 {
			if initialized {
				continue
			}
			initialized = true
			if err := send(2, "_x.ai/billing", map[string]any{}); err != nil {
				return grokBilling{}, fmt.Errorf("grok billing request: %w", err)
			}
			continue
		}
		if !initialized {
			return grokBilling{}, fmt.Errorf("grok billing: response before initialization")
		}
		var billing grokBilling
		if err := json.Unmarshal(reply.Result, &billing); err != nil {
			return grokBilling{}, fmt.Errorf("grok billing: invalid response schema")
		}
		return billing, nil
	}
	if err := ctx.Err(); err != nil {
		return grokBilling{}, fmt.Errorf("grok billing: %w", err)
	}
	if err := scanner.Err(); err != nil {
		return grokBilling{}, fmt.Errorf("grok billing stream: %w", err)
	}
	return grokBilling{}, fmt.Errorf("grok billing: client closed before returning usage")
}

func grokUsageReport(b grokBilling, now time.Time) (*core.UsageReport, error) {
	if b.Config == nil || b.Config.Used == nil || b.Config.Period == nil {
		return nil, fmt.Errorf("grok billing: subscription allowance or period is missing")
	}
	p := b.Config.Period
	start, err1 := time.Parse(time.RFC3339Nano, p.Start)
	end, err2 := time.Parse(time.RFC3339Nano, p.End)
	if err1 != nil || err2 != nil || !end.After(start) {
		return nil, fmt.Errorf("grok billing: invalid subscription period")
	}
	used := *b.Config.Used
	if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
		return nil, fmt.Errorf("grok billing: invalid usage percentage")
	}
	window := core.UsageWindow{Name: p.Type, UsedPercent: int(math.Min(100, math.Round(used))),
		WindowSeconds: int(end.Sub(start).Seconds()), ResetAtUnix: end.Unix(),
		ResetAfterSeconds: int(math.Max(0, end.Sub(now).Seconds()))}
	tier := b.Tier
	if tier == "" {
		tier = "Grok"
	}
	return &core.UsageReport{Provider: "xAI", Plan: tier,
		Buckets: []core.UsageBucket{{Name: "subscription", Allowed: used < 100, LimitReached: used >= 100, Windows: []core.UsageWindow{window}}},
	}, nil
}
