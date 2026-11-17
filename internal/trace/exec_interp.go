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
)

// funcDef is a shell function's body and the state and depth where it was defined.
type funcDef struct {
	body  [][]string
	st    *shellState
	depth int
}

// callResult is what analyzing a call changed in the caller's state.
type callResult struct {
	cwd     string
	dirs    []string
	vars    map[string][]string
	unknown bool
}

// maxCalls bounds the function calls whose bodies are analyzed. Calls in the same state
// are analyzed once (memo); past the bound, a function's body is analyzed once more in a
// scope of its own, and the caller's state is no longer followed when the body may
// change it (mayChange).
const maxCalls = 200

// shellState is what the next command of a script sees: its working directory, the
// variables assigned so far and the pushd stack; scopes saves them at "(" for ")".
// unknown says the state is no longer followed (a cd to an unknown directory, a call past
// maxCalls that may have changed it): the commands that follow may read anything in the
// repository.
type shellState struct {
	cwd     string
	vars    map[string][]string
	dirs    []string
	scopes  []shellState
	unknown bool
}

// subshell returns a copy of st that st's later changes do not affect.
func (st *shellState) subshell() *shellState {
	return &shellState{cwd: st.cwd, vars: maps.Clone(st.vars), dirs: slices.Clone(st.dirs), unknown: st.unknown}
}

// shellText analyzes shell source (a -c string or a script file). script is the path of
// the script ("" for -c strings), used for $0 and $(dirname "$0"); pos are its positional
// parameters, as shell words (nil: unknown).
func (a *execAnalysis) shellText(text, cwd, script string, depth int, pos []string) {
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
	for _, m := range funcDefRe.FindAllStringSubmatch(text, -1) {
		a.funcs[m[1]] = true
	}
	text = a.substitutions(text, cwd, depth)
	if strings.Contains(text, "`") || evalCmd.MatchString(text) {
		a.unresolved(cwd) // backticks or eval: cannot follow
	}
	st := &shellState{cwd: cwd, vars: map[string][]string{}}
	segs := splitShell(text)
	// A script that shifts or sets its parameters anywhere (a loop over "$@" with
	// shift) has other values for $1 than the first argument: they are not followed.
	if pos != nil && !changesPositional(segs) {
		bindPositional(st.vars, pos)
	}
	a.run(st, segs, depth)
}

// bindPositional sets the positional parameters to pos.
func bindPositional(vars map[string][]string, pos []string) {
	for i, w := range pos {
		if i < 9 {
			vars[strconv.Itoa(i+1)] = []string{w}
		}
	}
	vars["@"], vars["*"] = pos, pos
}

// changesPositional reports whether shell code (segs) may change its positional
// parameters: shift, getopts, set with operands.
func changesPositional(segs [][]string) bool {
	for _, seg := range segs {
		w := trimKeywords(trimAssignments(seg))
		if len(w) > 0 && (w[0] == "shift" || w[0] == "getopts" || (w[0] == "set" && setsPositional(w[1:]))) {
			return true
		}
	}
	return false
}

// positionalVars are the variables holding positional parameters ($1...$9, $@, $*).
var positionalVars = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "@", "*"}

// unbindPositional forgets the positional parameters of a script or function: shift or
// set changed them, or a function body (which has parameters of its own) is analyzed.
func unbindPositional(vars map[string][]string) {
	for _, v := range positionalVars {
		delete(vars, v)
	}
}

// positional returns the positional parameters of a script started with args (words of
// shell code when shell is set, literal words of an argv otherwise), as shell words.
func positional(args []string, shell bool) []string {
	pos := []string{}
	for i := 0; i < len(args); i++ {
		w := args[i]
		if shell && (w == "<" || w == ">" || w == ">>" || w == "<<" || w == "<<<") {
			i++ // a redirection
			continue
		}
		if !shell {
			w = quoteMarks.Replace(strings.ReplaceAll(w, "$", literalDollar))
		}
		pos = append(pos, w)
	}
	return pos
}

