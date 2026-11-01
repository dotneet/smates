package trace

import (
	"maps"
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
//     including shell globs (and the directories they search), on whether the named
//     paths that do not exist yet exist, and on the working directory when they name no
//     path (ls lists it, find, rg and grep -r search it);
//   - launchers (uv run, uvx, bunx) are analyzed as the command they run;
//   - any other program may read anything below the directory it runs in, so the test
//     depends on that whole tree ("<dir>/**"), plus the files named on its command line
//     (the script itself).
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
// file), 'f' a script or pattern file, 'p' another file the command reads, 'o' anything
// else (a number, a separator, a name, an output file).
var readerOpts = map[string]map[string]string{
	"grep": grepOpts, "egrep": grepOpts, "fgrep": grepOpts,
	"rg": {
		"-e": "s", "--regexp": "s", "-f": "f", "--file": "f", "-g": "o", "--glob": "o", "--iglob": "o",
		"-t": "o", "--type": "o", "-T": "o", "--type-not": "o", "-m": "o", "--max-count": "o",
		"-A": "o", "-B": "o", "-C": "o", "--context": "o", "-r": "o", "--replace": "o", "-j": "o",
		"--threads": "o", "-M": "o", "--max-columns": "o", "-E": "o", "--encoding": "o",
	},
	"sed":    {"-e": "s", "--expression": "s", "-f": "f", "--file": "f"},
	"awk":    {"-f": "f", "-v": "o", "-F": "o"},
	"jq":     {"-f": "f", "--from-file": "f", "--arg": "oo", "--argjson": "oo", "--slurpfile": "op", "--rawfile": "op", "--indent": "o"},
	"yq":     {"-f": "o", "--from-file": "f", "-o": "o", "--output-format": "o", "-p": "o", "--input-format": "o", "-I": "o", "--indent": "o"},
	"head":   {"-n": "o", "-c": "o"},
	"tail":   {"-n": "o", "-c": "o"},
	"sort":   {"-k": "o", "-t": "o", "-o": "o", "-S": "o", "-T": "o"},
	"cut":    {"-d": "o", "-f": "o", "-c": "o", "-b": "o"},
	"uniq":   {"-f": "o", "-s": "o"},
	"diff":   {"-U": "o", "-C": "o", "-x": "o", "-X": "p"},
	"cmp":    {"-i": "o", "-n": "o"},
	"stat":   {"-c": "o"},
	"shasum": {"-a": "o"},
	"base64": {"-i": "p", "-o": "o"},
	"file":   {"-m": "p"},
}

var grepOpts = map[string]string{
	"-e": "s", "--regexp": "s", "-f": "f", "--file": "f", "-m": "o", "--max-count": "o",
	"-A": "o", "-B": "o", "-C": "o", "--context": "o", "-d": "o", "-D": "o",
	"--include": "o", "--exclude": "o", "--exclude-dir": "o",
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
	word = strings.ReplaceAll(word, literalDollar, "$")
	if !strings.ContainsAny(word, "*?[") {
		return
	}
	dir := filepath.Dir(filepath.Clean(word))
	tree := false
	for strings.ContainsAny(dir, "*?[") {
		dir, tree = filepath.Dir(dir), true
	}
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
	// A ( ... ) subshell gets its own working directory and variables.
	type scope struct {
		cwd  string
		vars map[string][]string
	}
	var outer []scope
	for _, seg := range splitShell(text) {
		switch seg[0] {
		case subshellOpen:
			outer = append(outer, scope{cwd, vars})
			vars = maps.Clone(vars)
			continue
		case subshellClose:
			if n := len(outer); n > 0 {
				cwd, vars, outer = outer[n-1].cwd, outer[n-1].vars, outer[:n-1]
			}
			continue
		}
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
		if len(seg) == 0 || seg[0] == subshellOpen || seg[0] == subshellClose {
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
			if strings.HasSuffix(cmd[0], ".py") {
				cmd = append([]string{"python"}, cmd...) // uv run script.py
			} else if rt := a.scriptRuntime(cmd[0], dir); rt != "" {
				cmd = append([]string{rt}, cmd[1:]...)
			}
			a.command(cmd, dir, depth)
		}
		return
	}
	traced := tracedPrograms[prog] || strings.HasPrefix(prog, "python3.")
	grepLike := prog == "grep" || prog == "egrep" || prog == "fgrep"
	recursive := prog == "rg" || prog == "find"
	for i, w := range seg[1:] {
		cluster := strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--")
		if w == "-r" || w == "-R" || w == "--recursive" || (grepLike && cluster && strings.ContainsAny(w, "rR")) || (prog == "ls" && cluster && strings.Contains(w, "R")) {
			recursive = true
		}
		if w == "<" && i+2 < len(seg) && !strings.Contains(seg[i+2], "$") {
			for _, m := range expand(seg[i+2], cwd) {
				a.add(m)
			}
		}
	}
	files, script, scriptFiles := operands(prog, seg[1:])
	// Without a path, ls lists and find, rg and grep -r search the working directory.
	if len(files) == 0 && !traced {
		switch {
		case prog == "ls" && !recursive:
			a.listing(cwd)
		case prog == "ls" || prog == "find" || prog == "rg" || (grepLike && recursive):
			a.opaque(cwd)
		}
	}
	// A script that reads files or runs commands itself (awk's getline and system(),
	// yq's load) makes the reader opaque; so does an unresolved awk or sed script.
	scripted := false
	switch prog {
	case "awk":
		scripted = awkIO.MatchString(script)
		for _, f := range scriptFiles {
			for _, m := range expand(f, cwd) {
				if data, err := os.ReadFile(m); err == nil && awkIO.Match(data) {
					scripted = true
				}
			}
		}
	case "yq":
		scripted = strings.Contains(script, "load")
	}
	unresolved := (prog == "awk" || prog == "sed") && strings.Contains(script, "$")
	for _, w := range files {
		if strings.Contains(w, "$") {
			unresolved = true
			continue
		}
		a.glob(w, cwd)
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
			case fi.IsDir() && (recursive || !readers[prog]):
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

// operands returns the words of a command's arguments that name files (its operands
// and the file arguments of its options, but not redirection targets), and for a
// scriptFirst reader its inline script or pattern and the files it reads one from.
func operands(prog string, args []string) (files []string, script string, scriptFiles []string) {
	switch prog {
	case "find":
		// find [-H|-L|-P] path... expression: the expression names no file to read.
		for _, w := range args {
			if w == "-H" || w == "-L" || w == "-P" {
				continue
			}
			if strings.HasPrefix(w, "-") || w == "!" || w == "(" || w == "<" || w == ">" || w == ">>" {
				break
			}
			files = append(files, w)
		}
		return files, "", nil
	case "test", "[":
		// Only the operands of file operators are paths: [ -f x ], [ a -nt b ].
		for i, w := range args {
			if testFileOps[w] && i+1 < len(args) {
				files = append(files, args[i+1])
			}
			if (w == "-nt" || w == "-ot" || w == "-ef") && i > 0 {
				files = append(files, args[i-1])
			}
		}
		return files, "", nil
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
				if i+1 >= len(args) {
					return
				}
				i++
				arg = args[i]
			}
			switch kind {
			case 's':
				needScript = false
			case 'f':
				needScript = false
				files = append(files, arg)
				scriptFiles = append(scriptFiles, arg)
			case 'p':
				files = append(files, arg)
			}
		}
	}
	end := false
	for i = 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "<" || w == ">" || w == ">>":
			i++ // standard input (recorded by the caller) or an output
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
			script = w
		case prog == "awk" && assign.MatchString(w):
			// var=value operands set awk variables
		default:
			files = append(files, w)
		}
	}
	return files, script, scriptFiles
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
					dir = filepath.Join(dir, val) // relative to the previous one, or absolute
					if filepath.IsAbs(val) {
						dir = val
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

// scriptRuntime returns the runtime ("python", "node", "bun") a launched program runs
// on when it is a script of one: the program named by path, or else the console script
// of that name in a .venv/bin or node_modules/.bin from dir up to the repository root.
// It returns "" for anything else (a native binary, a program it cannot find).
func (a *execAnalysis) scriptRuntime(name, dir string) string {
	var cands []string
	if strings.Contains(name, "/") {
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		cands = append(cands, name)
	} else {
		for d := dir; ; {
			cands = append(cands, filepath.Join(d, ".venv", "bin", name), filepath.Join(d, "node_modules", ".bin", name))
			parent := filepath.Dir(d)
			if d == a.root || parent == d || !within(d, a.root) {
				break
			}
			d = parent
		}
	}
	for _, c := range cands {
		f, err := os.Open(c)
		if err != nil {
			continue
		}
		head := make([]byte, 1024)
		n, _ := f.Read(head)
		f.Close()
		text := string(head[:n])
		line, _, _ := strings.Cut(text, "\n")
		if !strings.HasPrefix(line, "#!") {
			return ""
		}
		for _, rt := range []string{"python", "node", "bun"} {
			if strings.Contains(line, rt) {
				return rt
			}
		}
		// Shell wrappers: pnpm's node_modules/.bin shims, uv's trampoline for long paths.
		switch {
		case strings.Contains(text, "exec node ") || strings.Contains(text, `/node" `):
			return "node"
		case strings.Contains(text, "'''exec'") && strings.Contains(text, "python"):
			return "python"
		}
		return ""
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

// Segments of splitShell that open and close a ( ... ) subshell.
const (
	subshellOpen  = "\x02("
	subshellClose = "\x02)"
)

// splitShell splits a shell command line into simple commands (on |, ||, &&, ;, &,
// newlines) and words, honoring quotes. Redirection operators become separate words;
// parentheses become subshellOpen and subshellClose segments.
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
		case c == '(' || c == ')':
			flushSeg()
			if c == '(' {
				segs = append(segs, []string{subshellOpen})
			} else {
				segs = append(segs, []string{subshellClose})
			}
		case c == '|' || c == ';' || c == '&' || c == '\n':
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
