// Package hints points at the likely cause of a failing test: the changed files among
// the inputs smates knows the test used (recorded dependencies, the static import
// closure, what the failing run itself traced), ranked by how directly the test
// depends on them.
package hints

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/state"
)

// Verdicts.
const (
	// VerdictChanged: some inputs of the test changed; they are the candidates.
	VerdictChanged = "changed-inputs"
	// VerdictUnattributed: none of the test's known inputs changed, but some changed
	// files are not attributed to any test; they are the candidates.
	VerdictUnattributed = "unattributed-changes"
	// VerdictUnchanged: none of the test's known inputs changed (flaky, environmental,
	// or caused by something tracing cannot see).
	VerdictUnchanged = "no-changed-inputs"
	// VerdictNoRecord: nothing to compare with (no recorded run and no diff basis).
	VerdictNoRecord = "no-record"
)

// Bases: what "changed" was measured against.
const (
	BasisLastRun = "last-run" // the content recorded when this test last ran
	BasisDiff    = "diff"     // the changed files of this run's selection (snapshot or base revision)
	BasisNone    = "none"
)

// Kinds of candidate files, in rank order.
const (
	KindSource  = "source"  // code (a direct import of the test ranks first)
	KindTest    = "test"    // the unit's own test file
	KindData    = "data"    // fixtures and other data the test read
	KindEnv     = "env"     // an environment variable the test read ("$NAME")
	KindConfig  = "config"  // suite inputs: manifests, lockfiles, runner configuration
	KindListing = "listing" // a directory listing, existence check or opaque tree
)

// How a test reaches a candidate file (Candidate.Link).
const (
	LinkTestFile     = "test file"
	LinkDirect       = "direct import"
	LinkStatic       = "static import"
	LinkInput        = "suite input"
	LinkTraced       = "traced"       // observed at runtime only
	LinkUnattributed = "unattributed" // a changed file no test is known to depend on
	LinkService      = "server code"  // declared with depends_on/serves in smates.yml
)

// MaxCandidates bounds the candidates kept per hint.
const MaxCandidates = 10

// Candidate is one changed input of a failing test.
type Candidate struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	// Link says how the test reaches the file (Link* constants).
	Link string `json:"link"`
	// Change: "modified", "added" or "deleted".
	Change string `json:"change"`
	// Old and New are the content hashes before and now ("" when absent).
	Old string `json:"-"`
	New string `json:"-"`

	tier  int
	mtime time.Time
	near  int
}

// Hint is the deterministic explanation of one failing test unit.
type Hint struct {
	Suite string `json:"suite"`
	Test  string `json:"test"`
	// Inferred is set when the failure was derived from the exit code of a run covering
	// several units: it may belong to another test.
	Inferred bool   `json:"inferred,omitempty"`
	Verdict  string `json:"verdict"`
	Basis    string `json:"basis"`
	// Since is the commit and LastStatus the outcome of the recorded run compared with.
	Since      string `json:"since,omitempty"`
	LastStatus string `json:"last_status,omitempty"`
	// Inputs is the number of known inputs; Changed how many of them changed.
	Inputs     int         `json:"inputs"`
	Changed    int         `json:"changed"`
	Candidates []Candidate `json:"candidates"`
	// More counts the candidates left out (beyond MaxCandidates).
	More int `json:"more,omitempty"`
	// Output is the failure output the runner reported for the test (bounded; empty
	// for runners without a per-test reporter).
	Output string `json:"output,omitempty"`
	// Explanation is the agent's reading of the failure (optional).
	Explanation *Explanation `json:"explanation,omitempty"`
}

// Explanation is a short pointer to the probable cause produced by the agent.
type Explanation struct {
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	Reason     string `json:"reason"`
	Confidence string `json:"confidence"` // low | medium | high
	Model      string `json:"model,omitempty"`
	Cached     bool   `json:"cached,omitempty"`
}

