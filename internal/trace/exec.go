package trace

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dotneet/smates/internal/depkey"
)

// Child processes a test starts are traced directly when they are Node or Python
// processes (they inherit the shims). Everything else (shell scripts, grep, make, a
// compiled binary) is opaque, so dependencies are inferred from the command line:
//
//   - well-known commands that only read their file arguments (cat, grep, jq, ...)
//     depend on the repository files and directories named on the command line,
//     including shell globs;
//   - any other program may read anything below the directory it runs in, so the test
//     depends on that whole tree ("<dir>/**"), plus the files named on its command line
//     (the script itself).
//
// This over-approximates on purpose: an opaque command is exactly where a dependency
// could hide.

// tracedPrograms trace themselves (or only start programs that do).
var tracedPrograms = map[string]bool{
	"node": true, "nodejs": true, "bun": true, "python": true, "python3": true, "pypy3": true,
	"npm": true, "npx": true, "pnpm": true, "yarn": true, "bunx": true, "uv": true, "uvx": true,
}

// readers only read the paths named on their command line (recursively for -r tools).
var readers = map[string]bool{
	"cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true, "rg": true,
	"sed": true, "awk": true, "sort": true, "uniq": true, "wc": true, "cut": true, "tr": true,
	"diff": true, "cmp": true, "jq": true, "yq": true, "ls": true, "find": true, "test": true,
	"[": true, "echo": true, "printf": true, "true": true, "false": true, "md5sum": true,
	"sha1sum": true, "sha256sum": true, "shasum": true, "base64": true, "stat": true, "file": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true, "pwd": true,
	"uname": true, "hostname": true, "whoami": true, "id": true, "nproc": true, "arch": true,
	"getconf": true, "sw_vers": true,
}

// nameOnly commands never open files, whatever their arguments. That includes the
// system information commands libraries run on their own: platform.processor() runs
// `uname -p` in every pytest-xdist worker, for example.
var nameOnly = map[string]bool{
	"basename": true, "dirname": true, "pwd": true, "echo": true, "printf": true, "true": true, "false": true,
	"uname": true, "hostname": true, "whoami": true, "id": true, "nproc": true, "arch": true,
	"getconf": true, "sw_vers": true,
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true}

// execDeps infers dependencies of a child process. It returns repo-relative keys:
// plain paths (content), "dir/" (listing), "dir/**" (whole tree) and "path?" (existence).
func execDeps(args []string, shell bool, cwd, root string, rel func(string) (string, bool)) []string {
	a := &execAnalysis{rel: rel, root: root, funcs: map[string]bool{}}
	if shell {
		a.shellText(strings.Join(args, " "), cwd, "", 0)
	} else {
		a.command(args, cwd, 0)
	}
	return a.out
}

type execAnalysis struct {
	rel   func(string) (string, bool)
	root  string
	out   []string
	funcs map[string]bool // shell functions defined by the analyzed scripts
}

func (a *execAnalysis) add(p string) {
	if r, ok := a.rel(p); ok {
		a.out = append(a.out, r)
	}
}

// opaque records that something below dir may be read. A command running outside the
// repository (in a temp directory, say) only reaches it through paths on its command
// line, which are recorded separately.
func (a *execAnalysis) opaque(dir string) {
	if r, ok := a.rel(dir); ok {
		a.out = append(a.out, depkey.Tree(r))
		return
	}
	if rel, err := filepath.Rel(a.root, dir); err == nil && rel == "." {
		a.out = append(a.out, "./**")
	}
}

