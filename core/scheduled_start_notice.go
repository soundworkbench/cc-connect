package core

import "strings"

var scheduledStartNoticeExemptPrefixes = []string{"✅", "❌", "💬"}

var scheduledStartNoticeSuppressedPrefixes = []string{"💬"}

func scheduledStartNotice(desc string) string {
	for _, prefix := range scheduledStartNoticeExemptPrefixes {
		if strings.HasPrefix(desc, prefix) {
			return desc
		}
	}
	return "⏰ " + desc
}

func timerStartNotice(desc string) string {
	for _, prefix := range scheduledStartNoticeSuppressedPrefixes {
		if strings.HasPrefix(desc, prefix) {
			return ""
		}
	}
	return scheduledStartNotice(desc)
}
