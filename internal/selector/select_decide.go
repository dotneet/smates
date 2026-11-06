package selector

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// decideTests decides each unit of the suites Select decides (Input.Suites) and
// explains how its changed dependencies reached it, then marks the suites whose every
// test is selected (Suite.All).
func (s *selection) decideTests() {
	s.unrecordedReads = s.findUnrecordedReads()
	for _, ss := range s.suites {
		if !ss.selecting {
			continue
		}
		for _, u := range ss.units {
			ss.sr.Tests = append(ss.sr.Tests, s.decideUnit(ss, u))
		}
		ss.sr.All = allSelected(ss.sr)
	}
}

// findUnrecordedReads returns, in a DiffUnrecorded selection with a suite that has
// units with no recorded entry, the files changed since Base that diff mode would find
// orphans (no dependency known without a recording explains them) but the recorded
// dependencies of other units explain, by the suites the orphan policy would run for
// them (by root, see orphanOwners). Tests read such a file as data, so a unit that has
// never run may read it too, which its static closure cannot tell. The changes nothing
// explains are orphans of this selection and left to the orphan policy. Like orphans,
// these files select nothing when the run applies no orphan policy
// (Input.NoOrphanPolicy).
func (s *selection) findUnrecordedReads() map[string][]string {
	in, res := s.in, s.res
	if s.baseChanged == nil || !slices.ContainsFunc(s.suites, func(ss *suiteSel) bool { return ss.selecting && ss.sr.Unrecorded }) {
		return nil
	}
	own := NewOwnership(in.Suites, in.Others)
	ignore := IgnoreGlobs(in.Config)
	files := make([]string, 0, len(s.baseChanged))
	for f := range s.baseChanged {
		files = append(files, f)
	}
	sort.Strings(files)
	out := map[string][]string{}
	n := 0
	for _, f := range files {
		if pathutil.IsVendored(f) || pathutil.Match(ignore, f) {
			continue
		}
		// Whether f was added since Base is not known: only a deletion counts as an
		// existence change, as for diff mode's orphans (existedBefore).
		if depkey.Covers(s.knownCovered, f, in.Snap[f] == "") != depkey.CoverNone || slices.ContainsFunc(res.declared, func(d *ServiceDeps) bool { return d.match(f) }) {
			continue
		}
		if !res.Explains(f, in.Snap[f] == "" || !existedBefore(in, f)) {
			continue // the orphan policy's (or handled by it before the snapshot)
		}
		n++
		for _, name := range own.Owners(f) {
			out[name] = append(out[name], f)
		}
	}
	if n > 0 && in.NoOrphanPolicy {
		res.Notes = append(res.Notes, fmt.Sprintf("%d file(s) changed since the base revision that tests read may affect tests with no recorded run: none selected for them (no orphan policy)", n))
		return nil
	}
	return out
}

// decideUnit decides one unit against its D(T): in a DiffUnrecorded selection a unit
// with no recorded entry is decided by static analysis against Base, and selected when
// tests read a file of its suite that changed since Base and that diff mode would find
// an orphan (findUnrecordedReads); any other in the selection's mode against its entry
// (see decide).
func (s *selection) decideUnit(ss *suiteSel, u *unitSel) *Test {
	t := &Test{ID: u.id}
	if u.entry != nil {
		t.Duration = u.entry.DurationMs
	}
	deps := ss.sr.Deps[u.id]
	sets := ss.setChanged
	if u.entry == nil && s.res.Mode == ModeState && s.baseChanged != nil {
		decide(t, ModeDiff, nil, deps, ss.hasher, ss.baseChanged)
		sets = ss.setChangedBase
		if reads := s.unrecordedReads[ss.sr.Name]; !t.Selected && len(reads) > 0 {
			t.Selected, t.Reason = true, ReasonNew
			t.Detail = "no recorded run, and tests read " + reads[0] + " (changed since the base revision)"
			if len(reads) > 1 {
				t.Detail = fmt.Sprintf("no recorded run, and tests read %s and %d more file(s) changed since the base revision", reads[0], len(reads)-1)
			}
		}
	} else {
		decide(t, s.res.Mode, u.entry, deps, ss.hasher, ss.changed)
	}
	s.runAnyway(t, ss, u)
	s.reportConfig(t, u.via)
	explain(t, ss.sr, u.via, sets)
	return t
}

// runAnyway selects a unit decide left out that runs whatever changed: the suite's
// volatile lists it (a compile check is never volatile: it does not run the tests), or
// its last recorded run is older than the suite's max_age.
func (s *selection) runAnyway(t *Test, ss *suiteSel, u *unitSel) {
	su := ss.sr.Suite
	if t.Selected {
		return
	}
	if len(su.Volatile) > 0 && !runner.IsBuildCheck(u.id) && UnitMatches(su, u.id, ss.sr.Files[u.id], su.Volatile) {
		t.Selected, t.Reason, t.Detail = true, ReasonVolatile, "listed in volatile: runs every time"
		return
	}
	if su.MaxAge > 0 && u.entry != nil {
		now := s.in.Now
		if now.IsZero() {
			now = time.Now()
		}
		if age := now.Sub(u.entry.At); age > su.MaxAge {
			t.Selected, t.Reason = true, ReasonExpired
			t.Detail = fmt.Sprintf("last run %s ago, older than max_age %s", age.Round(time.Minute), su.MaxAge)
		}
	}
}

