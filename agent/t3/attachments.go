package t3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// T3 0.1.12 takes message attachments in two steps: attachments.createUploadUrl
// returns an id and a signed upload path, the bytes are POSTed to that path,
// and message.dispatch then lists the id. Images are limited to four formats
// and 10 MiB each (80 MiB per message); other files to 50 MiB.
const (
	maxImageBytes      = 10 << 20
	maxTotalImageBytes = 80 << 20
	maxFileBytes       = 50 << 20
	uploadTimeout      = 2 * time.Minute
)

var imageExts = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// prepareAttachments uploads the images and files to T3 and returns the
// attachment list for message.dispatch plus the prompt, extended with one line
// for every attachment T3 could not take so the model knows it is missing.
func (s *Session) prepareAttachments(ctx context.Context, prompt string, images []core.ImageAttachment, files []core.FileAttachment) (string, []any) {
	atts := []any{}
	var notes []string
	imageTotal := 0
	for i, img := range images {
		name, mt := imageName(img, i)
		switch {
		case len(img.Data) == 0:
			continue
		case imageExts[mt] == "":
			notes = append(notes, fmt.Sprintf("图片 %s 的格式不受支持，没有传入", name))
		case len(img.Data) > maxImageBytes || imageTotal+len(img.Data) > maxTotalImageBytes:
			notes = append(notes, fmt.Sprintf("图片 %s 超过大小限制，没有传入", name))
		default:
			a, err := s.upload(ctx, "image", name, mt, img.Data)
			if err != nil {
				slog.Warn("t3: upload image", "error", err)
				notes = append(notes, fmt.Sprintf("图片 %s 上传失败，没有传入", name))
				continue
			}
			imageTotal += len(img.Data)
			atts = append(atts, a)
		}
	}
	for i, f := range files {
		name := strings.TrimSpace(filepath.Base(f.FileName))
		if name == "" || name == "." || name == string(filepath.Separator) {
			name = fmt.Sprintf("file-%d", i+1)
		}
		mt := fileMime(f)
		switch {
		case len(f.Data) == 0:
			continue
		case len(f.Data) > maxFileBytes:
			notes = append(notes, fmt.Sprintf("文件 %s 超过大小限制，没有传入", name))
		default:
			a, err := s.upload(ctx, "file", name, mt, f.Data)
			if err != nil {
				slog.Warn("t3: upload file", "error", err)
				notes = append(notes, fmt.Sprintf("文件 %s 上传失败，没有传入", name))
				continue
			}
			atts = append(atts, a)
		}
	}
	if len(notes) > 0 {
		if strings.TrimSpace(prompt) != "" {
			prompt += "\n\n"
		}
		prompt += "（" + strings.Join(notes, "；") + "）"
	}
	return prompt, atts
}

func imageName(img core.ImageAttachment, i int) (name, mt string) {
	mt = strings.ToLower(strings.TrimSpace(strings.SplitN(http.DetectContentType(img.Data), ";", 2)[0]))
	base := strings.TrimSpace(filepath.Base(img.FileName))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = fmt.Sprintf("image-%d", i+1)
	}
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" {
		base = fmt.Sprintf("image-%d", i+1)
	}
	return base + imageExts[mt], mt
}

func fileMime(f core.FileAttachment) string {
	mt, _, err := mime.ParseMediaType(f.MimeType)
	if err != nil || mt == "" {
		mt, _, _ = mime.ParseMediaType(http.DetectContentType(f.Data))
	}
	if mt == "" {
		mt = "application/octet-stream"
	}
	return mt
}

// upload stores one attachment in T3 and returns its message.dispatch entry.
func (s *Session) upload(ctx context.Context, kind, name, mimeType string, data []byte) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	raw, err := s.call(ctx, "attachments.createUploadUrl", map[string]any{"type": kind, "name": name, "mimeType": mimeType, "sizeBytes": len(data)})
	if err != nil {
		return nil, err
	}
	var up struct {
		AttachmentID string `json:"attachmentId"`
		RelativeURL  string `json:"relativeUrl"`
	}
	if json.Unmarshal(raw, &up) != nil || up.AttachmentID == "" || up.RelativeURL == "" {
		return nil, fmt.Errorf("t3: createUploadUrl returned no upload url")
	}
	base := s.agent.server
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("t3: parse server: %w", err)
	}
	target := strings.TrimRight(u.String(), "/") + up.RelativeURL
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mimeType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("t3: upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("t3: upload: %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	return map[string]any{"type": kind, "id": up.AttachmentID, "name": name, "mimeType": mimeType, "sizeBytes": len(data)}, nil
}
