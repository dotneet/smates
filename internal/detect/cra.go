package detect

import (
	"regexp"
	"strings"

	"github.com/dotneet/smates/internal/runner"
)

// envAssign is a leading VAR=value word of a shell command.
var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// craBins are the commands that run jest with the configuration Create React App
// generates: react-scripts itself and the wrappers that patch it (CRACO,
// react-app-rewired).
var craBins = []string{"react-scripts", "craco", "react-app-rewired"}

// craTests are the test patterns of Create React App's jest configuration (roots
// <rootDir>/src and its testMatch), relative to the package.
var craTests = []string{"src/**/__tests__/**/*.{js,jsx,ts,tsx}", "src/**/*.{spec,test}.{js,jsx,ts,tsx}"}

// craTest finds the `<bin> test` invocation of a Create React App wrapper in an npm
// script: the command, the index of the bin word in it, and the bin's name.
func craTest(script string) (cmd []string, at int, bin string) {
	for _, cmd := range scriptCommands(script) {
		for i := 0; i+1 < len(cmd); i++ {
			if cmd[i+1] != "test" {
				continue
			}
			for _, b := range craBins {
				if cmd[i] == b || strings.HasSuffix(cmd[i], "/"+b) {
					return cmd, i, b
				}
			}
		}
	}
	return nil, -1, ""
}

// cmdEnv returns the VAR=value assignments among words (those before a command,
// directly or through cross-env/env), or nil if there are none.
func cmdEnv(words []string) map[string]string {
	var env map[string]string
	for _, w := range words {
		if envAssign.MatchString(w) {
			if env == nil {
				env = map[string]string{}
			}
			k, v, _ := strings.Cut(w, "=")
			env[k] = v
		}
	}
	return env
}

// craCommand turns a Create React App test script into a suite command and
// environment: the `react-scripts test` (or `craco test`, `react-app-rewired test`)
// invocation with the script's own arguments (up to the next shell operator), never in
// watch mode, run through the project's package manager, and the VAR=value
// assignments before it (directly or through cross-env/env).
func craCommand(repoRoot, dir, script string) (string, map[string]string) {
	var env map[string]string
	args := []string{"test"}
	bin := "react-scripts"
	if cmd, at, b := craTest(script); at >= 0 {
		bin = b
		env = cmdEnv(cmd[:at])
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
	}
	args = append(args, "--watchAll=false")
	return runner.JSBinCommand(repoRoot, dir, bin, strings.Join(args, " ")), env
}

// nodeTest finds a `node [options] --test` invocation in an npm script. command is the
// invocation up to --test when node gets other options before it (node --import tsx
// --test), so that the suite runs test files the way the script does; it is "" for a
// plain `node --test`, the runner's default. File arguments after --test are dropped:
// smates passes test files itself.
func nodeTest(script string) (command string, env map[string]string, ok bool) {
	for _, cmd := range scriptCommands(script) {
		for i, w := range cmd {
			if w != "node" && !strings.HasSuffix(w, "/node") {
				continue
			}
			for j := i + 1; j < len(cmd); j++ {
				if cmd[j] != "--test" {
					continue
				}
				env = cmdEnv(cmd[:i])
				if j == i+1 {
					return "", env, true
				}
				words := []string{shellQuote(w)}
				for _, a := range cmd[i+1 : j+1] {
					words = append(words, shellQuote(a))
				}
				return strings.Join(words, " "), env, true
			}
		}
	}
	return "", nil, false
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
