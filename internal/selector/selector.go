// Package selector decides which tests must run.
package selector

import (
	"github.com/dotneet/smates/internal/config"
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
	ReasonOnly       = "only"       // named with smates run --only
	ReasonCaller     = "caller"     // the caller added it (smates run --add)
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
	OrphanSkipped   = "skipped"   // the caller opted out of the orphan policy (--orphans=skip)
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
	// NoOrphanPolicy: the run applies no orphan policy (orphans: ignore,
	// --orphans=skip), so the changes that would be orphans for the tests
	// DiffUnrecorded decides by static analysis select none of them either.
	NoOrphanPolicy bool
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
	// Files maps each test to its test files (the file defining it, for a Go test
	// function).
	Files map[string][]string `json:"-"`
	// Unrecorded is set when some test of the suite has no recorded entry: its
	// dependencies are known from the static closure only.
	Unrecorded bool `json:"-"`
	// Services are the suite's depends_on/serves declarations, expanded at HEAD.
	Services []*ServiceDeps `json:"-"`
	// Browser is set when the suite's runner configuration may run tests in a browser
	// (runner.RunnerConfig): what they load there is never traced.
	Browser bool `json:"-"`
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

// applies reports whether the declaration covers a unit of suite su whose test files
// are files.
func (d *ServiceDeps) applies(su *runner.Suite, unit string, files []string) bool {
	return len(d.Tests) == 0 || UnitMatches(su, unit, files, d.Tests)
}

// UnitMatches reports whether the tests globs of a rule or a depends_on declaration
// (relative to the suite root) select unit, whose test files are files. A Go test
// function ("dir#TestName") is matched by its package directory, like the package unit
// it belongs to, and by the test file defining it.
func UnitMatches(su *runner.Suite, unit string, files, globs []string) bool {
	if pathutil.Match(globs, pathutil.RelTo(unit, su.Root)) {
		return true
	}
	if dir, _, ok := runner.SplitFuncUnit(unit); ok {
		if pathutil.Match(globs, pathutil.RelTo(dir, su.Root)) {
			return true
		}
		for _, f := range files {
			if pathutil.Match(globs, pathutil.RelTo(f, su.Root)) {
				return true
			}
		}
	}
	return false
}

// ServiceVia returns the label of the first depends_on/serves declaration through
// which unit depends on file f ("" when none does).
func (s *Suite) ServiceVia(unit, f string) string {
	for _, d := range s.Services {
		if d.match(f) && d.applies(s.Suite, unit, s.Files[unit]) {
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
	// own (see Ownership.Owned): not orphans of this run (the jobs running those suites
	// handle them), but tests of this run that name them are still selected (References).
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
	// loaded are the files a suite's runner loads through its configuration
	// (runner.RunnerConfig): never untested code.
	loaded map[string]bool
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

// IgnoreGlobs are the changes that never need tests unless a test reads them.
func IgnoreGlobs(c *config.Config) []string {
	return append(append([]string{}, c.Ignore...), config.DefaultIgnore...)
}

// allSelected reports whether the suite has tests and every one is selected.
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
