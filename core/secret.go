package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Saved secrets: one file per secret in ~/.secrets (directory 0700, files
// 0600). The person fills a secret card; the assistant reads the file in its
// commands with $(cat ~/.secrets/<name>). Values never go into the
// conversation, cards or logs, and every text the bridge sends is masked
// with SecretStore.Redact before it leaves.

const (
	// SecretRedactMinLen is the shortest value that is masked in outgoing text;
	// shorter ones would hit ordinary words.
	SecretRedactMinLen = 8
	// secretCardMaxLen is what the card's password box takes (Feishu caps an
	// input at 1000 characters, on one line).
	secretCardMaxLen = 1000
	// secretFileMaxBytes bounds a file read for masking; larger files are not
	// values anyone pastes into a reply.
	secretFileMaxBytes = 64 << 10
	// A trailing partial value shorter than this is left alone in a growing
	// preview.
	secretTailMinLen = 6

	secretCheckInterval  = time.Second
	secretReloadInterval = 30 * time.Second
)

var (
	secretNameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])$`)
	// Files the assistant puts in the directory itself (an SSH key, say) may
	// have other names; any of these is shown in the placeholder.
	secretFileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

	ErrSecretNotFound = errors.New("no saved secret with that name")
)

// ValidSecretName reports whether name may be used for a saved secret:
// 2 to 40 lowercase letters, digits and hyphens, not starting or ending with
// a hyphen.
func ValidSecretName(name string) bool { return secretNameRe.MatchString(name) }

func secretNameError(name string) error {
	return fmt.Errorf("invalid secret name %q: use 2 to 40 lowercase letters, digits and hyphens, e.g. hubspot-token", name)
}

// SecretPlaceholder is what replaces a saved value in outgoing text.
func SecretPlaceholder(name string) string { return "‹密钥:" + name + "›" }

// DefaultSecretDir is ~/.secrets of the user the bridge runs as; the
// assistant runs as the same user, so ~/.secrets is the same directory for it.
func DefaultSecretDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".secrets")
}

// SecretInfo is one saved secret as listed: never its value.
type SecretInfo struct {
	Name    string    `json:"name"`
	SavedAt time.Time `json:"saved_at"`
}

type secretEntry struct {
	placeholder string
	value       string
	variants    []string // value as it appears inside JSON, once and twice encoded
}

// SecretStore saves secrets and masks their values. The masking list is
// cached: the directory is looked at no more than once a second and the
// files are read again only when it changed, or every 30 seconds to catch a
// file rewritten in place.
type SecretStore struct {
	dir string

	mu        sync.Mutex
	checkedAt time.Time
	loadedAt  time.Time
	dirMod    time.Time
	entries   []secretEntry
}

// NewSecretStore returns a store for dir. A nil store (empty dir) saves
// nothing and masks nothing.
func NewSecretStore(dir string) *SecretStore {
	if dir == "" {
		return nil
	}
	return &SecretStore{dir: dir}
}

// Dir is the directory the secrets are kept in.
func (s *SecretStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *SecretStore) path(name string) string { return filepath.Join(s.dir, name) }

// Exists reports whether a secret with this name is saved.
func (s *SecretStore) Exists(name string) bool {
	if s == nil || !ValidSecretName(name) {
		return false
	}
	st, err := os.Stat(s.path(name))
	return err == nil && st.Mode().IsRegular()
}

// Save writes the value atomically and reports whether it replaced an
// existing secret. The directory is kept at 0700 and the file at 0600.
func (s *SecretStore) Save(name, value string) (updated bool, err error) {
	if s == nil {
		return false, errors.New("no secret directory")
	}
	if !ValidSecretName(name) {
		return false, secretNameError(name)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return false, fmt.Errorf("secret: create directory: %w", err)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return false, fmt.Errorf("secret: protect directory: %w", err)
	}
	updated = s.Exists(name)
	// The temporary file sits in the protected directory, so the value never
	// touches a shared temp directory, and the rename stays on one filesystem.
	tmp, err := os.CreateTemp(s.dir, ".tmp-"+name+"-*")
	if err != nil {
		return false, fmt.Errorf("secret: create file: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, err error) (bool, error) {
		tmp.Close()
		os.Remove(tmpPath)
		return false, fmt.Errorf("secret: %s: %w", step, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail("protect file", err)
	}
	if _, err := tmp.WriteString(value); err != nil {
		return fail("write file", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync file", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close file", err)
	}
	if err := os.Rename(tmpPath, s.path(name)); err != nil {
		os.Remove(tmpPath)
		return false, fmt.Errorf("secret: save file: %w", err)
	}
	s.invalidate()
	return updated, nil
}

// Delete removes a saved secret.
func (s *SecretStore) Delete(name string) error {
	if s == nil {
		return ErrSecretNotFound
	}
	if !ValidSecretName(name) {
		return secretNameError(name)
	}
	if err := os.Remove(s.path(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrSecretNotFound
		}
		return fmt.Errorf("secret: delete: %w", err)
	}
	s.invalidate()
	return nil
}

// List returns the saved secrets by name with the time each was saved.
func (s *SecretStore) List() ([]SecretInfo, error) {
	if s == nil {
		return nil, nil
	}
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("secret: list: %w", err)
	}
	var out []SecretInfo
	for _, ent := range ents {
		if !ent.Type().IsRegular() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		out = append(out, SecretInfo{Name: ent.Name(), SavedAt: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *SecretStore) invalidate() {
	s.mu.Lock()
	s.checkedAt, s.dirMod = time.Time{}, time.Time{}
	s.mu.Unlock()
}

// current returns the masking list, reloading it when the directory changed.
func (s *SecretStore) current() []secretEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !s.checkedAt.IsZero() && now.Sub(s.checkedAt) < secretCheckInterval {
		return s.entries
	}
	s.checkedAt = now
	st, err := os.Stat(s.dir)
	if err != nil {
		s.entries, s.dirMod = nil, time.Time{}
		return nil
	}
	if st.ModTime().Equal(s.dirMod) && now.Sub(s.loadedAt) < secretReloadInterval {
		return s.entries
	}
	s.dirMod, s.loadedAt = st.ModTime(), now
	s.entries = loadSecretEntries(s.dir)
	return s.entries
}

func loadSecretEntries(dir string) []secretEntry {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []secretEntry
	for _, ent := range ents {
		name := ent.Name()
		if !ent.Type().IsRegular() || strings.HasPrefix(name, ".") {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.Size() > secretFileMaxBytes {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(data))
		if len(value) < SecretRedactMinLen {
			continue
		}
		label := name
		if !secretFileRe.MatchString(name) {
			label = "?"
		}
		out = append(out, secretEntry{placeholder: SecretPlaceholder(label), value: value, variants: jsonVariants(value)})
	}
	// Longer values first, so a value containing another is masked whole.
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].value) > len(out[j].value) })
	return out
}

// jsonVariants lists how a value appears in a JSON request body: as is,
// encoded once (a text field) and twice (a card or message content that is
// itself a JSON string), with and without HTML escaping.
func jsonVariants(v string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(v)
	for _, html := range []bool{true, false} {
		once := jsonEscape(v, html)
		add(once)
		add(jsonEscape(once, html))
	}
	return out
}

func jsonEscape(s string, html bool) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(html)
	_ = enc.Encode(s)
	out := strings.TrimSuffix(b.String(), "\n")
	return out[1 : len(out)-1]
}

// HasSecrets reports whether any value is masked.
func (s *SecretStore) HasSecrets() bool { return len(s.current()) > 0 }

// Redact replaces every saved value in text with its placeholder.
func (s *SecretStore) Redact(text string) string {
	for _, e := range s.current() {
		if strings.Contains(text, e.value) {
			text = strings.ReplaceAll(text, e.value, e.placeholder)
		}
	}
	return text
}

// RedactJSON masks saved values in a JSON request or response body, at any
// encoding depth. It reports whether anything was replaced.
func (s *SecretStore) RedactJSON(body []byte) ([]byte, bool) {
	entries := s.current()
	if len(entries) == 0 {
		return body, false
	}
	text := string(body)
	changed := false
	for _, e := range entries {
		for _, v := range e.variants {
			if strings.Contains(text, v) {
				text = strings.ReplaceAll(text, v, e.placeholder)
				changed = true
			}
		}
	}
	if !changed {
		return body, false
	}
	return []byte(text), true
}

// MaskStreaming is Redact for a preview that grows as the reply is written:
// it also hides the start of a value at the very end of the text, so a value
// being typed out never shows piece by piece.
func (s *SecretStore) MaskStreaming(text string) string {
	entries := s.current()
	if len(entries) == 0 {
		return text
	}
	text = s.Redact(text)
	body, ellipsis := strings.CutSuffix(text, "…") // the preview's length cut
	for _, e := range entries {
		for k := len(e.value) - 1; k >= secretTailMinLen; k-- {
			if strings.HasSuffix(body, e.value[:k]) {
				body = body[:len(body)-k] + e.placeholder
				break
			}
		}
	}
	if ellipsis {
		body += "…"
	}
	return body
}
