package trace

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dotneet/smates/internal/depkey"
)

// Child processes a test starts are traced directly when they are Node or Python
// processes (they inherit the shims). Everything else (shell scripts, grep, make, a
// compiled binary) is opaque, so dependencies are inferred from the command line:
//
//   - well-known commands that only read their file arguments (cat, grep, jq, ...)
//     depend on the repository files and directories named on the command line,
//     including shell globs (and the directories they search), on whether the named
//     paths that do not exist yet exist, and on the working directory when they name no
//     path (ls lists it, find, rg and grep -r search it);
//   - launchers (uv run, uvx, bunx) and the commands of find -exec are analyzed as the
//     command they run; sed scripts as the files their r commands read;
//   - shell code is followed through cd, pushd/popd, source, variables, for-loops, brace
//     expansion, case items, input redirections, command substitutions and the bodies of
//     the functions it calls (in the caller's directory);
//   - any other program may read anything below the directory it runs in, so the test
//     depends on that whole tree ("<dir>/**"), plus the files named on its command line
//     (the script itself) and the program when it is named by path (./build, ../bin/tool).
//
// This over-approximates on purpose: an opaque command is exactly where a dependency
// could hide.

// tracedPrograms trace themselves (or only start programs that do). npm, npx, pnpm and
// yarn are Node programs: the commands they start go through child_process and are
// analyzed there. Native launchers (uv run, uvx, bunx) execute their command directly
// and are analyzed as the command they run (launched).
var tracedPrograms = map[string]bool{
	"node": true, "nodejs": true, "bun": true, "python": true, "python3": true, "pypy3": true,
	"npm": true, "npx": true, "pnpm": true, "yarn": true,
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
// `uname -p` in every pytest-xdist worker, for example. tr only reads its standard input.
var nameOnly = map[string]bool{
	"basename": true, "dirname": true, "pwd": true, "echo": true, "printf": true, "true": true, "false": true,
	"uname": true, "hostname": true, "whoami": true, "id": true, "nproc": true, "arch": true,
	"getconf": true, "sw_vers": true, "tr": true,
}

// scriptFirst readers take a script or pattern as their first operand, unless an option
// gives one (grep -e, sed -f, awk -f ...).
var scriptFirst = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "sed": true, "awk": true, "jq": true, "yq": true,
}

// readerOpts lists, per reader, the options whose arguments are separate words, one
// letter per argument: 's' an inline script or pattern (the first operand is then a
// file), 'f' a script or pattern file, 'p' another file the command reads, 'd' a
// directory it reads files from, 'l' a file listing more files to read (NUL-separated),
// 'x' a program it runs, '%' a format (an argument only when it contains "%": BSD stat -f
// FORMAT, GNU stat -f takes none), 'o' anything else (a number, a separator, a name, an
// output file).
var readerOpts = map[string]map[string]string{
	"grep": grepOpts, "egrep": grepOpts, "fgrep": grepOpts,
	"rg": {
		"-e": "s", "--regexp": "s", "-f": "f", "--file": "f", "-g": "o", "--glob": "o", "--iglob": "o",
		"-t": "o", "--type": "o", "-T": "o", "--type-not": "o", "-m": "o", "--max-count": "o",
		"-A": "o", "-B": "o", "-C": "o", "--context": "o", "-r": "o", "--replace": "o", "-j": "o",
		"--threads": "o", "-M": "o", "--max-columns": "o", "-E": "o", "--encoding": "o",
		"--ignore-file": "p", "-d": "o", "--max-depth": "o", "--max-filesize": "o", "--sort": "o",
		"--sortr": "o", "--type-add": "o", "--colors": "o", "--path-separator": "o", "--pre": "x",
		"--pre-glob": "o",
	},
	"sed":    {"-e": "s", "--expression": "s", "-f": "f", "--file": "f"},
	"awk":    {"-f": "f", "-v": "o", "-F": "o"},
	"jq":     {"-f": "f", "--from-file": "f", "--arg": "oo", "--argjson": "oo", "--slurpfile": "op", "--rawfile": "op", "--indent": "o", "-L": "d"},
	"yq":     {"-f": "o", "--from-file": "f", "-o": "o", "--output-format": "o", "-p": "o", "--input-format": "o", "-I": "o", "--indent": "o"},
	"head":   {"-n": "o", "-c": "o"},
	"tail":   {"-n": "o", "-c": "o"},
	"sort":   {"-k": "o", "--key": "o", "-t": "o", "--field-separator": "o", "-o": "o", "--output": "o", "-S": "o", "-T": "o", "--files0-from": "l"},
	"wc":     {"--files0-from": "l"},
	"cut":    {"-d": "o", "-f": "o", "-c": "o", "-b": "o"},
	"uniq":   {"-f": "o", "-s": "o"},
	"diff":   {"-U": "o", "-C": "o", "-x": "o", "-X": "p", "-I": "o", "--ignore-matching-lines": "o"},
	"cmp":    {"-i": "o", "-n": "o"},
	"stat":   {"-c": "o", "-f": "%", "-t": "%"},
	"shasum": {"-a": "o"},
	"base64": {"-i": "p", "-o": "o"},
	"file":   {"-m": "p"},
}

var grepOpts = map[string]string{
	"-e": "s", "--regexp": "s", "-f": "f", "--file": "f", "-m": "o", "--max-count": "o",
	"-A": "o", "-B": "o", "-C": "o", "--context": "o", "-d": "o", "--directories": "o", "-D": "o", "--devices": "o",
	"--include": "o", "--exclude": "o", "--exclude-dir": "o", "--exclude-from": "p", "--label": "o",
	"--binary-files": "o", "--group-separator": "o",
}

// testFileOps are the test/[ operators whose operand is a path.
var testFileOps = map[string]bool{
	"-e": true, "-f": true, "-d": true, "-r": true, "-w": true, "-x": true, "-s": true, "-L": true, "-h": true,
	"-b": true, "-c": true, "-p": true, "-S": true, "-g": true, "-u": true, "-k": true, "-O": true, "-G": true,
	"-N": true, "-nt": true, "-ot": true, "-ef": true,
}

// awkIO matches awk programs that read or write files or run commands themselves.
var awkIO = regexp.MustCompile(`\bgetline\b|\bsystem[ \t]*\(|\bprintf?\b[^;}\n]*[>|]`)

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true}

