package selector

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/static/js"
)

// untracedUntrusted says why a reported pass of unit u, which the suite's untraced
// lists (UntracedUnit), cannot be recorded with its known dependencies (known) and what
// its run observed: "" when it can be.
//
// An untraced unit runs without code coverage (runner.RunContext.Untraced): the shims
// still see the files it reads, the directories it lists and the environment variables
// it reads, but the code it executes is known only from the static import closure. So
// a pass is trusted only when the runner records untraced units at all, the unit's
// test files are JavaScript or TypeScript (whose imports smates analyzes), and no known
// source may load code the closure misses (untracedBlind). Anything else stays unknown
// and runs again next time.
func (s *selection) untracedUntrusted(ss *suiteSel, u string, known []string, static bool) string {
	su := ss.sr.Suite
	if !runner.TracesUntraced(su.Runner) {
		return fmt.Sprintf("the %s runner runs untraced tests without any tracing", su.Runner)
	}
	if !static {
		return "static analysis is off"
	}
	if ss.loadedUnsure {
		return "smates cannot read all that the runner's configuration loads"
	}
	for _, f := range ss.sr.Files[u] {
		if !js.IsSource(f) {
			return fmt.Sprintf("%s is not JavaScript or TypeScript, whose imports smates analyzes", f)
		}
	}
	for _, k := range known {
		f, ok := depkey.File(k)
		if !ok || !js.IsSource(f) {
			continue
		}
		if spec := s.in.Graph.Unresolved(f); len(spec) > 0 {
			return fmt.Sprintf("%s imports %q, which smates cannot resolve", f, spec[0])
		}
		if why := untracedBlind(f, s.in.Graph.Read(f)); why != "" {
			return why
		}
	}
	return ""
}

// blindBuiltins are the builtin modules through which code runs other code (child
// processes, worker threads, modules required by a computed name) or reads files
// natively, out of the shims' sight: without coverage nothing records that code.
var blindBuiltins = map[string]bool{
	"child_process": true, "worker_threads": true, "cluster": true, "module": true,
	"wasi": true, "sqlite": true,
	"bun": true, // Bun.spawn, Bun.$, Bun.file
}

var (
	// runtimeRe matches the Bun and Deno globals that start processes or read files
	// without going through node:fs.
	runtimeRe = regexp.MustCompile(`\bBun\.(file|spawn|spawnSync|write|\$)|\bDeno\.(read|open|run|Command|stat|lstat)\w*\b`)
	// computedRe matches a dynamic import or require whose argument does not start with
	// a literal (a string, or a template the analyzer expands): the module is named at
	// run time.
	computedRe = regexp.MustCompile("\\b(import|require)\\s*\\(\\s*[^\\s'\"`)]")
)

// untracedBlind says why source f (whose content is src) may run code static analysis
// does not see, so that an untraced test depending on it cannot be trusted: it imports
// a builtin that starts processes or threads, loads modules by a computed name or reads
// files natively, uses a Bun or Deno API that does, or smates could not read it. ""
// when none. Matches in comments and strings count too: more is distrusted than needs
// to be, never less.
func untracedBlind(f string, src []byte) string {
	if src == nil {
		return fmt.Sprintf("smates could not read %s", f)
	}
	for _, imp := range js.Imports(f, src) {
		if imp.TypeOnly {
			continue
		}
		if name := strings.TrimPrefix(imp.Spec, "node:"); blindBuiltins[name] {
			return fmt.Sprintf("%s imports %q, which may run code no trace sees without coverage", f, imp.Spec)
		}
	}
	if m := runtimeRe.Find(src); m != nil {
		return fmt.Sprintf("%s uses %s, which may run or read what no trace sees", f, m)
	}
	if computedRe.Match(src) {
		return fmt.Sprintf("%s loads a module it names at run time", f)
	}
	return ""
}
