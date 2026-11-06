package selector

import (
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/static"
)

// suiteSel is one suite of a selection in progress.
type suiteSel struct {
	sr *Suite
	// selecting is set for the suites of Input.Suites (decided, in Result.Suites); the
	// suites of Input.Others only add to the coverage.
	selecting bool
	// hasher hashes the keys of the suite's tests (the suite's environment, the sets
	// of its depends_on/serves declarations).
	hasher *Hasher
	// setChanged and setChangedBase are the changed files of each declaration of the
	// suite, against the state's snapshot (or Base) and since Base (DiffUnrecorded).
	setChanged, setChangedBase map[*ServiceDeps][]string
	// changed and baseChanged are the selection's changed files plus the keys of the
	// declarations with changed files, so that diff mode sees a set key change.
	changed, baseChanged map[string]bool
	// eraseTypes is set when the suite's runner erases TypeScript types without
	// checking them (runner.ErasesTypes): its static closures leave type-only imports
	// out (see knownDeps).
	eraseTypes bool
	// rules are the suite's rules with their globs expanded (newSuiteRules).
	rules []*suiteRule
	units []*unitSel
}

// unitSel is one unit of a suite, between suiteDeps and decideTests.
type unitSel struct {
	id string
	// entry is the unit's recorded entry (nil: none).
	entry *state.Entry
	// via holds the unit's known dependencies (Suite.Known) by strongest explanation
	// (Via*); a dependency it does not hold was traced.
	via map[string]string
}

// suiteDeps computes D(T) for every unit of every suite, the suites left out
// included: Suite.Deps, Known, Inputs, Files and Services, Suite.Unrecorded, and the
// coverage of the changes (Result.Covered: the union of every D(T), plus the
// configuration file, which its settings key explains).
func (s *selection) suiteDeps() {
	in, res := s.in, s.res
	res.Covered = map[string]bool{}
	if s.baseChanged != nil {
		s.knownCovered = map[string]bool{}
	}
	if in.Config.Path != "" {
		res.Covered[in.Config.Path] = true // explained by depkey.Config
		if s.knownCovered != nil {
			s.knownCovered[in.Config.Path] = true
		}
	}
	hasher := NewHasher(in.Snap, IgnoreGlobs(in.Config)...).WithConfig(in.Config)
	if in.Repo != nil {
		hasher.WithRoot(in.Repo.Root)
	}
	every := s.everySuite()
	services := newServiceExpander(in.Config, every, s.files)
	for i, su := range every {
		ss := s.newSuiteSel(su, i < len(in.Suites), hasher, services)
		s.suites = append(s.suites, ss)
		if ss.selecting {
			res.Suites = append(res.Suites, ss.sr)
		}
		s.unitDeps(ss)
	}
}

// newSuiteSel starts the selection of suite su: its result, its depends_on/serves
// declarations expanded at HEAD, its hasher and the changes of its declarations.
func (s *selection) newSuiteSel(su *runner.Suite, selecting bool, hasher *Hasher, services *serviceExpander) *suiteSel {
	sr := &Suite{Suite: su, Name: su.Name, Deps: map[string][]string{}, Known: map[string][]string{}, Inputs: map[string]bool{}}
	sr.Services = services.forSuite(su)
	ss := &suiteSel{sr: sr, selecting: selecting, hasher: hasher.ForSuite(su).WithSets(sr.Services)}
	ss.setChanged, ss.setChangedBase = changedSets(sr.Services, s.changed), changedSets(sr.Services, s.baseChanged)
	ss.changed, ss.baseChanged = withSetKeys(s.changed, ss.setChanged), withSetKeys(s.baseChanged, ss.setChangedBase)
	return ss
}

// unitDeps computes D(T) for each unit of a suite: the dependencies known without a
// recording (knownDeps, Suite.Known) plus those of the unit's recorded entry, less the
// environment variables env_ignore now lists. A unit with no entry marks the suite
// Unrecorded. Every dependency is covered.
func (s *selection) unitDeps(ss *suiteSel) {
	in, sr, su := s.in, ss.sr, ss.sr.Suite
	tests := su.TestFiles(s.files)
	units := su.Adapter.Units(su, tests)
	sr.Files = groupUnitFiles(su, tests)
	inputs := append(append([]string{}, s.globalInputs...), suiteInputFiles(su, s.files)...)
	for _, f := range inputs {
		sr.Inputs[f] = true
	}
	static := config.Bool(su.Static, true) && in.Graph != nil
	if static {
		var starts []string
		for _, u := range units {
			starts = append(starts, sr.Files[u]...)
		}
		in.Graph.Warm(starts)
	}
	mapper, _ := su.Adapter.(runner.StaticMapper)
	// A runner that erases types without checking them never loads what tests reach
	// only through type-only imports (see runner.TypeEraser).
	ss.eraseTypes = static && mapper == nil && runner.ErasesTypes(su, runner.Sources{
		Files: s.files, Read: in.Graph.Read, Closure: in.Graph.Closure,
	})
	st := in.State.Suites[su.Name]
	ss.rules = newSuiteRules(su, s.files)

	for _, u := range units {
		var entry *state.Entry
		if st != nil {
			entry = st.Tests[u]
		}
		via, typeOnly := s.knownDeps(ss, u, entry, inputs, static, mapper)
		// Files reached only through type-only imports are no dependency, but a change
		// to them is explained (not an orphan): it cannot affect the tests.
		for _, f := range typeOnly {
			s.res.Covered[f] = true
			if s.knownCovered != nil {
				s.knownCovered[f] = true
			}
		}
		deps := make(map[string]bool, len(via))
		known := make([]string, 0, len(via))
		for f := range via {
			deps[f] = true
			known = append(known, f)
			if s.knownCovered != nil {
				s.knownCovered[f] = true
			}
		}
		sort.Strings(known)
		sr.Known[u] = known
		if entry == nil {
			sr.Unrecorded = true
		} else {
			for f := range entry.Deps {
				// A variable env_ignore lists now (it may have been recorded before).
				if !EnvIgnored(in.Config, su, f) {
					deps[f] = true
				}
			}
		}
		dl := make([]string, 0, len(deps))
		for f := range deps {
			dl = append(dl, f)
			s.res.Covered[f] = true
		}
		sort.Strings(dl)
		sr.Deps[u] = dl
		ss.units = append(ss.units, &unitSel{id: u, entry: entry, via: via})
	}
}