// shellText analyzes shell source (a -c string or a script file). script is the path of
// the script ("" for -c strings), used for $0 and $(dirname "$0").
func (a *execAnalysis) shellText(text, cwd, script string, depth int) {
	if script != "" {
		// bash's ${BASH_SOURCE[0]} names the script like $0 (the dirname idioms below).
		text = bashSourcePrefix.ReplaceAllLiteralString(text, "${0%")
		text = bashSource.ReplaceAllLiteralString(text, "$0")
		dir := filepath.Dir(script)
		for _, pat := range []string{`"$(cd "$(dirname "$0")" && pwd)"`, `$(cd "$(dirname "$0")" && pwd)`, `"$(dirname "$0")"`, `$(dirname "$0")`, `$(dirname $0)`, `"${0%/*}"`, `${0%/*}`} {
			text = strings.ReplaceAll(text, pat, dir)
		}
		text = strings.ReplaceAll(text, `"$0"`, script)
	}
	text = stripComments(text)
	for _, m := range funcDef.FindAllStringSubmatch(text, -1) {
		a.funcs[m[1]] = true
	}
	text = a.substitutions(text, cwd, depth)
	if strings.Contains(text, "`") || strings.Contains(text, "eval ") {
		a.opaque(cwd) // backticks or eval: cannot follow
	}
	vars := map[string][]string{}
	for _, seg := range splitShell(text) {
		seg = expandVars(seg, vars)
		if len(seg) > 0 && isAssignments(seg) {
			for _, w := range seg {
				name, value, _ := strings.Cut(w, "=")
				vars[name] = []string{value}
			}
			continue
		}
		if len(seg) >= 3 && seg[0] == "for" && seg[2] == "in" {
			vars[seg[1]] = append([]string{}, seg[3:]...)
			continue
		}
		if len(seg) > 0 && (a.funcs[seg[0]] || strings.HasSuffix(seg[0], "()")) {
			continue // a function call (its body is analyzed where it is defined)
		}
		if next, ok := a.builtin(seg, cwd, depth); ok {
			cwd = next
			continue
		}
		a.command(seg, cwd, depth)
	}
}

var (
	funcDef = regexp.MustCompile(`(?m)^[ \t]*(?:function[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\([ \t]*\)`)
	varRef  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)
	assign  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

	bashSource       = regexp.MustCompile(`\$\{BASH_SOURCE(?:\[0\])?\}|\$BASH_SOURCE\b`)
	bashSourcePrefix = regexp.MustCompile(`\$\{BASH_SOURCE(?:\[0\])?%`)
	// cwdChange matches anything that may change the working directory of a script
	// (directly or through sourced code), so $(pwd) cannot be taken as the start cwd.
	cwdChange = regexp.MustCompile(`\b(?:cd|pushd|popd|source|eval)\b|(?:^|[;&|(\n])[ \t]*\.[ \t]`)
)

// unresolvedSubst replaces the output of a command substitution. Like an unknown
// variable it contains "$", so a word using it is unresolved (an opaque command, an
// opaque cd) and never becomes a path.
const unresolvedSubst = "${__smates_subst__}"

func isAssignments(seg []string) bool {
	for _, w := range seg {
		if !assign.MatchString(w) {
			return false
		}
	}
	return true
}

// expandVars substitutes shell variables assigned earlier in the script; a word using
// a variable with several values (a for-loop list) becomes several words. Unknown
// variables are left in place (and make the command unresolved).
func expandVars(seg []string, vars map[string][]string) []string {
	var out []string
	for _, w := range seg {
		if !strings.Contains(w, "$") {
			out = append(out, w)
			continue
		}
		words := []string{w}
		for _, m := range varRef.FindAllStringSubmatch(w, -1) {
			name := m[1] + m[2]
			vals, ok := vars[name]
			if !ok {
				continue
			}
			var next []string
			for _, cur := range words {
				for _, v := range vals {
					next = append(next, strings.Replace(cur, m[0], v, 1))
				}
			}
			words = next
		}
		out = append(out, words...)
	}
	return out
}