// Input describes one failing unit.
type Input struct {
	Suite, Unit string
	Inferred    bool
	// TestFiles are the unit's own test files (repo-relative).
	TestFiles []string
	// Deps are the test's known inputs (recorded, static and rule dependencies);
	// Observed what the failing run traced. Both are dependency keys (see selector.Hasher).
	Deps     []string
	Observed map[string]bool
	// Static holds the static import closure; Inputs the suite inputs.
	Static map[string]bool
	Inputs map[string]bool
	// Entry is the recorded entry from before this run (nil when there is none).
	Entry *state.Entry
	// Changed is the set of files changed in this run's selection (nil when there is
	// no basis, e.g. a full run).
	Changed map[string]bool
	// Orphans are changed files no test is known to depend on, with their previous
	// hash ("" when unknown): the candidates when none of the test's inputs changed.
	Orphans map[string]string
	// Hash returns a dependency key's current hash ("" when absent).
	Hash func(string) string
	// Unchanged reports whether a key recorded with a hash other than its current one
	// still counts as unchanged (selector.Hasher.Unchanged: a hash recorded without the
	// executable marker; nil allowed).
	Unchanged func(key, recorded string) bool
	// Direct returns the static direct imports of a file (nil allowed).
	Direct func(string) []string
	// Service reports files the unit depends on through depends_on/serves (nil allowed).
	Service func(string) bool
	// Root is the repository root, used for modification times ("" to skip).
	Root string
}

// Compute returns the hint for one failing unit.
func Compute(in Input) *Hint {
	h := &Hint{Suite: in.Suite, Test: in.Unit, Inferred: in.Inferred, Candidates: []Candidate{}}
	keys := map[string]bool{}
	for _, d := range in.Deps {
		keys[d] = true
	}
	for d := range in.Observed {
		keys[d] = true
	}
	h.Inputs = len(keys)
	switch {
	case in.Entry != nil:
		h.Basis, h.Since, h.LastStatus = BasisLastRun, in.Entry.Commit, in.Entry.Status
	case in.Changed != nil:
		h.Basis = BasisDiff
	default:
		h.Basis, h.Verdict = BasisNone, VerdictNoRecord
		return h
	}

	own := map[string]bool{}
	direct := map[string]bool{}
	for _, f := range in.TestFiles {
		own[f] = true
		if in.Direct != nil {
			for _, d := range in.Direct(f) {
				direct[d] = true
			}
		}
	}
	var cands []Candidate
	for k := range keys {
		c, ok := changed(in, k)
		if !ok {
			continue
		}
		if kind, f := depkey.Parse(k); kind == depkey.KindDecls || kind == depkey.KindDecl {
			// The declarations of a Go file, or one of them, stand for the file.
			if keys[f] {
				if _, ok := changed(in, f); ok {
					continue
				}
			}
			c.Path, c.New, c.Old = f, in.Hash(f), ""
			if in.Entry != nil {
				c.Old = in.Entry.Deps[f]
			}
		}
		classify(&c, in, own, direct)
		cands = append(cands, c)
	}
	h.Changed = len(cands)
	h.Verdict = VerdictChanged
	if len(cands) == 0 {
		if len(in.Orphans) == 0 {
			h.Verdict = VerdictUnchanged
			return h
		}
		h.Verdict = VerdictUnattributed
		for o, old := range in.Orphans {
			c := Candidate{Path: o, Old: old, New: in.Hash(o), Change: "modified"}
			if c.New == "" {
				c.Change = "deleted"
			}
			classify(&c, in, own, direct)
			c.Link = LinkUnattributed
			cands = append(cands, c)
		}
	}
	if in.Root != "" {
		for i := range cands {
			if fi, err := os.Stat(filepath.Join(in.Root, filepath.FromSlash(cands[i].Path))); err == nil {
				cands[i].mtime = fi.ModTime()
			}
		}
	}
	for i := range cands {
		for _, f := range in.TestFiles {
			cands[i].near = max(cands[i].near, commonDirs(cands[i].Path, f))
		}
	}
	Rank(cands)
	if len(cands) > MaxCandidates {
		h.More = len(cands) - MaxCandidates
		cands = cands[:MaxCandidates]
	}
	h.Candidates = cands
	return h
}