// knownDeps returns the dependencies of unit u known without its recording, each with
// how it reaches the test; later sources win: the sets of the depends_on/serves
// declarations that apply to it, its static import closure (static), the suite's
// inputs, the suite's rules and the unit's own test files. A declaration a unit
// depends on is declared (see declare). A Go compile check (runner.IsBuildCheck)
// depends on what the compiler reads, not on what the tests do at run time: neither
// declarations nor rules apply to it. With suiteSel.eraseTypes the static closure is
// the one at run time (static.Graph.RuntimeClosure), and typeOnly returns the files
// the full closure adds to it. The reaches of a rule see the full file closure, before
// the mapper keys it by declaration, and entry (the unit's recorded entry, nil: none)
// for the environment variables the unit read (see suiteRule.reaches).
func (s *selection) knownDeps(ss *suiteSel, u string, entry *state.Entry, inputs []string, static bool, mapper runner.StaticMapper) (via map[string]string, typeOnly []string) {
	sr, su := ss.sr, ss.sr.Suite
	files := sr.Files[u]
	via = map[string]string{}
	check := runner.IsBuildCheck(u)
	for _, d := range sr.Services {
		if !check && d.applies(su, u, files) {
			via[d.Key] = ViaService
			s.declare(d)
		}
	}
	reach := unitReach{entry: entry}
	if static {
		closure := s.in.Graph.Closure(files...)
		reach.closure, reach.known = closure, s.in.Graph.Analyzes(files...)
		if ss.eraseTypes {
			full := closure
			closure = s.in.Graph.RuntimeClosure(files...)
			if len(closure) != len(full) {
				typeOnly = sortedMinus(full, closure)
			}
		}
		if mapper != nil {
			closure = mapper.MapStatic(su, u, closure)
		}
		for _, f := range closure {
			via[f] = ViaImport
		}
	}
	for _, f := range inputs {
		via[f] = ViaInput
	}
	if !check {
		for _, r := range ss.rules {
			if r.applies(su, u, files, reach) {
				for _, f := range r.deps {
					via[f] = ViaInput
				}
			}
		}
	}
	// The unit's own test files, unless the mapper keyed them by declaration (a Go
	// test function depends on its own declaration and what it reaches, not on the
	// other functions of its file): those keys are the test itself then.
	for _, f := range files {
		own := false
		if static && mapper != nil {
			prefix := depkey.Decl(f, "")
			for k := range via {
				if strings.HasPrefix(k, prefix) {
					via[k], own = ViaTest, true
				}
			}
		}
		if !own {
			via[f] = ViaTest
		}
	}
	// Record never writes vendored paths (a committed node_modules file an input glob
	// matches): as known deps they would count as unrecorded, and changed, forever.
	for f := range via {
		if pathutil.IsVendored(f) {
			delete(via, f)
		}
	}
	return via, typeOnly
}

// sortedMinus returns the elements of the sorted list a that the sorted list b lacks.
func sortedMinus(a, b []string) []string {
	var out []string
	for _, x := range a {
		if _, found := slices.BinarySearch(b, x); !found {
			out = append(out, x)
		}
	}
	return out
}

// declare records that some test depends on declaration d (Result.declared) and
// covers its files, once per declaration however many tests depend on it.
func (s *selection) declare(d *ServiceDeps) {
	if slices.Contains(s.res.declared, d) {
		return
	}
	s.res.declared = append(s.res.declared, d)
	for _, f := range d.Files {
		s.res.Covered[f] = true
	}
}

// groupUnitFiles maps unit IDs to their test files.
func groupUnitFiles(su *runner.Suite, tests []string) map[string][]string {
	out := map[string][]string{}
	for _, f := range tests {
		for _, u := range su.Adapter.Units(su, []string{f}) {
			out[u] = append(out[u], f)
		}
	}
	return out
}

