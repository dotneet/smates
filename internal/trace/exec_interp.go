package trace

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The interpreter follows the state of a shell through the syntax tree of its code
// (see exec_lex.go). The state at a point of a script is the set of states it may be in
// there: every working directory and every value of each variable the paths reaching
// that point may have. Branches (if, case, && and ||, loops) are joined where they
// merge, so a command after them is analyzed for all of them.

// shellState is what the next command of a script may see: its candidate working
// directories, the candidate values of the variables assigned so far (a variable not in
// vars is unknown: set outside the script, read from a command, assigned differently on
// paths that merged), the variables with attributes changing what is assigned to them
// (attrs), and the pushd stack. unknown says the state is no longer followed
// (a cd to an unknown directory, eval, a call past maxCalls that may have changed it):
// the commands that follow may read anything in the repository. A nil state is
// unreachable (after exit).
type shellState struct {
	cwds []string
	vars map[string][]string
	// attrs are the variables declared with an attribute that changes the values
	// assigned to them (declare -l, -u, -c, -i: attrValue) or makes them a reference to
	// another variable (declare -n: attrRef); on any path, so a join keeps them all. An
	// attributed variable is never in vars (see assigned).
	attrs    map[string]byte
	dirs     [][]string // the pushd stack, each entry the candidate directories
	dirsLost bool       // paths with pushd stacks of different heights merged
	unknown  bool
	work     *int // commands analyzed in the shell (shared by its states, see maxWork)
}

// maxWork bounds the commands analyzed in one shell (loops are followed several
// times, functions at each call in a new state): past it, the shell may read anything.
const maxWork = 100000

// Bounds on the candidates of a state: past them, the working directory or the variable
// is unknown.
const (
	maxCwds   = 32
	maxValues = maxWords
	// maxAlts bounds the commands a command line with variables of several values
	// stands for (one per combination of their values); past it, the command is
	// analyzed once with every value of each variable.
	maxAlts = 64
	// maxIter bounds the iterations of a loop's body followed until its state stops
	// changing; past it, what still changes is unknown.
	maxIter = 4
)

func newState(cwd string) *shellState {
	return &shellState{cwds: []string{cwd}, vars: map[string][]string{}, work: new(int)}
}

// clone returns a copy of st that st's later changes do not affect (and conversely):
// states are never changed once shared.
func (st *shellState) clone() *shellState {
	return &shellState{cwds: slices.Clone(st.cwds), vars: maps.Clone(st.vars), attrs: maps.Clone(st.attrs), dirs: slices.Clone(st.dirs),
		dirsLost: st.dirsLost, unknown: st.unknown, work: st.work}
}

// The attributes of attrs.
const (
	attrValue byte = 'v'
	attrRef   byte = 'n'
)

// assigned applies the attributes of the variables names, just assigned (or unset) in
// st, a state not shared yet: the value of one with attrValue is unknown; one with
// attrRef assigned another variable, any of them.
func (st *shellState) assigned(names ...string) {
	for _, name := range names {
		switch st.attrs[name] {
		case 0:
		case attrRef:
			clear(st.vars)
			return
		default:
			delete(st.vars, name)
		}
	}
}

// hasRef reports whether st has a reference variable (declare -n).
func (st *shellState) hasRef() bool {
	for _, v := range st.attrs {
		if v == attrRef {
			return true
		}
	}
	return false
}

// joinAttrs returns the attributes of x and y (attrRef where they differ).
func joinAttrs(x, y map[string]byte) map[string]byte {
	if len(y) == 0 {
		return x
	}
	if len(x) == 0 {
		return y
	}
	r := maps.Clone(x)
	for k, v := range y {
		if r[k] != attrRef {
			r[k] = v
		}
	}
	return r
}

// spend counts a command analyzed in st's shell; false past maxWork.
func (st *shellState) spend() bool {
	if st.work == nil {
		st.work = new(int)
	}
	*st.work++
	return *st.work <= maxWork
}