// substitutions handles $(...) and `...` command substitutions: commands that only read
// (pwd, basename, cat ...) are analyzed like any other command, anything else makes the
// surrounding directory opaque. The output is unknown (unresolvedSubst), except for
// $(pwd) in a script that never changes directory.
func (a *execAnalysis) substitutions(text, cwd string, depth int) string {
	pwd := unresolvedSubst
	if !cwdChange.MatchString(text) && !strings.ContainsAny(cwd, " \t\n\"'$\\`;&|<>()*?[") {
		pwd = cwd
	}
	for n := 0; n < 100; n++ {
		start := strings.LastIndex(text, "$(")
		if start < 0 {
			break
		}
		end := strings.IndexByte(text[start:], ')')
		if end < 0 {
			a.opaque(cwd)
			return text[:start]
		}
		text = text[:start] + a.substitution(text[start+2:start+end], cwd, pwd, depth) + text[start+end+1:]
	}
	for n := 0; n < 100; n++ {
		start := strings.IndexByte(text, '`')
		if start < 0 {
			break
		}
		end := strings.IndexByte(text[start+1:], '`')
		if end < 0 {
			a.opaque(cwd)
			return text[:start]
		}
		text = text[:start] + a.substitution(text[start+1:start+1+end], cwd, pwd, depth) + text[start+end+2:]
	}
	return text
}

// substitution analyzes the commands of one command substitution and returns what
// stands for its output.
func (a *execAnalysis) substitution(inner, cwd, pwd string, depth int) string {
	if strings.TrimSpace(inner) == "pwd" {
		return pwd
	}
	for _, seg := range splitShell(inner) {
		if len(seg) == 0 {
			continue
		}
		if readers[filepath.Base(seg[0])] || seg[0] == "cd" {
			if seg[0] != "cd" {
				a.command(seg, cwd, depth)
			}
			continue
		}
		a.opaque(cwd)
	}
	return unresolvedSubst
}

var keywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true, "for": true, "while": true, "until": true,
	"do": true, "done": true, "case": true, "esac": true, "in": true, "function": true, "{": true, "}": true,
	"!": true, "time": true, "command": true, "builtin": true, "exec": true, "then:": true,
}

var noops = map[string]bool{
	"set": true, "export": true, "local": true, "readonly": true, "unset": true, "shift": true, "return": true,
	"exit": true, "trap": true, "wait": true, "declare": true, "typeset": true, "alias": true, ":": true,
	"popd": true, "umask": true, "read": true, "break": true, "continue": true,
}

// builtin handles shell builtins; it returns the new working directory and whether the
// segment was fully handled.
func (a *execAnalysis) builtin(seg []string, cwd string, depth int) (string, bool) {
	seg = trimAssignments(seg)
	for len(seg) > 0 && keywords[seg[0]] {
		seg = seg[1:]
	}
	if len(seg) == 0 {
		return cwd, true
	}
	switch seg[0] {
	case "cd", "pushd":
		if len(seg) < 2 || strings.Contains(seg[1], "$") || seg[1] == "-" {
			a.opaque(cwd)
			return cwd, true
		}
		d := seg[1]
		if !filepath.IsAbs(d) {
			d = filepath.Join(cwd, d)
		}
		return filepath.Clean(d), true
	case "source", ".":
		switch {
		case len(seg) < 2:
		case strings.Contains(seg[1], "$"):
			a.opaque(cwd) // unresolved path: cannot follow
		default:
			a.script(seg[1], cwd, depth)
		}
		return cwd, true
	}
	if noops[seg[0]] {
		return cwd, true
	}
	return cwd, false
}

