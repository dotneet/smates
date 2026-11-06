package selector

import (
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/static"
)

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

// Ownership answers which suites own a change for a --suite run: the suites the
// orphan policy over every suite would run for it (see orphanOwners), among the suites
// the run picked and the suites it left out.
type Ownership struct {
	all   []*runner.Suite
	roots []string
	n     int // the first n of all are picked
}

// NewOwnership returns the ownership of changes between the suites a --suite run
// picked and the suites it left out.
func NewOwnership(picked, left []*runner.Suite) Ownership {
	all := append(slices.Clip(picked), left...)
	roots := make([]string, len(all))
	for i, s := range all {
		roots[i] = s.Root
	}
	return Ownership{all: all, roots: roots, n: len(picked)}
}

// Picked returns the names of the suites the run picked.
func (o Ownership) Picked() []string {
	var out []string
	for _, s := range o.all[:o.n] {
		out = append(out, s.Name)
	}
	return out
}

// Owners returns the names of the suites the orphan policy would run for f.
func (o Ownership) Owners(f string) []string {
	var out []string
	for _, i := range orphanOwners(f, o.roots) {
		out = append(out, o.all[i].Name)
	}
	return out
}

// Owned reports whether a change belongs to at least one of the picked suites: the
// orphan policy over every suite would run one of them for it. A change only suites
// left out own is theirs to handle, in the job that runs them.
func (o Ownership) Owned(f string) bool {
	for _, i := range orphanOwners(f, o.roots) {
		if i < o.n {
			return true
		}
	}
	return false
}

