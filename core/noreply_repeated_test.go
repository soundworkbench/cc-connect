package core

import "testing"

func TestResolveSilentReply_RepeatedMarkers(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		silent bool
	}{
		{"NO_REPLYNO_REPLY", "", true},
		{"NO_REPLY\nNO_REPLY", "", true},
		{"no_reply NO_REPLY ", "", true},
		{"中途说明\n\nNO_REPLYNO_REPLY", "中途说明", false},
		{"写了 NO_REPLY 这个词", "写了 NO_REPLY 这个词", false},
		{"NO_REPLYX", "NO_REPLYX", false},
	}
	for _, c := range cases {
		got, silent := resolveSilentReply(c.in)
		if got != c.want || silent != c.silent {
			t.Errorf("resolveSilentReply(%q) = (%q, %v), want (%q, %v)", c.in, got, silent, c.want, c.silent)
		}
	}
}
