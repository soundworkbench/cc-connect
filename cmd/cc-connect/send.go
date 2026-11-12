package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func runSend(args []string) {
	for _, a := range args {
		switch a {
		case "--decision", "--form":
			runSendCardFile(args, a)
			return
		case "--decision-void":
			runDecisionVoid(args)
			return
		}
	}
	req, dataDir, err := parseSendArgs(args)
	if err != nil {
		if errors.Is(err, errSendUsage) {
			printSendUsage()
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printSendUsage()
		os.Exit(1)
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}

	payload, err := buildSendPayload(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to encode send payload: %v\n", err)
		os.Exit(1)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := client.Post("http://unix/send", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	fmt.Println("Message sent successfully.")
}

// runSendCardFile posts a decision or form card described by a JSON file:
//
//	cc-connect send --decision card.json
//	cc-connect send --form form.json
//
// The call returns as soon as the card is sent; the answer arrives later as a
// normal message in the same session.
func runSendCardFile(args []string, flag string) {
	endpoint, done := "/decision", "Decision card sent"
	if flag == "--form" {
		endpoint, done = "/form", "Form card sent"
	}
	var file, project, sessionKey, dataDir string
	for i := 0; i < len(args); i++ {
		next := func() string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				os.Exit(1)
			}
			i++
			return args[i]
		}
		switch args[i] {
		case flag:
			file = next()
		case "--project", "-p":
			project = next()
		case "--session", "-s":
			sessionKey = next()
		case "--data-dir":
			dataDir = next()
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	payload, err := cardFilePayload(raw, project, sessionKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid %s JSON: %v\n", strings.TrimPrefix(flag, "--"), err)
		os.Exit(1)
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}}
	resp, err := client.Post("http://unix"+endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	fmt.Println(cardSentLine(done, body))
}

// cardSentLine is the confirmation printed after a decision or form card is
// sent, with the card's ID (used by --decision-void).
func cardSentLine(done string, body []byte) string {
	var r struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &r) == nil && r.ID != "" {
		return done + ": " + r.ID
	}
	return done + "."
}

// runDecisionVoid withdraws a pending decision or form card:
//
//	cc-connect send --decision-void d1a2b3c4
func runDecisionVoid(args []string) {
	var id, dataDir string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--decision-void", "--data-dir":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "Error: %s requires a value\n", args[i])
				os.Exit(1)
			}
			if args[i] == "--data-dir" {
				dataDir = args[i+1]
			} else {
				id = args[i+1]
			}
			i++
		}
	}
	payload, _ := json.Marshal(core.DecisionVoidRequest{ID: strings.TrimSpace(id)})
	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}}
	resp, err := client.Post("http://unix/decision/void", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	line, warn := decisionVoidLines(id, body)
	fmt.Println(line)
	if warn != "" {
		fmt.Fprintln(os.Stderr, warn)
	}
}

// decisionVoidLines turns the /decision/void reply into the confirmation and
// an optional warning that the card itself could not be rewritten.
func decisionVoidLines(id string, body []byte) (line, warn string) {
	var r struct {
		Card string `json:"card"`
	}
	_ = json.Unmarshal(body, &r)
	line = "Decision card voided: " + id
	if r.Card != "" && r.Card != "updated" {
		warn = "Warning: voided, but the card was " + r.Card
	}
	return line, warn
}

// cardFilePayload fills project and session_key into a card JSON object:
// the flag wins, then the file's own value, then CC_PROJECT / CC_SESSION_KEY.
func cardFilePayload(raw []byte, project, sessionKey string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("expected a JSON object")
	}
	set := func(key, flagValue, env string) {
		if flagValue != "" {
			obj[key] = flagValue
		} else if v, _ := obj[key].(string); v == "" {
			if e := strings.TrimSpace(os.Getenv(env)); e != "" {
				obj[key] = e
			}
		}
	}
	set("project", project, "CC_PROJECT")
	set("session_key", sessionKey, "CC_SESSION_KEY")
	return json.Marshal(obj)
}

var errSendUsage = errors.New("show send usage")

