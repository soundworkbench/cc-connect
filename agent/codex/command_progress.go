package codex

import (
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/chenhg5/cc-connect/core"
)

var progressWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,39}$`)
var progressDuration = regexp.MustCompile(`^[0-9]+([.][0-9]+)?[smhd]?$`)
var progressAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// Only known subcommands are public labels. A positional argument is not
// necessarily a subcommand (e.g. echo/printf); never copy it into a card.
var progressSubcommands = map[string]string{
	"git":     "status log diff show branch checkout switch fetch pull push add commit reset restore rev-parse ls-files worktree",
	"make":    "test build lint",
	"go":      "test build vet run version env mod list",
	"npm":     "test run install ci build",
	"node":    "test",
	"docker":  "ps inspect logs build run exec images",
	"codex":   "exec resume review",
	"python3": "",
}

// This is a conservative display parser, never a shell evaluator. Quoted
// contents, option values, paths and command arguments cannot become labels.
// Unknown wrapper syntax degrades to the localized generic command label.
func codexCommandProgress(command string) string {
	label := core.NewI18n(core.LangChinese).T(core.MsgProgressCommand)
	lower := strings.ToLower(command)
	for _, word := range []string{"token", "secret", "key", "password", "bearer"} {
		if strings.Contains(lower, word) {
			return label
		}
	}
	words, ok := progressCommandPrefix(progressCommandWords(command))
	if !ok {
		return label
	}
	if len(words) == 0 {
		return label
	}
	program := path.Base(words[0])
	if !progressWord.MatchString(program) {
		return label
	}
	label += " · " + program
	words = words[1:]
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		flag := words[0]
		words = words[1:]
		// Known options consume one value; unknown options stop extraction,
		// since their following value might look like a subcommand.
		switch flag {
		case "-C", "-c", "--git-dir", "--work-tree":
			if len(words) == 0 {
				return label
			}
			words = words[1:]
		case "--", "--no-pager", "--version", "--help":
		default:
			return label
		}
	}
	if len(words) > 0 && progressWord.MatchString(words[0]) {
		for _, sub := range strings.Fields(progressSubcommands[program]) {
			if words[0] == sub {
				return label + " " + sub
			}
		}
	}
	return label
}

// progressCommandPrefix strips assignments and known wrapper options.
func progressCommandPrefix(words []string) ([]string, bool) {
	for len(words) > 0 {
		if progressAssignment.MatchString(words[0]) {
			words = words[1:]
			continue
		}
		wrapper := path.Base(words[0])
		if wrapper != "sudo" && wrapper != "timeout" && wrapper != "env" {
			break
		}
		words = words[1:]
		for len(words) > 0 && strings.HasPrefix(words[0], "-") {
			flag := words[0]
			words = words[1:]
			if flag == "--" {
				break
			}
			// Options have different arity for each wrapper: sudo -S/-s
			// take no value, while timeout -s does. Never let an option
			// consume the actual program and expose a positional argument.
			valueFlags := map[string]string{
				"sudo":    "-u -g -h -p -C -T --user --group --host --prompt --close-from --command-timeout",
				"timeout": "-k -s --kill-after --signal",
				"env":     "-u -C --unset --chdir",
			}
			plainFlags := map[string]string{
				"sudo":    "-n -E -H -S -s -i --non-interactive --preserve-env --set-home --stdin --shell --login",
				"timeout": "--preserve-status --foreground --verbose",
				"env":     "-i --ignore-environment",
			}
			name, _, attached := strings.Cut(flag, "=")
			if strings.Contains(" "+valueFlags[wrapper]+" ", " "+name+" ") {
				if !attached {
					if len(words) == 0 {
						return nil, false
					}
					words = words[1:]
				}
			} else if attached || !strings.Contains(" "+plainFlags[wrapper]+" ", " "+name+" ") {
				// env --split-string executes quoted input; do not reinterpret
				// later arguments as a program. Unknown flags also fail closed.
				return nil, false
			}
		}
		if wrapper == "timeout" {
			if len(words) == 0 || !progressDuration.MatchString(words[0]) {
				return nil, false
			}
			words = words[1:]
		}
	}
	return words, true
}

// Quoted or escaped tokens become empty placeholders, preserving their position
// so an argument after a quoted token cannot masquerade as the program.
func progressCommandWords(command string) []string {
	var words []string
	var token strings.Builder
	quote := rune(0)
	unsafe, active := false, false
	flush := func() {
		if active {
			if unsafe {
				if progressAssignment.MatchString(token.String()) {
					name, _, _ := strings.Cut(token.String(), "=")
					words = append(words, name+"=")
				} else {
					words = append(words, "")
				}
			} else {
				words = append(words, token.String())
			}
		}
		token.Reset()
		unsafe, active = false, false
	}
	for _, r := range command {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' || r == '`' {
			quote, unsafe, active = r, true, true
			continue
		}
		if strings.ContainsRune(";&|<>\n\r", r) {
			break
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		active = true
		if strings.ContainsRune("\\$(){}", r) {
			unsafe = true
		}
		token.WriteRune(r)
	}
	flush()
	return words
}