// execDeps infers dependencies of a child process. It returns repo-relative keys:
// plain paths (content), "dir/" (listing), "dir/**" (whole tree) and "path?" (existence).
func execDeps(args []string, shell bool, cwd, root string, rel func(string) (string, bool)) []string {
	a := &execAnalysis{rel: rel, root: root, funcs: map[string]bool{}, bodies: map[string][][]string{}, calling: map[string]bool{}, shell: shell}
	if shell {
		a.shellText(strings.Join(args, " "), cwd, "", 0)
	} else {
		a.command(args, cwd, 0)
	}
	return a.out
}

type execAnalysis struct {
	rel     func(string) (string, bool)
	root    string
	out     []string
	funcs   map[string]bool       // shell functions defined by the analyzed scripts
	bodies  map[string][][]string // their bodies, analyzed where they are called
	calling map[string]bool       // functions whose body is being analyzed (recursion)
	calls   int                   // function bodies analyzed at a call (bounded)
	shell   bool                  // the words come from shell code (a shell expands their globs)
}

// maxCalls bounds the function calls whose bodies are analyzed; past it a call is opaque.
const maxCalls = 200

// shellState is what the next command of a script sees: its working directory, the
// variables assigned so far and the pushd stack; scopes saves them at "(" for ")".
type shellState struct {
	cwd    string
	vars   map[string][]string
	dirs   []string
	scopes []shellState
}