// run analyzes the commands of a script in order. The body of a function is analyzed
// where it is called, in the caller's state (a cd in it changes the caller's directory,
// as in the shell); flush analyzes the functions no call reached.
func (a *execAnalysis) run(st *shellState, segs [][]string, depth int) {
	for k := 0; k < len(segs); k++ {
		if name, body, next, ok := funcDefAt(segs, k); ok {
			a.define(name, body, st, depth)
			k = next - 1
			continue
		}
		seg := segs[k]
		switch seg[0] {
		case subshellOpen:
			// A ( ... ) subshell gets its own working directory and variables.
			st.scopes = append(st.scopes, shellState{cwd: st.cwd, vars: st.vars, dirs: st.dirs, unknown: st.unknown})
			st.vars, st.dirs = maps.Clone(st.vars), slices.Clone(st.dirs)
			continue
		case subshellClose:
			if n := len(st.scopes); n > 0 {
				s := st.scopes[n-1]
				st.cwd, st.vars, st.dirs, st.unknown, st.scopes = s.cwd, s.vars, s.dirs, s.unknown, st.scopes[:n-1]
			}
			continue
		}
		if st.unknown {
			// The working directory and the variables are not known: the command may read
			// anything in the repository.
			if !a.lost {
				a.lost = true
				a.unresolved(a.root)
			}
			continue
		}
		seg, ok := braceSeg(seg)
		if ok {
			seg, ok = expandVars(seg, st.vars)
		}
		if !ok {
			a.opaque(st.cwd) // too many words to follow
			a.absoluteTrees(segs[k], st.cwd, st.vars)
			continue
		}
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
			a.callWith(st, w[0], w[1:])
			continue
		}
		if a.builtin(seg, st, depth) {
			continue
		}
		a.command(seg, st.cwd, depth, true)
	}
}

// define records the definition of shell function name in state st. A new body
// invalidates the calls analyzed so far (they may call it).
func (a *execAnalysis) define(name string, body [][]string, st *shellState, depth int) {
	a.funcs[name] = true
	if d := a.defs[name]; d != nil && &d.body[0] == &body[0] && len(d.body) == len(body) {
		return // the same definition again (a body defining a function, called twice)
	}
	// $1 in the body is the function's own parameter (callWith), unknown where no call
	// reaches it (flush).
	def := st.subshell()
	unbindPositional(def.vars)
	a.defs[name] = &funcDef{body: body, st: def, depth: depth}
	a.pending = append(a.pending, name)
	clear(a.memo)
	clear(a.relevant)
	clear(a.changes)
}

// callWith analyzes a call of function name with the arguments args (shell words): its
// body sees them as its positional parameters (unbound when it shifts or sets them),
// and the caller's are restored afterwards.
func (a *execAnalysis) callWith(st *shellState, name string, args []string) {
	saved := map[string][]string{}
	for _, v := range positionalVars {
		if vals, ok := st.vars[v]; ok {
			saved[v] = vals
		}
	}
	unbindPositional(st.vars)
	if d := a.defs[name]; d != nil && !changesPositional(d.body) {
		bindPositional(st.vars, positional(args, true))
	}
	a.call(st, name)
	unbindPositional(st.vars)
	maps.Copy(st.vars, saved)
}

