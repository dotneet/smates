// Package selector decides which tests must run.
package selector

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/static"
)

// Reason codes.
const (
	ReasonAll        = "all"        // forced full run
	ReasonNew        = "new"        // no recorded entry for this test
	ReasonChanged    = "changed"    // an input changed since the test last passed
	ReasonFailed     = "failed"     // the test did not pass last time
	ReasonOrphan     = "orphan"     // orphan policy "all" pulled the suite in
	ReasonReference  = "reference"  // a test references an orphan file by name
	ReasonAgent      = "agent"      // the agent asked for it
	ReasonDiff       = "diff"       // diff mode: static deps changed since base
	ReasonRerun      = "rerun"      // smates watch reran the previous selection
	ReasonUnaffected = "unaffected" // skipped
)

// How a changed dependency reached a test (Test.Via).
const (
	ViaTest      = "test"      // the test's own file
	ViaInput     = "input"     // a suite or global input, or a rule in smates.yml
	ViaService   = "service"   // depends_on/serves in smates.yml (Test.Service says which)
	ViaImport    = "import"    // the static import graph
	ViaTraced    = "traced"    // recorded by tracing a previous run (reads, listings, commands)
	ViaReference = "reference" // the test names an orphan file
	ViaOrphan    = "orphan"    // orphan policy "all"
)

// How an orphan (a change no test depends on) was handled (Result.OrphanHandling).
const (
	OrphanReference = "reference" // tests that name it were selected
	OrphanAgent     = "agent"     // the agent decided
	OrphanNoImpact  = "no-impact" // the agent judged it irrelevant to every test
	OrphanPolicy    = "policy"    // orphan policy "all": the suites it may affect run in full
	OrphanIgnored   = "ignored"   // orphan policy "ignore"
	OrphanUntested  = "untested"  // source code no test reaches (untested: skip)
)

// Input is everything selection needs.
type Input struct {
	Repo   *gitx.Repo
	Config *config.Config
	Suites []*runner.Suite
	// Others are the configured suites left out (--suite): none of their tests is
	// selected, but a change one of them depends on is not an orphan, so that the
	// selection of Suites is the same as in a run over every suite.
	Others []*runner.Suite
	State  *state.State
	Snap   gitx.Snapshot
	Graph  *static.Graph
	// Base is an optional git revision for diff mode (used for tests with no entry and
	// for orphan detection when the state has no snapshot).
	Base string
	// All forces every test.
	All bool
	// DiffUnrecorded decides tests that have no recorded entry by static analysis
	// against Base instead of selecting them as new (local use, where the state only
	// covers the tests that ran on this machine).
	DiffUnrecorded bool
}

// Test is the decision for one test unit.
type Test struct {
	ID       string   `json:"id"`
	Selected bool     `json:"selected"`
	Reason   string   `json:"reason"`
	Detail   string   `json:"detail,omitempty"`
	Changed  []string `json:"changed,omitempty"`
	// Via says, for each entry of Changed, how it reached the test (Via* constants).
	Via map[string]string `json:"via,omitempty"`
	// Service names, for each entry of Changed reached via ViaService, the declaration
	// that added it ("depends_on suite api", "serves backend/**").
	Service  map[string]string `json:"service,omitempty"`
	Duration int64             `json:"duration_ms,omitempty"`
	// Shard is the shard owning the test when the selection is sharded (1-based).
	Shard int `json:"shard,omitempty"`
	// Priority is how early `smates run` starts the test (1 = first; see
	// cli.prioritize), set on the tests a run starts.
	Priority int `json:"priority,omitempty"`
}

// Suite is the decision for one suite.
type Suite struct {
	Suite *runner.Suite `json:"-"`
	Name  string        `json:"name"`
	Tests []*Test       `json:"tests"`
	// All is true when every test of the suite is selected (the runner may then use its
	// own discovery instead of an explicit file list).
	All bool `json:"all"`
	// Deps holds D(T) for each test (repo-relative paths), used for explain and refscan.
	Deps map[string][]string `json:"-"`
	// Known holds D(T) without the previously recorded dependencies (static closure,
	// inputs and rules); recording unions it with what the new run observed.
	Known map[string][]string `json:"-"`
	// Inputs are the files every test of the suite depends on (suite and global inputs).
	Inputs map[string]bool `json:"-"`
	// Unrecorded is set when some test of the suite has no recorded entry: its
	// dependencies are known from the static closure only.
	Unrecorded bool `json:"-"`
	// Services are the suite's depends_on/serves declarations, expanded at HEAD.
	Services []*ServiceDeps `json:"-"`
}