// subshell returns a copy of st that st's later changes do not affect.
func (st *shellState) subshell() *shellState {
	return &shellState{cwd: st.cwd, vars: maps.Clone(st.vars), dirs: slices.Clone(st.dirs)}
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

// listing records that the entries of dir are read.
func (a *execAnalysis) listing(dir string) {
	if r, ok := a.rel(dir); ok {
		a.out = append(a.out, depkey.Listing(r))
	} else if isRoot(dir, a.root) {
		a.out = append(a.out, depkey.Listing("."))
	}
}

// existence records that a command depends on whether p exists.
func (a *execAnalysis) existence(p string) {
	if r, ok := a.rel(p); ok {
		a.out = append(a.out, depkey.Existence(r))
	}
}

// glob records what the matches of a glob pattern (word, relative to cwd) depend on:
// the entries of the directory holding the pattern, or every file below the directory
// before the first pattern component when a directory component is a pattern too.
func (a *execAnalysis) glob(word, cwd string) {
	if !strings.ContainsAny(word, "*?[") {
		return // no pattern (quoted pattern characters are literal, see splitShell)
	}
	dir := filepath.Dir(filepath.Clean(word))
	tree := false
	for strings.ContainsAny(dir, "*?[") {
		dir, tree = filepath.Dir(dir), true
	}
	dir = literal(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	if tree {
		a.opaque(dir)
	} else {
		a.listing(dir)
	}
}

// shellText analyzes shell source (a -c string or a script file). script is the path of
// the script ("" for -c strings), used for $0 and $(dirname "$0").
func (a *execAnalysis) shellText(text, cwd, script string, depth int) {
	a.shell = true
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
	segs := splitShell(text)
	called := map[string]bool{}
	calledNames(segs, called)
	a.run(&shellState{cwd: cwd, vars: map[string][]string{}}, segs, called, depth)
}

// run analyzes the commands of a script in order. called holds the names the script
// runs as commands: the body of a function it calls is analyzed at each call, in the
// caller's state (a cd in it changes the caller's directory, as in the shell); the body
// of one it never calls by name (a trap, a function for the scripts sourcing this one)
// where it is defined, in a scope of its own.
func (a *execAnalysis) run(st *shellState, segs [][]string, called map[string]bool, depth int) {
	for k := 0; k < len(segs); k++ {
		if name, body, next, ok := funcDefAt(segs, k); ok {
			a.funcs[name], a.bodies[name] = true, body
			if !called[name] {
				a.run(st.subshell(), body, called, depth)
			}
			k = next - 1
			continue
		}
		seg := segs[k]
		switch seg[0] {
		case subshellOpen:
			// A ( ... ) subshell gets its own working directory and variables.
			st.scopes = append(st.scopes, shellState{cwd: st.cwd, vars: st.vars, dirs: st.dirs})
			st.vars, st.dirs = maps.Clone(st.vars), slices.Clone(st.dirs)
			continue
		case subshellClose:
			if n := len(st.scopes); n > 0 {
				s := st.scopes[n-1]
				st.cwd, st.vars, st.dirs, st.scopes = s.cwd, s.vars, s.dirs, st.scopes[:n-1]
			}
			continue
		}
		seg = expandVars(braceSeg(seg), st.vars)
		if len(seg) > 0 && isAssignments(seg) {
			for _, w := range seg {
				name, value, _ := strings.Cut(w, "=")
				st.vars[name] = []string{globChars.Replace(value)}
			}
			continue
		}
		if len(seg) >= 3 && seg[0] == "for" && seg[2] == "in" {
			// Quoting protects glob characters only where they are written: an unquoted
			// use of the variable expands them ($P with P='src/*.py').
			var vals []string
			for _, w := range seg[3:] {
				vals = append(vals, globChars.Replace(w))
			}
			st.vars[seg[1]] = vals
			continue
		}
		if w := trimKeywords(trimAssignments(seg)); len(w) > 0 && (a.funcs[w[0]] || strings.HasSuffix(w[0], "()")) {
			a.redirects(w, st.cwd)
			a.call(st, w[0], called, depth)
			continue
		}
		if a.builtin(seg, st, depth) {
			continue
		}
		a.command(seg, st.cwd, depth)
	}
}

// call analyzes the body of the shell function name called in state st.
func (a *execAnalysis) call(st *shellState, name string, called map[string]bool, depth int) {
	body, ok := a.bodies[name]
	switch {
	case !ok || a.calling[name]:
		// Defined where it cannot be followed (its body was analyzed there), or a
		// recursive call (the body is being analyzed).
	case a.calls >= maxCalls:
		a.opaque(st.cwd)
	default:
		a.calls++
		a.calling[name] = true
		a.run(st, body, called, depth)
		delete(a.calling, name)
	}
}

// funcDefAt recognizes a function definition starting at segs[k], "name() { ... }" or
// "function name { ... }" (splitShell makes "()" a subshell that opens and closes): it
// returns the function's name, the segments of its body (from "{" to the matching "}")
// and the index of the segment after it.
func funcDefAt(segs [][]string, k int) (name string, body [][]string, next int, ok bool) {
	seg, i := segs[k], k+1
	parens := func() bool {
		return i+1 < len(segs) && len(segs[i]) == 1 && segs[i][0] == subshellOpen && len(segs[i+1]) == 1 && segs[i+1][0] == subshellClose
	}
	var first []string
	switch {
	case len(seg) == 1 && funcName.MatchString(seg[0]) && !keywords[seg[0]] && parens():
		name, i = seg[0], i+2
	case len(seg) >= 2 && seg[0] == "function" && funcName.MatchString(seg[1]):
		name = seg[1]
		if len(seg) > 2 {
			first = seg[2:]
		} else if parens() {
			i += 2
		}
	default:
		return "", nil, 0, false
	}
	if first == nil {
		if i >= len(segs) {
			return "", nil, 0, false
		}
		first, i = segs[i], i+1
	}
	if first[0] != "{" {
		return "", nil, 0, false // a ( ... ) body or a malformed definition: analyzed in place
	}
	body = [][]string{first}
	for open := braceDepth(first); open > 0; i++ {
		if i >= len(segs) {
			return "", nil, 0, false
		}
		body = append(body, segs[i])
		open += braceDepth(segs[i])
	}
	return name, body, i, true
}

// braceDepth is how many { ... } groups the keywords that start seg open (or close).
func braceDepth(seg []string) int {
	n := 0
	for _, w := range seg {
		if !keywords[w] {
			break
		}
		switch w {
		case "{":
			n++
		case "}":
			n--
		}
	}
	return n
}

// calledNames adds to called the names the segments run as commands (outside function
// definitions, inside their bodies).
func calledNames(segs [][]string, called map[string]bool) {
	for k := 0; k < len(segs); k++ {
		if _, body, next, ok := funcDefAt(segs, k); ok {
			calledNames(body, called)
			k = next - 1
			continue
		}
		if w := trimKeywords(trimAssignments(segs[k])); len(w) > 0 {
			called[w[0]] = true
		}
	}
}

func trimKeywords(seg []string) []string {
	for len(seg) > 0 && keywords[seg[0]] {
		seg = seg[1:]
	}
	return seg
}

// redirects records the files the input redirections (< file) of a command read.
func (a *execAnalysis) redirects(seg []string, cwd string) {
	for i := 0; i+1 < len(seg); i++ {
		if seg[i] == "<" && !strings.Contains(seg[i+1], "$") {
			for _, m := range expand(seg[i+1], cwd) {
				a.add(m)
			}
		}
	}
}

var (
	funcDef = regexp.MustCompile(`(?m)^[ \t]*(?:function[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\([ \t]*\)`)
	// funcName matches the names of shell functions (bash also allows "-", "." and ":").
	funcName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]*$`)
	varRef   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)
	assign   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

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

// substitutions handles $(...) and `...` command substitutions: their commands are
// analyzed like any other command, following a cd among them; $(<file) reads file. The
// output is unknown (unresolvedSubst), except for $(pwd) in a script that never changes
// directory.
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
	st := &shellState{cwd: cwd, vars: map[string][]string{}}
	lost := false // after a cd to an unknown directory
	for _, seg := range splitShell(inner) {
		if len(seg) == 0 || seg[0] == subshellOpen || seg[0] == subshellClose {
			continue
		}
		seg = braceSeg(seg)
		w := trimKeywords(trimAssignments(seg))
		switch {
		case len(w) == 0:
		case w[0] == "cd" || w[0] == "pushd":
			// $(cd "$(dirname "$0")" && pwd) reads nothing; what follows the cd runs there.
			d, ok := cdTarget(w, st.cwd)
			st.cwd, lost = d, lost || !ok
		case w[0] == "<":
			a.redirects(w, st.cwd) // $(<file)
		case lost:
			if !nameOnly[filepath.Base(w[0])] {
				a.opaque(cwd)
			}
		case a.builtin(seg, st, depth):
		default:
			a.command(seg, st.cwd, depth)
		}
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
	"umask": true, "read": true, "break": true, "continue": true,
}

// builtin handles shell builtins (updating st); it reports whether the segment was fully
// handled.
func (a *execAnalysis) builtin(seg []string, st *shellState, depth int) bool {
	seg = trimAssignments(seg)
	for len(seg) > 0 && keywords[seg[0]] {
		if seg[0] == "case" {
			return true // case WORD in: splitShell leaves out the patterns
		}
		seg = seg[1:]
	}
	if len(seg) == 0 {
		return true
	}
	switch seg[0] {
	case "<", ">", ">>", "<<", "<<<":
		// Redirections of a compound command: "} < file", "done < list".
		a.redirects(seg, st.cwd)
		return true
	case "cd", "pushd":
		if seg[0] == "pushd" {
			st.dirs = append(st.dirs, st.cwd)
		}
		if d, ok := cdTarget(seg, st.cwd); ok {
			st.cwd = d
		} else {
			a.opaque(st.cwd)
		}
		return true
	case "popd":
		if n := len(st.dirs); n > 0 {
			st.cwd, st.dirs = st.dirs[n-1], st.dirs[:n-1]
		}
		return true
	case "source", ".":
		switch {
		case len(seg) < 2:
		case strings.Contains(seg[1], "$"):
			a.opaque(st.cwd) // unresolved path: cannot follow
		default:
			a.script(literal(seg[1]), st.cwd, depth)
		}
		return true
	}
	if noops[seg[0]] {
		a.redirects(seg, st.cwd) // read -r V < VERSION
		return true
	}
	return false
}

// cdTarget returns the directory a cd or pushd command (seg) run in cwd changes to; ok
// is false when it is unknown (no operand, a variable, "-", a pushd stack rotation).
func cdTarget(seg []string, cwd string) (string, bool) {
	args := seg[1:]
	for len(args) > 0 && (args[0] == "-L" || args[0] == "-P" || args[0] == "-e" || args[0] == "-@") {
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 || strings.Contains(args[0], "$") || args[0] == "-" || strings.HasPrefix(args[0], "+") ||
		strings.HasPrefix(args[0], "<") || strings.HasPrefix(args[0], ">") || (strings.HasPrefix(args[0], "-") && isDigits(args[0][1:])) {
		return "", false
	}
	d := literal(args[0])
	if !filepath.IsAbs(d) {
		d = filepath.Join(cwd, d)
	}
	return filepath.Clean(d), true
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
	// A native launcher executes its command itself: analyze that command, which traces
	// itself only when it runs on Node or Python.
	if cmd, dir, ok := launched(seg); ok {
		if strings.Contains(dir, "$") {
			a.opaque(cwd)
			return
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		if len(cmd) > 0 {
			switch {
			case strings.HasSuffix(cmd[0], ".py"):
				cmd = append([]string{"python"}, cmd...) // uv run script.py
			case strings.Contains(cmd[0], "/"):
				// A program named by path: a Node or Python script runs on its runtime
				// (and is read); anything else (a shell script, a binary) is analyzed
				// as itself.
				p := cmd[0]
				if !filepath.IsAbs(p) {
					p = filepath.Join(dir, p)
				}
				if rt := scriptRuntime(p, false); rt != "" {
					cmd = append([]string{rt}, cmd...)
				}
			default:
				if rt := a.consoleScript(cmd[0], dir); rt != "" {
					cmd = append([]string{rt}, cmd[1:]...)
				}
			}
			a.command(cmd, dir, depth)
		}
		return
	}
	traced := tracedPrograms[prog] || strings.HasPrefix(prog, "python3.")
	// A program named by its path (./build, ../bin/tool, /repo/bin/tool) is read to run
	// it, wherever the command runs; one that is missing may appear. A bare name is looked
	// up in PATH.
	if !traced && strings.Contains(seg[0], "/") {
		p := literal(seg[0])
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			a.add(p)
		} else if err != nil && !within(p, cwd) {
			a.existence(p) // below cwd, the tree of the opaque command covers it
		}
	}
	grepLike := prog == "grep" || prog == "egrep" || prog == "fgrep"
	recursive := prog == "rg" || prog == "find"
	for i, w := range seg[1:] {
		cluster := strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--")
		switch {
		case prog == "ls":
			// ls -r reverses the order; only -R recurses.
			recursive = recursive || w == "--recursive" || (cluster && strings.Contains(w, "R"))
		case w == "-r" || w == "-R" || w == "--recursive" || (grepLike && cluster && strings.ContainsAny(w, "rR")):
			recursive = true
		case grepLike && (w == "-drecurse" || w == "--directories=recurse" ||
			((w == "-d" || w == "--directories") && i+2 < len(seg) && seg[i+2] == "recurse")):
			recursive = true // grep -d recurse is grep -r
		}
	}
	a.redirects(seg, cwd)
	args := operands(prog, seg[1:])
	// --files0-from lists the files to read, NUL-separated.
	for _, l := range args.lists {
		if l == "-" || strings.Contains(l, "$") {
			args.runs = true // names read from stdin, or an unresolved list
			continue
		}
		for _, m := range expand(l, cwd) {
			a.add(m)
			data, _ := os.ReadFile(m)
			for n, name := range strings.Split(string(data), "\x00") {
				if name != "" && n < 10000 {
					// Names in the list are literal: no glob, no variable.
					args.files = append(args.files, quoteMarks.Replace(strings.ReplaceAll(name, "$", literalDollar)))
				}
			}
		}
	}
	// find -exec runs a command for the files found: {} stands for them, below the
	// paths searched (recorded as trees). -execdir runs it in the directory of each.
	for _, x := range args.execs {
		roots := args.roots
		if len(roots) == 0 {
			roots = []string{"."}
		}
		if x.dir {
			for _, r := range roots {
				for _, d := range expand(r, cwd) {
					a.command(replacePlaceholder(x.cmd, []string{"."}), d, depth)
				}
			}
		} else {
			a.command(replacePlaceholder(x.cmd, roots), cwd, depth)
		}
	}
	// Without a path, ls lists and find, rg and grep -r search the working directory.
	if len(args.files) == 0 && !traced {
		switch {
		case prog == "ls" && !recursive:
			a.listing(cwd)
		case prog == "ls" || prog == "find" || prog == "rg" || (grepLike && recursive):
			a.opaque(cwd)
		}
	}
	// A script that reads files or runs commands itself (awk's getline and system(),
	// yq's load, rg --pre) makes the reader opaque; so does an unresolved awk or sed
	// script.
	scripted := args.runs
	switch prog {
	case "awk":
		scripted = scripted || awkIO.MatchString(literal(args.script))
		for _, f := range args.scriptFiles {
			for _, m := range expand(f, cwd) {
				if data, err := os.ReadFile(m); err == nil && awkIO.Match(data) {
					scripted = true
				}
			}
		}
	case "sed":
		// The r and R commands read a file; e and s///e run commands.
		scripts := []string{literal(args.script)}
		for _, f := range args.scriptFiles {
			for _, m := range expand(f, cwd) {
				if data, err := os.ReadFile(m); err == nil {
					scripts = append(scripts, string(data))
				}
			}
		}
		for _, s := range scripts {
			reads, runs := sedScript(s)
			scripted = scripted || runs
			for _, r := range reads {
				if strings.Contains(r, "$") {
					scripted = true
					continue
				}
				if !filepath.IsAbs(r) {
					r = filepath.Join(cwd, r)
				}
				a.add(r)
			}
		}
	case "yq":
		scripted = strings.Contains(args.script, "load")
	}
	unresolved := (prog == "awk" || prog == "sed") && strings.Contains(args.script, "$")
	for _, w := range args.dirs {
		if strings.Contains(w, "$") {
			unresolved = true
			continue
		}
		for _, m := range expand(w, cwd) {
			a.opaque(m)
		}
	}
	for _, w := range args.files {
		if strings.Contains(w, "$") {
			unresolved = true
			continue
		}
		// The shell expands an unquoted glob into the files it matches: the command
		// line changes with the directory. Programs that only print names are left out
		// (echo * output is not a read).
		if a.shell && !nameOnly[prog] {
			a.glob(w, cwd)
		}
		for _, m := range expand(w, cwd) {
			fi, err := os.Stat(m)
			switch {
			case err != nil:
				// A path that does not exist (yet): the command behaves differently once
				// it does. Paths below the directory of an opaque command are covered by
				// its tree.
				if !traced && !nameOnly[prog] && (readers[prog] || !within(m, cwd)) {
					a.existence(m)
				}
			case fi.IsDir() && traced:
				// A runtime given a directory runs code that traces itself.
			case fi.IsDir() && (recursive || !readers[prog] || prog == "diff"):
				// diff of two directories compares the files they have in common.
				a.opaque(m)
			case fi.IsDir():
				a.listing(m)
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
	case readers[prog] && !unresolved && !scripted:
		// Reads only what it names.
	default:
		a.opaque(cwd)
	}
}

// cmdArgs is what the arguments of a command name.
type cmdArgs struct {
	files       []string   // operands and file arguments of options (not redirection targets)
	dirs        []string   // directories it reads files from (jq -L)
	lists       []string   // files listing, NUL-separated, more files to read (--files0-from)
	script      string     // the inline script or pattern of a scriptFirst reader (-e ones too)
	scriptFiles []string   // the files a scriptFirst reader reads its script from
	runs        bool       // an option runs another program (rg --pre)
	roots       []string   // the paths find searches
	execs       []findExec // the commands find runs
}

// findExec is a command find runs: -exec / -ok (dir: -execdir / -okdir).
type findExec struct {
	cmd []string
	dir bool
}

// replacePlaceholder replaces find's {} in the words of cmd by each of paths.
func replacePlaceholder(cmd, paths []string) []string {
	var out []string
	for _, w := range cmd {
		if !strings.Contains(w, "{}") {
			out = append(out, w)
			continue
		}
		for _, p := range paths {
			out = append(out, strings.ReplaceAll(w, "{}", p))
		}
	}
	return out
}

// operands parses the arguments of a command.
func operands(prog string, args []string) (c cmdArgs) {
	switch prog {
	case "find":
		// find [-H|-L|-P] path... expression: the expression names no file to read
		// except the reference files of -newer and -samefile, the list of -files0-from
		// and what the commands of -exec and -ok read.
		paths := true
		for i := 0; i < len(args); i++ {
			w := args[i]
			switch {
			case w == "<" || w == ">" || w == ">>" || w == "<<" || w == "<<<":
				paths = false
			case paths && (w == "-H" || w == "-L" || w == "-P"):
			case w == "-exec" || w == "-execdir" || w == "-ok" || w == "-okdir":
				paths = false
				// The command ends at ";" or, after {}, at "+".
				j := i + 1
				for j < len(args) && args[j] != ";" && (args[j] != "+" || args[j-1] != "{}") {
					j++
				}
				if j > i+1 {
					c.execs = append(c.execs, findExec{cmd: args[i+1 : j], dir: strings.HasSuffix(w, "dir")})
				}
				i = j
			case strings.HasPrefix(w, "-") || w == "!" || w == "(":
				paths = false
				switch {
				case i+1 >= len(args):
				case strings.HasPrefix(w, "-newer") || w == "-anewer" || w == "-cnewer" || w == "-samefile":
					c.files = append(c.files, args[i+1])
				case w == "-files0-from":
					c.lists = append(c.lists, args[i+1])
				}
			case paths:
				c.files = append(c.files, w)
				c.roots = append(c.roots, w)
			}
		}
		return c
	case "test", "[":
		// Only the operands of file operators are paths: [ -f x ], [ a -nt b ].
		for i, w := range args {
			if testFileOps[w] && i+1 < len(args) && args[i+1] != "]" && args[i+1] != "]]" {
				c.files = append(c.files, args[i+1])
			}
			if (w == "-nt" || w == "-ot" || w == "-ef") && i > 0 {
				c.files = append(c.files, args[i-1])
			}
		}
		return c
	}
	opts := readerOpts[prog]
	needScript := scriptFirst[prog]
	// take consumes the arguments of an option (kinds, see readerOpts); the first one is
	// inline when the option word carries it (-n5, --file=x).
	var i int
	take := func(kinds, inline string, hasInline bool) {
		for k, kind := range kinds {
			arg := inline
			if k > 0 || !hasInline {
				if i+1 >= len(args) || (kind == '%' && !strings.Contains(args[i+1], "%")) {
					return
				}
				i++
				arg = args[i]
			}
			switch kind {
			case 's':
				needScript = false
				if c.script != "" {
					c.script += "\n"
				}
				c.script += arg
			case 'l':
				c.lists = append(c.lists, arg)
			case 'f':
				needScript = false
				c.files = append(c.files, arg)
				c.scriptFiles = append(c.scriptFiles, arg)
			case 'p':
				c.files = append(c.files, arg)
			case 'd':
				c.dirs = append(c.dirs, arg)
			case 'x':
				c.runs = true
			}
		}
	}
	end := false
	for i = 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "<" || w == ">" || w == ">>" || w == "<<" || w == "<<<":
			i++ // standard input (recorded by the caller), an output, a here-document
			continue
		case end || w == "-" || !strings.HasPrefix(w, "-"):
		case w == "--":
			end = true
			continue
		case strings.HasPrefix(w, "--"):
			if prog == "rg" && (w == "--files" || w == "--type-list") {
				needScript = false
			}
			name, val, ok := strings.Cut(w, "=")
			take(opts[name], val, ok)
			continue
		default:
			// A cluster of short options; the first one taking an argument takes the rest
			// of the word (-n5) or the next word.
			for j := 1; j < len(w); j++ {
				if kinds := opts["-"+w[j:j+1]]; kinds != "" {
					take(kinds, w[j+1:], j+1 < len(w))
					break
				}
			}
			continue
		}
		switch {
		case w == "" || w == "-":
			// BSD sed -i '', standard input
		case needScript && prog == "yq" && (w == "eval" || w == "e" || w == "eval-all" || w == "ea"):
		case needScript:
			needScript = false
			c.script = w
		case prog == "awk" && assign.MatchString(w):
			// var=value operands set awk variables
		default:
			c.files = append(c.files, w)
		}
	}
	return c
}

// launcherOpts are the options of uv (global, run and tool run options) and bun that
// take a separate argument.
var launcherOpts = map[string]bool{
	"--directory": true, "--project": true, "--cache-dir": true, "--config-file": true, "--color": true,
	"--python": true, "-p": true, "--with": true, "-w": true, "--with-editable": true,
	"--with-requirements": true, "--from": true, "--package": true, "--extra": true, "--group": true,
	"--no-group": true, "--only-group": true, "--env-file": true, "--index": true, "--default-index": true,
	"--index-url": true, "-i": true, "--extra-index-url": true, "--find-links": true, "-f": true,
	"--index-strategy": true, "--keyring-provider": true, "--resolution": true, "--prerelease": true,
	"--exclude-newer": true, "--link-mode": true, "--refresh-package": true, "--reinstall-package": true,
	"--upgrade-package": true, "-P": true, "--config-setting": true, "-C": true,
	"--no-binary-package": true, "--no-build-package": true, "--allow-insecure-host": true,
	"--python-preference": true, "--python-platform": true, "--cwd": true,
}

// launched returns the command a native launcher runs (uv run, uv tool run, uvx, bunx,
// bun x) and the directory it runs in, relative to the launcher's ("" for the same);
// ok is false when seg does not start one. uv run --module and --script run Python.
func launched(seg []string) (cmd []string, dir string, ok bool) {
	prog, args := filepath.Base(seg[0]), seg[1:]
	py := ""
	// options skips the launcher options at the start of args.
	options := func() {
		for len(args) > 0 {
			w := args[0]
			if w == "--" {
				args = args[1:]
				return
			}
			if !strings.HasPrefix(w, "-") || w == "-" {
				return
			}
			args = args[1:]
			name, val, inline := strings.Cut(w, "=")
			switch {
			case prog == "uv" && (w == "-m" || w == "--module"):
				py = "-m"
			case prog == "uv" && (w == "-s" || w == "--script" || w == "--gui-script"):
				py = "script"
			case !launcherOpts[name]:
			case !inline && len(args) == 0:
				return
			default:
				if !inline {
					val, args = args[0], args[1:]
				}
				if name == "--directory" || name == "--cwd" {
					dir = filepath.Join(dir, literal(val)) // relative to the previous one, or absolute
					if filepath.IsAbs(val) {
						dir = literal(val)
					}
				}
			}
		}
	}
	switch prog {
	case "uvx", "bunx":
	case "uv", "bun":
		options()
		switch {
		case prog == "uv" && len(args) > 0 && args[0] == "run":
			args = args[1:]
		case prog == "uv" && len(args) > 1 && args[0] == "tool" && args[1] == "run":
			args = args[2:]
		case prog == "bun" && len(args) > 0 && args[0] == "x":
			args = args[1:]
		default:
			return nil, "", false
		}
	default:
		return nil, "", false
	}
	options()
	switch {
	case len(args) == 0:
	case py == "-m":
		args = append([]string{"python", "-m"}, args...)
	case py == "script":
		args = append([]string{"python"}, args...)
	}
	return args, dir, true
}

// consoleScript returns the runtime of the console script name in a .venv/bin or
// node_modules/.bin from dir up to the repository root (see scriptRuntime), or "" when
// there is none (a native tool, one the launcher downloads).
func (a *execAnalysis) consoleScript(name, dir string) string {
	for d := dir; ; {
		for _, c := range []string{filepath.Join(d, ".venv", "bin", name), filepath.Join(d, "node_modules", ".bin", name)} {
			if _, err := os.Stat(c); err == nil {
				return scriptRuntime(c, true)
			}
		}
		parent := filepath.Dir(d)
		if d == a.root || parent == d || !within(d, a.root) {
			return ""
		}
		d = parent
	}
}

// scriptRuntime returns the runtime ("python", "node", "bun") the #! line of the file p
// names, or "" (a native binary, a shell script, a missing file). With wrappers, the
// shell wrappers package managers install are recognized too: pnpm's node_modules/.bin
// shims and uv's trampoline for long interpreter paths.
func scriptRuntime(p string, wrappers bool) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	head := make([]byte, 1024)
	n, _ := f.Read(head)
	f.Close()
	text := string(head[:n])
	line, _, _ := strings.Cut(text, "\n")
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if !strings.HasPrefix(line, "#!") || len(fields) == 0 {
		return ""
	}
	interp := filepath.Base(fields[0])
	if interp == "env" {
		for _, w := range fields[1:] {
			if !strings.HasPrefix(w, "-") {
				interp = filepath.Base(w)
				break
			}
		}
	}
	switch {
	case strings.HasPrefix(interp, "python") || strings.HasPrefix(interp, "pypy"):
		return "python"
	case interp == "node" || interp == "nodejs":
		return "node"
	case interp == "bun":
		return "bun"
	case !wrappers || !shells[interp]:
		return ""
	case strings.Contains(text, "exec node ") || strings.Contains(text, `/node" `):
		return "node"
	case strings.Contains(text, "'''exec'") && strings.Contains(text, "python"):
		return "python"
	}
	return ""
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
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
				stdinFile = literal(args[i+1])
				i++
			}
		case w == ">" || w == ">>" || w == "<<" || w == "<<<":
			i++ // an output, a here-document or here-string (stdin: commands, unless a script is named)
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
		script = literal(args[operand])
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

