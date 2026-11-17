package selector

import (
	"fmt"
	"slices"
	"sort"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
)

// changes computes the changed files (Result.Changed): against the state's snapshot,
// or against Base when the state has none. In ModeAll nothing is compared.
//
// A state with entries but no snapshot and no Base (a state started in diff mode, or
// one only --suite runs wrote, where no base branch can be found: a shallow clone, no
// upstream) has nothing to tell the changes no test depends on from: they would select
// nothing, silently. The changes are then taken since the oldest commit the entries of
// the selection's suites ran at (entryBase), and when none of those is in the clone
// every test runs (ModeAll, which records a snapshot), with a note saying why.
func (s *selection) changes() error {
	if s.res.Mode == ModeAll {
		return nil
	}
	base := s.in.Base
	if len(s.in.State.Files) == 0 && base == "" {
		if base = s.entryBase(); base == "" {
			s.res.Mode = ModeAll
			s.res.Notes = append(s.res.Notes, "the state holds no snapshot of the files and there is no base revision (none given, and no commit the recorded tests ran at is in this clone): running everything, since a change no test depends on could not be told (a full run records the snapshot; or pass --base)")
			return nil
		}
		s.res.Notes = append(s.res.Notes, fmt.Sprintf("the state holds no snapshot of the files and there is no base revision: changes are taken since %s, the oldest commit the recorded tests ran at", shortSHA(base)))
		s.res.ChangesSince = base
	}
	changed, err := changedFiles(s.in, base)
	if err != nil {
		return err
	}
	s.changed = changed
	s.markerFilter()
	for f := range s.changed {
		s.res.Changed = append(s.res.Changed, f)
	}
	sort.Strings(s.res.Changed)
	return nil
}

// markerFilter drops the changes the suites of this selection have all seen: entries
// --suite runs wrote as markers (cli saveState, state.MarkerSnap) are unchanged for
// this run when every suite it runs saw the current content. A suite that has never
// been recorded (no entry in the state) has nothing to compare a marker with: its
// tests are decided against Base (DiffUnrecorded) or run as new. So a marker every
// recorded suite of the run saw is unchanged too, once some suite saw it; without
// that, a partial recording (`smates run --all --suite X`) would leave every file of
// the tree changed for an unscoped run, and an orphan of every suite. Such a file is
// still an orphan of the never-recorded suites that own it when it changed since Base
// and no test explains it (neverRecordedOrphans). This holds in state mode only: with
// no entry at all (diff mode) the changes against the snapshot decide every test.
//
// A suite whose entries all come from selective runs, after only partial recordings of
// the others (no full snapshot: State.Commit is unset, and no marker names it), has no
// snapshot to compare a marker with either: it counts as never recorded too, so that
// its first ordinary run does not turn every marker back into a change. Its entries
// are decided by their own dependencies, which markers do not affect. Only where its
// changes are judged against Base (DiffUnrecorded): without a base revision the
// markers stay changed for it, and the unexplained ones orphans, as before.
func (s *selection) markerFilter() {
	in := s.in
	if len(in.State.Files) == 0 {
		return
	}
	names := NewOwnership(in.Suites, in.Others).Picked()
	var snapshotted func(string) bool // computed once
	unsnapshotted := func(name string) bool {
		if in.State.Commit != "" || in.Base == "" || !in.DiffUnrecorded {
			return false
		}
		if snapshotted == nil {
			snapshotted = Snapshotted(in.State)
		}
		return !snapshotted(name)
	}
	var recorded, never []string
	for _, name := range names {
		if st := in.State.Suites[name]; st != nil && len(st.Tests) > 0 && !unsnapshotted(name) {
			recorded = append(recorded, name)
		} else {
			never = append(never, name)
		}
	}
	partial := s.res.Mode == ModeState && len(never) > 0
	if len(in.Others) == 0 && !partial {
		return
	}
	n := 0
	for f := range s.changed {
		v, _ := in.State.Snap(f)
		h, ok := v.SeenHash()
		if !ok || !gitx.ByContent.Same(h, in.Snap[f]) {
			continue
		}
		switch {
		case v.SeenByAll(names):
			delete(s.changed, f)
		case partial && len(v.SeenBy()) > 0 && v.SeenByAll(recorded):
			delete(s.changed, f)
			if s.unseen == nil {
				s.unseen = map[string]bool{}
			}
			s.unseen[f] = true
			n++
		}
	}
	if n > 0 {
		s.neverRecorded = never
	}
}

