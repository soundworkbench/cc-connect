package core

import (
	"strings"
	"unicode/utf8"
)

func substantiveProgress(text string, cfg ProgressMergeCfg) bool {
	if utf8.RuneCountInString(text) >= cfg.MinChars {
		return true
	}
	if cfg.Tables {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "|---") || strings.HasPrefix(strings.TrimSpace(line), "| ---") {
				return true
			}
		}
	}
	return false
}

// Caller owns sp.mu; only original public segments are eligible, never thinking
// or tool arguments. Card folding must not discard these delivery candidates.
func (sp *streamPreview) mergeProgressLocked(body string) string {
	if !sp.mergeCfg.Enabled {
		return body
	}
	var missing []string
	for _, text := range sp.substantiveProgress {
		if strings.Contains(body, text) || strings.Contains(strings.Join(missing, "\n\n"), text) {
			continue
		}
		missing = append(missing, text)
	}
	if len(missing) == 0 {
		return body
	}
	return sp.progressI18n.T(MsgIntermediateContent) + "\n\n" + strings.Join(missing, "\n\n") + "\n\n" + body
}
