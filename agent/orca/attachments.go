package orca

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	maxAttachmentBytes = 50 << 20 // Orca per-file attachment cap (50 MiB)
	maxImageCount      = 20       // Orca per-message image cap
	maxFileCount       = 10       // Orca per-message non-image file cap
)

type savedAttachments struct {
	ImagePaths []string
	FilePaths  []string
}

func (a savedAttachments) AllPaths() []string {
	if len(a.ImagePaths) == 0 && len(a.FilePaths) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.ImagePaths)+len(a.FilePaths))
	out = append(out, a.ImagePaths...)
	out = append(out, a.FilePaths...)
	return out
}

func (a savedAttachments) Empty() bool {
	return len(a.ImagePaths) == 0 && len(a.FilePaths) == 0
}

// appendRefsToBody formats local attachment paths into the message body for
// normal (non-item) Orca bot messages, since `orca orchestration send --file`
// is only accepted together with `--item <n>`.
func (a savedAttachments) appendRefsToBody(body string) string {
	if a.Empty() {
		return body
	}
	var lines []string
	lines = append(lines, "[飞书附件，已保存至本机，请直接读取查看]")
	for _, p := range a.ImagePaths {
		lines = append(lines, "- 图片："+p)
	}
	for _, p := range a.FilePaths {
		lines = append(lines, "- 文件："+p)
	}
	block := strings.Join(lines, "\n")
	if strings.TrimSpace(body) == "" {
		return block
	}
	return body + "\n\n" + block
}

// prepareAttachments persists incoming Feishu images and files to
// <dataDir>/orca-attachments/<msgDir>/ (mode 0700/0600, readable by the same
// host user running Orca). Any attachment that exceeds Orca limits or fails
// to write is noted in the returned prompt so the bot knows.
func prepareAttachments(dataDir, messageID, prompt string, images []core.ImageAttachment, files []core.FileAttachment) (string, savedAttachments) {
	if len(images) == 0 && len(files) == 0 {
		return prompt, savedAttachments{}
	}
	if strings.TrimSpace(dataDir) == "" {
		dataDir = ".cc-connect"
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		absDataDir = dataDir
	}
	scope := sanitizeBaseName(messageID)
	if scope == "" {
		scope = fmt.Sprintf("msg-%d-%s", time.Now().UnixNano(), shortRandHex(4))
	}
	dir := filepath.Join(absDataDir, "orca-attachments", scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("orca: create attachments dir failed", "dir", dir, "error", err)
		return appendAttachmentNotes(prompt, []string{"附件目录创建失败，附件未能传入"}), savedAttachments{}
	}

	var saved savedAttachments
	var notes []string

	for i, img := range images {
		if len(img.Data) == 0 {
			continue
		}
		name := buildImageFileName(img, i)
		if len(saved.ImagePaths) >= maxImageCount {
			notes = append(notes, fmt.Sprintf("图片 %s 超过单次数量上限（%d 张），未传入", name, maxImageCount))
			continue
		}
		if len(img.Data) > maxAttachmentBytes {
			notes = append(notes, fmt.Sprintf("图片 %s 超过 50 MB 限制，未传入", name))
			continue
		}
		p, err := writeUniqueFile(dir, name, img.Data)
		if err != nil {
			slog.Warn("orca: write image attachment failed", "name", name, "error", err)
			notes = append(notes, fmt.Sprintf("图片 %s 保存失败，未传入", name))
			continue
		}
		saved.ImagePaths = append(saved.ImagePaths, p)
	}

	for i, f := range files {
		if len(f.Data) == 0 {
			continue
		}
		name := sanitizeBaseName(f.FileName)
		if name == "" {
			name = fmt.Sprintf("file-%d", i+1)
		}
		if len(saved.FilePaths) >= maxFileCount {
			notes = append(notes, fmt.Sprintf("文件 %s 超过单次数量上限（%d 个），未传入", name, maxFileCount))
			continue
		}
		if len(f.Data) > maxAttachmentBytes {
			notes = append(notes, fmt.Sprintf("文件 %s 超过 50 MB 限制，未传入", name))
			continue
		}
		p, err := writeUniqueFile(dir, name, f.Data)
		if err != nil {
			slog.Warn("orca: write file attachment failed", "name", name, "error", err)
			notes = append(notes, fmt.Sprintf("文件 %s 保存失败，未传入", name))
			continue
		}
		saved.FilePaths = append(saved.FilePaths, p)
	}

	return appendAttachmentNotes(prompt, notes), saved
}

func appendAttachmentNotes(prompt string, notes []string) string {
	if len(notes) == 0 {
		return prompt
	}
	if strings.TrimSpace(prompt) != "" {
		prompt += "\n\n"
	}
	return prompt + "（" + strings.Join(notes, "；") + "）"
}

func buildImageFileName(img core.ImageAttachment, idx int) string {
	mimeType := strings.ToLower(strings.TrimSpace(strings.SplitN(img.MimeType, ";", 2)[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = strings.ToLower(strings.TrimSpace(strings.SplitN(http.DetectContentType(img.Data), ";", 2)[0]))
	}
	ext := core.ExtFromMime(mimeType)
	base := sanitizeBaseName(img.FileName)
	if base == "" {
		return fmt.Sprintf("image-%d%s", idx+1, ext)
	}
	if existingExt := filepath.Ext(base); existingExt == "" {
		return base + ext
	}
	return base
}

func sanitizeBaseName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '\x00') {
		return ""
	}
	return name
}

func writeUniqueFile(dir, name string, data []byte) (string, error) {
	for i := 0; ; i++ {
		candidate := name
		if i > 0 {
			ext := filepath.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			candidate = fmt.Sprintf("%s_%d%s", stem, i, ext)
		}
		fullPath := filepath.Join(dir, candidate)
		f, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			_ = os.Remove(fullPath)
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(fullPath)
			return "", err
		}
		return fullPath, nil
	}
}

func shortRandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