// Quoted "$", "*", "?", "[", "{" and "}" in words produced by splitShell are marked so
// they are not mistaken for an unresolved variable, a glob pattern or a brace expansion;
// literal restores them.
const (
	literalDollar = "\x01"
	literalStar   = "\x03"
	literalQuery  = "\x04"
	literalSet    = "\x05"
	literalOpen   = "\x06"
	literalClose  = "\x07"
)

var (
	quoteMarks   = strings.NewReplacer("*", literalStar, "?", literalQuery, "[", literalSet, "{", literalOpen, "}", literalClose)
	unquoteMarks = strings.NewReplacer(literalDollar, "$", literalStar, "*", literalQuery, "?", literalSet, "[", literalOpen, "{", literalClose, "}")
	// globChars makes quoted glob characters active again (a variable used unquoted);
	// braces stay marked: the shell expands braces before variables.
	globChars = strings.NewReplacer(literalStar, "*", literalQuery, "?", literalSet, "[") // keeps literalDollar
)

func literal(word string) string { return unquoteMarks.Replace(word) }

// maxBraceWords bounds the words one brace expansion makes.
const maxBraceWords = 1024

// braceSeg brace-expands the words of a command (bash, zsh and ksh do, and /bin/sh is
// bash on macOS; dash would open a file literally named "{a,b}", which nobody creates).
// Assignments are not expanded.
func braceSeg(seg []string) []string {
	var out []string
	for _, w := range seg {
		if assign.MatchString(w) {
			out = append(out, w)
			continue
		}
		out = append(out, braceExpand(w)...)
	}
	return out
}

