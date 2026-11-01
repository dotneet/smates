package selector

import (
	"slices"
	"sort"

	"github.com/dotneet/smates/internal/runner"
)

// Select computes the selection. Orphans are reported but not acted upon; callers apply
// the orphan policy with ApplyOrphans (after an optional agent call).
//
// It runs as a pipeline of steps over a selection:
//
//	newSelection     how the selection is computed (Mode) and the snapshot's files
//	changes          the changed files, less the markers of --suite runs (select_changes.go)
//	configInputs     the global inputs and the suites' settings key (select_changes.go)
//	baseChanges      the files changed since Base, for unrecorded tests (select_changes.go)
//	suiteDeps        D(T) of every unit of every suite and the coverage (select_deps.go)
//	decideTests      the decision and its explanation for each test (select_decide.go)
//	classifyOrphans  the changes no D(T) explains: orphans or LeftOut (select_orphans.go)
func Select(in *Input) (*Result, error) {
	s := newSelection(in)
	if err := s.changes(); err != nil {
		return nil, err
	}
	s.configInputs()
	if err := s.baseChanges(); err != nil {
		return nil, err
	}
	s.suiteDeps()
	s.decideTests()
	s.classifyOrphans()
	return s.res, nil
}

// selection is a selection in progress: the input, the result being filled and what
// the steps of Select pass on to the next ones.
type selection struct {
	in  *Input
	res *Result
	// files are the paths of the snapshot, sorted.
	files []string
	// changed are the changed files (deleted ones included) against the state's
	// snapshot or Base, plus the settings key when the configuration file changed; nil
	// in ModeAll.
	changed map[string]bool
	// baseChanged are the files changed since Base, used to decide the tests with no
	// recorded entry by static analysis (Input.DiffUnrecorded); nil otherwise.
	baseChanged map[string]bool
	// globalInputs are the files every test depends on (top-level inputs and the
	// settings key of the configuration file).
	globalInputs []string
	// suites are the suites of Input.Suites then Input.Others, as suiteDeps saw them.
	suites []*suiteSel
}

// newSelection starts a selection: its Mode (every test with All or with neither a
// recorded state nor a base revision, diff mode against Base with no recorded state,
// state mode otherwise) and the files of the snapshot.
func newSelection(in *Input) *selection {
	res := &Result{Mode: ModeState}
	switch {
	case in.All:
		res.Mode = ModeAll
	case in.State.Empty() && in.Base == "":
		res.Mode = ModeAll
		res.Notes = append(res.Notes, "no recorded state and no base revision: running everything (run `smates run` once on the default branch to record)")
	case in.State.Empty():
		res.Mode = ModeDiff
	}

	files := make([]string, 0, len(in.Snap))
	for f := range in.Snap {
		files = append(files, f)
	}
	sort.Strings(files)
	return &selection{in: in, res: res, files: files}
}

// everySuite returns the suites selection decides (Input.Suites) followed by the
// suites left out (Input.Others), which only add to the coverage.
func (s *selection) everySuite() []*runner.Suite {
	return append(slices.Clip(s.in.Suites), s.in.Others...)
}