// call analyzes the body of the shell function name called in state st, applying what
// it changes to st. A call in a state analyzed before replays its result.
func (a *execAnalysis) call(st *shellState, name string) {
	d := a.defs[name]
	if d == nil || a.calling[name] {
		// Not defined yet (flush analyzes it), or a recursive call (the body is being
		// analyzed).
		return
	}
	key := a.callKey(st, name)
	if r, ok := a.memo[key]; ok {
		st.cwd, st.dirs = r.cwd, slices.Clone(r.dirs)
		maps.Copy(st.vars, r.vars)
		st.unknown = st.unknown || r.unknown
		return
	}
	a.calling[name] = true
	defer delete(a.calling, name)
	a.analyzed[name] = true
	if a.calls >= maxCalls {
		if scope := name + "\x00scope"; !a.analyzed[scope] {
			a.analyzed[scope] = true
			a.run(st.subshell(), d.body, d.depth)
		}
		// What the body changes is not applied: when it may change the working
		// directory, the pushd stack or variables, the caller's state is lost.
		if a.mayChange(name) {
			st.unknown = true
		}
		return
	}
	a.calls++
	before := maps.Clone(st.vars)
	a.run(st, d.body, d.depth)
	changed := map[string][]string{}
	for k, v := range st.vars {
		if old, ok := before[k]; !ok || !slices.Equal(old, v) {
			changed[k] = v
		}
	}
	a.memo[key] = callResult{cwd: st.cwd, dirs: slices.Clone(st.dirs), vars: changed, unknown: st.unknown}
}

// mayChange reports whether the body of function name, or a function it calls, may
// change the caller's state (see run): a cd, pushd or popd, an assignment, a for loop.
func (a *execAnalysis) mayChange(name string) bool {
	if v, ok := a.changes[name]; ok {
		return v
	}
	// Only the answer for name is memoized: bodyMayChange cuts a recursive call short,
	// so its answer for a function reached through one may be incomplete.
	v := a.bodyMayChange(name, map[string]bool{})
	a.changes[name] = v
	return v
}

func (a *execAnalysis) bodyMayChange(name string, seen map[string]bool) bool {
	d := a.defs[name]
	if d == nil || seen[name] {
		return false
	}
	seen[name] = true
	for _, seg := range d.body {
		if w := trimKeywords(seg); len(w) > 0 && isAssignments(w) || len(seg) >= 3 && seg[0] == "for" && seg[2] == "in" {
			return true
		}
		w := trimKeywords(trimAssignments(seg))
		if len(w) == 0 {
			continue
		}
		switch w[0] {
		case "cd", "pushd", "popd":
			return true
		}
		if a.defs[w[0]] != nil && a.bodyMayChange(w[0], seen) {
			return true
		}
	}
	return false
}

// callKey identifies the state a call of name is analyzed in: the working directory,
// the pushd stack and the values of the variables its body (and the functions it calls)
// may use.
func (a *execAnalysis) callKey(st *shellState, name string) string {
	var b strings.Builder
	b.WriteString(name + "\x00" + st.cwd + "\x00" + strings.Join(st.dirs, "\x1f"))
	for _, v := range a.relevantVars(name, map[string]bool{}) {
		vals, ok := st.vars[v]
		fmt.Fprintf(&b, "\x00%s=%v:%s", v, ok, strings.Join(vals, "\x1f"))
	}
	return b.String()
}

// relevantVars returns the variables the body of function name refers to, also through
// the functions it calls.
func (a *execAnalysis) relevantVars(name string, seen map[string]bool) []string {
	if vs, ok := a.relevant[name]; ok {
		return vs
	}
	d := a.defs[name]
	if d == nil || seen[name] {
		return nil
	}
	seen[name] = true
	set := map[string]bool{}
	for _, seg := range d.body {
		for _, w := range seg {
			for _, m := range varRef.FindAllStringSubmatch(w, -1) {
				set[m[1]+m[2]] = true
			}
		}
		if w := trimKeywords(trimAssignments(seg)); len(w) > 0 && a.defs[w[0]] != nil {
			for _, v := range a.relevantVars(w[0], seen) {
				set[v] = true
			}
		}
	}
	vs := slices.Sorted(maps.Keys(set))
	a.relevant[name] = vs
	return vs
}

