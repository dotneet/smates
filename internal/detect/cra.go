package detect

import (
	"path"
	"regexp"
	"strings"

	"github.com/dotneet/smates/internal/runner"
)

// envAssign is a leading VAR=value word of a shell command.
var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// redirection matches a shell redirection word (2>&1, >out.log, >, &>, <in).
var redirection = regexp.MustCompile(`^(?:[0-9]*[<>]|&>)`)

// redirectionOperand reports whether redirection word w is an operator alone, its
// target the next word (2> err.log, not 2>&1 or >out.log).
func redirectionOperand(w string) bool {
	return strings.TrimRight(strings.TrimLeft(w, "0123456789&"), "<>|") == ""
}

// craBins are the commands that run jest with the configuration Create React App
// generates: react-scripts itself and the wrappers that patch it (CRACO,
// react-app-rewired).
var craBins = []string{"react-scripts", "craco", "react-app-rewired"}

// craTests are the test patterns of Create React App's jest configuration (roots
// <rootDir>/src and its testMatch), relative to the package.
var craTests = []string{"src/**/__tests__/**/*.{js,jsx,ts,tsx}", "src/**/*.{spec,test}.{js,jsx,ts,tsx}"}

// craWrapperConfig matches the configuration files of CRA wrappers, which may change
// the jest roots and testMatch react-scripts uses (craco.config.js jest.configure,
// react-app-rewired's config-overrides.js).
var craWrapperConfig = regexp.MustCompile(`^(craco\.config\.(js|ts|cjs|mjs)|\.cracorc(\.(js|ts|json|ya?ml))?|config-overrides\.(js|cjs|mjs))$`)

// nestedCommands returns the simple commands of an npm script and, recursively, of its
// words that hold a command line themselves: the quoted arguments of sh -c,
// concurrently, npm-run-all and the like.
func nestedCommands(script string) [][]string {
	var out [][]string
	var walk func(s string, depth int)
	walk = func(s string, depth int) {
		for _, cmd := range scriptCommands(s) {
			out = append(out, cmd)
			if depth >= 3 {
				continue
			}
			for _, w := range cmd {
				if strings.ContainsAny(w, " \t\n") {
					walk(w, depth+1)
				}
			}
		}
	}
	walk(script, 0)
	return out
}

// scriptClosure returns script and the package scripts it runs, recursively (npm run
// x, npm test, pnpm [run] x, yarn [run] x, bun run x, bun x; bun test is bun's test
// runner, not the script test).
func scriptClosure(scripts map[string]string, script string) []string {
	out := []string{script}
	seen := map[string]bool{}
	var walk func(s string, depth int)
	walk = func(s string, depth int) {
		if depth > 3 {
			return
		}
		for _, cmd := range nestedCommands(s) {
			for i, w := range cmd {
				pm := path.Base(w)
				if pm != "npm" && pm != "pnpm" && pm != "yarn" && pm != "bun" {
					continue
				}
				j := i + 1
				for j < len(cmd) && strings.HasPrefix(cmd[j], "-") {
					j++
				}
				if j >= len(cmd) {
					continue
				}
				viaRun := cmd[j] == "run" || cmd[j] == "run-script"
				if viaRun {
					j++
					for j < len(cmd) && strings.HasPrefix(cmd[j], "-") {
						j++
					}
					if j >= len(cmd) {
						continue
					}
				}
				name := cmd[j]
				switch {
				case pm == "bun" && name == "test" && !viaRun:
					continue // bun's own test runner
				case pm == "npm" && !viaRun && name != "test" && name != "t":
					continue // npm runs only test (and start, stop, restart) without run
				}
				if name == "t" {
					name = "test"
				}
				if body, ok := scripts[name]; ok && !seen[name] {
					seen[name] = true
					out = append(out, body)
					walk(body, depth+1)
				}
			}
		}
	}
	walk(script, 0)
	return out
}

// runsJest reports whether an npm script invokes jest as a command (jest,
// node_modules/.bin/jest, npx jest, node .../jest/bin/jest.js), not a word that only
// contains "jest" (a script named test:jest-compat, ts-jest).
func runsJest(script string) bool {
	for _, cmd := range nestedCommands(script) {
		for _, w := range cmd {
			if b := path.Base(w); b == "jest" || b == "jest.js" && strings.Contains(w, "jest/bin/") {
				return true
			}
		}
	}
	return false
}

// runsBunTest reports whether an npm script runs bun's test runner (bun test), not a
// package script whose name starts with test (bun test:unit).
func runsBunTest(script string) bool {
	for _, cmd := range nestedCommands(script) {
		for i := 0; i+1 < len(cmd); i++ {
			if path.Base(cmd[i]) == "bun" && cmd[i+1] == "test" {
				return true
			}
		}
	}
	return false
}

