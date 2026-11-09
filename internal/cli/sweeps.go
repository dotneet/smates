package cli

// Source sweeps: many units of a Go per-function suite depending on (nearly) every source
// file of a directory as whole files, so that any edit there selects all of them. Such
// units track executed code per declaration, so a whole-file dependency means the file
// was read at runtime (a test helper globbing, parsing or hashing the sources in every
// test's process) or that coverage could not tell which bodies ran. Suites that track
// files whole are left out: there, a test importing most modules of a package looks the
// same, and is no mistake.

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

const (
	// sweepMinFiles: a directory with fewer source files is not worth reporting.
	sweepMinFiles = 10
	// sweepShare: a unit sweeps a directory when it depends on at least this share of
	// the directory's source files as whole files.
	sweepShare = 0.9
	// sweepMinUnits and sweepMinUnitShare: how many of a suite's recorded units must
	// sweep the same directory for it to be reported.
	sweepMinUnits     = 5
	sweepMinUnitShare = 0.2
)

// sourceSweep is one directory that many units of a suite depend on in full.
type sourceSweep struct {
	Suite   string
	Dir     string // repo-relative, "." for the root
	Files   int    // source files of Dir (not test files)
	Units   int    // units that depend on at least sweepShare of them
	Total   int    // units of the suite recorded in state
	Example string // one such unit
}

// sourceSweeps finds, for each suite of fine (the suites whose units track Go code per
// declaration), the directories whose source files (by extension, test files of any
// suite left out) most of its units depend on as whole files. files are the
// repository's files; tests the test files of every suite. Compile checks, which depend
// on their package's whole closure by design, are not counted.
func sourceSweeps(st *state.State, files []string, tests map[string]bool, fine map[string]bool) []sourceSweep {
	sources := map[string]map[string]bool{} // dir -> source files
	for _, f := range files {
		if !sourceExts[path.Ext(f)] || tests[f] || strings.Contains("/"+f, "/node_modules/") {
			continue
		}
		d := path.Dir(f)
		if sources[d] == nil {
			sources[d] = map[string]bool{}
		}
		sources[d][f] = true
	}
	var out []sourceSweep
	for _, name := range slices.Sorted(maps.Keys(st.Suites)) {
		if !fine[name] {
			continue
		}
		entries := map[string]*state.Entry{}
		for u, e := range st.Suites[name].Tests {
			if !runner.IsBuildCheck(u) {
				entries[u] = e
			}
		}
		sweeping := map[string][]string{} // dir -> units
		for _, unit := range slices.Sorted(maps.Keys(entries)) {
			perDir := map[string]int{}
			for k := range entries[unit].Deps {
				if depkey.KindOf(k) != depkey.KindContent {
					continue
				}
				if d := path.Dir(k); sources[d][k] {
					perDir[d]++
				}
			}
			for d, n := range perDir {
				if total := len(sources[d]); total >= sweepMinFiles && float64(n) >= sweepShare*float64(total) {
					sweeping[d] = append(sweeping[d], unit)
				}
			}
		}
		for _, d := range slices.Sorted(maps.Keys(sweeping)) {
			units := sweeping[d]
			if len(units) < sweepMinUnits || float64(len(units)) < sweepMinUnitShare*float64(len(entries)) {
				continue
			}
			out = append(out, sourceSweep{Suite: name, Dir: d, Files: len(sources[d]), Units: len(units), Total: len(entries), Example: units[0]})
		}
	}
	return out
}

// doctorSourceSweeps warns about the source sweeps of the recorded state.
func doctorSourceSweeps(p *project, r *doctorReport) {
	fine := map[string]bool{}
	for _, s := range p.suites {
		if s.Runner == "go" && s.Granularity == config.GranularityFunc {
			fine[s.Name] = true
		}
	}
	for _, sw := range sourceSweeps(p.state, p.files, testFilesOf(p.suites, p.files), fine) {
		r.add(checkWarn, "state-source-sweep", "%s", sw.message()).about(sw.Suite + ": " + sw.Dir).
			fix(doctorFix{Command: "smates deps '" + sw.Example + "'", Doc: docURL + "reference/troubleshooting/#a-small-change-selects-most-tests"})
	}
}

func (sw sourceSweep) message() string {
	dir := sw.Dir + "/"
	if sw.Dir == "." {
		dir = "the repository root"
	}
	return fmt.Sprintf("suite %s: %d of %d test units depend on (nearly) all %d source files of %s as whole files, so a change to any of them selects all %d. Executed Go code counts per declaration, so the tests read these files at runtime (a helper globbing, parsing or hashing the sources in every test's process), or coverage could not tell which bodies ran: compute what the tests need from the sources once (a generated or checked-in file, a cache outside the test process) or in one dedicated test, then record again. One such unit: %s",
		sw.Suite, sw.Units, sw.Total, sw.Files, dir, sw.Units, sw.Example)
}

// testFilesOf lists the test files of suites, for sourceSweeps.
func testFilesOf(suites []*runner.Suite, files []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range suites {
		for _, f := range s.TestFiles(files) {
			out[f] = true
		}
	}
	return out
}