// flush analyzes the bodies of the functions no call reached (a trap handler, one
// called through a variable or a command substitution, a library function for the
// scripts sourcing it), each in a scope of its own, in the state where it was defined.
func (a *execAnalysis) flush() {
	for i := 0; i < len(a.pending); i++ { // analyzing a body may define more
		name := a.pending[i]
		if a.analyzed[name] {
			continue
		}
		a.analyzed[name] = true
		d := a.defs[name]
		a.calling[name] = true
		a.run(d.st.subshell(), d.body, d.depth)
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

func trimKeywords(seg []string) []string {
	for len(seg) > 0 && keywords[seg[0]] {
		seg = seg[1:]
	}
	return seg
}

// redirects records the files the input redirections (< file) of a command read.
func (a *execAnalysis) redirects(seg []string, cwd string) {
	for i := 0; i+1 < len(seg); i++ {
		switch {
		case seg[i] != "<":
		case strings.Contains(seg[i+1], "$"):
			a.unresolved(cwd) // an unresolved input (< "$1"), like cat "$1"
		default:
			for _, m := range expand(seg[i+1], cwd) {
				a.add(m)
			}
		}
	}
}

var (
	funcDefRe = regexp.MustCompile(`(?m)^[ \t]*(?:function[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\([ \t]*\)`)
	// funcName matches the names of shell functions (bash also allows "-", "." and ":").
	funcName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]*$`)
	// varRef also matches positional parameters ($1...$9, $@, $*; $10 is ${1}0).
	varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*|[1-9@*])\}|\$([A-Za-z_][A-Za-z0-9_]*|[1-9@*])`)
	assign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

	bashSource       = regexp.MustCompile(`\$\{BASH_SOURCE(?:\[0\])?\}|\$BASH_SOURCE\b`)
	bashSourcePrefix = regexp.MustCompile(`\$\{BASH_SOURCE(?:\[0\])?%`)
	// cwdChange matches anything that may change the working directory of a script
	// (directly or through sourced code), so $(pwd) cannot be taken as the start cwd.
	cwdChange = regexp.MustCompile(`\b(?:cd|pushd|popd|source|eval)\b|(?:^|[;&|(\n])[ \t]*\.[ \t]`)
	// evalCmd matches an eval with arguments (eval, then any whitespace).
	evalCmd = regexp.MustCompile(`\beval\s`)
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
// a variable with several values (a for-loop list) becomes a word per value (the same
// value for every use of the variable in the word). Unknown variables are left in place
// (and make the command unresolved). ok is false when the command would have more than
// maxWords words (and more than it had).
func expandVars(seg []string, vars map[string][]string) (out []string, ok bool) {
	limit := max(maxWords, len(seg))
	for i, w := range seg {
		if !strings.Contains(w, "$") {
			out = append(out, w)
			continue
		}
		words := []string{w}
		done := map[string]bool{}
		for _, m := range varRef.FindAllStringSubmatch(w, -1) {
			name := m[1] + m[2]
			vals, ok := vars[name]
			if !ok || done[name] {
				continue
			}
			done[name] = true
			var next []string
			for _, cur := range words {
				for _, v := range vals {
					next = append(next, varRef.ReplaceAllStringFunc(cur, func(ref string) string {
						if r := varRef.FindStringSubmatch(ref); r[1]+r[2] == name {
							return v
						}
						return ref
					}))
					if len(out)+len(next)+len(seg)-i-1 > limit {
						return nil, false
					}
				}
			}
			words = next
		}
		out = append(out, words...)
	}
	return out, true
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
			a.unresolved(cwd)
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
			a.unresolved(cwd)
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
		seg, ok := braceSeg(seg)
		w := trimKeywords(trimAssignments(seg))
		switch {
		case !ok:
			a.unresolved(cwd) // too many words to follow
		case len(w) == 0:
		case w[0] == "cd" || w[0] == "pushd":
			// $(cd "$(dirname "$0")" && pwd) reads nothing; what follows the cd runs there.
			d, ok := cdTarget(w, st.cwd)
			st.cwd, lost = d, lost || !ok
		case w[0] == "<":
			a.redirects(w, st.cwd) // $(<file)
		case lost:
			if !nameOnly[filepath.Base(w[0])] {
				a.unresolved(cwd)
			}
		case a.builtin(seg, st, depth):
		default:
			a.command(seg, st.cwd, depth, true)
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
			// The directory is not known: the commands that follow do not run in the old
			// one, and may read anything in the repository.
			a.unresolved(st.cwd)
			st.unknown = true
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
			a.unresolved(st.cwd) // unresolved path: cannot follow
		default:
			a.script(literal(seg[1]), st.cwd, depth, nil)
		}
		return true
	case "shift", "getopts":
		unbindPositional(st.vars)
	case "set":
		if setsPositional(seg[1:]) {
			unbindPositional(st.vars)
		}
	}
	if noops[seg[0]] {
		a.redirects(seg, st.cwd) // read -r V < VERSION
		return true
	}
	return false
}

