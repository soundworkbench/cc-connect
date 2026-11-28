package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultOrcaHome       = ""
	defaultSenderID       = "feishu-bridge-orca"
	defaultPollIntervalMs = 600
	defaultTurnTimeoutSec = 1800
	defaultWorkPollMs     = 2000
	defaultItemPollMs     = 3000
	defaultReadLimit      = 50
	defaultCLITimeout     = 20 * time.Second
)

// resolveDefaultOrcaBin finds the Orca CLI binary when orca_bin is not configured:
//  1. <orcaHome>/.config/orca/cli/bin/orca (falling back to os.UserHomeDir() when orcaHome is empty)
//  2. exec.LookPath("orca")
//  3. "orca"
func resolveDefaultOrcaBin(orcaHome string) string {
	home := strings.TrimSpace(orcaHome)
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = strings.TrimSpace(h)
		}
	}
	if home != "" {
		candidate := filepath.Join(home, ".config", "orca", "cli", "bin", "orca")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	if p, err := exec.LookPath("orca"); err == nil && strings.TrimSpace(p) != "" {
		return p
	}
	return "orca"
}

// cliRunner abstracts execution of the official Orca CLI for testability.
type cliRunner interface {
	Run(ctx context.Context, args []string, stdin []byte) ([]byte, error)
}

type execRunner struct {
	bin        string
	home       string
	cliTimeout time.Duration
}

func (r *execRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	timeout := r.cliTimeout
	if timeout <= 0 {
		timeout = defaultCLITimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.bin, args...)
	cmd.Env = buildCleanOrcaEnv(os.Environ(), r.home)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("orca %s: %s: %w", strings.Join(args[:min(len(args), 3)], " "), detail, err)
	}
	return stdout.Bytes(), nil
}

// buildCleanOrcaEnv strips inherited ORCA_* variables so cc-connect always
// invokes the Orca CLI as the host user rather than inheriting a bot-scoped
// session token. When orcaHome is non-empty it overrides HOME; otherwise the
// process HOME is preserved.
func buildCleanOrcaEnv(base []string, orcaHome string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "ORCA_") {
			continue
		}
		if orcaHome != "" && k == "HOME" {
			continue
		}
		out = append(out, kv)
	}
	if orcaHome != "" {
		out = append(out, "HOME="+orcaHome)
	}
	return out
}

type orcaClient struct {
	runner cliRunner
}