// ServiceDeps is one depends_on/serves declaration of a suite expanded to files: code
// the suite's tests exercise across a boundary tracing cannot follow (a server running
// in another process or container). A test depends on the declaration through a single
// key (Key, see depkey.Set) whose hash covers every file, so the state grows with the
// number of declarations, not with files × tests.
type ServiceDeps struct {
	// Key is the dependency key of the whole set ("@set:suite:api").
	Key string
	// Label describes the declaration ("depends_on suite api", "serves backend/**").
	Label string
	// Tests narrow the units that get the dependency (globs relative to the suite
	// root; empty: all).
	Tests []string
	// Files are the files of the set at HEAD, sorted.
	Files []string
	// match reports whether a path belongs to the set (also a deleted one).
	match func(string) bool
}

// Match reports whether path f belongs to the declaration (also when f no longer
// exists).
func (d *ServiceDeps) Match(f string) bool { return d.match(f) }

// applies reports whether the declaration covers a unit of suite su.
func (d *ServiceDeps) applies(su *runner.Suite, unit string) bool {
	return len(d.Tests) == 0 || pathutil.Match(d.Tests, pathutil.RelTo(unit, su.Root))
}

// ServiceVia returns the label of the first depends_on/serves declaration through
// which unit depends on file f ("" when none does).
func (s *Suite) ServiceVia(unit, f string) string {
	for _, d := range s.Services {
		if d.match(f) && d.applies(s.Suite, unit) {
			return d.Label
		}
	}
	return ""
}

// service returns the declaration whose set key is key (nil when none is).
func (s *Suite) service(key string) *ServiceDeps {
	for _, d := range s.Services {
		if d.Key == key {
			return d
		}
	}
	return nil
}

// ExpandSets replaces the set keys among deps by the files of their sets.
func (s *Suite) ExpandSets(deps []string) []string {
	var out []string
	for _, k := range deps {
		if d := s.service(k); d != nil {
			out = append(out, d.Files...)
		} else {
			out = append(out, k)
		}
	}
	return out
}

// Mode is how a selection was computed.
type Mode string

const (
	ModeState Mode = "state" // against the recorded state
	ModeDiff  Mode = "diff"  // nothing recorded: static deps against the base revision
	ModeAll   Mode = "all"   // every test runs
)

// Result is the full selection.
type Result struct {
	Mode    Mode     `json:"mode"`
	Suites  []*Suite `json:"suites"`
	Changed []string `json:"changed"`
	Orphans []string `json:"orphans"`
	// LeftOut are the changes no test depends on that only suites left out by --suite
	// own (see Owned): not orphans of this run (the jobs running those suites handle
	// them), but tests of this run that name them are still selected (References).
	LeftOut []string `json:"left_out,omitempty"`
	// OrphanHandling says how each orphan was handled (Orphan* constants).
	OrphanHandling map[string]string `json:"orphan_handling,omitempty"`
	Notes          []string          `json:"notes,omitempty"`
	// Covered is the union of every test's D(T), including the suites --suite left out
	// (Input.Others): a changed file it does not explain (Covers) is an orphan.
	Covered map[string]bool `json:"-"`
	// declared are the depends_on/serves declarations some test depends on (their
	// files are in Covered; Explains also matches deleted ones).
	declared []*ServiceDeps
}

// HandleOrphans records how orphans were handled; earlier records win.
func (r *Result) HandleOrphans(files []string, how string) {
	if r.OrphanHandling == nil {
		r.OrphanHandling = map[string]string{}
	}
	for _, f := range files {
		if _, ok := r.OrphanHandling[f]; !ok {
			r.OrphanHandling[f] = how
		}
	}
}

// Selected returns the selected test IDs of a suite.
func (s *Suite) Selected() []string {
	var out []string
	for _, t := range s.Tests {
		if t.Selected {
			out = append(out, t.ID)
		}
	}
	return out
}

// Counts returns selected and total test counts.
func (r *Result) Counts() (sel, total int) {
	for _, s := range r.Suites {
		for _, t := range s.Tests {
			total++
			if t.Selected {
				sel++
			}
		}
	}
	return
}