// setsPositional reports whether set with args assigns the positional parameters: it
// has operands (set -- a b, set a b) besides options (set -e, set -o pipefail).
func setsPositional(args []string) bool {
	for i := 0; i < len(args); i++ {
		w := args[i]
		if w == "--" || w == "-" || len(w) < 2 || (w[0] != '-' && w[0] != '+') {
			return true
		}
		if strings.ContainsRune(w, 'o') {
			i++ // -o NAME
		}
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

// shellInvocation analyzes a shell started with args (the words after its name):
// "-c text" (after any options, or combined as in -ec), a script file (its arguments
// are positional parameters, not files), or commands read from stdin. shell says whether
// args are words of shell code (or of an argv).
func (a *execAnalysis) shellInvocation(args []string, cwd string, depth int, shell bool) {
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
	var pos []string
	if operand >= 0 {
		script = literal(args[operand])
		pos = positional(args[operand+1:], shell)
		if cmd && len(pos) > 0 {
			pos = pos[1:] // sh -c CODE NAME ARG...: NAME is $0
		}
	}
	switch {
	case cmd && operand < 0:
		a.unresolved(cwd)
	case cmd:
		if depth >= 3 {
			a.unresolved(cwd)
		} else {
			a.shellText(script, cwd, "", depth+1, pos)
		}
	case operand >= 0 && !stdin:
		if strings.Contains(script, "$") {
			a.unresolved(cwd)
		} else {
			a.script(script, cwd, depth, pos)
		}
	case stdinFile != "" && !strings.Contains(stdinFile, "$"):
		a.script(stdinFile, cwd, depth, nil) // sh < script.sh
		return
	default:
		// Commands from stdin (a pipe, a terminal, a here-document) or an unresolved
		// script.
		a.unresolved(cwd)
		return
	}
	// Commands run by the shell may read its redirected stdin.
	switch {
	case stdinFile == "":
	case strings.Contains(stdinFile, "$"):
		a.unresolved(cwd)
	default:
		for _, m := range expand(stdinFile, cwd) {
			a.add(m)
		}
	}
}

// script analyzes a shell script file (bounded recursion) started with the positional
// parameters pos (nil: unknown).
func (a *execAnalysis) script(name, cwd string, depth int, pos []string) {
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	a.add(p)
	data, err := os.ReadFile(p)
	if err != nil || depth >= 3 || len(data) > 256*1024 {
		// Not followed: missing (deleted after the run, say), too deep or too big.
		a.unresolved(cwd)
		return
	}
	a.shellText(string(data), cwd, p, depth+1, pos)
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

func trimAssignments(seg []string) []string {
	for len(seg) > 0 && strings.Contains(seg[0], "=") && !strings.HasPrefix(seg[0], "-") && !strings.Contains(strings.SplitN(seg[0], "=", 2)[0], "/") {
		seg = seg[1:]
	}
	return seg
}
