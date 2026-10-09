package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/chenhg5/cc-connect/core"
)

// The CLI panel can hide limits[] behind a feature flag. Read the account's
// structured response instead; never infer a model quota from the general one.
func (a *Agent) fetchClaudeUsageAPI(ctx context.Context) (*core.UsageReport, error) {
	env := make(map[string]string)
	for _, item := range core.MergeEnv(os.Environ(), a.usageProbeEnv()) {
		if k, v, ok := strings.Cut(item, "="); ok {
			env[k] = v
		}
	}
	token, err := claudeUsageToken(env)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	for _, key := range []string{"CLAUDE_PROXY", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if env[key] == "" {
			continue
		}
		proxy, err := url.Parse(env[key])
		if err != nil || proxy.Host == "" {
			return nil, errors.New("claudecode: invalid usage proxy")
		}
		transport.Proxy = http.ProxyURL(proxy)
		break
	}
	client := &http.Client{Transport: transport, Timeout: 4 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return requestClaudeUsage(ctx, client, "https://api.anthropic.com/api/oauth/usage", token)
}

func claudeUsageToken(env map[string]string) (string, error) {
	// An API-key/custom-provider session must not display an unrelated local
	// subscription's quota. The existing configured CLI remains its fallback.
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if env[key] != "" {
			return "", errors.New("claudecode: usage OAuth not applicable")
		}
	}
	if token := env["CLAUDE_CODE_OAUTH_TOKEN"]; token != "" {
		return token, nil
	}
	directory := env["CLAUDE_CONFIG_DIR"]
	if directory == "" {
		if env["HOME"] == "" {
			return "", errors.New("claudecode: usage credential directory unavailable")
		}
		directory = filepath.Join(env["HOME"], ".claude")
	}
	data, err := os.ReadFile(filepath.Join(directory, ".credentials.json"))
	if err != nil {
		return "", errors.New("claudecode: usage OAuth credentials unavailable")
	}
	var credentials struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &credentials) != nil || credentials.OAuth.AccessToken == "" {
		return "", errors.New("claudecode: usage OAuth credentials unavailable")
	}
	return credentials.OAuth.AccessToken, nil
}

func requestClaudeUsage(ctx context.Context, client *http.Client, endpoint, token string) (*core.UsageReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("claudecode: invalid usage endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("claudecode: usage API request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claudecode: usage API HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, errors.New("claudecode: usage API response unavailable")
	}
	return parseClaudeUsageAPI(data, time.Now())
}

type claudeAPIWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

func parseClaudeUsageAPI(data []byte, now time.Time) (*core.UsageReport, error) {
	var payload struct {
		Session *claudeAPIWindow `json:"five_hour"`
		Week    *claudeAPIWindow `json:"seven_day"`
		Limits  []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, errors.New("claudecode: invalid usage API JSON")
	}
	general := core.UsageBucket{Name: "Usage", Allowed: true}
	for i, raw := range []*claudeAPIWindow{payload.Session, payload.Week} {
		if raw == nil {
			continue
		}
		seconds := claudeUsageSessionWindowSeconds
		if i == 1 {
			seconds = claudeUsageWeekWindowSeconds
		}
		window, err := claudeAPIUsageWindow(raw.Utilization, raw.ResetsAt, seconds, now)
		if err != nil {
			return nil, err
		}
		general.Windows = append(general.Windows, window)
	}
	report := &core.UsageReport{Provider: "claudecode"}
	if len(general.Windows) > 0 {
		report.Buckets = append(report.Buckets, general)
	}
	for _, limit := range payload.Limits {
		if limit.Kind != "weekly_scoped" || limit.Scope == nil || limit.Scope.Model == nil {
			continue
		}
		label := strings.TrimSpace(limit.Scope.Model.DisplayName)
		if label == "" || len(label) > 100 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
			return nil, errors.New("claudecode: invalid model quota label")
		}
		window, err := claudeAPIUsageWindow(limit.Percent, limit.ResetsAt, claudeUsageWeekWindowSeconds, now)
		if err != nil {
			return nil, err
		}
		report.Buckets = append(report.Buckets, core.UsageBucket{Name: label, Allowed: window.UsedPercent < 100,
			LimitReached: window.UsedPercent >= 100, Windows: []core.UsageWindow{window}})
	}
	if len(report.Buckets) == 0 {
		return nil, errors.New("claudecode: usage API has no quota windows")
	}
	return report, nil
}

func claudeAPIUsageWindow(percent *float64, reset string, seconds int, now time.Time) (core.UsageWindow, error) {
	if percent == nil || math.IsNaN(*percent) || math.IsInf(*percent, 0) || *percent < 0 || *percent > 100 {
		return core.UsageWindow{}, errors.New("claudecode: invalid usage percentage")
	}
	window := core.UsageWindow{UsedPercent: int(math.Ceil(*percent)), WindowSeconds: seconds}
	if reset != "" {
		at, err := time.Parse(time.RFC3339Nano, reset)
		if err != nil {
			return core.UsageWindow{}, errors.New("claudecode: invalid usage reset timestamp")
		}
		window.ResetAtUnix = at.Unix()
		window.ResetAfterSeconds = max(0, int(at.Sub(now).Seconds()))
	}
	return window, nil
}