func parseSendArgs(args []string) (core.SendRequest, string, error) {
	var req core.SendRequest
	var dataDir string
	var useStdin bool
	var imagePaths []string
	var filePaths []string
	var audioPaths []string
	var videoPaths []string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--project requires a value")
			}
			i++
			req.Project = args[i]
		case "--session", "-s":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--session requires a value")
			}
			i++
			req.SessionKey = args[i]
		case "--message", "-m":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--message requires a value")
			}
			i++
			req.Message = args[i]
		case "--cwd", "--work-dir":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("%s requires a value", args[i])
			}
			i++
			req.WorkDir = args[i]
		case "--tts":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("%s requires a value", args[i])
			}
			i++
			req.TTSText = args[i]
		case "--react":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("%s requires an emoji type", args[i])
			}
			i++
			req.React = strings.TrimSpace(args[i])
		case "--image":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--image requires a path")
			}
			i++
			imagePaths = append(imagePaths, args[i])
		case "--file":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--file requires a path")
			}
			i++
			filePaths = append(filePaths, args[i])
		case "--audio":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--audio requires a path")
			}
			i++
			audioPaths = append(audioPaths, args[i])
		case "--video":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--video requires a path")
			}
			i++
			videoPaths = append(videoPaths, args[i])
		case "--stdin":
			useStdin = true
		case "--at-users":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--at-users requires a value")
			}
			i++
			for _, uid := range strings.Split(args[i], ",") {
				uid = strings.TrimSpace(uid)
				if uid != "" {
					req.AtUsers = append(req.AtUsers, uid)
				}
			}
		case "--at-all":
			req.AtAll = true
		case "--data-dir":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--data-dir requires a value")
			}
			i++
			dataDir = args[i]
		case "--help", "-h":
			return req, "", errSendUsage
		default:
			positional = append(positional, args[i])
		}
	}

	if useStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return req, "", fmt.Errorf("reading stdin: %w", err)
		}
		req.Message = strings.TrimSpace(string(data))
	}
	if req.Project == "" {
		req.Project = strings.TrimSpace(os.Getenv("CC_PROJECT"))
	}
	if req.SessionKey == "" {
		req.SessionKey = strings.TrimSpace(os.Getenv("CC_SESSION_KEY"))
	}
	if req.Message == "" {
		req.Message = strings.Join(positional, " ")
	}

	maxAtt := resolveMaxAttachmentSize(loadSendConfigBestEffort())

	images, err := loadImageAttachments(imagePaths, maxAtt)
	if err != nil {
		return req, "", err
	}
	files, err := loadFileAttachments(filePaths, maxAtt)
	if err != nil {
		return req, "", err
	}
	audioFiles, err := loadTypedFileAttachments(audioPaths, "audio", maxAtt)
	if err != nil {
		return req, "", err
	}
	videoFiles, err := loadTypedFileAttachments(videoPaths, "video", maxAtt)
	if err != nil {
		return req, "", err
	}
	req.Images = images
	// Keep audio / video clips on dedicated fields. Routing them through
	// req.Files would force the engine to dispatch them via FileSender —
	// that loses the native voice-bubble / video-bubble path on platforms
	// that implement AudioSender / VideoSender (e.g. Feishu's ffmpeg
	// transcode for mp3 → opus). See cc-connect internal task
	// t-20260615-cqjbk1.
	req.Files = files
	req.Audios = audioFiles
	req.Videos = videoFiles

	if req.Message == "" && req.TTSText == "" && req.React == "" && len(req.Images) == 0 && len(req.Files) == 0 && len(req.Audios) == 0 && len(req.Videos) == 0 {
		return req, "", fmt.Errorf("message, tts text, react, or attachment is required")
	}

	return req, dataDir, nil
}

func loadImageAttachments(paths []string, maxSize int64) ([]core.ImageAttachment, error) {
	images := make([]core.ImageAttachment, 0, len(paths))
	for _, path := range paths {
		data, fileName, mimeType, err := readAttachment(path, maxSize)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("%s is not an image (detected mime: %s)", path, mimeType)
		}
		images = append(images, core.ImageAttachment{MimeType: mimeType, Data: data, FileName: fileName})
	}
	return images, nil
}

func loadFileAttachments(paths []string, maxSize int64) ([]core.FileAttachment, error) {
	files := make([]core.FileAttachment, 0, len(paths))
	for _, path := range paths {
		data, fileName, mimeType, err := readAttachment(path, maxSize)
		if err != nil {
			return nil, err
		}
		files = append(files, core.FileAttachment{MimeType: mimeType, Data: data, FileName: fileName})
	}
	return files, nil
}

func loadTypedFileAttachments(paths []string, mediaType string, maxSize int64) ([]core.FileAttachment, error) {
	files := make([]core.FileAttachment, 0, len(paths))
	for _, path := range paths {
		data, fileName, mimeType, err := readAttachment(path, maxSize)
		if err != nil {
			return nil, err
		}
		if !attachmentMatchesMediaType(mimeType, fileName, mediaType) {
			return nil, fmt.Errorf("%s is not %s media (detected mime: %s)", path, mediaType, mimeType)
		}
		files = append(files, core.FileAttachment{MimeType: mimeType, Data: data, FileName: fileName})
	}
	return files, nil
}

func attachmentMatchesMediaType(mimeType, fileName, mediaType string) bool {
	ext := strings.ToLower(filepath.Ext(fileName))
	switch mediaType {
	case "audio":
		if strings.HasPrefix(mimeType, "audio/") {
			return true
		}
		switch ext {
		case ".aac", ".flac", ".m4a", ".mp3", ".oga", ".ogg", ".opus", ".wav":
			return true
		}
	case "video":
		if strings.HasPrefix(mimeType, "video/") {
			return true
		}
		switch ext {
		case ".avi", ".m4v", ".mkv", ".mov", ".mp4", ".webm":
			return true
		}
	}
	return false
}