// changed reports whether a dependency key changed against the hint's basis.
func changed(in Input, key string) (Candidate, bool) {
	cur := in.Hash(key)
	c := Candidate{Path: key, New: cur}
	recorded := false
	if in.Entry != nil {
		var old string
		old, recorded = in.Entry.Deps[key]
		if recorded {
			if old == cur || in.Unchanged != nil && in.Unchanged(key, old) {
				return c, false
			}
			c.Old = old
		}
	}
	if !recorded && !in.Changed[key] {
		// Not recorded for this test and unchanged in the diff: a new input that did not
		// change itself (whatever made the test use it, an edited import, is the
		// candidate).
		return c, false
	}
	switch {
	case cur == "" || cur == "absent":
		c.Change = "deleted"
	case recorded && (c.Old == "" || c.Old == "absent"):
		c.Change = "added"
	default:
		c.Change = "modified"
	}
	return c, true
}

func classify(c *Candidate, in Input, own, direct map[string]bool) {
	p := c.Path
	switch {
	case depkey.IsEnv(p):
		c.Kind, c.tier = KindEnv, 2
	case depkey.IsPseudo(p):
		c.Kind, c.tier = KindListing, 5
	case own[p]:
		c.Kind, c.tier = KindTest, 2
	case in.Inputs[p] || isConfig(p):
		c.Kind, c.tier = KindConfig, 4
	case isCode(p) && direct[p]:
		c.Kind, c.tier = KindSource, 0
	case isCode(p):
		c.Kind, c.tier = KindSource, 1
	default:
		c.Kind, c.tier = KindData, 3
	}
	switch {
	case own[p]:
		c.Link = LinkTestFile
	case direct[p]:
		c.Link = LinkDirect
	case in.Static[p]:
		c.Link = LinkStatic
	case in.Inputs[p]:
		c.Link = LinkInput
	case in.Service != nil && in.Service(p):
		c.Link = LinkService
	default:
		c.Link = LinkTraced
	}
}

// Rank orders candidates: code the test imports directly, other code, the test file
// itself and environment variables, data, configuration, then directory listings; within a tier the most
// recently modified first, then the files closest to the test, then by path.
func Rank(cands []Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if !a.mtime.Equal(b.mtime) {
			return a.mtime.After(b.mtime)
		}
		if a.near != b.near {
			return a.near > b.near
		}
		return a.Path < b.Path
	})
}

var codeExt = map[string]bool{
	".js": true, ".mjs": true, ".cjs": true, ".jsx": true, ".ts": true, ".mts": true, ".cts": true, ".tsx": true,
	".vue": true, ".svelte": true, ".py": true, ".pyi": true, ".go": true, ".s": true, ".c": true, ".h": true,
	".cc": true, ".cpp": true, ".rs": true, ".java": true, ".kt": true, ".rb": true, ".php": true, ".sh": true,
	".sql": true, ".proto": true,
}

func isCode(p string) bool { return codeExt[strings.ToLower(path.Ext(p))] }

var configNames = map[string]bool{
	"package.json": true, "package-lock.json": true, "pnpm-lock.yaml": true, "pnpm-workspace.yaml": true,
	"yarn.lock": true, "bun.lock": true, "bun.lockb": true, "npm-shrinkwrap.json": true, ".npmrc": true,
	"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true, "pyproject.toml": true, "uv.lock": true,
	"poetry.lock": true, "requirements.txt": true, "setup.cfg": true, "setup.py": true, "pytest.ini": true,
	"tox.ini": true, ".env": true, ".nvmrc": true, ".node-version": true,
	".python-version": true, ".tool-versions": true, "smates.yml": true, "smates.yaml": true, "bunfig.toml": true,
}

func isConfig(p string) bool {
	base := path.Base(p)
	if configNames[base] {
		return true
	}
	if strings.HasPrefix(base, "tsconfig") && strings.HasSuffix(base, ".json") || strings.HasPrefix(base, ".env.") {
		return true
	}
	// vitest.config.ts, jest.config.js, babel.config.cjs, ...
	return strings.HasSuffix(strings.TrimSuffix(base, path.Ext(base)), ".config")
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// CleanOutput strips terminal escapes and surrounding blank space from runner output.
func CleanOutput(s string) string {
	return strings.TrimSpace(ansiRe.ReplaceAllString(s, ""))
}

// commonDirs counts the leading directory segments a and b share.
func commonDirs(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as)-1 && n < len(bs)-1 && as[n] == bs[n] {
		n++
	}
	return n
}
