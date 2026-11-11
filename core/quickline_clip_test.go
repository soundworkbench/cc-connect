package core

import "testing"

func TestClipQuickLineFitsOneLine(t *testing.T) {
	cases := map[string]string{
		"收到，我先去查一下日志。": "收到，我先去查一下日志。",
		"收到，我先查洛杉矶和家里两台服务器的磁盘与内存，再看最近一小时的告警记录和重启情况。": "收到，我先查洛杉矶和家里两台服务器的磁盘与内存。",
		"收到我先去把所有助理的配置文件逐个核对一遍然后统一更新到最新版本再重启":        "收到我先去把所有助理的配置文件逐个核对一遍然后统一更新到最…",
	}
	for in, want := range cases {
		got := clipQuickLine(in)
		if got != want {
			t.Errorf("clipQuickLine(%q) = %q, want %q", in, got, want)
		}
		if n := len([]rune(got)); n > quickLineMaxRunes {
			t.Errorf("clipQuickLine(%q) has %d runes, over %d", in, n, quickLineMaxRunes)
		}
	}
}