// expandInputs returns the files among files that match globs (repo-relative).
func expandInputs(globs []string, files []string) []string {
	if len(globs) == 0 {
		return nil
	}
	var out []string
	for _, f := range files {
		if pathutil.Match(globs, f) {
			out = append(out, f)
		}
	}
	return out
}

// suiteInputFiles returns the files among files that match su's inputs.
func suiteInputFiles(su *runner.Suite, files []string) []string {
	var out []string
	for _, f := range files {
		if su.InputMatch(f) {
			out = append(out, f)
		}
	}
	return out
}

// suiteRule is a rule of a suite with its globs expanded at HEAD.
type suiteRule struct {
	config.Rule
	// deps are the files Deps match.
	deps []string
	// reachFiles and reachEnv are Reaches split into file globs and variable name
	// patterns (config.Rule.ReachesEnv).
	reachFiles, reachEnv []string
	// matched memoizes whether reachFiles match a file: a file is in the closures of
	// many units.
	matched map[string]bool
}

// newSuiteRules expands the rules of su against files (the snapshot's).
func newSuiteRules(su *runner.Suite, files []string) []*suiteRule {
	var out []*suiteRule
	for _, r := range su.Rules {
		sr := &suiteRule{Rule: r, deps: expandInputs(r.Deps, files), matched: map[string]bool{}}
		sr.reachFiles, sr.reachEnv = r.ReachesEnv()
		out = append(out, sr)
	}
	return out
}

// unitReach is what the reaches of a rule are matched against for a unit.
type unitReach struct {
	// closure is the unit's file-level static closure; known is false when it tells
	// nothing (static analysis off, or no analyzer reads the unit's files).
	closure []string
	known   bool
	// entry is the unit's recorded entry (nil: none).
	entry *state.Entry
}

// applies reports whether the rule gives its deps to unit (with test files files):
// the unit matches its tests, when it has any, and reaches what its reaches names,
// when it has any.
func (r *suiteRule) applies(su *runner.Suite, unit string, files []string, reach unitReach) bool {
	if len(r.Tests) > 0 && !UnitMatches(su, unit, files, r.Tests) {
		return false
	}
	return len(r.Reaches) == 0 || r.reaches(reach)
}

// reaches reports whether a unit reaches what the rule's reaches names: its static
// closure holds a file a file glob matches, or its recorded entry an environment
// variable a "$NAME" pattern matches. A rule adds dependencies, but it also turns the
// files of its deps from orphans (which run the suites, or ask the agent) into
// dependencies of the units it applies to only; so when it cannot tell (the unit's
// closure is unknown, for a file glob; the unit has no recorded entry, for a variable
// pattern), the rule applies. The entry is not searched for files: it records the
// deps the rule added, and a rule reaching those would keep itself applying.
func (r *suiteRule) reaches(reach unitReach) bool {
	if len(r.reachFiles) > 0 {
		if !reach.known {
			return true
		}
		for _, f := range reach.closure {
			m, ok := r.matched[f]
			if !ok {
				m = pathutil.Match(r.reachFiles, f)
				r.matched[f] = m
			}
			if m {
				return true
			}
		}
	}
	if len(r.reachEnv) > 0 {
		if reach.entry == nil {
			return true
		}
		for k := range reach.entry.Deps {
			if kind, name := depkey.Parse(k); kind == depkey.KindEnv && matchName(r.reachEnv, name) {
				return true
			}
		}
	}
	return false
}

// matchName reports whether an environment variable name matches one of the patterns
// (path.Match globs).
func matchName(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// RuleReach counts, for each rule of su with reaches (by index in su.Rules), the units
// of the suite it applies to, as a selection over files with graph g (nil: no static
// analysis) and the suite's recorded entries st (nil: none) applies it. Compile checks
// get no rules and are not counted.
func RuleReach(su *runner.Suite, files []string, g *static.Graph, st *state.Suite) map[int]int {
	rules := newSuiteRules(su, files)
	out := map[int]int{}
	for i, r := range rules {
		if len(r.Reaches) > 0 {
			out[i] = 0
		}
	}
	if len(out) == 0 {
		return out
	}
	tests := su.TestFiles(files)
	unitFiles := groupUnitFiles(su, tests)
	units := su.Adapter.Units(su, tests)
	static := config.Bool(su.Static, true) && g != nil
	if static {
		var starts []string
		for _, u := range units {
			starts = append(starts, unitFiles[u]...)
		}
		g.Warm(starts)
	}
	for _, u := range units {
		if runner.IsBuildCheck(u) {
			continue
		}
		var reach unitReach
		if static {
			reach.closure, reach.known = g.Closure(unitFiles[u]...), g.Analyzes(unitFiles[u]...)
		}
		if st != nil {
			reach.entry = st.Tests[u]
		}
		for i := range out {
			if rules[i].applies(su, u, unitFiles[u], reach) {
				out[i]++
			}
		}
	}
	return out
}