// command analyzes one simple command (argv) run in cwd.
func (a *execAnalysis) command(seg []string, cwd string, depth int) {
	seg = trimAssignments(seg)
	for len(seg) > 0 && keywords[seg[0]] {
		seg = seg[1:]
	}
	if len(seg) == 0 {
		return
	}
	// "${PYTHON:-python}" names the program through a variable with a default: judge
	// the program by the default (the child traces itself if it is a runtime).
	if m := varDefault.FindStringSubmatch(seg[0]); m != nil {
		seg = append([]string{m[1]}, seg[1:]...)
	}
	prog := filepath.Base(seg[0])
	if strings.Contains(seg[0], "$") {
		a.opaque(cwd)
		return
	}
	// A shell (running a -c string, a script file or stdin), or a script started
	// directly: analyze the shell code.
	if shells[prog] {
		a.shellInvocation(seg[1:], cwd, depth)
		return
	}
	if isScriptFile(seg[0], cwd) {
		a.script(seg[0], cwd, depth)
		return
	}
	traced := tracedPrograms[prog] || strings.HasPrefix(prog, "python3.")
	recursive := prog == "rg" || prog == "find"
	for i, w := range seg[1:] {
		if w == "-r" || w == "-R" || w == "--recursive" || (prog == "grep" && strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.ContainsAny(w, "rR")) {
			recursive = true
		}
		if w == "<" && i+2 < len(seg) && !strings.Contains(seg[i+2], "$") {
			for _, m := range expand(seg[i+2], cwd) {
				a.add(m)
			}
		}
	}
	unresolved := false
	for _, w := range seg[1:] {
		if strings.HasPrefix(w, "-") || w == "<" || w == ">" || w == ">>" {
			continue
		}
		if strings.Contains(w, "$") {
			unresolved = true
			continue
		}
		for _, m := range expand(w, cwd) {
			fi, err := os.Stat(m)
			switch {
			case err != nil:
			case fi.IsDir() && traced:
				// A runtime given a directory runs code that traces itself.
			case fi.IsDir() && (recursive || !readers[prog]):
				a.opaque(m)
			case fi.IsDir():
				if r, ok := a.rel(m); ok {
					a.out = append(a.out, depkey.Listing(r))
				}
			default:
				a.add(m)
			}
		}
	}
	switch {
	case traced:
		// The child traces itself.
	case nameOnly[prog]:
		// Only manipulates names, never opens files.
	case readers[prog] && !unresolved:
		// Reads only what it names.
	default:
		a.opaque(cwd)
	}
}

// shellInvocation analyzes a shell started with args (the words after its name):
// "-c text" (after any options, or combined as in -ec), a script file (its arguments
// are positional parameters, not files), or commands read from stdin.
func (a *execAnalysis) shellInvocation(args []string, cwd string, depth int) {
	cmd, stdin, endOpts := false, false, false
	operand, stdinFile := -1, ""
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "<":
			if i+1 < len(args) {
				stdinFile = strings.ReplaceAll(args[i+1], literalDollar, "$")
				i++
			}
		case w == ">" || w == ">>":
			i++
		case operand >= 0:
			// $0, $1, ...: positional parameters.
		case endOpts || len(w) < 2 || (w[0] != '-' && w[0] != '+'):
			operand = i
		case w == "--":
			endOpts = true
		case strings.HasPrefix(w, "--"):
			if w == "--rcfile" || w == "--init-file" {
				i++
			}
		default: // a cluster of short options: -e, -ec, -o pipefail, +x
			for _, c := range w[1:] {
				switch {
				case c == 'c' && w[0] == '-':
					cmd = true
				case c == 's' && w[0] == '-':
					stdin = true
				case c == 'o' || c == 'O':
					i++ // takes an argument
				}
			}
		}
	}
	var script string
	if operand >= 0 {
		script = strings.ReplaceAll(args[operand], literalDollar, "$")
	}
	switch {
	case cmd && operand < 0:
		a.opaque(cwd)
	case cmd:
		if depth >= 3 {
			a.opaque(cwd)
		} else {
			a.shellText(script, cwd, "", depth+1)
		}
	case operand >= 0 && !stdin:
		if strings.Contains(script, "$") {
			a.opaque(cwd)
		} else {
			a.script(script, cwd, depth)
		}
	case stdinFile != "" && !strings.Contains(stdinFile, "$"):
		a.script(stdinFile, cwd, depth) // sh < script.sh
		return
	default:
		a.opaque(cwd) // commands from stdin (a pipe, a terminal) or an unresolved script
		return
	}
	// Commands run by the shell may read its redirected stdin.
	switch {
	case stdinFile == "":
	case strings.Contains(stdinFile, "$"):
		a.opaque(cwd)
	default:
		for _, m := range expand(stdinFile, cwd) {
			a.add(m)
		}
	}
}