// loadSendConfigBestEffort loads config.toml — resolved the same way the
// daemon resolves it — so the standalone `cc-connect send` subcommand (a
// separate process that otherwise has no config) can honour
// max_attachment_size_mb. Errors are ignored and nil is returned, so a missing
// or invalid config never breaks sending; the caller then falls back to the
// env var / default via resolveMaxAttachmentSize.
func loadSendConfigBestEffort() *config.Config {
	cfg, err := config.Load(resolveConfigPath(""))
	if err != nil {
		return nil
	}
	return cfg
}

// readAttachment reads a single attachment file, rejecting anything larger than
// maxSize bytes (resolved by the caller from config/env/default). The limit is
// enforced before the file is read into memory.
func readAttachment(path string, maxSize int64) ([]byte, string, string, error) {
	cleaned := filepath.Clean(path)

	info, err := os.Stat(cleaned)
	if err != nil {
		return nil, "", "", fmt.Errorf("read attachment %s: %w", path, err)
	}
	if info.Size() > maxSize {
		return nil, "", "", fmt.Errorf("attachment %s exceeds size limit (%d MB)", path, maxSize>>20)
	}

	data, err := os.ReadFile(cleaned)
	if err != nil {
		return nil, "", "", fmt.Errorf("read attachment %s: %w", path, err)
	}
	fileName := filepath.Base(cleaned)
	return data, fileName, detectAttachmentMimeType(fileName, data), nil
}

func detectAttachmentMimeType(fileName string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(fileName))
	switch ext {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	}
	if byExt := mime.TypeByExtension(ext); byExt != "" {
		return byExt
	}
	if len(data) == 0 {
		return "application/octet-stream"
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	return http.DetectContentType(sniff)
}

func buildSendPayload(req core.SendRequest) ([]byte, error) {
	return json.Marshal(req)
}

func decodeSendPayload(data []byte, req *core.SendRequest) error {
	return json.Unmarshal(data, req)
}

func resolveSocketPath(dataDir string) string {
	if dataDir != "" {
		return filepath.Join(dataDir, "run", "api.sock")
	}
	// Check CC_DATA_DIR env var for custom data_dir configuration
	if envDataDir := strings.TrimSpace(os.Getenv("CC_DATA_DIR")); envDataDir != "" {
		return filepath.Join(envDataDir, "run", "api.sock")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cc-connect", "run", "api.sock")
	}
	return filepath.Join(".cc-connect", "run", "api.sock")
}

func printSendUsage() {
	fmt.Println(`Usage: cc-connect send [options] <message>
       cc-connect send [options] -m <message>
       cc-connect send [options] --stdin < file
       cc-connect send [options] --image <path>
       cc-connect send [options] --file <path>
       cc-connect send [options] --audio <path>
       cc-connect send [options] --video <path>
       cc-connect send [options] --tts <text>
       cc-connect send [options] --react <emoji>
       cc-connect send [options] --decision <card.json>
       cc-connect send [options] --form <form.json>
       cc-connect send --decision-void <id>
       echo "msg" | cc-connect send [options] --stdin

Send a message, attachment, or synthesized voice message to an active cc-connect session.

Options:
  -m, --message <text>     Message to send (preferred over positional args)
      --cwd <path>         Start a new session in this working directory
      --work-dir <path>    Alias for --cwd
      --tts <text>         Synthesize text and send it as a voice/audio message
      --react <emoji>      Add an emoji reaction to the message that started this turn (Feishu emoji type, e.g. OK, THUMBSUP, DONE)
      --decision <file>    Send a decision card (JSON: question, options[recommended first], note, no_default, deadline_minutes, danger, multi); the answer comes back as a message
      --decision-void <id> Void a pending decision or form card (the ID printed when it was sent): the card greys out, its default is cancelled, later taps do nothing
      --form <file>        Send a form card (JSON: title, note, submit, fields[label, type input|date|select|multi_select, options, placeholder, required]); the values come back as one message
      --image <path>       Send an image attachment (repeatable)
      --file <path>        Send a file attachment (repeatable)
      --audio <path>       Send an audio attachment (repeatable)
      --video <path>       Send a video attachment (repeatable)
      --stdin              Read message from stdin (best for long/special-char messages)
      --at-users <ids>     @ user IDs, comma-separated (DingTalk)
      --at-all             @ everyone (DingTalk)
  -p, --project <name>     Target project (optional if only one project)
  -s, --session <key>      Target session key (optional, picks first active)
      --data-dir <path>    Data directory (default: ~/.cc-connect)
  -h, --help               Show this help

Examples:
  cc-connect send "Daily summary: ..."
  cc-connect send -m "Build completed successfully"
  cc-connect send --message "Chart generated" --image /tmp/chart.png
  cc-connect send --file /tmp/report.pdf
  cc-connect send --video /tmp/demo.mp4
  cc-connect send --audio /tmp/voice.opus
  cc-connect send --tts "Hello from cc-connect"
  cc-connect send --react OK
  cc-connect send --stdin <<'EOF'
    Long message with "special" chars, $variables, and newlines
  EOF`)
}
