package claudecode

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

type claudeUsageTerminal struct {
	mu       sync.RWMutex
	lines    [][]rune
	row      int
	col      int
	savedRow int
	savedCol int
	pending  []byte
}

func newClaudeUsageTerminal() *claudeUsageTerminal {
	return &claudeUsageTerminal{
		lines: [][]rune{nil},
	}
}

func (t *claudeUsageTerminal) Write(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) > 0 {
		p = append(t.pending, p...)
		t.pending = nil
	}

	for i := 0; i < len(p); {
		switch p[i] {
		case 0x1b:
			next := t.consumeEscape(p[i:])
			if next == 0 {
				t.pending = append([]byte(nil), p[i:]...)
				return
			} else {
				i += next
			}
		case '\r':
			t.col = 0
			i++
		case '\n':
			t.row++
			t.ensureRow(t.row)
			i++
		case '\b':
			if t.col > 0 {
				t.col--
			}
			i++
		case '\t':
			advance := 4 - (t.col % 4)
			for j := 0; j < advance; j++ {
				t.writeRune(' ')
			}
			i++
		default:
			if p[i] < 0x20 || p[i] == 0x7f {
				i++
				continue
			}
			if !utf8.FullRune(p[i:]) {
				t.pending = append([]byte(nil), p[i:]...)
				return
			}
			r, size := utf8.DecodeRune(p[i:])
			if r == utf8.RuneError && size == 1 {
				i++
				continue
			}
			t.writeRune(r)
			i += size
		}
	}
}

func (t *claudeUsageTerminal) String() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	lines := make([]string, 0, len(t.lines))
	for _, line := range t.lines {
		lines = append(lines, strings.TrimRight(string(line), " "))
	}
	return strings.Join(lines, "\n")
}

func (t *claudeUsageTerminal) consumeEscape(p []byte) int {
	if len(p) < 2 {
		return 0
	}
	switch p[1] {
	case '[':
		for i := 2; i < len(p); i++ {
			if p[i] >= '@' && p[i] <= '~' {
				t.applyCSI(string(p[2:i]), p[i])
				return i + 1
			}
		}
		return 0
	case ']':
		for i := 2; i < len(p); i++ {
			if p[i] == '\a' {
				return i + 1
			}
			if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '\\' {
				return i + 2
			}
		}
		return 0
	case '7':
		t.savedRow, t.savedCol = t.row, t.col
		return 2
	case '8':
		t.row, t.col = t.savedRow, t.savedCol
		return 2
	case '(', ')':
		if len(p) < 3 {
			return 0
		}
		return 3
	default:
		return 2
	}
}

func (t *claudeUsageTerminal) applyCSI(params string, final byte) {
	switch final {
	case 'G', '`':
		// Current Claude terminals use absolute columns between words.
		// Ignoring CHA joins "Current session" into "Currentsession".
		t.col = parseCSIInt(params, 1) - 1
	case 'd':
		t.row = parseCSIInt(params, 1) - 1
		t.ensureRow(t.row)
	case 's':
		t.savedRow, t.savedCol = t.row, t.col
	case 'u':
		t.row, t.col = t.savedRow, t.savedCol
	case 'A', 'B', 'C', 'D':
		n := parseCSIInt(params, 1)
		switch final {
		case 'A':
			t.row -= n
			if t.row < 0 {
				t.row = 0
			}
		case 'B':
			t.row += n
			t.ensureRow(t.row)
		case 'C':
			t.col += n
		case 'D':
			t.col -= n
			if t.col < 0 {
				t.col = 0
			}
		}
	case 'H', 'f':
		row, col := parseCSICursor(params)
		if row < 0 {
			row = 0
		}
		if col < 0 {
			col = 0
		}
		t.row, t.col = row, col
		t.ensureRow(t.row)
	case 'J':
		if params == "2" || params == "" {
			t.lines = [][]rune{nil}
			t.row, t.col = 0, 0
		}
	case 'K':
		t.ensureRow(t.row)
		switch params {
		case "", "0":
			if t.col < len(t.lines[t.row]) {
				t.lines[t.row] = t.lines[t.row][:t.col]
			}
		case "2":
			t.lines[t.row] = nil
			t.col = 0
		}
	default:
	}
}

func (t *claudeUsageTerminal) writeRune(r rune) {
	if t.row >= maxTerminalRows || t.col >= maxTerminalCols {
		return
	}
	t.ensureCell(t.row, t.col)
	if t.row < len(t.lines) && t.col < len(t.lines[t.row]) {
		t.lines[t.row][t.col] = r
	}
	t.col++
}

const maxTerminalRows = 500
const maxTerminalCols = 500

func (t *claudeUsageTerminal) ensureRow(row int) {
	if row >= maxTerminalRows {
		return
	}
	for len(t.lines) <= row {
		t.lines = append(t.lines, nil)
	}
}

func (t *claudeUsageTerminal) ensureCell(row, col int) {
	if row >= maxTerminalRows || col >= maxTerminalCols {
		return
	}
	t.ensureRow(row)
	for len(t.lines[row]) <= col {
		t.lines[row] = append(t.lines[row], ' ')
	}
}

func parseCSIInt(raw string, fallback int) int {
	raw = strings.TrimPrefix(raw, "?")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func parseCSICursor(raw string) (int, int) {
	raw = strings.TrimPrefix(raw, "?")
	if raw == "" {
		return 0, 0
	}
	parts := strings.Split(raw, ";")
	row := parseCSIInt(parts[0], 1) - 1
	col := 0
	if len(parts) > 1 {
		col = parseCSIInt(parts[1], 1) - 1
	}
	return row, col
}
