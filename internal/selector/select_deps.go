package selector

import (
	"slices"
	"sort"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
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
	units                []*unitSel
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
	st := in.State.Suites[su.Name]

	for _, u := range units {
		via := s.knownDeps(ss, u, inputs, static, mapper)
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
		var entry *state.Entry
		if st != nil {
			entry = st.Tests[u]
		}
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
// declarations nor rules apply to it.
func (s *selection) knownDeps(ss *suiteSel, u string, inputs []string, static bool, mapper runner.StaticMapper) map[string]string {
	sr, su := ss.sr, ss.sr.Suite
	files := sr.Files[u]
	via := map[string]string{}
	check := runner.IsBuildCheck(u)
	for _, d := range sr.Services {
		if !check && d.applies(su, u, files) {
			via[d.Key] = ViaService
			s.declare(d)
		}
	}
	if static {
		closure := s.in.Graph.Closure(files...)
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
		for _, f := range ruleDeps(su, u, files, s.files) {
			via[f] = ViaInput
		}
	}
	for _, f := range files {
		via[f] = ViaTest
	}
	// Record never writes vendored paths (a committed node_modules file an input glob
	// matches): as known deps they would count as unrecorded, and changed, forever.
	for f := range via {
		if pathutil.IsVendored(f) {
			delete(via, f)
		}
	}
	return via
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

// ruleDeps are the files the suite's rules declare for unit (with test files
// unitFiles), among files.
func ruleDeps(su *runner.Suite, unit string, unitFiles, files []string) []string {
	var out []string
	for _, r := range su.Rules {
		if !UnitMatches(su, unit, unitFiles, r.Tests) {
			continue
		}
		out = append(out, expandInputs(r.Deps, files)...)
	}
	return out
}
