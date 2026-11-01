package selector

import (
	"slices"
	"sort"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/state"
)

// decideTests decides each unit of the suites Select decides (Input.Suites) and
// explains how its changed dependencies reached it, then marks the suites whose every
// test is selected (Suite.All).
func (s *selection) decideTests() {
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

// decideUnit decides one unit against its D(T): in a DiffUnrecorded selection a unit
// with no recorded entry is decided by static analysis against Base, any other in the
// selection's mode against its entry (see decide).
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
	} else {
		decide(t, s.res.Mode, u.entry, deps, ss.hasher, ss.changed)
	}
	s.reportConfig(t, u.via)
	explain(t, ss.sr, u.via, sets)
	return t
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
// test that did not pass last time.
func decide(t *Test, mode Mode, entry *state.Entry, deps []string, h *Hasher, changed map[string]bool) {
	switch mode {
	case ModeAll:
		t.Selected, t.Reason = true, ReasonAll
		return
	case ModeDiff:
		for _, d := range deps {
			// Without a recording, a Go file's declarations change when the file does.
			f, isFile := depkey.File(d)
			if changed[d] || (isFile && changed[f]) {
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
		if !ok || h.Hash(d) != recorded {
			t.Changed = append(t.Changed, d)
		}
	}
	switch {
	case len(t.Changed) > 0:
		t.Selected, t.Reason = true, ReasonChanged
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