// OwnedOnly reports whether every suite the orphan policy would run for f is a picked
// one.
func (o Ownership) OwnedOnly(f string) bool {
	for _, i := range orphanOwners(f, o.roots) {
		if i >= o.n {
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

// Mention is a test that names a changed file in one of its own sources or
// dependencies (see Mentions).
type Mention struct {
	Suite  string
	Test   string
	Orphan string
	// File is the test source or dependency that names Orphan.
	File string
	// Qualified: File also names the orphan's directory (or full path), so the
	// mention explains the orphan.
	Qualified bool
}

// References finds tests that mention an orphan file by name in their own sources or
// dependencies (fixtures, migrations, schemas, scripts referenced as strings). It returns
// the additions and the orphans that were explained this way.
func (r *Result) References(g *static.Graph, orphans []string) ([]Addition, []string) {
	ms, ex := r.Mentions(g, orphans)
	return r.ReferenceAdditions(ms), ex
}

// ReferenceAdditions turns mentions into one addition per test not selected yet.
func (r *Result) ReferenceAdditions(ms []Mention) []Addition {
	type key struct{ suite, test string }
	var order []key
	why, named := map[key][]string{}, map[key][]string{}
	for _, m := range ms {
		k := key{m.Suite, m.Test}
		if _, ok := why[k]; !ok {
			order = append(order, k)
		}
		why[k] = append(why[k], m.Orphan+" (in "+m.File+")")
		if !slices.Contains(named[k], m.Orphan) {
			named[k] = append(named[k], m.Orphan)
		}
	}
	var adds []Addition
	for _, k := range order {
		s := r.SuiteByName(k.suite)
		if s == nil {
			continue
		}
		i := slices.IndexFunc(s.Tests, func(t *Test) bool { return t.ID == k.test })
		if i < 0 || s.Tests[i].Selected {
			continue
		}
		w, n := why[k], named[k]
		sort.Strings(w)
		sort.Strings(n)
		adds = append(adds, Addition{Suite: k.suite, Test: k.test, Reason: ReasonReference, Detail: "references " + strings.Join(w, ", "), Files: n})
	}
	return adds
}

// Mentions finds every test, selected or not, that mentions an orphan file by name in
// its own sources or dependencies, and the orphans explained this way (by a mention
// that also names the file's directory). Mentions in files every test of a suite
// depends on come last, and only for orphans no test-specific file explains.
func (r *Result) Mentions(g *static.Graph, orphans []string) ([]Mention, []string) {
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
	names := make([]string, len(needles))
	for i, n := range needles {
		names[i] = n.name
	}
	scan := newNameScanner(names)
	// Search each distinct source file once.
	hits := map[string]map[string]bool{} // file -> orphans referenced
	explainedTier0 := map[string]bool{}
	content := func(f string) string {
		if !isSourceLike(f) {
			return ""
		}
		return string(g.Read(f))
	}
	var out []Mention
	explained := map[string]bool{}
	// Files every test of a suite depends on (package.json, lockfiles, runner config)
	// mention scripts and setup files: a match there would select the whole suite, so
	// they only count for orphans no test-specific file mentions.
	for tier := 0; tier < 2; tier++ {
		for _, s := range r.Suites {
			for _, t := range s.Tests {
				if runner.IsBuildCheck(t.ID) {
					continue // a Go compile check reads no data file
				}
				var ms []Mention
				searched := map[string]bool{}
				for _, d := range s.Deps[t.ID] {
					if s.Inputs[d] != (tier == 1) {
						continue
					}
					// The declarations of a Go file are searched as the file itself (one
					// read for both keys).
					if f, ok := depkey.File(d); ok {
						d = f
					}
					if searched[d] {
						continue
					}
					searched[d] = true
					h, ok := hits[d]
					if !ok {
						h = map[string]bool{}
						c := content(d)
						if c != "" {
							for _, i := range scan.find(c) {
								n := needles[i]
								h[n.orphan] = strings.Contains(c, n.qualified)
							}
						}
						hits[d] = h
					}
					for o, qualified := range h {
						if tier == 1 && explainedTier0[o] {
							continue
						}
						ms = append(ms, Mention{Suite: s.Name, Test: t.ID, Orphan: o, File: d, Qualified: qualified})
						if qualified {
							explained[o] = true
						}
					}
				}
				sort.Slice(ms, func(i, j int) bool {
					if ms[i].Orphan != ms[j].Orphan {
						return ms[i].Orphan < ms[j].Orphan
					}
					return ms[i].File < ms[j].File
				})
				out = append(out, ms...)
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
	return out, ex
}

// nameScanner finds which of many names a text contains in one pass over the text,
// however many names there are (Aho-Corasick): a changed tree can hold thousands of
// orphans, and every test dependency is searched for all of them.
type nameScanner struct {
	names []string
	// root holds the transitions of the root by byte (0: stay at the root); every
	// other node lists its own.
	root  [256]int32
	nodes []scanNode
	stamp []uint32
	epoch uint32
}

type scanNode struct {
	edges []scanEdge
	// fail is the node of the longest proper suffix of this node's path that is a
	// path too; out the next node on that chain that ends a name (-1: none).
	fail, out int32
	// ends are the names this node's path is.
	ends []int
}

type scanEdge struct {
	b  byte
	to int32
}

// next returns the child of node n by byte b (-1: none).
func (x *nameScanner) next(n int32, b byte) int32 {
	if n == 0 {
		if c := x.root[b]; c != 0 {
			return c
		}
		return -1
	}
	for _, e := range x.nodes[n].edges {
		if e.b == b {
			return e.to
		}
	}
	return -1
}

func newNameScanner(names []string) *nameScanner {
	x := &nameScanner{names: names, nodes: []scanNode{{fail: 0, out: -1}}, stamp: make([]uint32, len(names))}
	for i, name := range names {
		n := int32(0)
		for j := 0; j < len(name); j++ {
			c := x.next(n, name[j])
			if c < 0 {
				c = int32(len(x.nodes))
				x.nodes = append(x.nodes, scanNode{out: -1})
				if n == 0 {
					x.root[name[j]] = c
				} else {
					x.nodes[n].edges = append(x.nodes[n].edges, scanEdge{name[j], c})
				}
			}
			n = c
		}
		x.nodes[n].ends = append(x.nodes[n].ends, i)
	}
	// Failure links, breadth first: a node's comes from its parent's.
	var queue []int32
	for b := 0; b < 256; b++ {
		if c := x.root[b]; c != 0 {
			queue = append(queue, c)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, e := range x.nodes[n].edges {
			f := x.nodes[n].fail
			for {
				if c := x.next(f, e.b); c >= 0 {
					f = c
					break
				}
				if f == 0 {
					break
				}
				f = x.nodes[f].fail
			}
			x.nodes[e.to].fail = f
			if len(x.nodes[f].ends) > 0 {
				x.nodes[e.to].out = f
			} else {
				x.nodes[e.to].out = x.nodes[f].out
			}
			queue = append(queue, e.to)
		}
	}
	return x
}

// find returns the indexes of the names text contains (as strings.Contains), in
// ascending order.
func (x *nameScanner) find(text string) []int {
	x.epoch++
	if x.epoch == 0 {
		clear(x.stamp)
		x.epoch = 1
	}
	var out []int
	add := func(n int32) {
		for _, i := range x.nodes[n].ends {
			if x.stamp[i] != x.epoch {
				x.stamp[i] = x.epoch
				out = append(out, i)
			}
		}
	}
	for _, name := range x.names {
		if name == "" {
			add(0) // contained in any text
		}
	}
	n := int32(0)
	for i := 0; i < len(text); i++ {
		for {
			if c := x.next(n, text[i]); c >= 0 {
				n = c
				break
			}
			if n == 0 {
				break
			}
			n = x.nodes[n].fail
		}
		if n == 0 {
			continue
		}
		add(n)
		for o := x.nodes[n].out; o > 0; o = x.nodes[o].out {
			add(o)
		}
	}
	sort.Ints(out)
	return out
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