// union returns the values of x followed by those of y not in x.
func union(x, y []string) []string {
	if slices.Equal(x, y) {
		return x
	}
	seen := make(map[string]bool, len(x)+len(y))
	out := make([]string, 0, len(x)+len(y))
	for _, s := range append(slices.Clone(x), y...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func sameSet(x, y []string) bool {
	if slices.Equal(x, y) {
		return true
	}
	if len(x) != len(y) {
		return false
	}
	set := make(map[string]bool, len(x))
	for _, s := range x {
		set[s] = true
	}
	for _, s := range y {
		if !set[s] {
			return false
		}
	}
	return true
}

// join is the state where paths in states x and y merge: every directory and value of
// either; a variable known on one path only is unknown.
func join(x, y *shellState) *shellState {
	switch {
	case x == nil:
		return y
	case y == nil || x == y:
		return x
	}
	r := &shellState{cwds: union(x.cwds, y.cwds), vars: map[string][]string{}, attrs: joinAttrs(x.attrs, y.attrs),
		unknown: x.unknown || y.unknown, work: x.work}
	if len(r.cwds) > maxCwds {
		r.unknown = true
	}
	for k, xv := range x.vars {
		if yv, ok := y.vars[k]; ok {
			if u := union(xv, yv); len(u) <= maxValues {
				r.vars[k] = u
			}
		}
	}
	if x.dirsLost || y.dirsLost || len(x.dirs) != len(y.dirs) {
		r.dirsLost = true
	} else {
		for i := range x.dirs {
			r.dirs = append(r.dirs, union(x.dirs[i], y.dirs[i]))
		}
	}
	return r
}

func equal(x, y *shellState) bool {
	if x == y {
		return true
	}
	if x == nil || y == nil || x.unknown != y.unknown || x.dirsLost != y.dirsLost || !sameSet(x.cwds, y.cwds) ||
		len(x.vars) != len(y.vars) || len(x.dirs) != len(y.dirs) || !maps.Equal(x.attrs, y.attrs) {
		return false
	}
	for k, xv := range x.vars {
		if yv, ok := y.vars[k]; !ok || !sameSet(xv, yv) {
			return false
		}
	}
	for i := range x.dirs {
		if !sameSet(x.dirs[i], y.dirs[i]) {
			return false
		}
	}
	return true
}

// widen returns next (a loop's state after one more iteration than old) with what
// still changes unknown.
func widen(old, next *shellState) *shellState {
	r := next.clone()
	if old == nil {
		return r
	}
	for k, v := range next.vars {
		if ov, ok := old.vars[k]; !ok || !sameSet(ov, v) {
			delete(r.vars, k)
		}
	}
	if !sameSet(old.cwds, next.cwds) {
		r.unknown = true
	}
	if len(old.dirs) != len(next.dirs) {
		r.dirsLost = true
	} else {
		for i := range old.dirs {
			if !sameSet(old.dirs[i], next.dirs[i]) {
				r.dirsLost = true
			}
		}
	}
	return r
}

// setVar sets variable name to vals; values the analysis does not know (a variable it
// does not follow, a command's output: "$" left in them), or too many, make it unknown.
func setVar(vars map[string][]string, name string, vals []string) {
	if vals == nil || len(vals) > maxValues {
		delete(vars, name)
		return
	}
	for _, v := range vals {
		if strings.Contains(v, "$") {
			delete(vars, name)
			return
		}
	}
	vars[name] = vals
}

// frame is a loop (break and continue), a function or sourced file (return), or a
// subshell or another shell (they end the search for one of the others), with the
// states its break, continue and return statements reach.
type frame struct {
	kind           byte // 'l', 'f', 's'
	brk, cont, ret *shellState
	// escaped says (in a function's frame) that a break or continue in it reached a
	// loop of its caller: the call's result is more than a state (not memoized).
	escaped bool
}

func (a *execAnalysis) push(kind byte) *frame {
	f := &frame{kind: kind}
	a.frames = append(a.frames, f)
	return f
}

func (a *execAnalysis) pop() { a.frames = a.frames[:len(a.frames)-1] }

// frameIndex returns the index of the n-th innermost frame of kind in the current
// shell, or -1.
func (a *execAnalysis) frameIndex(kind byte, n int) int {
	for i := len(a.frames) - 1; i >= 0; i-- {
		switch f := a.frames[i]; {
		case f.kind == 's':
			return -1
		case f.kind == kind:
			if n--; n <= 0 {
				return i
			}
		}
	}
	return -1
}

// loops returns the indexes of the loops enclosing the current command in its shell,
// innermost first.
func (a *execAnalysis) loops() []int {
	var out []int
	for n := 1; ; n++ {
		i := a.frameIndex('l', n)
		if i < 0 {
			return out
		}
		out = append(out, i)
	}
}

// flowBuiltins leave the commands that follow them.
var flowBuiltins = map[string]bool{"exit": true, "return": true, "break": true, "continue": true}

// flow runs exit, return, break or continue (head, its substitutions done) in st: it
// reports whether control leaves (st then reaches the frame it ends).
func (a *execAnalysis) flow(head []string, st *shellState) bool {
	switch head[0] {
	case "exit":
		return true
	case "return":
		if i := a.frameIndex('f', 1); i >= 0 {
			a.frames[i].ret = join(a.frames[i].ret, st)
			return true
		}
		return false
	}
	// break N leaves N loops (all of them when there are fewer), continue N resumes the
	// N-th one; with a count the analysis does not know, any of them.
	loops := a.loops()
	if len(loops) == 0 {
		return false
	}
	targets := loops[:1]
	if len(head) > 1 {
		arg := head[1]
		if strings.Contains(arg, "$") && !st.unknown {
			if vs := expandValue(arg, st.vars); len(vs) == 1 {
				arg = vs[0]
			}
		}
		if n, err := strconv.Atoi(literal(arg)); err != nil || !isDigits(literal(arg)) {
			targets = loops
		} else if n > 1 {
			targets = loops[min(n, len(loops))-1:][:1]
		}
	}
	for _, i := range targets {
		f := a.frames[i]
		if head[0] == "break" {
			f.brk = join(f.brk, st)
		} else {
			f.cont = join(f.cont, st)
		}
		for _, g := range a.frames[i+1:] {
			if g.kind == 'f' {
				g.escaped = true
			}
		}
	}
	return true
}

// funcDef is a shell function's body and the state and depth where it was defined.
type funcDef struct {
	body   shCmd
	st     *shellState
	depth  int
	locals []string // the variables it declares local (nil: not computed yet)
	// shifts memoizes changesPositional for the body (0: not computed yet, 1: no, 2:
	// yes): every call would otherwise walk it again.
	shifts int8
}

// memoBool returns the answer *m memoizes, computing it with f the first time.
func memoBool(m *int8, f func() bool) bool {
	if *m == 0 {
		*m = 1
		if f() {
			*m = 2
		}
	}
	return *m == 2
}

// callResult is what analyzing a call changed in the caller's state.
type callResult struct {
	dead     bool
	cwds     []string
	dirs     [][]string
	dirsLost bool
	unknown  bool
	attrs    map[string]byte
	set      map[string][]string
	del      []string
}

func diffStates(before, after *shellState) callResult {
	if after == nil {
		return callResult{dead: true}
	}
	r := callResult{cwds: after.cwds, dirs: after.dirs, dirsLost: after.dirsLost, unknown: after.unknown, attrs: after.attrs,
		set: map[string][]string{}}
	for k, v := range after.vars {
		if old, ok := before.vars[k]; !ok || !slices.Equal(old, v) {
			r.set[k] = v
		}
	}
	for k := range before.vars {
		if _, ok := after.vars[k]; !ok {
			r.del = append(r.del, k)
		}
	}
	return r
}

func (r callResult) apply(st *shellState) *shellState {
	if r.dead {
		return nil
	}
	n := st.clone()
	n.cwds, n.dirs, n.dirsLost, n.unknown = slices.Clone(r.cwds), slices.Clone(r.dirs), r.dirsLost, r.unknown
	maps.Copy(n.vars, r.set)
	for _, k := range r.del {
		delete(n.vars, k)
	}
	// The call may have declared attributes (they only add up): an attributed variable
	// is unknown.
	n.attrs = joinAttrs(n.attrs, r.attrs)
	for k := range n.attrs {
		delete(n.vars, k)
	}
	return n
}

// maxCalls bounds the function calls whose bodies are analyzed. Calls in the same state
// are analyzed once (memo); past the bound, a function's body is analyzed once more in a
// scope of its own, and the caller's state is no longer followed when the body may
// change it (mayChange).
const maxCalls = 200

// parse parses shell code and records the functions it defines. Code it cannot parse
// entirely, or with backticks, may run anything.
func (a *execAnalysis) parse(text string) *shList {
	l, bad, backtick := a.parseShell(text)
	if bad || backtick {
		a.unresolved()
	}
	a.visit(l, func(n any) {
		if d, ok := n.(*shFuncDef); ok {
			a.funcs[d.name] = true
		}
	})
	return l
}

// shellText analyzes shell source (a -c string or a script file) run by a new shell.
// script is the path of the script ("" for -c strings), used for $0 and $(dirname "$0");
// pos are its positional parameters, as shell words (nil: unknown).
func (a *execAnalysis) shellText(text, cwd, script string, depth int, pos []string) {
	l := a.parse(scriptIdioms(text, script, script))
	st := newState(cwd)
	// A script that shifts or sets its parameters anywhere (a loop over "$@" with
	// shift) has other values for $1 than the first argument: they are not followed.
	if pos != nil && !a.changesPositional(l) {
		bindPositional(st.vars, pos)
	}
	frames, zeros, traps := a.frames, a.zeros, a.traps
	a.frames, a.zeros, a.traps = nil, append(slices.Clone(a.zeros), script), nil
	a.endShell(traps, a.runList(l, st, depth))
	a.frames, a.zeros = frames, zeros
}

// scriptIdioms resolves the idioms naming the directory of a script: self is the file
// read (${BASH_SOURCE[0]}), zero the script the shell runs ($0; the same unless self is
// sourced; "" when unknown).
func scriptIdioms(text, self, zero string) string {
	if self == "" && zero == "" {
		return text
	}
	const src = "\x13" // stands for BASH_SOURCE while $0 is replaced
	text = bashSourcePrefix.ReplaceAllLiteralString(text, "${"+src+"%")
	text = bashSource.ReplaceAllLiteralString(text, "$"+src)
	for _, r := range []struct{ name, path string }{{src, self}, {"0", zero}} {
		if r.path == "" {
			continue
		}
		ref, dir := "$"+r.name, filepath.Dir(r.path)
		for _, pat := range []string{`"$(cd "$(dirname "` + ref + `")" && pwd)"`, `$(cd "$(dirname "` + ref + `")" && pwd)`,
			`"$(dirname "` + ref + `")"`, `$(dirname "` + ref + `")`, `$(dirname ` + ref + `)`, `"${` + r.name + `%/*}"`, `${` + r.name + `%/*}`} {
			text = strings.ReplaceAll(text, pat, dir)
		}
		text = strings.ReplaceAll(text, `"`+ref+`"`, r.path)
	}
	text = strings.ReplaceAll(text, "${"+src+"%", "${BASH_SOURCE%")
	return strings.ReplaceAll(text, "$"+src, "${BASH_SOURCE}")
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

// positionalVars are the variables holding positional parameters ($1...$9, $@, $*).
var positionalVars = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "@", "*"}

// unbindPositional forgets the positional parameters of a script or function: shift or
// set changed them, or a function body (which has parameters of its own) is analyzed.
func unbindPositional(vars map[string][]string) {
	for _, v := range positionalVars {
		delete(vars, v)
	}
}

func positionalOf(vars map[string][]string) map[string][]string {
	saved := map[string][]string{}
	for _, v := range positionalVars {
		if vals, ok := vars[v]; ok {
			saved[v] = vals
		}
	}
	return saved
}

// withPositional returns st with the positional parameters saved.
func withPositional(st *shellState, saved map[string][]string) *shellState {
	if st == nil {
		return nil
	}
	n := st.clone()
	unbindPositional(n.vars)
	maps.Copy(n.vars, saved)
	return n
}

// changesPositional reports whether shell code may change its positional parameters:
// shift, getopts, set with operands.
func (a *execAnalysis) changesPositional(code any) bool {
	changes := false
	a.visit(code, func(n any) {
		if s, ok := n.(*shSimple); ok {
			w := trimKeywords(trimAssignments(s.words))
			if len(w) > 0 && (w[0] == "shift" || w[0] == "getopts" || (w[0] == "set" && setsPositional(w[1:]))) {
				changes = true
			}
		}
	})
	return changes
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

// visit calls fn for every node of a syntax tree (also the bodies of functions and
// substitutions) and every word list (*shWords).
func (a *execAnalysis) visit(n any, fn func(any)) {
	if n == nil {
		return
	}
	fn(n)
	words := func(w *shWords) {
		fn(w)
		for _, word := range w.words {
			for _, id := range placeholders(word) {
				a.visitSubst(id, fn)
			}
		}
		for _, id := range w.extra {
			a.visitSubst(id, fn)
		}
	}
	list := func(l *shList) {
		if l != nil {
			a.visit(l, fn)
		}
	}
	switch n := n.(type) {
	case *shList:
		for _, ao := range n.items {
			for _, pl := range ao.pipes {
				for _, c := range pl.cmds {
					a.visit(c, fn)
				}
			}
		}
	case *shSimple:
		words(&n.shWords)
	case *shGroup:
		list(n.body)
		words(&n.redir)
	case *shIf:
		for i := range n.conds {
			list(n.conds[i])
			list(n.bodies[i])
		}
		list(n.els)
		words(&n.redir)
	case *shLoop:
		list(n.cond)
		list(n.body)
		words(&n.redir)
	case *shFor:
		if n.words != nil {
			words(n.words)
		}
		if n.arith != nil {
			a.visit(n.arith, fn)
		}
		list(n.body)
		words(&n.redir)
	case *shCase:
		words(&n.word)
		for _, it := range n.items {
			words(&it.patterns)
			list(it.body)
		}
		words(&n.redir)
	case *shArith:
		for _, id := range n.subs {
			a.visitSubst(id, fn)
		}
		words(&n.redir)
	case *shFuncDef:
		a.visit(n.body, fn)
	}
}

func (a *execAnalysis) visitSubst(id int, fn func(any)) {
	if id < 0 || id >= len(a.substs) {
		return
	}
	s := a.substs[id]
	if s.body != nil {
		a.visit(s.body, fn)
	}
	if s.arith != nil {
		a.visit(s.arith, fn)
	}
}

// Running code: run* functions take the state before and return the state after (nil:
// unreachable); flow variants return the states where the code succeeded (ok) and
// failed (fail). States are never changed in place.

func (a *execAnalysis) runList(l *shList, st *shellState, depth int) *shellState {
	for _, ao := range l.items {
		if st == nil {
			return nil
		}
		st = a.runAndOr(ao, st, depth)
	}
	return st
}

// runListFlow runs a list whose status decides a branch (the condition of if, while).
func (a *execAnalysis) runListFlow(l *shList, st *shellState, depth int) (ok, fail *shellState) {
	n := len(l.items)
	if st == nil || n == 0 {
		return st, st
	}
	for _, ao := range l.items[:n-1] {
		if st = a.runAndOr(ao, st, depth); st == nil {
			return nil, nil
		}
	}
	return a.runAndOrFlow(l.items[n-1], st, depth)
}

// runAndOr runs an and-or list: a single command is taken to succeed (the script ran
// to its end), a list joined by && or || may have ended either way, and so may a
// negated pipeline (! cmd: its status never stops a script, set -e or not).
func (a *execAnalysis) runAndOr(ao *shAndOr, st *shellState, depth int) *shellState {
	ok, fail := a.runAndOrFlow(ao, st, depth)
	if len(ao.ops) == 0 && !ao.pipes[0].bang && ok != nil {
		return ok
	}
	return join(ok, fail)
}

func (a *execAnalysis) runAndOrFlow(ao *shAndOr, st *shellState, depth int) (ok, fail *shellState) {
	if ao.bg {
		// Runs asynchronously, in a subshell.
		a.push('s')
		a.runAndOrFlow(&shAndOr{pipes: ao.pipes, ops: ao.ops}, st, depth)
		a.pop()
		return st, st
	}
	ok, fail = a.runPipeline(ao.pipes[0], st, depth)
	for i, op := range ao.ops {
		pl := ao.pipes[i+1]
		if op == "&&" {
			// The next pipeline runs where the list succeeded so far.
			if ok != nil {
				o, f := a.runPipeline(pl, ok, depth)
				ok, fail = o, join(fail, f)
			}
		} else if fail != nil {
			o, f := a.runPipeline(pl, fail, depth)
			ok, fail = join(ok, o), f
		}
	}
	return ok, fail
}

func (a *execAnalysis) runPipeline(pl *shPipeline, st *shellState, depth int) (ok, fail *shellState) {
	if len(pl.cmds) == 1 {
		ok, fail = a.runCmd(pl.cmds[0], st, depth)
	} else {
		// Every command of a pipeline runs in a subshell, except the last one in zsh
		// and ksh (bash runs it in a subshell too).
		a.push('s')
		for _, c := range pl.cmds[:len(pl.cmds)-1] {
			a.runCmd(c, st, depth)
		}
		a.pop()
		o, f := a.runCmd(pl.cmds[len(pl.cmds)-1], st, depth)
		ok, fail = join(st, o), join(st, f)
	}
	if pl.bang {
		ok, fail = fail, ok
	}
	return ok, fail
}

// subshell runs commands in a subshell of st and returns the state they end in.
func (a *execAnalysis) subshell(l *shList, st *shellState, depth int) *shellState {
	a.push('s')
	defer a.pop()
	traps := a.traps
	a.traps = nil // a subshell does not run the traps of its parent
	end := a.runList(l, st, depth)
	a.endShell(traps, end)
	return end
}

// trap is the code a trap command set and the states the shell may run it in: the join
// of the states of the commands that follow it in the shell (in any of them a signal
// may arrive, an error happen, an exit run it).
type trap struct {
	code     string
	body     *shList
	depth    int
	st, last *shellState // last: the state joined last
}

// setTrap records the trap code set in state st. The same code set again (in a loop,
// in a function called often) is one trap, run in the states of both.
func (a *execAnalysis) setTrap(code string, st *shellState, depth int) {
	for _, t := range a.traps {
		if t.code == code && t.depth == depth {
			t.st, t.last = join(t.st, st), st
			return
		}
	}
	a.traps = append(a.traps, &trap{code: code, body: a.parse(code), depth: depth, st: st, last: st})
}

// reached records that the shell reaches state st (a command runs in it): the traps set
// may run there.
func (a *execAnalysis) reached(st *shellState) {
	for _, t := range a.traps {
		if t.last != st {
			t.st, t.last = join(t.st, st), st
		}
	}
}

// endShell runs the traps the shell ending in state end has set, then restores those of
// its parent (saved). A trap setting traps sets them for the rest of the run: they run
// too, a few rounds of them.
func (a *execAnalysis) endShell(saved []*trap, end *shellState) {
	for round := 0; len(a.traps) > 0; round++ {
		traps := a.traps
		a.traps = nil
		if round == 4 {
			a.unresolved()
			break
		}
		for _, t := range traps {
			a.push('s')
			a.runList(t.body, join(t.st, end), t.depth)
			a.pop()
		}
	}
	a.traps = saved
}

func (a *execAnalysis) runCmd(c shCmd, st *shellState, depth int) (ok, fail *shellState) {
	if len(a.traps) > 0 {
		a.reached(st)
	}
	if !st.spend() {
		// Too much to follow: what is left may read anything.
		a.lose()
		if !st.unknown {
			st = st.clone()
			st.unknown = true
		}
		return st, st
	}
	switch n := c.(type) {
	case *shSimple:
		return a.runSimple(n, st, depth)
	case *shGroup:
		st = a.compoundRedirs(&n.redir, st, depth)
		if n.subshell {
			// A ( ... ) subshell gets its own working directory and variables.
			a.subshell(n.body, st, depth)
			return st, st
		}
		r := a.runList(n.body, st, depth)
		return r, r
	case *shIf:
		r := a.ifCmd(n, st, depth)
		return r, r
	case *shLoop:
		r := a.whileLoop(n, st, depth)
		return r, r
	case *shFor:
		r := a.forLoop(n, st, depth)
		return r, r
	case *shCase:
		r := a.caseCmd(n, st, depth)
		return r, r
	case *shArith:
		st = a.compoundRedirs(&n.redir, st, depth)
		if st.unknown {
			if len(n.subs) > 0 {
				a.lose()
			}
			return st, st
		}
		r := a.arith(n, st, depth)
		return r, r
	case *shFuncDef:
		a.define(n.name, n.body, st, depth)
		return st, nil
	}
	return st, st
}

func (a *execAnalysis) ifCmd(n *shIf, st *shellState, depth int) *shellState {
	st = a.compoundRedirs(&n.redir, st, depth)
	var out *shellState
	cur := st // where the conditions so far failed
	for i, cond := range n.conds {
		if cur == nil {
			break
		}
		ok, fail := a.runListFlow(cond, cur, depth)
		if ok != nil {
			out = join(out, a.runList(n.bodies[i], ok, depth))
		}
		cur = fail
	}
	if n.els != nil && cur != nil {
		return join(out, a.runList(n.els, cur, depth))
	}
	return join(out, cur)
}

// loop follows the body of a loop until the state at its head stops changing: iterate
// runs one iteration from a head state and returns the state it ends in; the loop's
// break and continue statements reach f. Past maxIter iterations, what still changes
// is unknown.
func (a *execAnalysis) loop(st *shellState, f *frame, iterate func(head *shellState) *shellState) {
	head := st
	for iter := 0; ; iter++ {
		f.cont = nil
		end := iterate(head)
		next := join(head, join(end, f.cont))
		if equal(next, head) {
			return
		}
		switch {
		case iter >= maxIter+4:
			return // unknown at the head: the body was analyzed in that state
		case iter >= maxIter+2:
			next = next.clone()
			next.unknown = true
		case iter >= maxIter:
			next = widen(head, next)
		}
		head = next
	}
}

func (a *execAnalysis) whileLoop(n *shLoop, st *shellState, depth int) *shellState {
	st = a.compoundRedirs(&n.redir, st, depth)
	f := a.push('l')
	defer a.pop()
	var exit *shellState
	a.loop(st, f, func(head *shellState) *shellState {
		ok, fail := a.runListFlow(n.cond, head, depth)
		if n.until {
			ok, fail = fail, ok
		}
		exit = join(exit, fail)
		if ok == nil {
			return nil
		}
		return a.runList(n.body, ok, depth)
	})
	return join(exit, f.brk)
}

func (a *execAnalysis) forLoop(n *shFor, st *shellState, depth int) *shellState {
	st = a.compoundRedirs(&n.redir, st, depth)
	// The values of the loop variable, and whether the list may be empty.
	var vals []string
	known, empty := true, true
	switch {
	case st.unknown:
	case n.arith != nil:
		st = a.arith(n.arith, st, depth)
	case n.words == nil:
		vals, known = st.vars["@"]
	default:
		var words []string
		words, st = a.substitute(n.words.words, nil, st, depth)
		empty = len(words) == 0
		for _, w := range n.words.words {
			// An expansion may stand for no word at all.
			empty = empty || strings.ContainsAny(w, "$") || strings.IndexByte(w, substOpen) >= 0
		}
		exp, ok := braceSeg(words)
		if ok {
			exp, ok = expandVars(exp, st.vars)
		}
		known = ok
		for _, w := range exp {
			// Quoting protects glob characters only where they are written: an unquoted
			// use of the variable expands them ($P with P='src/*.py').
			vals = append(vals, globChars.Replace(w))
		}
		if split, changed := splitFields(vals); changed {
			vals = union(vals, split)
		}
	}
	f := a.push('l')
	defer a.pop()
	var exit *shellState
	a.loop(st, f, func(head *shellState) *shellState {
		in := head
		if n.name != "" && !head.unknown {
			in = head.clone()
			if known {
				setVar(in.vars, n.name, vals)
			} else {
				delete(in.vars, n.name)
			}
			in.assigned(n.name)
		}
		end := a.runList(n.body, in, depth)
		exit = join(exit, join(end, f.cont))
		return end
	})
	if empty {
		exit = join(exit, st)
	}
	return join(exit, f.brk)
}

func (a *execAnalysis) caseCmd(n *shCase, st *shellState, depth int) *shellState {
	st = a.compoundRedirs(&n.redir, st, depth)
	if !st.unknown {
		_, st = a.substitute(n.word.words, nil, st, depth)
	}
	var out, prev *shellState
	catchAll := false
	for _, it := range n.items {
		if !st.unknown {
			a.substitute(it.patterns.words, nil, st, depth)
		}
		// ;& runs the next item's commands too, ;;& tests its patterns.
		end := a.runList(it.body, join(st, prev), depth)
		out = join(out, end)
		prev = nil
		if it.next == ";&" || it.next == ";;&" {
			prev = end
		}
		catchAll = catchAll || it.catchAll
	}
	if !catchAll {
		out = join(out, st) // no item matched
	}
	return out
}

// arith evaluates an arithmetic expression: it reads no file; its substitutions run and
// the variables it assigns become unknown.
func (a *execAnalysis) arith(ar *shArith, st *shellState, depth int) *shellState {
	for _, id := range ar.subs {
		_, st = a.substOutput(id, st, depth)
	}
	if names := assignedIn(ar.text); len(names) > 0 {
		st = st.clone()
		for _, name := range names {
			delete(st.vars, name)
		}
		st.assigned(names...)
	}
	return st
}

// compoundRedirs records what the redirections of a compound command read ("done <
// list", "} < file"); they are opened before its commands run.
func (a *execAnalysis) compoundRedirs(w *shWords, st *shellState, depth int) *shellState {
	if len(w.words) == 0 && len(w.extra) == 0 {
		return st
	}
	if st.unknown {
		a.lose()
		return st
	}
	words, st := a.substitute(w.words, w.extra, st, depth)
	alts, ok := argvs(words, st.vars)
	if !ok {
		a.unresolved()
		return st
	}
	for _, seg := range alts {
		for _, cwd := range st.cwds {
			a.redirects(seg, cwd)
		}
	}
	return st
}

// lose records, once, that commands run in a state no longer followed: they may read
// anything in the repository.
func (a *execAnalysis) lose() {
	if !a.lost {
		a.lost = true
		a.unresolved()
	}
}

// inert are builtins that read no file (unless redirected).
var inert = map[string]bool{
	":": true, "true": true, "false": true, "exit": true, "return": true, "break": true, "continue": true,
	"set": true, "shift": true, "export": true, "local": true, "declare": true, "typeset": true, "readonly": true,
	"unset": true, "cd": true, "pushd": true, "popd": true, "wait": true, "umask": true, "read": true,
	"alias": true, "getopts": true, "let": true, "mapfile": true, "readarray": true,
}

// harmless reports whether a simple command reads nothing, whatever the state: it is
// analyzed in a state no longer followed.
func (a *execAnalysis) harmless(c *shSimple) bool {
	if len(c.extra) > 0 {
		return false
	}
	for _, w := range c.words {
		if w == "<" || strings.IndexByte(w, substOpen) >= 0 {
			return false
		}
	}
	w := trimKeywords(trimAssignments(c.words))
	return len(w) == 0 || (!a.funcs[w[0]] && (nameOnly[filepath.Base(w[0])] || inert[w[0]]))
}

func hasSubst(c *shSimple) bool {
	if len(c.extra) > 0 {
		return true
	}
	for _, w := range c.words {
		if strings.IndexByte(w, substOpen) >= 0 {
			return true
		}
	}
	return false
}

// runSimple runs a simple command.
func (a *execAnalysis) runSimple(c *shSimple, st *shellState, depth int) (ok, fail *shellState) {
	if head := trimKeywords(trimAssignments(c.words)); len(head) > 0 && flowBuiltins[head[0]] {
		if hasSubst(c) {
			words := c.words
			if st.unknown {
				a.lose()
			} else {
				words, st = a.substitute(c.words, c.extra, st, depth)
			}
			head = trimKeywords(trimAssignments(words))
		}
		if a.flow(head, st) {
			return nil, nil
		}
		return st, st // return or break outside a function or loop: nothing happens
	}
	if st.unknown {
		// The working directory and the variables are not known: the command may read
		// anything in the repository.
		if !a.harmless(c) {
			a.lose()
		}
		return st, st
	}
	if assignmentsOnly(c.words) {
		return a.assignments(c, st, depth)
	}
	words, st := a.substitute(c.words, c.extra, st, depth)
	st = defaults(words, st)
	if merged := mergedVars(words, st.vars); len(merged) > 0 {
		if r, done := a.mergedRoles(words, merged, st); done {
			return r, r
		}
	}
	alts, okw := argvs(words, st.vars)
	if !okw {
		if w, bypass := commandWords(words); len(w) > 0 && (shellChangers[w[0]] || (!bypass && a.funcs[w[0]])) {
			// Too many words to follow a cd, a source or a call: the state is lost.
			return a.loseState(st)
		}
		for _, cwd := range st.cwds {
			a.opaque(cwd) // too many words to follow
			a.absoluteTrees(words, cwd, st.vars)
		}
		return st, st
	}
	for _, seg := range alts {
		o, f := a.runArgv(seg, st, depth)
		ok, fail = join(ok, o), join(fail, f)
	}
	return ok, fail
}

// redirOps are the redirection operators in words, each followed by its target.
var redirOps = map[string]bool{"<": true, ">": true, ">>": true, "<<": true, "<<<": true}

// assignmentsOnly reports whether words are assignments (and redirections): they assign
// variables of the shell.
func assignmentsOnly(words []string) bool {
	n := 0
	for i := 0; i < len(words); i++ {
		switch {
		case redirOps[words[i]]:
			i++
		case isAssign(words[i]):
			n++
		default:
			return false
		}
	}
	return n > 0
}

// assignments runs a command of assignments: each value is expanded in the state the
// assignments before it left.
func (a *execAnalysis) assignments(c *shSimple, st *shellState, depth int) (ok, fail *shellState) {
	st = st.clone()
	var redirs []string
	for i := 0; i < len(c.words); i++ {
		w := c.words[i]
		if redirOps[w] {
			if i+1 < len(c.words) {
				t, next := a.substituteWord(c.words[i+1], st, depth)
				redirs, st = append(redirs, w, t), next.clone()
			}
			i++
			continue
		}
		w, next := a.substituteWord(w, st, depth)
		st = defaults([]string{w}, next).clone()
		st.assigned(assignWord(st.vars, w))
	}
	for _, id := range c.extra {
		_, st = a.substOutput(id, st, depth)
	}
	if alts, ok := argvs(redirs, st.vars); ok {
		for _, seg := range alts {
			for _, cwd := range st.cwds {
				a.redirects(seg, cwd)
			}
		}
	} else {
		a.unresolved()
	}
	if hasSubst(c) {
		return st, st // the status of a command substitution
	}
	return st, nil
}

// assignTarget matches the start of an assignment word: NAME=, NAME+=, NAME[i]=.
var assignTarget = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(\[[^]]*\])?(\+)?=`)

// isAssign reports whether w is an assignment word (assign), at once for the words
// without "=" (most of them: regexp matching is costly under the race detector).
func isAssign(w string) bool { return strings.IndexByte(w, '=') > 0 && assign.MatchString(w) }

// assignMatch is assignTarget.FindStringSubmatch(w), nil at once without "=".
func assignMatch(w string) []string {
	if strings.IndexByte(w, '=') <= 0 {
		return nil
	}
	return assignTarget.FindStringSubmatch(w)
}

// assignWord applies the assignment word w to vars and returns the name it assigns.
func assignWord(vars map[string][]string, w string) string {
	m := assignMatch(w)
	if m == nil {
		return ""
	}
	name := m[1]
	if m[2] != "" {
		delete(vars, name) // an array element
		return name
	}
	vals := expandValue(w[len(m[0]):], vars)
	if m[3] != "" {
		old, ok := vars[name]
		if !ok || vals == nil {
			delete(vars, name)
			return name
		}
		var cat []string
		for _, o := range old {
			for _, v := range vals {
				cat = append(cat, o+v)
			}
		}
		vals = cat
	}
	setVar(vars, name, vals)
	return name
}

// expandValue returns the values a word assigned to a variable may have (no brace
// expansion, no field splitting); nil when there are too many.
func expandValue(value string, vars map[string][]string) []string {
	out, ok := expandVars([]string{value}, vars)
	if !ok {
		return nil
	}
	for i, v := range out {
		out[i] = globChars.Replace(v)
	}
	return union(out, nil)
}

// paramAssign matches ${NAME:=word} and ${NAME=word}, which assign NAME when it is
// unset (or empty).
var paramAssign = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*):?=([^}]*)\}`)

// defaults applies the ${NAME:=word} expansions of words: a known variable may now
// have word's values too.
func defaults(words []string, st *shellState) *shellState {
	var n *shellState
	for _, w := range words {
		if !strings.Contains(w, "${") {
			continue
		}
		for _, m := range paramAssign.FindAllStringSubmatch(w, -1) {
			old, ok := st.vars[m[1]]
			if !ok {
				continue // unknown stays unknown
			}
			if n == nil {
				n = st.clone()
			}
			vals := expandValue(m[2], st.vars)
			if vals == nil {
				delete(n.vars, m[1])
			} else {
				setVar(n.vars, m[1], union(old, vals))
			}
			n.assigned(m[1])
		}
	}
	if n == nil {
		return st
	}
	return n
}

// runArgv runs one command line (its words expanded) in st.
func (a *execAnalysis) runArgv(seg []string, st *shellState, depth int) (ok, fail *shellState) {
	w, bypass := commandWords(seg)
	if len(w) > 0 && !bypass && (a.funcs[w[0]] || strings.HasSuffix(w[0], "()")) {
		for _, cwd := range st.cwds {
			a.redirects(w, cwd)
		}
		r := a.callWith(st, w[0], w[1:])
		if (a.defs[w[0]] != nil || strings.HasSuffix(w[0], "()")) && !a.unsetFuncs[w[0]] && !a.unsetFuncs[""] {
			return r, r
		}
		// Not defined yet where it is called, or maybe unset since: the name runs a
		// builtin or a program too.
		o, f := a.runUncalled(seg, w, bypass, st, depth)
		return join(r, o), join(r, f)
	}
	return a.runUncalled(seg, w, bypass, st, depth)
}

// runUncalled runs a command line that calls no shell function (w and bypass as
// commandWords returns them).
func (a *execAnalysis) runUncalled(seg, w []string, bypass bool, st *shellState, depth int) (ok, fail *shellState) {
	if bypass {
		if len(w) > 0 && (w[0] == "-v" || w[0] == "-V") {
			return st, st // command -v NAME prints what NAME is
		}
		seg = w
	}
	if handled, ok, fail := a.builtin(seg, st, depth); handled {
		return ok, fail
	}
	for _, cwd := range st.cwds {
		a.command(seg, cwd, depth, true)
	}
	return st, st
}

// commandWords returns the words of a command from its name on (assignments and
// keywords trimmed); bypass says command or builtin preceded the name: a shell function
// of that name is not called.
func commandWords(seg []string) (w []string, bypass bool) {
	w = trimAssignments(seg)
	for len(w) > 0 && keywords[w[0]] {
		bypass = bypass || w[0] == "command" || w[0] == "builtin"
		w = w[1:]
	}
	if bypass {
		for len(w) > 0 && w[0] == "-p" {
			w = w[1:] // command -p: the default PATH
		}
	}
	return w, bypass
}

// shellChangers are the builtins whose arguments decide the state of the shell after
// them.
var shellChangers = map[string]bool{"cd": true, "pushd": true, "popd": true, "source": true, ".": true, "eval": true}

// loseState returns st no longer followed, after recording that what follows may read
// anything.
func (a *execAnalysis) loseState(st *shellState) (ok, fail *shellState) {
	a.unresolved()
	n := st.clone()
	n.unknown = true
	return n, n
}

// mergedVars returns the variables with several values seg uses when their
// combinations are more than maxAlts: argvs then puts every value of each in a single
// command line.
func mergedVars(seg []string, vars map[string][]string) []string {
	multi := multiVars(seg, vars)
	n := 1
	for _, m := range multi {
		if n *= len(vars[m]); n > maxAlts {
			return multi
		}
	}
	return nil
}

// mergedRoles handles a command whose variables with several values are merged into a
// single command line (mergedVars) where one value at a time matters: the program, the
// directory of a cd, the file a source reads, the parameters of a function, the
// arguments of a program that is not a mere reader (sh -c "$code", a launcher). The
// command may read anything; done says the state is lost too.
func (a *execAnalysis) mergedRoles(words, merged []string, st *shellState) (r *shellState, done bool) {
	w, bypass := commandWords(words)
	if len(w) == 0 {
		return st, false
	}
	uses := func(ws []string) bool {
		for _, x := range ws {
			for _, name := range varRefNames(x) {
				if slices.Contains(merged, name) {
					return true
				}
			}
		}
		return false
	}
	prog := filepath.Base(w[0])
	switch {
	case uses(w[:1]), uses(w[1:]) && (shellChangers[w[0]] || (!bypass && a.funcs[w[0]])):
		r, _ = a.loseState(st)
		return r, true
	case uses(w[1:]) && !readers[prog] && !nameOnly[prog] && !declarations[w[0]]:
		a.unresolved()
	}
	return st, false
}

// declarations are the builtins that assign variables: all the values of a variable
// merged into a command line are the values of the assigned one.
var declarations = map[string]bool{"export": true, "local": true, "declare": true, "typeset": true, "readonly": true}

// substitute evaluates the substitutions of words (in order) and of the here-documents
// extra lists, in state st: their commands are analyzed there. It returns the words
// with the substitutions' outputs and the state after them.
func (a *execAnalysis) substitute(words []string, extra []int, st *shellState, depth int) ([]string, *shellState) {
	out := make([]string, len(words))
	for i, w := range words {
		out[i], st = a.substituteWord(w, st, depth)
	}
	for _, id := range extra {
		_, st = a.substOutput(id, st, depth)
	}
	return out, st
}

func (a *execAnalysis) substituteWord(w string, st *shellState, depth int) (string, *shellState) {
	for {
		i := strings.IndexByte(w, substOpen)
		if i < 0 {
			return w, st
		}
		j := strings.IndexByte(w[i:], substClose)
		if j < 0 {
			return w, st
		}
		id, _ := strconv.Atoi(w[i+1 : i+j])
		var v string
		v, st = a.substOutput(id, st, depth)
		w = w[:i] + v + w[i+j+1:]
	}
}

// unresolvedSubst replaces the output of a command substitution. Like an unknown
// variable it contains "$", so a word using it is unresolved (an opaque command, an
// opaque cd) and never becomes a path.
const unresolvedSubst = "${__smates_subst__}"

// substOutput analyzes the commands of substitution id run in state st and returns
// what stands for its output and the state after it (an arithmetic expansion may assign
// variables; command substitutions run in a subshell). The output is unknown, except
// for $(pwd) and $(cd dir && pwd): the directory.
func (a *execAnalysis) substOutput(id int, st *shellState, depth int) (string, *shellState) {
	s := a.substs[id]
	switch s.kind {
	case '(':
		return unresolvedSubst, a.arith(s.arith, st, depth)
	case '<':
		a.subshell(s.body, st, depth)
		return "/dev/fd/63", st // a pipe
	}
	if pwd := pwdOnly(s.body); pwd != nil {
		a.push('s')
		ok, _ := a.runAndOrFlow(pwd, st, depth)
		a.pop()
		if ok != nil && !ok.unknown && len(ok.cwds) == 1 && !strings.ContainsAny(ok.cwds[0], " \t\n\"'$\\`;&|<>()*?[{}") {
			return ok.cwds[0], st
		}
		return unresolvedSubst, st
	}
	a.subshell(s.body, st, depth)
	return unresolvedSubst, st
}

// pwdOnly returns the and-or list of commands that only print a directory: "pwd", or
// cd commands joined by && and ending with pwd; nil otherwise.
func pwdOnly(l *shList) *shAndOr {
	if len(l.items) != 1 || l.items[0].bg {
		return nil
	}
	ao := l.items[0]
	for i, pl := range ao.pipes {
		s, ok := pl.cmds[0].(*shSimple)
		if len(pl.cmds) != 1 || pl.bang || !ok || len(s.extra) > 0 || (i > 0 && ao.ops[i-1] != "&&") {
			return nil
		}
		last := i == len(ao.pipes)-1
		if last && !slices.Equal(s.words, []string{"pwd"}) || !last && (len(s.words) == 0 || s.words[0] != "cd") {
			return nil
		}
	}
	return ao
}

// define records the definition of shell function name in state st. A new body
// invalidates the calls analyzed so far (they may call it).
func (a *execAnalysis) define(name string, body shCmd, st *shellState, depth int) {
	a.funcs[name] = true
	if d := a.defs[name]; d != nil && d.body == body {
		return // the same definition again (a body defining a function, called twice)
	}
	// $1 in the body is the function's own parameter (callWith), unknown where no call
	// reaches it (flush).
	def := st.clone()
	unbindPositional(def.vars)
	a.defs[name] = &funcDef{body: body, st: def, depth: depth}
	a.pending = append(a.pending, name)
	clear(a.memo)
	clear(a.relevant)
	clear(a.changes)
	clear(a.leaves)
}

// callWith analyzes a call of function name with the arguments args (shell words): its
// body sees them as its positional parameters (unbound when it shifts or sets them),
// and the caller's are restored afterwards.
func (a *execAnalysis) callWith(st *shellState, name string, args []string) *shellState {
	saved := positionalOf(st.vars)
	in := st.clone()
	unbindPositional(in.vars)
	if d := a.defs[name]; d != nil && !memoBool(&d.shifts, func() bool { return a.changesPositional(d.body) }) {
		bindPositional(in.vars, positional(args, true))
	}
	return withPositional(a.call(in, name), saved)
}

// call analyzes the body of the shell function name called in state st and returns
// the state after the call. A call in a state analyzed before replays its result.
func (a *execAnalysis) call(st *shellState, name string) *shellState {
	d := a.defs[name]
	if d == nil || a.calling[name] {
		// Not defined yet (flush analyzes it), or a recursive call (the body is being
		// analyzed).
		return st
	}
	// With a reference variable (declare -n), the body may assign any variable through
	// it: a call is not replayed in another state. Nor is it while traps are set: they
	// may run in any state of the body (see reached).
	refs := st.hasRef()
	key := a.callKey(st, name)
	if r, ok := a.memo[key]; ok && !refs && len(a.traps) == 0 {
		return r.apply(st)
	}
	a.calling[name] = true
	defer delete(a.calling, name)
	a.analyzed[name] = true
	if a.calls >= maxCalls {
		if scope := name + "\x00scope"; !a.analyzed[scope] {
			a.analyzed[scope] = true
			a.runBody(d, st)
		}
		// What the body changes is not applied: when it may change the working
		// directory, the pushd stack or variables, the caller's state is lost; when it
		// may break or continue the caller's loops, they may end or go on in a state
		// that is lost.
		n := st
		if a.mayChange(name) {
			n = st.clone()
			n.unknown = true
		}
		if a.mayLeave(name) {
			lost := st.clone()
			lost.unknown = true
			for _, i := range a.loops() {
				a.frames[i].brk = join(a.frames[i].brk, lost)
				a.frames[i].cont = join(a.frames[i].cont, lost)
			}
		}
		return n
	}
	a.calls++
	out, escaped := a.runBody(d, st)
	if out != nil {
		out = a.scopeLocals(d, st, out)
	}
	if !escaped && !refs && (out == nil || !out.hasRef()) {
		// A call whose break or continue reached the caller's loops did more than
		// change the state: it is analyzed again at each call.
		a.memo[key] = diffStates(st, out)
	}
	return out
}

// runBody runs the body of a function in st: the state after it is where it ends or
// returns; escaped says a break or continue in it reached a loop of the caller.
func (a *execAnalysis) runBody(d *funcDef, st *shellState) (out *shellState, escaped bool) {
	f := a.push('f')
	ok, fail := a.runCmd(d.body, st, d.depth)
	a.pop()
	return join(join(ok, fail), f.ret), f.escaped
}

// mayLeave reports whether the body of function name, or a function it calls, has a
// break or continue (that may reach a loop of its caller).
func (a *execAnalysis) mayLeave(name string) bool {
	if v, ok := a.leaves[name]; ok {
		return v
	}
	v := a.bodyHas(name, map[string]bool{}, func(w []string) bool { return w[0] == "break" || w[0] == "continue" })
	a.leaves[name] = v
	return v
}

// bodyHas reports whether the body of function name, or a function it calls, has a
// simple command (its words, keywords and assignments trimmed) match accepts.
func (a *execAnalysis) bodyHas(name string, seen map[string]bool, match func([]string) bool) bool {
	d := a.defs[name]
	if d == nil || seen[name] {
		return false
	}
	seen[name] = true
	found := false
	a.visit(d.body, func(n any) {
		if s, ok := n.(*shSimple); ok && !found {
			if w := trimKeywords(trimAssignments(s.words)); len(w) > 0 {
				found = match(w) || (a.defs[w[0]] != nil && a.bodyHas(w[0], seen, match))
			}
		}
	})
	return found
}

// scopeLocals returns the state after a call of d (out, called in st) with its local
// variables restored: a local may have been declared on some paths only, so each has
// the caller's values or the function's.
func (a *execAnalysis) scopeLocals(d *funcDef, st, out *shellState) *shellState {
	if d.locals == nil {
		d.locals = []string{}
		a.visit(d.body, func(n any) {
			s, ok := n.(*shSimple)
			if !ok {
				return
			}
			w := trimKeywords(s.words)
			if len(w) == 0 || (w[0] != "local" && w[0] != "declare" && w[0] != "typeset") || slices.Contains(w, "-g") {
				return
			}
			for _, arg := range w[1:] {
				name, _, _ := strings.Cut(arg, "=")
				if name = strings.TrimSuffix(name, "+"); validName.MatchString(name) {
					d.locals = append(d.locals, name)
				}
			}
		})
	}
	if len(d.locals) == 0 {
		return out
	}
	out = out.clone()
	for _, name := range d.locals {
		before, ok1 := st.vars[name]
		after, ok2 := out.vars[name]
		if ok1 && ok2 {
			setVar(out.vars, name, union(before, after))
		} else {
			delete(out.vars, name)
		}
	}
	return out
}

var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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

// changers are the builtins that may change the state of the shell running them.
var changers = map[string]bool{
	"cd": true, "pushd": true, "popd": true, "read": true, "mapfile": true, "readarray": true, "getopts": true,
	"printf": true, "let": true, "export": true, "local": true, "declare": true, "typeset": true, "readonly": true,
	"source": true, ".": true, "eval": true, "shift": true, "set": true, "unset": true,
}

func (a *execAnalysis) bodyMayChange(name string, seen map[string]bool) bool {
	d := a.defs[name]
	if d == nil || seen[name] {
		return false
	}
	seen[name] = true
	changes := false
	a.visit(d.body, func(n any) {
		switch n := n.(type) {
		case *shFor:
			changes = true
		case *shArith:
			changes = changes || len(assignedIn(n.text)) > 0
		case *shWords:
			for _, w := range n.words {
				changes = changes || (strings.Contains(w, "${") && paramAssign.MatchString(w))
			}
		case *shSimple:
			w := trimKeywords(n.words)
			if len(w) == 0 {
				return
			}
			changes = changes || isAssign(w[0]) || changers[w[0]] ||
				(a.defs[w[0]] != nil && a.bodyMayChange(w[0], seen))
		}
	})
	return changes
}

// callKey identifies the state a call of name is analyzed in: the working directories,
// the pushd stack and the values of the variables its body (and the functions it calls)
// may use or assign.
func (a *execAnalysis) callKey(st *shellState, name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%s\x00%v\x00%v\x00%v", name, strings.Join(st.cwds, "\x1f"), st.dirs, st.dirsLost, st.attrs)
	for _, v := range a.relevantVars(name, map[string]bool{}) {
		vals, ok := st.vars[v]
		fmt.Fprintf(&b, "\x00%s=%v:%s", v, ok, strings.Join(vals, "\x1f"))
	}
	return b.String()
}

// paramRef matches the variables a word refers to ($X, ${X...}, positional parameters).
var paramRef = regexp.MustCompile(`\$\{?[#!]?([A-Za-z_][A-Za-z0-9_]*|[1-9@*])`)

// relevantVars returns the variables the body of function name uses or assigns, also
// through the functions it calls.
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
	a.visit(d.body, func(n any) {
		switch n := n.(type) {
		case *shWords:
			for _, w := range n.words {
				if strings.IndexByte(w, '$') >= 0 {
					for _, m := range paramRef.FindAllStringSubmatch(w, -1) {
						set[m[1]] = true
					}
				}
				if m := assignMatch(w); m != nil {
					set[m[1]] = true
				}
			}
		case *shFor:
			set[n.name] = true
		case *shSimple:
			if w := trimKeywords(trimAssignments(n.words)); len(w) > 0 && a.defs[w[0]] != nil {
				for _, v := range a.relevantVars(w[0], seen) {
					set[v] = true
				}
			}
		}
	})
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
		frames, traps := a.frames, a.traps
		a.frames, a.traps = nil, nil
		out, _ := a.runBody(d, d.st)
		a.endShell(traps, out) // the traps it sets run at the end of a shell
		a.frames = frames
		delete(a.calling, name)
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
		switch {
		case seg[i] != "<":
		case strings.Contains(seg[i+1], "$"):
			a.unresolved() // an unresolved input (< "$1"), like cat "$1"
		default:
			for _, m := range expand(seg[i+1], cwd) {
				a.add(m)
			}
		}
	}
}