// craTest finds the `<bin> test` invocation of a Create React App wrapper in an npm
// script (also inside a quoted argument: concurrently "react-scripts test", sh -c '…'):
// the command, the index of the bin word in it, and the bin's name.
func craTest(script string) (cmd []string, at int, bin string) {
	for _, cmd := range nestedCommands(script) {
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
			case redirection.MatchString(a):
				// The script's redirections (2>&1, > test.log) are no arguments.
				if redirectionOperand(a) {
					i++
				}
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

// nodeValueOpts are node options whose value may be the next word: those node 24's
// --help lists as --name=... (watch options aside, see nodeTestOptions), and options of
// older versions.
var nodeValueOpts = map[string]bool{
	"--allow-fs-read": true, "--allow-fs-write": true, "--build-snapshot-config": true,
	"-C": true, "--conditions": true, "--cpu-prof-dir": true, "--cpu-prof-interval": true,
	"--cpu-prof-name": true, "--diagnostic-dir": true, "--disable-proto": true,
	"--disable-warning": true, "--dns-result-order": true, "--env-file": true,
	"--env-file-if-exists": true, "-e": true, "--eval": true,
	"--experimental-config-file": true, "--loader": true, "--experimental-loader": true,
	"--experimental-package-map": true, "--experimental-sea-config": true,
	"--experimental-test-tag-filter": true, "--heap-prof-dir": true,
	"--heap-prof-interval": true, "--heap-prof-name": true,
	"--heapsnapshot-near-heap-limit": true, "--heapsnapshot-signal": true,
	"--icu-data-dir": true, "--import": true, "--input-type": true,
	"--inspect-publish-uid": true, "--localstorage-file": true, "--max-http-header-size": true,
	"--max-old-space-size-percentage": true, "--network-family-autoselection-attempt-timeout": true,
	"--openssl-config": true, "--redirect-warnings": true, "--report-directory": true, "--report-dir": true,
	"--report-filename": true, "--report-signal": true, "-r": true, "--require": true,
	"--run": true, "--secure-heap": true, "--secure-heap-min": true, "--snapshot-blob": true,
	"--test-concurrency": true, "--test-coverage-branches": true,
	"--test-coverage-exclude": true, "--test-coverage-functions": true,
	"--test-coverage-include": true, "--test-coverage-lines": true,
	"--test-global-setup": true, "--experimental-test-isolation": true,
	"--test-isolation": true, "--test-name-pattern": true, "--test-random-seed": true,
	"--test-reporter": true, "--test-reporter-destination": true,
	"--test-rerun-failures": true, "--test-shard": true, "--test-skip-pattern": true,
	"--test-timeout": true, "--title": true, "--tls-cipher-list": true, "--tls-keylog": true,
	"--trace-event-categories": true, "--trace-event-file-pattern": true,
	"--trace-require-module": true, "--unhandled-rejections": true, "--use-largepages": true,
	"--v8-pool-size": true,
	// Older versions.
	"--experimental-specifier-resolution": true, "--inspect-port": true, "--experimental-policy": true,
}

// nodeTest finds a `node [options] --test` invocation in an npm script (also inside a
// quoted argument). command is the invocation with node's other options, before or
// after --test, all put before --test (node --import tsx --test, node --test
// --test-concurrency=1), so that the suite runs test files the way the script does; it
// is "" for a plain `node --test`, the runner's default. Watch options are dropped (the
// suite must exit), and so are file arguments after --test: smates passes test files
// itself. A node that runs a script (node scripts/run.js --test) is not a test run.
func nodeTest(script string) (command string, env map[string]string, ok bool) {
	for _, cmd := range nestedCommands(script) {
		for i, w := range cmd {
			if w != "node" && !strings.HasSuffix(w, "/node") {
				continue
			}
			opts, ok := nodeTestOptions(cmd[i+1:])
			if !ok {
				continue
			}
			env = cmdEnv(cmd[:i])
			if len(opts) == 0 {
				return "", env, true
			}
			words := []string{shellQuote(w)}
			for _, a := range opts {
				words = append(words, shellQuote(a))
			}
			return strings.Join(words, " ") + " --test", env, true
		}
	}
	return "", nil, false
}

// nodeTestOptions returns node's options other than --test (without watch options), or
// false if node runs no tests: no --test before the first positional word (the script
// node runs). After --test, positional words are test files and globs (left out) and
// the options end at "--" or a redirection.
func nodeTestOptions(args []string) ([]string, bool) {
	var opts []string
	test := false
	for k := 0; k < len(args); k++ {
		a := args[k]
		name, _, hasValue := strings.Cut(a, "=")
		switch {
		case a == "--test":
			test = true
		case test && (a == "--" || redirection.MatchString(a)):
			return opts, true
		case !strings.HasPrefix(a, "-") || a == "-":
			if !test {
				return nil, false
			}
		case name == "--watch" || name == "--watch-preserve-output":
		case name == "--watch-path" || name == "--watch-kill-signal":
			if !hasValue {
				k++
			}
		default:
			opts = append(opts, a)
			if nodeValueOpts[name] && !hasValue && k+1 < len(args) {
				k++
				opts = append(opts, args[k])
			}
		}
	}
	return opts, test
}

// scriptCommands splits an npm script into its simple commands, each a list of
// unquoted words. Quotes and backslashes are honored as a POSIX shell does; unquoted
// &, |, ; and subshell parentheses separate commands. Redirections stay words (2>&1,
// &>, > out.log: see redirection). Other shell syntax is not interpreted.
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
		case c == '&' && ((i+1 < len(s) && s[i+1] == '>') || (inWord && strings.HasSuffix(cur.String(), ">")) || (inWord && strings.HasSuffix(cur.String(), "<"))):
			// A redirection: &> out, 2>&1, <&3.
			cur.WriteByte(c)
			inWord = true
		case c == '&' || c == '|' || c == ';' || c == '(' || c == ')':
			// Subshell parentheses group commands: "(node --test)".
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
