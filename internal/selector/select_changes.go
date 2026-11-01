package selector

import (
	"fmt"
	"sort"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
)

// changes computes the changed files (Result.Changed): against the state's snapshot,
// or against Base when the state has none. In ModeAll nothing is compared.
func (s *selection) changes() error {
	if s.res.Mode == ModeAll {
		return nil
	}
	changed, err := changedFiles(s.in)
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

// markerFilter drops, for a --suite run, the changes every suite it runs has already
// seen: entries --suite runs wrote as markers (cli saveState, state.MarkerSnap) are
// unchanged for this run when every suite it runs saw the current content.
func (s *selection) markerFilter() {
	in := s.in
	if len(in.Others) == 0 || len(in.State.Files) == 0 {
		return
	}
	names := NewOwnership(in.Suites, in.Others).Picked()
	for f := range s.changed {
		v, _ := in.State.Snap(f)
		if h, ok := v.SeenHash(); ok && gitx.SameHash(h, in.Snap[f]) && v.SeenByAll(names) {
			delete(s.changed, f)
		}
	}
}

// changedFiles returns the files that differ between the snapshot and the state's
// snapshot (deleted ones included; a hash recorded without the executable marker
// matches, see gitx.SameHash), or, when the state has none, the files changed since
// Base (none without Base).
func changedFiles(in *Input) (map[string]bool, error) {
	out := map[string]bool{}
	if len(in.State.Files) > 0 {
		for f, h := range in.Snap {
			if old, ok := in.State.Files[f]; !ok || !gitx.SameHash(old, h) {
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
	if in.Base == "" {
		return out, nil
	}
	list, err := in.Repo.ChangedSince(in.Base)
	if err != nil {
		return nil, fmt.Errorf("diff against %s: %w", in.Base, err)
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