// SuiteByName finds a suite.
func (r *Result) SuiteByName(name string) *Suite {
	for _, s := range r.Suites {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Select computes the selection. Orphans are reported but not acted upon; callers apply
// the orphan policy with ApplyOrphans (after an optional agent call).
func Select(in *Input) (*Result, error) {
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

	// Changed files: against the recorded snapshot, or the base revision.
	var changedSet map[string]bool
	if res.Mode != ModeAll {
		var err error
		changedSet, err = changedFiles(in)
		if err != nil {
			return nil, err
		}
		if len(in.Others) > 0 && len(in.State.Files) > 0 {
			// Entries --suite runs wrote as markers (cli saveState): unchanged
			// for this run when every suite it runs saw the current content.
			var names []string
			for _, s := range in.Suites {
				names = append(names, s.Name)
			}
			for f := range changedSet {
				if h, seen, ok := ParseMarker(in.State.Files[f]); ok && h == in.Snap[f] && subset(names, seen) {
					delete(changedSet, f)
				}
			}
		}
		for f := range changedSet {
			res.Changed = append(res.Changed, f)
		}
		sort.Strings(res.Changed)
	}

	globalInputs := expandInputs(in.Config.Inputs, files)
	if in.Config.Path != "" {
		// Every test depends on its suite's settings (ConfigKey), not on the whole
		// file: a test that reads smates.yml itself records the file's content.
		globalInputs = append(globalInputs, ConfigKey(in.Config.Path))
		if changedSet[in.Config.Path] {
			changedSet[ConfigKey(in.Config.Path)] = true
		}
	}

	var baseChanged map[string]bool
	if in.DiffUnrecorded && in.Base != "" && res.Mode == ModeState {
		list, err := in.Repo.ChangedSince(in.Base)
		if err != nil {
			return nil, fmt.Errorf("diff against %s: %w", in.Base, err)
		}
		baseChanged = map[string]bool{}
		for _, f := range list {
			baseChanged[f] = true
		}
		if in.Config.Path != "" && baseChanged[in.Config.Path] {
			baseChanged[ConfigKey(in.Config.Path)] = true
		}
	}
	covered := map[string]bool{} // union of all D(T)
	if in.Config.Path != "" {
		covered[in.Config.Path] = true // explained by ConfigKey
	}
	res.Covered = covered
	hasher := NewHasher(in.Snap, IgnoreGlobs(in.Config)...).WithConfig(in.Config)
	if in.Repo != nil {
		hasher.WithRoot(in.Repo.Root)
	}
	everySuite := append(slices.Clip(in.Suites), in.Others...)
	services := newServiceExpander(in.Config, everySuite, files)
	for i, su := range everySuite {
		selecting := i < len(in.Suites) // in.Others only add to covered
		sr := &Suite{Suite: su, Name: su.Name, Deps: map[string][]string{}, Known: map[string][]string{}, Inputs: map[string]bool{}}
		sr.Services = services.forSuite(su)
		hs := hasher.ForSuite(su).WithSets(sr.Services)
		// The changed files of each declaration, and the changed sets with them so
		// that diff mode sees a set key change.
		setChanged, setChangedBase := changedSets(sr.Services, changedSet), changedSets(sr.Services, baseChanged)
		suiteChanged, suiteBaseChanged := withSetKeys(changedSet, setChanged), withSetKeys(baseChanged, setChangedBase)
		if selecting {
			res.Suites = append(res.Suites, sr)
		}
		tests := su.TestFiles(files)
		units := su.Adapter.Units(su, tests)
		unitFiles := groupUnitFiles(su, tests)
		suiteInputs := append(append([]string{}, globalInputs...), suiteInputFiles(su, files)...)
		for _, f := range suiteInputs {
			sr.Inputs[f] = true
		}
		st := in.State.Suites[su.Name]

		var staticStarts []string
		for _, u := range units {
			staticStarts = append(staticStarts, unitFiles[u]...)
		}
		if config.Bool(su.Static, true) && in.Graph != nil {
			in.Graph.Warm(staticStarts)
		}

		for _, u := range units {
			deps := map[string]bool{}
			via := map[string]string{} // known deps by strongest explanation
			for _, d := range sr.Services {
				if d.applies(su, u) {
					deps[d.Key], via[d.Key] = true, ViaService
					if !slices.Contains(res.declared, d) {
						res.declared = append(res.declared, d)
						for _, f := range d.Files { // once per declaration, not per test
							covered[f] = true
						}
					}
				}
			}
			if config.Bool(su.Static, true) && in.Graph != nil {
				for _, f := range in.Graph.Closure(unitFiles[u]...) {
					deps[f], via[f] = true, ViaImport
				}
			}
			for _, f := range suiteInputs {
				deps[f], via[f] = true, ViaInput
			}
			for _, f := range ruleDeps(su, u, files) {
				deps[f], via[f] = true, ViaInput
			}
			for _, f := range unitFiles[u] {
				deps[f], via[f] = true, ViaTest
			}
			known := make([]string, 0, len(deps))
			for f := range deps {
				known = append(known, f)
			}
			sort.Strings(known)
			sr.Known[u] = known
			var entry *state.Entry
			if st != nil {
				entry = st.Tests[u]
			}
			if entry == nil {
				sr.Unrecorded = true
			}
			if entry != nil {
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
				covered[f] = true
			}
			sort.Strings(dl)
			sr.Deps[u] = dl
			if !selecting {
				continue
			}

			t := &Test{ID: u}
			if entry != nil {
				t.Duration = entry.DurationMs
			}
			setFiles := setChanged
			if entry == nil && res.Mode == ModeState && baseChanged != nil {
				decide(t, ModeDiff, nil, dl, hs, suiteBaseChanged)
				setFiles = setChangedBase
			} else {
				decide(t, res.Mode, entry, dl, hs, suiteChanged)
			}
			if in.Config.Path != "" {
				// The suite's settings key is reported as the file.
				if i := slices.Index(t.Changed, ConfigKey(in.Config.Path)); i >= 0 {
					t.Changed = slices.Delete(t.Changed, i, i+1)
					if !slices.Contains(t.Changed, in.Config.Path) {
						t.Changed = append(t.Changed, in.Config.Path)
						sort.Strings(t.Changed)
						if via[in.Config.Path] == "" {
							via[in.Config.Path] = ViaInput
						}
					}
				}
			}
			explain(t, sr, via, setFiles)
			sr.Tests = append(sr.Tests, t)
		}
		sr.All = allSelected(sr)
	}

	if res.Mode != ModeAll {
		all := append(slices.Clip(in.Suites), in.Others...)
		for _, f := range res.Changed {
			if pathutil.IsVendored(f) || pathutil.Match(IgnoreGlobs(in.Config), f) {
				continue
			}
			existenceChanged := in.Snap[f] == "" || !existedBefore(in, f)
			if !res.Explains(f, existenceChanged) {
				if len(in.Others) > 0 && !Owned(f, all, len(in.Suites)) {
					res.LeftOut = append(res.LeftOut, f)
					continue
				}
				res.Orphans = append(res.Orphans, f)
			}
		}
		if len(res.LeftOut) > 0 {
			res.Notes = append(res.Notes, fmt.Sprintf("%d unattributed change(s) belong only to suites left out by --suite: left to the jobs that run them", len(res.LeftOut)))
		}
	}
	return res, nil
}

// Explains reports whether some test's D(T) explains a change to f (see Covers): f is
// covered, or belongs to a depends_on/serves declaration some test depends on (also
// when f no longer exists, which Covered cannot tell).
func (r *Result) Explains(f string, existenceChanged bool) bool {
	return Covers(r.Covered, f, existenceChanged) != "" || slices.ContainsFunc(r.declared, func(d *ServiceDeps) bool { return d.match(f) })
}

// changedSets returns, for each declaration with changed files in changed, those
// files (sorted, deleted ones included).
func changedSets(decls []*ServiceDeps, changed map[string]bool) map[*ServiceDeps][]string {
	if len(decls) == 0 || len(changed) == 0 {
		return nil
	}
	out := map[*ServiceDeps][]string{}
	for f := range changed {
		for _, d := range decls {
			if d.match(f) {
				out[d] = append(out[d], f)
			}
		}
	}
	for _, l := range out {
		sort.Strings(l)
	}
	return out
}

// withSetKeys returns changed plus the keys of the declarations with changed files.
func withSetKeys(changed map[string]bool, sets map[*ServiceDeps][]string) map[string]bool {
	if len(sets) == 0 {
		return changed
	}
	out := maps.Clone(changed)
	for d := range sets {
		out[d.Key] = true
	}
	return out
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

// IgnoreGlobs are the changes that never need tests unless a test reads them.
func IgnoreGlobs(c *config.Config) []string {
	return append(append([]string{}, c.Ignore...), config.DefaultIgnore...)
}

// Covers reports how a dependency set explains a change to f ("" when it does not):
// "content" (f itself), "listing" or "existence" (a directory listing or an existence
// check, which explain f appearing or disappearing but not edits: an edited file only
// listed or probed may still be read by something tracing cannot see), or "tree" (a
// command smates cannot see into ran below a directory containing f). A tree at the
// repository root explains nothing: it would hide every change from the orphan check.
func Covers(deps map[string]bool, f string, existenceChanged bool) string {
	switch {
	case deps[f], deps[f+configKeySuffix]:
		return "content"
	case existenceChanged && deps[depkey.Listing(path.Dir(f))]:
		return "listing"
	case existenceChanged && deps[depkey.Existence(f)]:
		return "existence"
	}
	for d := path.Dir(f); d != "." && d != "/"; d = path.Dir(d) {
		if deps[depkey.Tree(d)] {
			return "tree"
		}
	}
	// A submodule (recorded as one entry for its commit) is explained by tests that
	// read files inside it.
	prefix := f + "/"
	for d := range deps {
		if strings.HasPrefix(d, prefix) {
			return "content"
		}
	}
	return ""
}

// existedBefore reports whether f existed when the state was recorded (or at the base
// revision in diff mode; unknown counts as existing, which keeps f an orphan).
func existedBefore(in *Input, f string) bool {
	if len(in.State.Files) > 0 {
		_, ok := SnapshotHash(in.State.Files, f)
		return ok
	}
	return true
}

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// SnapshotHash returns the hash a snapshot holds for f and whether it holds one. An
// Unrecorded entry counts as none: the hash after the marker is only what --suite runs
// of other suites compare (Select), never content the file's own suites have seen.
func SnapshotHash(files map[string]string, f string) (string, bool) {
	v, ok := files[f]
	if strings.HasPrefix(v, Unrecorded) {
		return "", false
	}
	return v, ok
}

func decide(t *Test, mode Mode, entry *state.Entry, deps []string, h *Hasher, changed map[string]bool) {
	switch mode {
	case ModeAll:
		t.Selected, t.Reason = true, ReasonAll
		return
	case ModeDiff:
		for _, d := range deps {
			if changed[d] {
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

func changedFiles(in *Input) (map[string]bool, error) {
	out := map[string]bool{}
	if len(in.State.Files) > 0 {
		for f, h := range in.Snap {
			if in.State.Files[f] != h {
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

func allSelected(s *Suite) bool {
	if len(s.Tests) == 0 {
		return false
	}
	for _, t := range s.Tests {
		if !t.Selected {
			return false
		}
	}
	return true
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

func suiteInputFiles(su *runner.Suite, files []string) []string {
	var out []string
	for _, f := range files {
		if su.InputMatch(f) {
			out = append(out, f)
		}
	}
	return out
}

// serviceExpander expands depends_on/serves declarations to files, each suite's code
// once per selection however many suites and tests depend on it.
type serviceExpander struct {
	ignore []string
	suites []*runner.Suite
	files  []string
	code   map[string]*suiteCode // suite name -> its code
}

// suiteCode is the code of a suite: a predicate and the files at HEAD.
type suiteCode struct {
	match func(string) bool
	files []string
}

func newServiceExpander(c *config.Config, suites []*runner.Suite, files []string) *serviceExpander {
	return &serviceExpander{ignore: IgnoreGlobs(c), suites: suites, files: files, code: map[string]*suiteCode{}}
}

// suiteCode is what {suite: name} stands for: the files under the suite's root other
// than test files (of any suite: two suites may share a root), ignored and vendored
// files, and files of other suites rooted below it (a suite at the repository root does
// not take in every other suite).
func (e *serviceExpander) suiteCode(name string) *suiteCode {
	if c, ok := e.code[name]; ok {
		return c
	}
	var target *runner.Suite
	var nested []string
	for _, s := range e.suites {
		if s.Name == name {
			target = s
		}
	}
	c := &suiteCode{match: func(string) bool { return false }}
	e.code[name] = c
	if target == nil {
		// Validation guarantees the suite exists; a caller that passed only some suites
		// (neither Suites nor Others has it) gets no dependency rather than a guess.
		return c
	}
	for _, s := range e.suites {
		if s.Root != target.Root && pathutil.Under(s.Root, target.Root) {
			nested = append(nested, s.Root)
		}
	}
	suites, ignore := e.suites, e.ignore
	c.match = func(f string) bool {
		return pathutil.Under(f, target.Root) && !pathutil.IsVendored(f) && !pathutil.Match(ignore, f) &&
			!slices.ContainsFunc(nested, func(r string) bool { return pathutil.Under(f, r) }) &&
			!slices.ContainsFunc(suites, func(s *runner.Suite) bool { return s.IsTestFile(f) })
	}
	for _, f := range e.files {
		if c.match(f) {
			c.files = append(c.files, f)
		}
	}
	return c
}

// forSuite expands the declarations of su. The suite's own test files are left out: a
// test does not depend on the other tests of its suite.
func (e *serviceExpander) forSuite(su *runner.Suite) []*ServiceDeps {
	var out []*ServiceDeps
	for _, d := range su.Dependencies() {
		var cand []string
		var in func(string) bool
		var id string
		if d.Suite != "" {
			c := e.suiteCode(d.Suite)
			cand, in, id = c.files, c.match, "suite:"+d.Suite
		} else {
			globs := d.Paths
			cand, in, id = expandInputs(globs, e.files), func(f string) bool { return pathutil.Match(globs, f) }, "paths:"+strings.Join(globs, ",")
		}
		sd := &ServiceDeps{Key: depkey.Set(id), Label: d.String(), Tests: d.Tests,
			match: func(f string) bool { return in(f) && !su.IsTestFile(f) && !pathutil.IsVendored(f) }}
		for _, f := range cand {
			if sd.match(f) {
				sd.Files = append(sd.Files, f)
			}
		}
		out = append(out, sd)
	}
	return out
}

func ruleDeps(su *runner.Suite, unit string, files []string) []string {
	var out []string
	rel := pathutil.RelTo(unit, su.Root)
	for _, r := range su.Rules {
		if !pathutil.Match(r.Tests, rel) {
			continue
		}
		out = append(out, expandInputs(r.Deps, files)...)
	}
	return out
}

// ---- orphan handling ----

// Addition is a test (or a whole suite) added on top of the deterministic selection.
type Addition struct {
	Suite  string
	Test   string // "" = every test of the suite
	Reason string
	Detail string
	// Files are the changed files that caused the addition (orphans), if known.
	Files []string `json:",omitempty"`
}

// viaFor is how the files of an addition with reason reached the test (only additions
// for orphans, by name reference or by the orphan policy, carry files).
func viaFor(reason string) string {
	switch reason {
	case ReasonReference:
		return ViaReference
	case ReasonOrphan:
		return ViaOrphan
	}
	return ""
}

// Apply marks additional tests as selected. Unknown tests are ignored and returned.
func (r *Result) Apply(adds []Addition) (unknown []Addition) {
	for _, a := range adds {
		s := r.SuiteByName(a.Suite)
		if s == nil {
			unknown = append(unknown, a)
			continue
		}
		found := false
		for _, t := range s.Tests {
			if a.Test == "" || t.ID == a.Test {
				found = true
				if !t.Selected {
					t.Selected, t.Reason, t.Detail, t.Changed, t.Via, t.Service = true, a.Reason, a.Detail, nil, nil, nil
					for _, f := range a.Files {
						if t.Via == nil {
							t.Via = map[string]string{}
						}
						t.Changed = append(t.Changed, f)
						t.Via[f] = viaFor(a.Reason)
					}
				}
			}
		}
		if !found {
			unknown = append(unknown, a)
		}
		s.All = allSelected(s)
	}
	return unknown
}

// OrphanAll returns additions that run, for each orphan, every suite it may affect:
// suites whose root contains the orphan (a suite rooted at the repository root contains
// every file), or all suites for orphans outside any non-root suite.
func (r *Result) OrphanAll(orphans []string) []Addition {
	roots := make([]string, len(r.Suites))
	for i, s := range r.Suites {
		roots[i] = s.Suite.Root
	}
	pick := map[string][]string{} // suite -> orphans that pull it in
	for _, o := range orphans {
		for _, i := range orphanOwners(o, roots) {
			pick[r.Suites[i].Name] = append(pick[r.Suites[i].Name], o)
		}
	}
	var adds []Addition
	for _, s := range r.Suites {
		if files, ok := pick[s.Name]; ok {
			adds = append(adds, Addition{Suite: s.Name, Reason: ReasonOrphan, Detail: "unattributed change: " + files[0], Files: files})
		}
	}
	return adds
}

// Unrecorded prefixes a snapshot entry that only some suites have compared: a
// --suite run records a file the suites left out also own (or own alone) as
// "?<hash>@<suites>", the hash it saw and the suites it ran (Marker). A --suite run
// whose suites all saw that hash finds the file unchanged; any other run, a run over
// every suite included, finds it changed until it records the file itself.
const Unrecorded = "?"

// Marker is the snapshot entry saying that suites saw a file with hash ("" = absent).
func Marker(hash string, suites []string) string {
	s := slices.Clone(suites)
	sort.Strings(s)
	return Unrecorded + hash + "@" + strings.Join(slices.Compact(s), ",")
}

// ParseMarker splits a Marker entry; ok is false for a content hash.
func ParseMarker(v string) (hash string, suites []string, ok bool) {
	rest, ok := strings.CutPrefix(v, Unrecorded)
	if !ok {
		return "", nil, false
	}
	hash, names, _ := strings.Cut(rest, "@")
	if names != "" {
		suites = strings.Split(names, ",")
	}
	return hash, suites, true
}

// MarkSeen returns the entry after suites saw a file with hash: the suites that
// already saw that same hash keep it, the others do not.
func MarkSeen(v, hash string, suites []string) string {
	if h, seen, ok := ParseMarker(v); ok && h == hash {
		return Marker(hash, append(seen, suites...))
	}
	return Marker(hash, suites)
}

// Owners returns the names of the suites of all the orphan policy would run for f.
func Owners(f string, all []*runner.Suite) []string {
	roots := make([]string, len(all))
	for i, s := range all {
		roots[i] = s.Root
	}
	var out []string
	for _, i := range orphanOwners(f, roots) {
		out = append(out, all[i].Name)
	}
	return out
}

// OwnedOnly reports whether every suite the orphan policy would run for f (see Owned)
// is one of the first n suites of all.
func OwnedOnly(f string, all []*runner.Suite, n int) bool {
	roots := make([]string, len(all))
	for i, s := range all {
		roots[i] = s.Root
	}
	for _, i := range orphanOwners(f, roots) {
		if i >= n {
			return false
		}
	}
	return true
}

// orphanOwners returns the indexes of the suites (by root) an orphan may affect: the
// suites whose root contains it, where a suite rooted at the repository root contains
// every file but only counts on its own when no other suite contains the orphan (then
// every suite may be affected).
func orphanOwners(o string, roots []string) []int {
	var out []int
	for i, r := range roots {
		if r != "." && r != "" && pathutil.Under(o, r) {
			out = append(out, i)
		}
	}
	if len(out) == 0 {
		for i := range roots {
			out = append(out, i)
		}
		return out
	}
	for i, r := range roots {
		if r == "." || r == "" {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// Owned reports whether a change belongs to at least one of the first n suites of all
// (the suites a --suite run selects; the rest are the suites left out): the orphan
// policy over every suite would run one of them for it. A change only suites left out
// own is theirs to handle, in the job that runs them.
func Owned(f string, all []*runner.Suite, n int) bool {
	roots := make([]string, len(all))
	for i, s := range all {
		roots[i] = s.Root
	}
	for _, i := range orphanOwners(f, roots) {
		if i < n {
			return true
		}
	}
	return false
}

// References finds tests that mention an orphan file by name in their own sources or
// dependencies (fixtures, migrations, schemas, scripts referenced as strings). It returns
// the additions and the orphans that were explained this way.
func (r *Result) References(g *static.Graph, orphans []string) ([]Addition, []string) {
	if g == nil || len(orphans) == 0 {
		return nil, nil
	}
	// A match on the file name selects the test; only a match that also names the
	// file's directory (or its full path) counts as explaining the orphan, so that a
	// same-named file elsewhere does not hide the change from the agent or the policy.
	type needle struct{ orphan, name, qualified string }
	var needles []needle
	for _, o := range orphans {
		base := path.Base(o)
		stem := strings.TrimSuffix(base, path.Ext(base))
		// Names that are too generic would match everything.
		if len(stem) < 4 || genericNames[strings.ToLower(stem)] {
			continue
		}
		q := o
		if d := path.Dir(o); d != "." {
			q = path.Base(d) + "/" + base
		}
		needles = append(needles, needle{o, base, q})
	}
	if len(needles) == 0 {
		return nil, nil
	}
	// Search each distinct source file once.
	hits := map[string]map[string]bool{} // file -> orphans referenced
	explainedTier0 := map[string]bool{}
	content := func(f string) string {
		if !isSourceLike(f) {
			return ""
		}
		return string(g.Read(f))
	}
	var adds []Addition
	explained := map[string]bool{}
	// Files every test of a suite depends on (package.json, lockfiles, runner config)
	// mention scripts and setup files: a match there would select the whole suite, so
	// they only count for orphans no test-specific file mentions.
	for tier := 0; tier < 2; tier++ {
		for _, s := range r.Suites {
			for _, t := range s.Tests {
				var why, named []string
				for _, d := range s.Deps[t.ID] {
					if s.Inputs[d] != (tier == 1) {
						continue
					}
					h, ok := hits[d]
					if !ok {
						h = map[string]bool{}
						c := content(d)
						if c != "" {
							for _, n := range needles {
								if strings.Contains(c, n.name) {
									h[n.orphan] = strings.Contains(c, n.qualified)
								}
							}
						}
						hits[d] = h
					}
					for o, qualified := range h {
						if tier == 1 && explainedTier0[o] {
							continue
						}
						why = append(why, o+" (in "+d+")")
						if !slices.Contains(named, o) {
							named = append(named, o)
						}
						if qualified {
							explained[o] = true
						}
					}
				}
				if len(why) > 0 && !t.Selected {
					sort.Strings(why)
					sort.Strings(named)
					adds = append(adds, Addition{Suite: s.Name, Test: t.ID, Reason: ReasonReference, Detail: "references " + strings.Join(why, ", "), Files: named})
				}
			}
		}
		if tier == 0 {
			for o := range explained {
				explainedTier0[o] = true
			}
		}
	}
	var ex []string
	for o := range explained {
		ex = append(ex, o)
	}
	sort.Strings(ex)
	return adds, ex
}

var genericNames = map[string]bool{
	"index": true, "main": true, "init": true, "test": true, "tests": true, "util": true, "utils": true,
	"config": true, "readme": true, "package": true, "setup": true, "types": true, "data": true, "base": true,
	"common": true, "helpers": true, "helper": true, "default": true, "__init__": true, "conftest": true,
	"makefile": true, "dockerfile": true, "license": true, "schema": true, "constants": true,
}

func isSourceLike(f string) bool {
	switch strings.ToLower(path.Ext(f)) {
	case ".js", ".mjs", ".cjs", ".jsx", ".ts", ".mts", ".cts", ".tsx", ".py", ".go", ".json", ".yaml", ".yml", ".toml", ".sh", ".sql", ".vue", ".svelte":
		return true
	}
	return false
}

// staticLangs are the source file extensions each runner's static analysis follows.
var staticLangs = map[string][]string{
	"go":     {".go"},
	"pytest": {".py"},
	"vitest": jsExts, "jest": jsExts, "node": jsExts, "bun": jsExts,
}

var jsExts = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// Untested returns the orphans that are source code no test can reach: a file in a
// language whose imports a suite follows statically (Go, Python, JS/TS) under that
// suite's root. Every test's import closure is part of its dependencies, so such a file
// is imported by no test, directly or through other code; and no test read or executed
// it in its last recorded run. Changing it cannot change a test's outcome — unless
// something outside the repository's tests runs it (a server an e2e suite talks to,
// which depends_on/rules declare, or a script started by a command smates cannot see
// into). Tool and runner configuration written as code (vite.config.ts, setup.py, ...)
// is never counted as untested.
func (r *Result) Untested(orphans []string) []string {
	if r.Mode != ModeState {
		// Without a recorded state D(T) holds the static closure only: files tests
		// read as data (fixtures, testdata) or import by name are not known yet.
		return nil
	}
	var out []string
	for _, o := range orphans {
		if r.untested(o) {
			out = append(out, o)
		}
	}
	return out
}

func (r *Result) untested(f string) bool {
	base := strings.ToLower(path.Base(f))
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if strings.HasSuffix(base, ".d.ts") || strings.Contains(stem, ".config") || strings.HasSuffix(stem, "rc") || untestedExcluded[stem] {
		return false
	}
	for _, seg := range strings.Split(path.Dir(f), "/") {
		if dataDirs[strings.ToLower(seg)] {
			return false // source files tests read as data
		}
	}
	// Every suite that may run f's code must have recorded all its tests: a test
	// with no entry is known by its import closure only, which misses code loaded by
	// name (a settings module, a plugin) and data reads.
	for _, s := range r.Suites {
		if s.Unrecorded && pathutil.Under(f, s.Suite.Root) {
			return false
		}
	}
	for _, s := range r.Suites {
		if !config.Bool(s.Suite.Static, true) || !slices.Contains(staticLangs[s.Suite.Runner], ext) {
			continue
		}
		if pathutil.Under(f, s.Suite.Root) && !pathutil.Match(s.Suite.Exclude, pathutil.RelTo(f, s.Suite.Root)) {
			return true
		}
	}
	return false
}

// dataDirs are directories whose source files tests use as data (inputs of analyzers
// and generators, golden files, fixture projects, mocks loaded by the runner).
var dataDirs = map[string]bool{
	"testdata": true, "fixtures": true, "fixture": true, "__fixtures__": true, "__mocks__": true,
	"__snapshots__": true, "golden": true,
}

// untestedExcluded are file stems of code that configures or drives tools rather than
// being imported: a change to them is left to the orphan policy.
var untestedExcluded = map[string]bool{
	"setup": true, "noxfile": true, "tasks": true, "fabfile": true, "conftest": true, "manage": true,
	"gulpfile": true, "gruntfile": true, "jakefile": true, "magefile": true, "dangerfile": true,
	"globalsetup": true, "global-setup": true, "globalteardown": true, "global-teardown": true, "setuptests": true,
	"setup-tests": true, "test-setup": true, "testsetup": true, "vitest.workspace": true, "jest.preset": true,
}