var (
	// funcName matches the names of shell functions (bash also allows "-", "." and ":").
	funcName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]*$`)
	// assign matches assignment words: NAME=, NAME+=, NAME[i]=.
	assign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?\+?=`)

	bashSource       = regexp.MustCompile(`\$\{BASH_SOURCE(?:\[0\])?\}|\$BASH_SOURCE\b`)
	bashSourcePrefix = regexp.MustCompile(`\$\{BASH_SOURCE(?:\[0\])?%`)
)

// varRefAt returns the end of the variable reference w[i:] starts with ($NAME, ${NAME},
// positional parameters $1...$9, $@, $*, also braced; $10 is ${1}0) and the variable's
// name; end is 0 when there is none. It is a scanner, not a regexp: it runs for most
// words, and regexp matching is costly, under the race detector especially.
func varRefAt(w string, i int) (end int, name string) {
	j := i + 1
	braced := j < len(w) && w[j] == '{'
	if braced {
		j++
	}
	k := j
	switch {
	case k < len(w) && isNameStart(w[k]):
		for k < len(w) && isNameChar(w[k]) {
			k++
		}
	case k < len(w) && (w[k] >= '1' && w[k] <= '9' || w[k] == '@' || w[k] == '*'):
		k++
	default:
		return 0, ""
	}
	if !braced {
		return k, w[j:k]
	}
	if k < len(w) && w[k] == '}' {
		return k + 1, w[j:k]
	}
	return 0, ""
}