// braceExpand expands the unquoted brace expressions of a word, {a,b} and {1..3} or
// {a..e} (with an increment), as the shell does before any other expansion; "{}", "{x}"
// and ${...} stay as they are. Quoted braces are marked (see splitShell).
func braceExpand(w string) []string {
	for i := 0; i < len(w); i++ {
		if w[i] != '{' {
			continue
		}
		j := matchBrace(w, i)
		if j < 0 {
			return []string{w}
		}
		if i > 0 && w[i-1] == '$' {
			i = j // ${...}
			continue
		}
		alts := braceAlternatives(w[i+1 : j])
		if alts == nil {
			continue // literal braces; the ones inside may still expand
		}
		var out []string
		for _, alt := range alts {
			out = append(out, braceExpand(w[:i]+alt+w[j+1:])...)
			if len(out) >= maxBraceWords {
				return out[:maxBraceWords]
			}
		}
		return out
	}
	return []string{w}
}

// matchBrace returns the index of the "}" closing the "{" at w[i], or -1.
func matchBrace(w string, i int) int {
	depth := 0
	for j := i; j < len(w); j++ {
		switch w[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

var braceRange = regexp.MustCompile(`^(-?[0-9]+|[A-Za-z])\.\.(-?[0-9]+|[A-Za-z])(?:\.\.(-?[0-9]+))?$`)

// braceAlternatives returns the words a brace expression's body (between the braces)
// stands for: its comma-separated parts or the elements of its sequence; nil when it is
// neither.
func braceAlternatives(body string) []string {
	var parts []string
	depth, start := 0, 0
	for k := 0; k < len(body); k++ {
		switch body[k] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:k])
				start = k + 1
			}
		}
	}
	if parts != nil {
		return append(parts, body[start:])
	}
	m := braceRange.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	step := 1
	if m[3] != "" {
		step, _ = strconv.Atoi(m[3])
		if step < 0 {
			step = -step
		}
		if step == 0 {
			step = 1
		}
	}
	from, errFrom := strconv.Atoi(m[1])
	to, errTo := strconv.Atoi(m[2])
	letters := errFrom != nil && errTo != nil
	switch {
	case letters:
		from, to = int(m[1][0]), int(m[2][0])
	case errFrom != nil || errTo != nil:
		return nil // {1..a}
	}
	// {01..10}: a leading zero pads every number to the widest end.
	width := 0
	if !letters && (strings.HasPrefix(strings.TrimPrefix(m[1], "-"), "0") && len(strings.TrimPrefix(m[1], "-")) > 1 ||
		strings.HasPrefix(strings.TrimPrefix(m[2], "-"), "0") && len(strings.TrimPrefix(m[2], "-")) > 1) {
		width = max(len(m[1]), len(m[2]))
	}
	var out []string
	for v := from; len(out) < maxBraceWords; {
		switch {
		case letters:
			out = append(out, string(rune(v)))
		case width > 0:
			out = append(out, fmt.Sprintf("%0*d", width, v))
		default:
			out = append(out, strconv.Itoa(v))
		}
		if v == to || (from < to && v+step > to) || (from > to && v-step < to) {
			break
		}
		if from < to {
			v += step
		} else {
			v -= step
		}
	}
	return out
}