type orcaEnvelope struct {
	OK     *bool           `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func decodeOrcaResult(raw []byte, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("orca: empty JSON response")
	}
	var env orcaEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return fmt.Errorf("orca: decode response: %w", err)
	}
	if env.OK != nil && !*env.OK {
		return fmt.Errorf("orca: %s", extractOrcaError(env.Error))
	}
	payload := env.Result
	if len(payload) == 0 {
		payload = trimmed
	}
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("orca: decode result: %w", err)
	}
	return nil
}

func extractOrcaError(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "command returned ok=false"
	}
	var str string
	if json.Unmarshal(trimmed, &str) == nil && str != "" {
		return str
	}
	var obj struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(trimmed, &obj) == nil && (obj.Message != "" || obj.Code != "") {
		if obj.Code != "" && obj.Message != "" {
			return obj.Code + ": " + obj.Message
		}
		if obj.Message != "" {
			return obj.Message
		}
		return obj.Code
	}
	return string(trimmed)
}

type orcaChat struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	LastActivityAt int64  `json:"lastActivityAt"`
	LastAsked      string `json:"lastAsked"`
	LastSaid       string `json:"lastSaid"`
	Home           bool   `json:"home"`
	Self           bool   `json:"self"`
	Closed         bool   `json:"closed"`
}

type orcaChatsResult struct {
	Bot   string     `json:"bot"`
	Chats []orcaChat `json:"chats"`
}

func (c *orcaClient) listChats(ctx context.Context, botID string) ([]orcaChat, error) {
	raw, err := c.runner.Run(ctx, []string{"bot", "chats", "--bot", botID, "--json"}, nil)
	if err != nil {
		return nil, err
	}
	var res orcaChatsResult
	if err := decodeOrcaResult(raw, &res); err != nil {
		return nil, err
	}
	return res.Chats, nil
}

func (c *orcaClient) findHomeChat(ctx context.Context, botID string) (string, error) {
	chats, err := c.listChats(ctx, botID)
	if err != nil {
		return "", err
	}
	for _, ch := range chats {
		if ch.Home && !ch.Closed && strings.TrimSpace(ch.ID) != "" {
			return ch.ID, nil
		}
	}
	return "", nil
}

type orcaEntry struct {
	Kind  string `json:"kind"`
	Role  string `json:"role,omitempty"`
	Text  string `json:"text,omitempty"`
	Name  string `json:"name,omitempty"`
	State string `json:"state,omitempty"`
}

type orcaReadResult struct {
	OrcaSessionID string      `json:"orcaSessionId"`
	Kind          string      `json:"kind"`
	Agent         string      `json:"agent"`
	Source        string      `json:"source"`
	Entries       []orcaEntry `json:"entries"`
	Limited       bool        `json:"limited"`
	State         string      `json:"state"`
}

func (c *orcaClient) readSession(ctx context.Context, sessionID string, limit int) (*orcaReadResult, error) {
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > 100 {
		limit = 100
	}
	ref := sessionID
	if !strings.HasPrefix(ref, "orca_session_id:") {
		ref = "orca_session_id:" + ref
	}
	raw, err := c.runner.Run(ctx, []string{"agent", "read", ref, "--limit", strconv.Itoa(limit), "--json"}, nil)
	if err != nil {
		return nil, err
	}
	var res orcaReadResult
	if err := decodeOrcaResult(raw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

type sendMessageReq struct {
	BotID      string
	From       string
	Type       string
	Subject    string
	Body       string
	ChatID     string
	ItemNumber int
	Files      []string
}

func (c *orcaClient) sendMessage(ctx context.Context, req sendMessageReq) error {
	msgType := req.Type
	if msgType == "" {
		msgType = "question"
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = "飞书消息"
	}
	args := []string{
		"orchestration", "send",
		"--to", "bot:" + req.BotID,
		"--from", req.From,
		"--type", msgType,
		"--subject", subject,
	}
	if req.ItemNumber > 0 {
		args = append(args, "--item", strconv.Itoa(req.ItemNumber))
		// Orca CLI only accepts --file together with --to bot:<name> --item <n>.
		for _, f := range req.Files {
			if strings.TrimSpace(f) != "" {
				args = append(args, "--file", f)
			}
		}
	} else if strings.TrimSpace(req.ChatID) != "" {
		args = append(args, "--chat", strings.TrimSpace(req.ChatID))
	}
	args = append(args, "--body-file", "-")

	_, err := c.runner.Run(ctx, args, []byte(req.Body))
	return err
}

// orcaWorkListResult decodes `orca bot work list --bot <id> --json`.
type orcaWorkListResult struct {
	Items      []map[string]any `json:"items"`
	Workers    []map[string]any `json:"workers"`
	BotName    string           `json:"botName"`
	Workspaces []map[string]any `json:"workspaces"`
}

func (c *orcaClient) listWork(ctx context.Context, botID string) ([]orcaWorkerSnapshot, error) {
	raw, err := c.runner.Run(ctx, []string{"bot", "work", "list", "--bot", botID, "--json"}, nil)
	if err != nil {
		return nil, err
	}
	var res orcaWorkListResult
	if err := decodeOrcaResult(raw, &res); err != nil {
		return nil, err
	}
	rows := res.Items
	if len(rows) == 0 && len(res.Workers) > 0 {
		rows = res.Workers
	}
	out := make([]orcaWorkerSnapshot, 0, len(rows))
	for _, row := range rows {
		if snap, ok := parseWorkerRow(row); ok {
			out = append(out, snap)
		}
	}
	return out, nil
}

type orcaItemChat struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

type orcaItemReply struct {
	Label       string `json:"label"`
	Recommended bool   `json:"recommended"`
}

type orcaItemDecision struct {
	Question string          `json:"question"`
	Replies  []orcaItemReply `json:"replies"`
}

type orcaItemQuestion struct {
	EventID   string             `json:"eventId"`
	Gist      string             `json:"gist"`
	Text      string             `json:"text"`
	Decisions []orcaItemDecision `json:"decisions"`
}

type orcaItem struct {
	UID         string            `json:"uid"`
	Number      int               `json:"number"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Status      string            `json:"status"`
	Version     int               `json:"version"`
	LastEventID string            `json:"lastEventId"`
	Chat        *orcaItemChat     `json:"chat"`
	Question    *orcaItemQuestion `json:"question"`
}

type orcaItemListResult struct {
	Items []orcaItem `json:"items"`
}

type orcaItemShowResult struct {
	Item *orcaItem `json:"item"`
}

func (c *orcaClient) listNeedsYouItems(ctx context.Context, botID string) ([]orcaItem, error) {
	raw, err := c.runner.Run(ctx, []string{"bot", "item", "list", "--bot", botID, "--status", "needs-you", "--json"}, nil)
	if err != nil {
		return nil, err
	}
	var res orcaItemListResult
	if err := decodeOrcaResult(raw, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

func (c *orcaClient) showItem(ctx context.Context, number int) (*orcaItem, error) {
	raw, err := c.runner.Run(ctx, []string{"bot", "item", "show", strconv.Itoa(number), "--json"}, nil)
	if err != nil {
		return nil, err
	}
	var res orcaItemShowResult
	if err := decodeOrcaResult(raw, &res); err != nil {
		return nil, err
	}
	if res.Item == nil {
		return nil, fmt.Errorf("orca: item #%d not found in response", number)
	}
	return res.Item, nil
}