// forVarRefs calls fn for each variable reference of w (see varRefAt), in order.
func forVarRefs(w string, fn func(start, end int, name string)) {
	for i := strings.IndexByte(w, '$'); i >= 0 && i < len(w); i++ {
		if w[i] != '$' {
			continue
		}
		if end, name := varRefAt(w, i); end > 0 {
			fn(i, end, name)
			i = end - 1
		}
	}
}

// varRefNames returns the names of the variables w refers to, in order.
func varRefNames(w string) []string {
	var names []string
	forVarRefs(w, func(_, _ int, name string) { names = append(names, name) })
	return names
}

// replaceVarRef returns w with its references to the variable name replaced by v.
func replaceVarRef(w, name, v string) string {
	var b strings.Builder
	last := -1
	forVarRefs(w, func(start, end int, n string) {
		if n == name {
			b.WriteString(w[max(last, 0):start])
			b.WriteString(v)
			last = end
		}
	})
	if last < 0 {
		return w
	}
	b.WriteString(w[last:])
	return b.String()
}

// multiVars returns the variables words use that have several values (not $@ and $*,
// which stand for several words at once).
func multiVars(seg []string, vars map[string][]string) []string {
	set := map[string]bool{}
	for _, w := range seg {
		for _, name := range varRefNames(w) {
			if name != "@" && name != "*" && len(vars[name]) > 1 {
				set[name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// argvs returns the command lines a command's words (seg) stand for: brace expansion,
// then variables. A command using variables with several values is a command line per
// combination of their values (cd "$D", the program, a function's parameters each take
// one at a time); past maxAlts combinations, a single command line has a word per value.
// A value with blanks is used both as a word and split into fields (an unquoted use).
// ok is false when a command line would have more than maxWords words.
func argvs(seg []string, vars map[string][]string) (alts [][]string, ok bool) {
	multi := multiVars(seg, vars)
	n := 1
	for _, m := range multi {
		if n *= len(vars[m]); n > maxAlts {
			break
		}
	}
	add := func(vs map[string][]string) bool {
		out, ok := braceSeg(seg)
		if ok {
			out, ok = expandVars(out, vs)
		}
		if !ok {
			return false
		}
		alts = append(alts, out)
		if split, changed := splitFields(out); changed {
			alts = append(alts, split)
		}
		return true
	}
	if len(multi) == 0 || n > maxAlts {
		return alts, add(vars)
	}
	idx := make([]int, len(multi))
	for {
		vs := maps.Clone(vars)
		for k, m := range multi {
			vs[m] = vars[m][idx[k] : idx[k]+1]
		}
		if !add(vs) {
			return nil, false
		}
		k := 0
		for ; k < len(multi); k++ {
			if idx[k]++; idx[k] < len(vars[multi[k]]) {
				break
			}
			idx[k] = 0
		}
		if k == len(multi) {
			return alts, true
		}
	}
}

// splitFields splits the words of seg with blanks (from a variable's value) into
// fields; changed says some word had blanks.
func splitFields(seg []string) (out []string, changed bool) {
	for _, w := range seg {
		if strings.ContainsAny(w, " \t\n") {
			changed = true
			out = append(out, strings.Fields(w)...)
		} else {
			out = append(out, w)
		}
	}
	return out, changed
}

// expandVars substitutes shell variables assigned earlier in the script; a word using
// a variable with several values becomes a word per value (the same value for every use
// of the variable in the word). Unknown variables are left in place (and make the
// command unresolved). ok is false when the command would have more than maxWords words
// (and more than it had).
func expandVars(seg []string, vars map[string][]string) (out []string, ok bool) {
	limit := max(maxWords, len(seg))
	for i, w := range seg {
		if !strings.Contains(w, "$") {
			out = append(out, w)
			continue
		}
		words := []string{w}
		done := map[string]bool{}
		for _, name := range varRefNames(w) {
			vals, ok := vars[name]
			if !ok || done[name] {
				continue
			}
			done[name] = true
			var next []string
			for _, cur := range words {
				for _, v := range vals {
					next = append(next, replaceVarRef(cur, name, v))
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

var keywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true, "for": true, "while": true, "until": true,
	"do": true, "done": true, "case": true, "esac": true, "in": true, "function": true, "{": true, "}": true,
	"!": true, "time": true, "command": true, "builtin": true, "exec": true, "then:": true,
}

var noops = map[string]bool{
	"set": true, "shift": true, "return": true, "exit": true, "wait": true, "alias": true, ":": true,
	"umask": true, "break": true, "continue": true,
}

// builtin handles shell builtins; it reports whether the command was one, and the
// states where it succeeded and failed.
func (a *execAnalysis) builtin(seg []string, st *shellState, depth int) (handled bool, ok, fail *shellState) {
	seg = trimKeywords(trimAssignments(seg))
	if len(seg) == 0 {
		return true, st, st
	}
	redirects := func() {
		for _, cwd := range st.cwds {
			a.redirects(seg, cwd) // read -r V < VERSION
		}
	}
	// unknown returns st with the variables names unknown (read from input).
	unknown := func(names ...string) *shellState {
		n := st.clone()
		for _, name := range names {
			delete(n.vars, name)
		}
		n.assigned(names...)
		return n
	}
	switch seg[0] {
	case "<", ">", ">>", "<<", "<<<":
		// Redirections of nothing: "< file".
		redirects()
		return true, st, st
	case "cd", "pushd":
		// A failed cd leaves the working directory as it was.
		return true, a.cd(seg, st), st
	case "popd":
		n := st.clone()
		switch {
		case n.dirsLost:
			n.unknown = true
		case len(n.dirs) > 0:
			n.cwds, n.dirs = n.dirs[len(n.dirs)-1], n.dirs[:len(n.dirs)-1]
		}
		return true, n, st
	case "source", ".":
		r := a.sourceCmd(seg, st, depth)
		return true, r, r
	case "eval":
		// Runs code the analysis does not know: anything may be read, the state is lost.
		a.unresolved()
		n := st.clone()
		n.unknown = true
		return true, n, n
	case "trap":
		// trap CODE SIGNAL...: the code runs when a signal arrives, at exit, at an
		// error: in any state the shell is in from now on (see endShell).
		args := seg[1:]
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) > 1 && args[0] != "-" && !strings.HasPrefix(args[0], "-") {
			if strings.Contains(args[0], "$") || strings.IndexByte(args[0], substOpen) >= 0 {
				a.unresolved()
			} else {
				a.setTrap(literal(args[0]), st, depth)
			}
		}
		return true, st, st
	case ":", "true":
		redirects()
		return true, st, nil
	case "false":
		redirects()
		return true, nil, st
	case "read":
		redirects()
		var names []string
		for i := 1; i < len(seg); i++ {
			w := seg[i]
			switch {
			case redirOps[w]:
				i++
			case w == "-a":
				if i+1 < len(seg) {
					names = append(names, seg[i+1])
				}
				i++
			case len(w) == 2 && strings.IndexByte("dinNptu", w[1]) >= 0 && w[0] == '-':
				i++ // an option's argument
			case strings.HasPrefix(w, "-"):
			default:
				names = append(names, w)
			}
		}
		if len(names) == 0 {
			names = []string{"REPLY"}
		}
		return true, unknown(names...), unknown(names...)
	case "mapfile", "readarray":
		redirects()
		name := "MAPFILE"
		for i := 1; i < len(seg); i++ {
			w := seg[i]
			switch {
			case redirOps[w]:
				i++
			case len(w) == 2 && strings.IndexByte("dnOsuCc", w[1]) >= 0 && w[0] == '-':
				i++
			case !strings.HasPrefix(w, "-"):
				name = w
			}
		}
		return true, unknown(name), unknown(name)
	case "getopts":
		unbound := unknown("OPTARG", "OPTIND")
		if len(seg) > 2 {
			delete(unbound.vars, seg[2])
			unbound.assigned(seg[2])
		}
		unbindPositional(unbound.vars)
		return true, unbound, unbound
	case "printf":
		if len(seg) > 2 && seg[1] == "-v" {
			n := unknown(seg[2])
			return true, n, n
		}
	case "let":
		var names []string
		for _, w := range seg[1:] {
			names = append(names, assignedIn(literal(w))...)
		}
		n := unknown(names...)
		return true, n, n
	case "export", "local", "declare", "typeset", "readonly":
		redirects()
		n := st.clone()
		array, funcs := false, false
		var attr byte // the attribute the options give the names
		vals := map[string][]string{}
		var names, bare []string
		for _, w := range seg[1:] {
			if strings.HasPrefix(w, "-") || strings.HasPrefix(w, "+") {
				array = array || strings.ContainsAny(w, "aA")
				if w[0] == '-' {
					a := declareAttr(seg[0], w[1:])
					funcs = funcs || a == 'f'
					if a == attrRef || (a == attrValue && attr == 0) {
						attr = a
					}
				}
				continue
			}
			m := assignMatch(w)
			if m == nil {
				if validName.MatchString(w) {
					bare = append(bare, w) // declared, not assigned
				}
				continue
			}
			if _, seen := vals[m[1]]; !seen {
				names = append(names, m[1])
			}
			if array || m[2] != "" || m[3] != "" {
				vals[m[1]] = nil
				continue
			}
			if v, ok := vals[m[1]]; !ok || v != nil {
				vals[m[1]] = append(v, globChars.Replace(w[len(m[0]):]))
			}
		}
		if funcs {
			return true, st, st // declare -f NAME: the names are functions
		}
		if attr != 0 {
			// declare -l X=v gives X a value the analysis does not follow, now and at
			// its later assignments; declare -n R=X makes R a reference to X (R=v then
			// assigns X), it assigns nothing through R.
			if n.attrs == nil {
				n.attrs = map[string]byte{}
			}
			for _, name := range append(bare, names...) {
				if n.attrs[name] != attrRef {
					n.attrs[name] = attr
				}
				delete(n.vars, name)
			}
			if attr == attrRef {
				return true, n, n
			}
		}
		for _, name := range names {
			if v := vals[name]; v == nil {
				delete(n.vars, name)
			} else {
				setVar(n.vars, name, union(v, nil))
			}
		}
		n.assigned(names...)
		return true, n, n
	case "unset":
		redirects()
		n := a.unset(seg[1:], st)
		return true, n, n
	case "shift":
		n := st.clone()
		unbindPositional(n.vars)
		return true, n, n
	case "set":
		if setsPositional(seg[1:]) {
			n := st.clone()
			unbindPositional(n.vars)
			return true, n, n
		}
	}
	if noops[seg[0]] {
		redirects()
		return true, st, st
	}
	return false, nil, nil
}

// unset runs unset with args in st: a variable unset is empty ($X expands to nothing);
// with an attribute, unknown, and through a reference (declare -n), the variable it
// refers to, any of them. A function unset (unset -f, or unset without options when no
// variable has the name) is no longer called: the name may run a program again
// (unsetFuncs). A name the analysis does not know may be any of them.
func (a *execAnalysis) unset(args []string, st *shellState) *shellState {
	n := st.clone()
	mode := 'b' // both a variable and a function
	i := 0
	for ; i < len(args); i++ {
		w := args[i]
		if w == "--" {
			i++
			break
		}
		if len(w) < 2 || w[0] != '-' {
			break
		}
		for _, c := range w[1:] {
			if c == 'f' || c == 'v' || c == 'n' {
				mode = c
			}
		}
	}
	for _, w := range args[i:] {
		name, _, element := strings.Cut(literal(w), "[")
		switch {
		case strings.Contains(w, "$") || strings.IndexByte(w, substOpen) >= 0 || !funcName.MatchString(name):
			// Unknown names: any variable, any function.
			clear(n.vars)
			a.unsetFuncs[""] = true
			continue
		case mode == 'f':
		case element || mode == 'n':
			delete(n.vars, name) // an array element, a reference itself: unknown
		case validName.MatchString(name):
			n.vars[name] = []string{""}
			n.assigned(name)
		}
		if mode == 'f' || mode == 'b' {
			a.unsetFuncs[name] = true
		}
	}
	return n
}

// declareAttr returns the attribute the option cluster opts (after "-") of the
// declaration builtin cmd gives the variables it names: attrRef for a reference
// (declare -n), attrValue for an attribute changing their values (-l, -u, -c, -i, an
// option the analysis does not know), 'f' for functions (-f, -F: the names are not
// variables), 0 otherwise.
func declareAttr(cmd, opts string) byte {
	harmless := "aAgprtxI" // arrays (handled apart), global, print, readonly, trace, export, inherit
	switch cmd {
	case "export":
		harmless = "pn" // export -n removes the export attribute
	case "readonly":
		harmless = "aAp"
	}
	var attr byte
	for _, c := range opts {
		switch {
		case c == 'f' || c == 'F':
			return 'f'
		case c == 'n' && cmd != "export" && cmd != "readonly":
			attr = attrRef
		case strings.ContainsRune(harmless, c):
		case attr == 0:
			attr = attrValue
		}
	}
	return attr
}

// cd returns the state after a cd or pushd command (seg) run in st.
func (a *execAnalysis) cd(seg []string, st *shellState) *shellState {
	n := st.clone()
	if seg[0] == "pushd" {
		n.dirs = append(n.dirs, slices.Clone(st.cwds))
	}
	var cwds []string
	for _, cwd := range st.cwds {
		ds, ok := a.cdTargets(seg, cwd)
		if !ok {
			// The directory is not known: the commands that follow do not run in the old
			// one, and may read anything in the repository.
			n.unknown = true
			return n
		}
		cwds = union(cwds, ds)
	}
	n.cwds = cwds
	if len(cwds) > maxCwds {
		n.unknown = true
	}
	return n
}

// sourceCmd runs a source (.) command in st: the file's commands run in the shell
// itself, changing its state.
func (a *execAnalysis) sourceCmd(seg []string, st *shellState, depth int) *shellState {
	if len(seg) < 2 {
		return st
	}
	if strings.Contains(seg[1], "$") {
		a.unresolved() // unresolved path: cannot follow
		n := st.clone()
		n.unknown = true
		return n
	}
	var out *shellState
	for _, cwd := range st.cwds {
		out = join(out, a.source(literal(seg[1]), cwd, st, depth, seg[2:]))
	}
	return out
}

func (a *execAnalysis) source(name, cwd string, st *shellState, depth int, args []string) *shellState {
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	a.add(p)
	data, err := readRegular(p, maxScript)
	if err != nil || depth >= 3 {
		// Not followed: missing (deleted after the run, say), not a regular file (a
		// device, a pipe), too big or too deep.
		a.unresolved()
		n := st.clone()
		n.unknown = true
		return n
	}
	zero := ""
	if len(a.zeros) > 0 {
		zero = a.zeros[len(a.zeros)-1]
	}
	key := p + "\x00" + zero
	l := a.sourced[key]
	if l == nil {
		l = a.parse(scriptIdioms(string(data), p, zero))
		a.sourced[key] = l
	}
	in := st
	var saved map[string][]string
	if len(args) > 0 {
		// . file ARGS runs the file with ARGS as its positional parameters.
		saved = positionalOf(st.vars)
		in = st.clone()
		unbindPositional(in.vars)
		bindPositional(in.vars, positional(args, true))
	}
	f := a.push('f')
	out := join(a.runList(l, in, depth+1), f.ret)
	a.pop()
	if saved != nil {
		out = withPositional(out, saved)
	}
	return out
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

// cdTargets returns the directories a cd or pushd command (seg) run in cwd may change
// to: a glob pattern (a shell expanded it, or a variable holds a for list's pattern)
// is each directory it matches, and the directory stays when none does (the cd
// fails). ok is false when the directory is unknown.
func (a *execAnalysis) cdTargets(seg []string, cwd string) ([]string, bool) {
	d, ok := cdTarget(seg, cwd)
	if !ok {
		return nil, false
	}
	arg := seg[len(seg)-1]
	for _, w := range seg[1:] {
		if !strings.HasPrefix(w, "-") || w == "-" {
			arg = w
			break
		}
	}
	if !strings.ContainsAny(arg, "*?[") {
		return []string{d}, true
	}
	a.glob(arg, cwd) // the matches change with the directory's entries
	var dirs []string
	for _, m := range expand(arg, cwd) {
		if fi, err := a.stat(m); err == nil && fi.IsDir() {
			dirs = append(dirs, filepath.Clean(m))
		}
	}
	if len(dirs) == 0 {
		return []string{cwd}, true
	}
	return dirs, true
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
		a.unresolved()
	case cmd:
		if depth >= 3 {
			a.unresolved()
		} else {
			a.shellText(script, cwd, "", depth+1, pos)
		}
	case operand >= 0 && !stdin:
		if strings.Contains(script, "$") {
			a.unresolved()
		} else {
			a.script(script, cwd, depth, pos)
		}
	case stdinFile != "" && !strings.Contains(stdinFile, "$"):
		a.script(stdinFile, cwd, depth, nil) // sh < script.sh
		return
	default:
		// Commands from stdin (a pipe, a terminal, a here-document) or an unresolved
		// script.
		a.unresolved()
		return
	}
	// Commands run by the shell may read its redirected stdin.
	switch {
	case stdinFile == "":
	case strings.Contains(stdinFile, "$"):
		a.unresolved()
	default:
		for _, m := range expand(stdinFile, cwd) {
			a.add(m)
		}
	}
}

// script analyzes a shell script file run by a new shell (bounded recursion) started
// with the positional parameters pos (nil: unknown).
func (a *execAnalysis) script(name, cwd string, depth int, pos []string) {
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	a.add(p)
	data, err := readRegular(p, maxScript)
	if err != nil || depth >= 3 {
		// Not followed: missing (deleted after the run, say), not a regular file (a
		// device, a pipe), too big or too deep.
		a.unresolved()
		return
	}
	a.shellText(string(data), cwd, p, depth+1, pos)
}

// maxScript bounds the size of the shell scripts analyzed (sourced or run).
const maxScript = 256 * 1024

// errNotRegular says a file is not read: it is not a regular file (a device such as
// /dev/zero or /dev/stdin, a pipe, a directory) or larger than the bound.
var errNotRegular = errors.New("not a regular file of a size the analysis reads")

// readRegular returns the content of the file p (symlinks followed) when it is a
// regular file of at most limit bytes; never reads anything else: a device, a pipe or
// a file of /proc may block or never end.
func readRegular(p string, limit int64) ([]byte, error) {
	f, err := openRegular(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errNotRegular
	}
	return data, err
}

// isRegular reports whether p is a regular file the analysis may read (openRegular).
func isRegular(p string) bool {
	f, err := openRegular(p)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// readHead returns the first n bytes (at most) of the regular file p.
func readHead(p string, n int64) ([]byte, error) {
	f, err := openRegular(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}

// openRegular opens the file p when it is a regular file outside the special file
// systems (/dev, /proc, /sys: their files may be endless or block even when regular).
func openRegular(p string) (*os.File, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, err
	}
	for _, special := range []string{"/dev", "/proc", "/sys"} {
		if resolved == special || strings.HasPrefix(resolved, special+"/") {
			return nil, errNotRegular
		}
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	// The file may have been replaced since: check the one opened.
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, errNotRegular
	}
	return f, nil
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
	head, err := readHead(p, 64)
	if err != nil {
		return false // missing, or not a regular file: no script
	}
	line := string(head)
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
