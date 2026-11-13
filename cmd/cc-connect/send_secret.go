package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Secret commands:
//
//	cc-connect send --secret <name> [--hint <one line>] [--url <https page>]
//	cc-connect send --secret-list
//	cc-connect send --secret-delete <name>
//
// The card is sent through the running bridge; list and delete work on
// ~/.secrets directly and never print a value.

func runSendSecret(args []string) {
	req, dataDir, err := parseSecretArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	payload, _ := json.Marshal(req)
	body, err := postToBridge(dataDir, "/secret", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(secretSentLine(req.Name, body))
}

func parseSecretArgs(args []string) (core.SecretRequest, string, error) {
	var req core.SecretRequest
	var dataDir string
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "--secret", "--hint", "--url", "--project", "-p", "--session", "-s", "--data-dir":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("%s requires a value", flag)
			}
			i++
			v := args[i]
			switch flag {
			case "--secret":
				req.Name = strings.TrimSpace(v)
			case "--hint":
				req.Hint = v
			case "--url":
				req.URL = strings.TrimSpace(v)
			case "--project", "-p":
				req.Project = v
			case "--session", "-s":
				req.SessionKey = v
			case "--data-dir":
				dataDir = v
			}
		default:
			return req, "", fmt.Errorf("unexpected argument %q (put the hint in quotes: --hint \"...\")", flag)
		}
	}
	if !core.ValidSecretName(req.Name) {
		return req, "", fmt.Errorf("invalid secret name %q: use 2 to 40 lowercase letters, digits and hyphens, e.g. hubspot-token", req.Name)
	}
	if req.Project == "" {
		req.Project = strings.TrimSpace(os.Getenv("CC_PROJECT"))
	}
	if req.SessionKey == "" {
		req.SessionKey = strings.TrimSpace(os.Getenv("CC_SESSION_KEY"))
	}
	return req, dataDir, nil
}

func secretSentLine(name string, body []byte) string {
	var r struct {
		Replaces bool `json:"replaces"`
	}
	_ = json.Unmarshal(body, &r)
	line := "Secret card sent: " + name + ". You get a message once it is saved; until then, do not ask for the value in chat."
	if r.Replaces {
		line += " A secret with this name is already saved; saving replaces it."
	}
	return line
}

func runSecretList(args []string) {
	store := core.NewSecretStore(core.DefaultSecretDir())
	list, err := store.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(secretListText(list))
}

func secretListText(list []core.SecretInfo) string {
	if len(list) == 0 {
		return "No saved secrets.\n"
	}
	var b strings.Builder
	for _, s := range list {
		fmt.Fprintf(&b, "%s\tsaved %s\n", s.Name, s.SavedAt.Local().Format("2006-01-02 15:04 MST"))
	}
	return b.String()
}

func runSecretDelete(args []string) {
	name := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--secret-delete" && i+1 < len(args) {
			name = strings.TrimSpace(args[i+1])
			i++
		}
	}
	if err := deleteSecret(core.NewSecretStore(core.DefaultSecretDir()), name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Secret deleted: " + name)
}

func deleteSecret(store *core.SecretStore, name string) error {
	if name == "" {
		return errors.New("--secret-delete requires a name")
	}
	if err := store.Delete(name); err != nil {
		if errors.Is(err, core.ErrSecretNotFound) {
			return fmt.Errorf("no saved secret named %q (see --secret-list)", name)
		}
		return err
	}
	return nil
}

// postToBridge posts a JSON body to the running bridge's local API.
func postToBridge(dataDir, endpoint string, payload []byte) ([]byte, error) {
	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("cc-connect is not running (socket not found: %s)", sockPath)
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}}
	resp, err := client.Post("http://unix"+endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(strings.TrimSpace(string(body)))
	}
	return body, nil
}