// script analyzes a shell script file (bounded recursion).
func (a *execAnalysis) script(name, cwd string, depth int) {
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	a.add(p)
	data, err := os.ReadFile(p)
	if err != nil || depth >= 3 || len(data) > 256*1024 {
		a.opaque(cwd)
		return
	}
	a.shellText(string(data), cwd, p, depth+1)
}

// isScriptFile reports whether a program path names a shell script in the repository.
func isScriptFile(prog, cwd string) bool {
	if !strings.Contains(prog, "/") {
		return false
	}
	p := prog
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 64)
	n, _ := f.Read(head)
	line := string(head[:n])
	if !strings.HasPrefix(line, "#!") {
		return strings.HasSuffix(p, ".sh")
	}
	for name := range shells {
		if strings.Contains(strings.SplitN(line, "\n", 2)[0], "/"+name) || strings.Contains(line, "env "+name) {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func trimAssignments(seg []string) []string {
	for len(seg) > 0 && strings.Contains(seg[0], "=") && !strings.HasPrefix(seg[0], "-") && !strings.Contains(strings.SplitN(seg[0], "=", 2)[0], "/") {
		seg = seg[1:]
	}
	return seg
}

// stripComments removes shell comments (a # starting a word, outside quotes).
func stripComments(text string) string {
	var b strings.Builder
	inS, inD := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		case c == '#' && !inS && !inD && (i == 0 || text[i-1] == ' ' || text[i-1] == '\t' || text[i-1] == '\n' || text[i-1] == ';'):
			for i < len(text) && text[i] != '\n' {
				i++
			}
			if i < len(text) {
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

var varDefault = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:?-([^}$]+)\}$`)

// literalDollar stands for a single-quoted "$" in words produced by splitShell.
const literalDollar = "\x01"

// expand resolves a command-line word relative to cwd, expanding glob patterns.
func expand(word, cwd string) []string {
	word = strings.ReplaceAll(word, literalDollar, "$")
	if word == "" {
		return nil
	}
	p := word
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if strings.ContainsAny(word, "*?[") {
		m, _ := filepath.Glob(p)
		return m
	}
	return []string{p}
}

// splitShell splits a shell command line into simple commands (on |, ||, &&, ;, &,
// newlines) and words, honoring quotes. Redirection operators become separate words.
func splitShell(cmd string) [][]string {
	var segs [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	flushWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	flushSeg := func() {
		flushWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\' && i+1 < len(cmd):
			i++
			cur.WriteByte(cmd[i])
			inWord = true
		case c == '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				j = len(cmd) - i - 1
			}
			// A $ inside single quotes is literal: mark it so it is not mistaken for an
			// unresolved variable (expand restores it).
			cur.WriteString(strings.ReplaceAll(cmd[i+1:i+1+j], "$", literalDollar))
			i += j + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(cmd) && cmd[i] != '"'; i++ {
				if cmd[i] == '\\' && i+1 < len(cmd) {
					i++
				}
				cur.WriteByte(cmd[i])
			}
			inWord = true
		case c == '&' && i+1 < len(cmd) && cmd[i+1] == '>':
			// &> file / &>> file: redirection, not a separator.
			flushWord()
			i++
			op := ">"
			if i+1 < len(cmd) && cmd[i+1] == '>' {
				i++
				op = ">>"
			}
			words = append(words, op)
		case c == '|' || c == ';' || c == '&' || c == '\n' || c == '(' || c == ')':
			flushSeg()
		case c == '<' || c == '>':
			// A file descriptor number right before the operator (2>file) is part of it.
			if inWord && isDigits(cur.String()) {
				cur.Reset()
				inWord = false
			}
			flushWord()
			op := string(c)
			if i+1 < len(cmd) && cmd[i+1] == '>' {
				op += ">"
				i++
			}
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				// >&2, 2>&1, <&0: duplicates a descriptor, no file involved.
				i++
				for i+1 < len(cmd) && (cmd[i+1] == '-' || (cmd[i+1] >= '0' && cmd[i+1] <= '9')) {
					i++
				}
				continue
			}
			words = append(words, op)
		case c == ' ' || c == '\t':
			flushWord()
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	flushSeg()
	return segs
}