// reportConfig reports a changed settings key (depkey.Config) among t.Changed as the
// configuration file, which reaches the test as an input unless via explains it
// otherwise.
func (s *selection) reportConfig(t *Test, via map[string]string) {
	path := s.in.Config.Path
	if path == "" {
		return
	}
	i := slices.Index(t.Changed, depkey.Config(path))
	if i < 0 {
		return
	}
	t.Changed = slices.Delete(t.Changed, i, i+1)
	if !slices.Contains(t.Changed, path) {
		t.Changed = append(t.Changed, path)
		sort.Strings(t.Changed)
		if via[path] == "" {
			via[path] = ViaInput
		}
	}
}

// decide sets whether t is selected and why, given its D(T) (deps): every test in
// ModeAll; in ModeDiff, a test with a changed dependency (a Go file's declarations
// change with the file); otherwise a test with no recorded entry, a test with a
// dependency whose hash differs from the recorded one (or that was not recorded) and a
// test that did not pass last time (skipping itself is no pass: ReasonUnproven).
func decide(t *Test, mode Mode, entry *state.Entry, deps []string, h *Hasher, changed map[string]bool) {
	switch mode {
	case ModeAll:
		t.Selected, t.Reason = true, ReasonAll
		return
	case ModeDiff:
		for _, d := range deps {
			// Without a recording, a Go file's declarations change when the file does,
			// and a Go package's shape and method sets when one of its Go files does.
			f, isFile := depkey.File(d)
			if changed[d] || (isFile && changed[f]) || goPackageChanged(d, changed) {
				t.Changed = append(t.Changed, d)
			}
		}
		if len(t.Changed) > 0 {
			t.Selected, t.Reason = true, ReasonDiff
		} else {
			t.Reason = ReasonUnaffected
		}
		return
	}
	if entry == nil {
		t.Selected, t.Reason, t.Detail = true, ReasonNew, "no recorded run"
		return
	}
	for _, d := range deps {
		recorded, ok := entry.Deps[d]
		if !ok && h.legacyConfig(d, entry.Deps) {
			continue
		}
		if !ok || !h.Unchanged(d, recorded) && !h.legacySuite(d, recorded) {
			t.Changed = append(t.Changed, d)
		}
	}
	switch {
	case len(t.Changed) > 0:
		t.Selected, t.Reason = true, ReasonChanged
	case entry.Status == state.Skip:
		t.Selected, t.Reason, t.Detail = true, ReasonUnproven, "skipped last run: not proven to pass"
	case entry.Status != state.Pass:
		t.Selected, t.Reason, t.Detail = true, ReasonFailed, "last run: "+entry.Status
	default:
		t.Reason = ReasonUnaffected
	}
}

// explain sets how each changed dependency reached t: via holds the known deps by
// strongest explanation (anything else was traced). A changed set key stands for the
// files of its declaration that changed (sets, against the snapshot or base revision
// decide compared with), each named with the declaration; it stays as the key only
// when none can be named (they changed between the test's run and the snapshot).
func explain(t *Test, sr *Suite, via map[string]string, sets map[*ServiceDeps][]string) {
	if len(t.Changed) == 0 {
		return
	}
	var changed []string
	t.Via = map[string]string{}
	add := func(c, how, service string) {
		if _, dup := t.Via[c]; dup {
			return
		}
		changed = append(changed, c)
		t.Via[c] = how
		if service != "" {
			if t.Service == nil {
				t.Service = map[string]string{}
			}
			t.Service[c] = service
		}
	}
	// Keys of their own first: a changed file that is also an import, an input or the
	// test itself keeps that stronger explanation over the declaration's.
	var setKeys []*ServiceDeps
	for _, c := range t.Changed {
		if d := sr.service(c); d != nil && via[c] == ViaService {
			setKeys = append(setKeys, d)
			continue
		}
		how := via[c]
		if how == "" {
			how = ViaTraced
		}
		add(c, how, "")
	}
	for _, d := range setKeys {
		if len(sets[d]) == 0 {
			add(d.Key, ViaService, d.Label)
		}
		for _, f := range sets[d] {
			add(f, ViaService, d.Label)
		}
	}
	sort.Strings(changed)
	t.Changed = changed
}

// goPackageChanged reports whether d is the shape or a method set of a Go package
// (depkey.Shape, depkey.Methods) one of whose Go files is among the changed files.
func goPackageChanged(d string, changed map[string]bool) bool {
	kind, dir := depkey.Parse(d)
	if kind != depkey.KindShape && kind != depkey.KindMethods {
		return false
	}
	for f := range changed {
		if strings.HasSuffix(f, ".go") && path.Dir(f) == dir {
			return true
		}
	}
	return false
}
