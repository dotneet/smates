package selector

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
)

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
