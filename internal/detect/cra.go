package detect

import (
	"regexp"
	"strings"

	"github.com/dotneet/smates/internal/runner"
)

// envAssign is a leading VAR=value word of a shell command.
var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// craCommand turns a Create React App test script into a suite command and
// environment: the `react-scripts test` invocation with the script's own arguments (up
// to the next shell operator), never in watch mode, run through the project's package
// manager, and the VAR=value assignments before it (directly or through cross-env/env).
func craCommand(repoRoot, dir, script string) (string, map[string]string) {
	var env map[string]string
	args := []string{"test"}
	for _, cmd := range scriptCommands(script) {
		at := -1
		for i := 0; i+1 < len(cmd); i++ {
			if strings.HasSuffix(cmd[i], "react-scripts") && cmd[i+1] == "test" {
				at = i
				break
			}
		}
		if at < 0 {
			continue
		}
		for _, w := range cmd[:at] {
			if envAssign.MatchString(w) {
				if env == nil {
					env = map[string]string{}
				}
				k, v, _ := strings.Cut(w, "=")
				env[k] = v
			}
		}
		rest := cmd[at+2:]
		for i := 0; i < len(rest); i++ {
			switch a := rest[i]; {
			case a == "--watch" || a == "--watchAll":
				if i+1 < len(rest) && (rest[i+1] == "true" || rest[i+1] == "false") {
					i++
				}
			case strings.HasPrefix(a, "--watch=") || strings.HasPrefix(a, "--watchAll="):
			default:
				args = append(args, shellQuote(a))
			}
		}
		break
	}
	args = append(args, "--watchAll=false")
	return runner.JSBinCommand(repoRoot, dir, "react-scripts", strings.Join(args, " ")), env
}

// scriptCommands splits an npm script into its simple commands, each a list of
// unquoted words. Quotes and backslashes are honored as a POSIX shell does; unquoted
// &, | and ; separate commands. Other shell syntax (subshells, redirections) is not
// interpreted.
func scriptCommands(s string) [][]string {
	var cmds [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			endWord()
		case c == '&' || c == '|' || c == ';':
			endWord()
			if len(words) > 0 {
				cmds = append(cmds, words)
				words = nil
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	endWord()
	if len(words) > 0 {
		cmds = append(cmds, words)
	}
	return cmds
}

// shellQuote quotes a word for runner.SplitWords (POSIX shell rules) when it holds
// anything but plain characters.
func shellQuote(w string) string {
	if w != "" && strings.Trim(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_=+.,/:@%^") == "" {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}