// Snapshotted returns whether the snapshot of st holds suite name's view of the tree:
// a full run took it (State.Commit is set), or a marker names the suite. Only a full
// recording of the suite (`--all --suite`) names it in markers; a selective run
// recording the orphans it handled names only suites that already count (see the
// cli's markHandledOrphans), so one handled orphan does not make a suite count as
// snapshotted and turn every marker it has not seen back into a change.
func Snapshotted(st *state.State) func(name string) bool {
	if st.Commit != "" {
		return func(string) bool { return true }
	}
	seen := map[string]bool{}
	for _, v := range st.Files {
		for _, n := range state.ParseSnap(v).SeenBy() {
			seen[n] = true
		}
	}
	return func(name string) bool { return seen[name] }
}

// entryBase returns the commit the changes are taken since when the state has no
// snapshot and there is no Base (see changes): the oldest commit of history HEAD shares
// with the commits the entries of the selection's suites (Input.Suites and Others) ran
// at that exist in the clone (gitx.Repo.OldestBase); "" when there is none.
func (s *selection) entryBase() string {
	in := s.in
	if in.Repo == nil {
		return ""
	}
	var commits []string
	for _, su := range s.everySuite() {
		if st := in.State.Suites[su.Name]; st != nil {
			for _, e := range st.Tests {
				commits = append(commits, e.Commit)
			}
		}
	}
	sort.Strings(commits)
	return in.Repo.OldestBase(slices.Compact(commits))
}

// shortSHA abbreviates a commit SHA for a note.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// changedFiles returns the files that differ between the snapshot and the state's
// snapshot (deleted ones included; compared as content, gitx.ByContent: a hash
// recorded without the executable marker matches, and so does a skip-worktree file
// not on disk), or, when the state has none, the files changed since base (none
// without base).
func changedFiles(in *Input, base string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(in.State.Files) > 0 {
		for f, h := range in.Snap {
			if old, ok := in.State.Files[f]; !ok || !gitx.ByContent.Same(old, h) {
				out[f] = true
			}
		}
		for f := range in.State.Files {
			if _, ok := in.Snap[f]; !ok {
				out[f] = true // deleted
			}
		}
		return out, nil
	}
	if base == "" {
		return out, nil
	}
	list, err := in.Repo.ChangedSince(base)
	if err != nil {
		return nil, fmt.Errorf("diff against %s: %w", base, err)
	}
	for _, f := range list {
		out[f] = true
	}
	return out, nil
}

// configInputs computes the global inputs: the files matching the top-level inputs
// and, with a configuration file, its settings key. Every test depends on its suite's
// settings (depkey.Config), not on the whole file: a test that reads smates.yml itself
// records the file's content. A change to the file is a change to the key.
func (s *selection) configInputs() {
	in := s.in
	s.globalInputs = expandInputs(in.Config.Inputs, s.files)
	if in.Config.Path != "" {
		s.globalInputs = append(s.globalInputs, depkey.Config(in.Config.Path))
		if s.changed[in.Config.Path] {
			s.changed[depkey.Config(in.Config.Path)] = true
		}
	}
}

// baseChanges computes, in state mode with Input.DiffUnrecorded and a Base, the files
// changed since Base (with the settings key when the configuration file is one): the
// tests with no recorded entry are decided against them by static analysis.
func (s *selection) baseChanges() error {
	in := s.in
	if !in.DiffUnrecorded || in.Base == "" || s.res.Mode != ModeState {
		return nil
	}
	list, err := in.Repo.ChangedSince(in.Base)
	if err != nil {
		return fmt.Errorf("diff against %s: %w", in.Base, err)
	}
	s.baseChanged = map[string]bool{}
	for _, f := range list {
		s.baseChanged[f] = true
	}
	if in.Config.Path != "" && s.baseChanged[in.Config.Path] {
		s.baseChanged[depkey.Config(in.Config.Path)] = true
	}
	return nil
}
