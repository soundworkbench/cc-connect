package core

import "strings"

func scheduledStartNotice(desc string) string {
	if strings.HasPrefix(desc, "✅") || strings.HasPrefix(desc, "❌") {
		return desc
	}
	return "⏰ " + desc
}