// sedScript parses a sed script: it returns the files its r and R commands read, and
// whether it runs commands (e, the e flag of s) or could not be parsed.
func sedScript(s string) (reads []string, runs bool) {
	i := 0
	skip := func(set string) {
		for i < len(s) && strings.IndexByte(set, s[i]) >= 0 {
			i++
		}
	}
	// toEOL returns the rest of the line (a file name, a command, a label).
	toEOL := func() string {
		start := i
		for i < len(s) && s[i] != '\n' {
			i++
		}
		return s[start:i]
	}
	// delimited skips a regular expression or replacement ending at delim; brackets
	// protect it in regular expressions.
	delimited := func(delim byte, regex bool) bool {
		for ; i < len(s); i++ {
			switch {
			case s[i] == '\\':
				i++
			case regex && s[i] == '[':
				j := i + 1
				if j < len(s) && s[j] == '^' {
					j++
				}
				if j < len(s) && s[j] == ']' {
					j++
				}
				for j < len(s) && s[j] != ']' {
					j++
				}
				i = j
			case s[i] == delim:
				i++
				return true
			}
		}
		return false
	}
	address := func() bool {
		switch {
		case i >= len(s):
			return false
		case s[i] >= '0' && s[i] <= '9':
			skip("0123456789")
			if i < len(s) && s[i] == '~' {
				i++
				skip("0123456789")
			}
		case s[i] == '$':
			i++
		case s[i] == '/':
			i++
			if !delimited('/', true) {
				return false
			}
			skip("IM")
		case s[i] == '\\' && i+1 < len(s):
			d := s[i+1]
			i += 2
			if !delimited(d, true) {
				return false
			}
			skip("IM")
		}
		return true
	}
	for {
		skip(" \t\n;")
		if i >= len(s) {
			return reads, runs
		}
		if !address() {
			return reads, true
		}
		skip(" \t")
		if i < len(s) && s[i] == ',' {
			i++
			skip(" \t")
			if i < len(s) && (s[i] == '+' || s[i] == '~') {
				i++
				skip("0123456789")
			} else if !address() {
				return reads, true
			}
		}
		skip(" \t!")
		if i >= len(s) {
			return reads, true
		}
		cmd := s[i]
		i++
		switch cmd {
		case '{', '}', '=', 'd', 'D', 'g', 'G', 'h', 'H', 'n', 'N', 'p', 'P', 'x', 'z', 'F':
		case '#':
			toEOL()
		case ':', 'b', 't', 'T', 'v':
			for i < len(s) && s[i] != '\n' && s[i] != ';' {
				i++
			}
		case 'a', 'i', 'c':
			// Text to the end of the line; a line ending in a backslash continues it.
			for {
				line := toEOL()
				if !strings.HasSuffix(line, "\\") || i >= len(s) {
					break
				}
				i++
			}
		case 'r', 'R', 'w', 'W':
			skip(" \t")
			name := toEOL()
			if (cmd == 'r' || cmd == 'R') && name != "" {
				reads = append(reads, name)
			}
		case 'e':
			toEOL()
			runs = true
		case 'l', 'L', 'q', 'Q':
			skip(" \t")
			skip("0123456789")
		case 's', 'y':
			if i >= len(s) || s[i] == '\\' || s[i] == '\n' {
				return reads, true
			}
			d := s[i]
			i++
			if !delimited(d, cmd == 's') || !delimited(d, false) {
				return reads, true
			}
			for cmd == 's' && i < len(s) && strings.IndexByte("gpiImMe0123456789w", s[i]) >= 0 {
				if s[i] == 'e' {
					runs = true
				}
				if s[i] == 'w' {
					toEOL() // writes a file
					break
				}
				i++
			}
		default:
			return reads, true
		}
	}
}

// expand resolves a command-line word relative to cwd, expanding glob patterns.
func expand(word, cwd string) []string {
	pattern := strings.ContainsAny(word, "*?[")
	word = literal(word)
	if word == "" {
		return nil
	}
	p := word
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if pattern {
		m, _ := filepath.Glob(p)
		return m
	}
	return []string{p}
}

// arithmetic reports whether cmd[i:] starts a (( expression )) command: "((" not right
// after another "(" (nested subshells), closed by "))" with no parenthesis between.
func arithmetic(cmd string, i int) bool {
	if !strings.HasPrefix(cmd[i:], "((") {
		return false
	}
	if before := strings.TrimRight(cmd[:i], " \t"); strings.HasSuffix(before, "(") {
		return false
	}
	end := strings.Index(cmd[i+2:], "))")
	return end >= 0 && !strings.ContainsAny(cmd[i+2:i+2+end], "()")
}

// Segments of splitShell that open and close a ( ... ) subshell.
const (
	subshellOpen  = "\x02("
	subshellClose = "\x02)"
)

// splitShell splits a shell command line into simple commands (on |, ||, &&, ;, &,
// newlines) and words, honoring quotes. Redirection operators become separate words;
// parentheses become subshellOpen and subshellClose segments. The patterns of case
// items are left out.
func splitShell(cmd string) [][]string {
	var segs [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	// cases counts the case commands open; pattern is set where the patterns of an item
	// start (after "case WORD in", ";;", ";&" or ";;&"). They run nothing, and the ")"
	// ending them closes no subshell.
	cases, pattern := 0, false
	flushWord := func() {
		if inWord {
			w := cur.String()
			words = append(words, w)
			cur.Reset()
			inWord = false
			switch n := len(words); {
			case w == "in" && n >= 3 && words[n-3] == "case":
				cases++
				pattern = true
			case w == "esac" && cases > 0 && len(trimKeywords(words)) == 0:
				cases--
			}
		}
	}
	flushSeg := func() {
		flushWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}
	type heredoc struct {
		delim string
		tabs  bool // <<-: leading tabs are stripped
	}
	var heredocs []heredoc // here-documents whose bodies follow the current line
	for i := 0; i < len(cmd); i++ {
		if pattern {
			flushSeg()
			pattern = false
			j := i
			for j < len(cmd) && strings.IndexByte(" \t\n", cmd[j]) >= 0 {
				j++
			}
			esac := strings.HasPrefix(cmd[j:], "esac") && (j+4 == len(cmd) || strings.IndexByte(" \t\n;|&)", cmd[j+4]) >= 0)
			if end := patternEnd(cmd, j); !esac && end >= 0 {
				i = end
				continue
			}
		}
		c := cmd[i]
		switch {
		case c == '\\' && i+1 < len(cmd) && cmd[i+1] == '\n':
			i++ // line continuation
		case c == '\\' && i+1 < len(cmd):
			i++
			cur.WriteString(quoteMarks.Replace(cmd[i : i+1]))
			inWord = true
		case c == '\\':
			// a trailing backslash escapes nothing
		case c == '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				j = len(cmd) - i - 1
			}
			// $ and glob characters inside single quotes are literal: mark them so they
			// are not mistaken for an unresolved variable or a pattern (literal restores
			// them).
			cur.WriteString(quoteMarks.Replace(strings.ReplaceAll(cmd[i+1:i+1+j], "$", literalDollar)))
			i += j + 1
			inWord = true
		case c == '"':
			i++
			param := 0 // inside ${...}, whose braces are not marked
			for ; i < len(cmd) && cmd[i] != '"'; i++ {
				switch {
				case cmd[i] == '\\' && i+1 < len(cmd):
					i++
					if cmd[i] == '\n' {
						continue // line continuation
					}
				case cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '{':
					cur.WriteString("${")
					i++
					param++
					continue
				case param > 0 && (cmd[i] == '{' || cmd[i] == '}'):
					if cmd[i] == '{' {
						param++
					} else {
						param--
					}
					cur.WriteByte(cmd[i])
					continue
				}
				cur.WriteString(quoteMarks.Replace(cmd[i : i+1]))
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
		case c == '(' && arithmetic(cmd, i) && !inWord && len(words) == 0:
			// (( expression )): reads no file; its << and < are operators.
			flushSeg()
			i += 2 + strings.Index(cmd[i+2:], "))") + 1
		case c == '(' || c == ')':
			flushSeg()
			if c == '(' {
				segs = append(segs, []string{subshellOpen})
			} else {
				segs = append(segs, []string{subshellClose})
			}
		case c == '\n' && len(heredocs) > 0:
			// Skip the bodies of the here-documents started on this line, when each ends
			// with its delimiter line (otherwise it is not one: keep parsing).
			flushSeg()
			j := i
			for _, h := range heredocs {
				found := false
				for !found && j < len(cmd) {
					line := cmd[j+1:]
					end := strings.IndexByte(line, '\n')
					if end < 0 {
						end = len(line)
					}
					line = line[:end]
					j += end + 1
					if h.tabs {
						line = strings.TrimLeft(line, "\t")
					}
					found = line == h.delim
				}
				if !found {
					j = i
					break
				}
			}
			i = j
			heredocs = nil
		case c == ';' && cases > 0 && i+1 < len(cmd) && (cmd[i+1] == ';' || cmd[i+1] == '&'):
			// ;; ;& ;;& end a case item: the next one starts with its patterns.
			flushSeg()
			i++
			if cmd[i] == ';' && i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
			}
			pattern = true
		case c == '|' || c == ';' || c == '&' || c == '\n':
			flushSeg()
		case c == '<' && strings.HasPrefix(cmd[i:], "<<<"):
			// <<< word: a here-string, the word is the input.
			flushWord()
			i += 2
			words = append(words, "<<<")
		case c == '<' && strings.HasPrefix(cmd[i:], "<<"):
			// <<DELIM, <<-DELIM, <<'DELIM': a here-document; its body follows the line.
			flushWord()
			i += 2
			h := heredoc{}
			if i < len(cmd) && cmd[i] == '-' {
				h.tabs = true
				i++
			}
			for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t') {
				i++
			}
			var d strings.Builder
			for ; i < len(cmd) && !strings.ContainsRune(" \t\n;|&<>()", rune(cmd[i])); i++ {
				if cmd[i] != '\'' && cmd[i] != '"' && cmd[i] != '\\' {
					d.WriteByte(cmd[i])
				}
			}
			i--
			h.delim = d.String()
			heredocs = append(heredocs, h)
			words = append(words, "<<", h.delim)
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

// patternEnd returns the index of the unquoted ")" ending the patterns of a case item
// that start at cmd[i], or -1.
func patternEnd(cmd string, i int) int {
	for ; i < len(cmd); i++ {
		switch cmd[i] {
		case '\\':
			i++
		case '\'', '"':
			j := strings.IndexByte(cmd[i+1:], cmd[i])
			if j < 0 {
				return -1
			}
			i += j + 1
		case ')':
			return i
		}
	}
	return -1
}
