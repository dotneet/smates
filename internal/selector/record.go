package selector

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/static/golang"
)

// Hasher returns content hashes of dependency keys. A key is a repo-relative file path;
// a directory path with a trailing "/" meaning "the list of entries of this directory"
// (recorded when a test lists a directory, so added or removed files are noticed); a
// path with a trailing "?" meaning "whether this path exists" (recorded for existence
// checks, including of paths that do not exist yet); or "$NAME", the value of an
// environment variable as the tests of a suite see it (see ForSuite). The kinds of key
// are defined in package depkey.
type Hasher struct {
	snap   gitx.Snapshot
	ignore []string
	root   string // repository root for files outside the snapshot ("" = none)
	env    func(string) (string, bool)
	// cfg and suite: the config file's key hashes as the part of the configuration
	// that can change how the suite's tests run (see ConfigHash).
	cfg   *config.Config
	suite *runner.Suite
	// sets are the declared sets of files (depends_on/serves) by key, see WithSets.
	sets map[string]*ServiceDeps
	*hashCache
}

// hashCache is shared by the hashers derived from one with ForSuite.
type hashCache struct {
	disk  map[string]string   // memoized hashes of files outside the snapshot
	exist map[string]string   // memoized existence checks of paths outside the snapshot
	dirs  map[string][]string // the snapshot's directories and their entries
	lists map[string]string   // memoized listing hashes (see listing)
	trees map[string]string
	setv  map[*ServiceDeps]string // memoized hashes of sets
	// since is when the tests started (see PinBefore); zero when not pinned.
	since time.Time
}

// Unsettled is the hash of an existence check of a path outside the snapshot that was
// created or modified while the tests ran and whose state before the run is not known:
// it matches no hash, so the tests that probed it run again (and then record the state
// it had before that run, PinBefore).
const Unsettled = "unsettled"

// NewHasher indexes a snapshot. Files matching ignore globs (docs, editor settings) are
// left out of "dir/**" tree hashes, so editing a README does not re-run every test that
// shells out to an opaque command.
func NewHasher(snap gitx.Snapshot, ignore ...string) *Hasher {
	return &Hasher{snap: snap, ignore: ignore, hashCache: &hashCache{}}
}

// WithRoot makes the hasher read files git does not list (ignored files such as .env
// or generated code, files inside submodules) from disk under root, so a test that
// reads them notices when they change.
func (h *Hasher) WithRoot(root string) *Hasher {
	h.root = root
	return h
}

// PinBefore makes the hasher hash existence checks of paths outside the snapshot as
// they were before the tests ran, like the snapshot itself: Record hashes after the
// run, and a test that probes a path and then creates it (an output directory) would
// otherwise be recorded as having seen it exist. It is called before the first test
// starts (at start) and checks now the paths outside the snapshot that the entries of
// the given suites probed. A path first probed later that exists and was modified at or
// after start may have been created by the tests and hashes as Unsettled; one absent
// when checked is taken to have been absent.
//
// The content of files outside the snapshot (git-ignored caches, reports, generated
// code) is hashed as the run left it: tests commonly rewrite what they read there
// (a timestamped report, a cache), and the state before the run would never match
// again. A git-ignored input edited by hand while the run is in progress is therefore
// recorded with its new content.
func (h *Hasher) PinBefore(st *state.State, suites []string, start time.Time) {
	if h.root == "" {
		return
	}
	for _, name := range suites {
		su := st.Suites[name]
		if su == nil {
			continue
		}
		for _, e := range su.Tests {
			for k := range e.Deps {
				if depkey.KindOf(k) == depkey.KindExistence {
					h.Hash(k)
				}
			}
		}
	}
	h.since = start
}

// runStartSlack is how much earlier than the start of the run a modification time may
// be and still count as during the run. File systems take timestamps from a coarser
// clock than time.Now: Linux uses the kernel's tick-granular clock (1-10ms), so a path
// created just after the start can carry an mtime slightly before it. Kept far below
// the time a build step before `smates run` takes to hand over (`make gen && smates
// run` must not rerun the tests probing what it generated).
const runStartSlack = 100 * time.Millisecond

// modifiedDuringRun reports whether a path outside the snapshot, checked for the first
// time, was modified since the tests started (PinBefore), give or take runStartSlack.
// A modification time in whole seconds comes from a file system with coarse
// timestamps (HFS+, ext3), on which a path created in the second the run started may
// look older than the start: it is compared with the start's second.
func (h *Hasher) modifiedDuringRun(fi os.FileInfo) bool {
	if h.since.IsZero() {
		return false
	}
	since, mt := h.since.Add(-runStartSlack), fi.ModTime()
	if sec := h.since.Truncate(time.Second); mt.Nanosecond() == 0 && sec.Before(since) {
		since = sec
	}
	return !mt.Before(since)
}

// WithConfig makes the hasher hash the settings key (depkey.Config) of the
// configuration file (c.Path) of a suite (see ForSuite) as ConfigHash, so edits that
// cannot change how the suite's tests run (the ignore list, the agent, another suite)
// select nothing.
func (h *Hasher) WithConfig(c *config.Config) *Hasher {
	h.cfg = c
	return h
}

// ForSuite returns a hasher (sharing h's caches) that hashes environment variables as
// the tests of su see them: the suite's env overrides smates' own environment. Without
// it, variables are looked up in smates' environment.
func (h *Hasher) ForSuite(su *runner.Suite) *Hasher {
	c := *h
	c.env = su.LookupEnv
	c.suite = su
	return &c
}

// isConfig reports whether key is the settings key (depkey.Config) of the
// configuration file the hasher was given (WithConfig).
func (h *Hasher) isConfig(key string) bool {
	return h.cfg != nil && h.cfg.Path != "" && key == depkey.Config(h.cfg.Path)
}

// legacyConfig reports whether key is the settings key of an entry an older smates
// recorded, when every test depended on the configuration file's content: the entry
// then holds the file's hash instead, and it counts as unchanged while the file is.
func (h *Hasher) legacyConfig(key string, recorded map[string]string) bool {
	if !h.isConfig(key) {
		return false
	}
	old, ok := recorded[h.cfg.Path]
	return ok && old != "" && old == h.file(h.cfg.Path)
}

// legacySuite reports whether recorded is the hash an older smates recorded for the
// settings key, over the suite with the adapter's defaults applied (the command, the
// excludes and inputs, which depend on the machine: a virtualenv, GOWORK): it counts
// as unchanged while those settings are the same.
func (h *Hasher) legacySuite(key, recorded string) bool {
	return h.isConfig(key) && h.suite != nil && recorded != "" && recorded == ConfigHash(h.cfg, &h.suite.Suite)
}

// configured is the suite's entry as smates.yml has it (runner.Suite.Configured), or
// the suite itself when it was not made by runner.NewSuite.
func configured(su *runner.Suite) *config.Suite {
	if su.Configured.Name == "" && su.Configured.Runner == "" {
		return &su.Suite
	}
	return &su.Configured
}

// ConfigHash fingerprints the settings of smates.yml that can change how the tests of
// su run or what they are recorded to depend on: the suite's own entry as written (not
// the defaults the adapter fills in, which differ between CI and a laptop with a
// virtualenv) and the top-level env_track. The rest (ignore, orphans, agent, flaky,
// state, inputs, env_ignore, full_run_branches, other suites) cannot change a test's
// outcome: input globs and rules add dependency keys of their own, ignore globs change
// the "dir/**" hashes they affect, and env_ignore applies when comparing. (A variable
// the top-level env_ignore listed was not recorded, so taking it out of the list
// selects nothing until the tests reading it run again; hashing the list here would
// instead run every test whenever a name is added, the usual remedy for a noisy
// variable.)
func ConfigHash(c *config.Config, su *config.Suite) string {
	data, err := json.Marshal(struct {
		Suite    *config.Suite
		EnvTrack []string
	}{su, c.EnvTrack})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "config:" + hex.EncodeToString(sum[:16])
}

// WithSets returns a hasher (sharing h's caches) that also hashes the set keys of
// declarations: one hash over the paths and contents of all their files, so a test
// records one key per declaration however many files the declaration covers.
func (h *Hasher) WithSets(decls []*ServiceDeps) *Hasher {
	if len(decls) == 0 {
		return h
	}
	c := *h
	c.sets = map[string]*ServiceDeps{}
	for k, v := range h.sets {
		c.sets[k] = v
	}
	for _, d := range decls {
		c.sets[d.Key] = d
	}
	return &c
}

func (h *Hasher) set(key string) string {
	d := h.sets[key]
	if d == nil {
		return ""
	}
	if v, ok := h.setv[d]; ok {
		return v
	}
	sum := sha1.New()
	for _, f := range d.Files { // sorted
		sum.Write([]byte(f + "\x00" + h.file(f) + "\n"))
	}
	v := "set:" + hex.EncodeToString(sum.Sum(nil))
	if h.setv == nil {
		h.setv = map[*ServiceDeps]string{}
	}
	h.setv[d] = v
	return v
}

// EnvHash is the hash recorded for an environment variable: "" when it is not set, so
// an unset variable and an empty one differ. Only the hash of a value is stored, never
// the value.
func EnvHash(value string, set bool) string {
	if !set {
		return ""
	}
	sum := sha256.Sum256([]byte("smates-env\x00" + value))
	return "env:" + hex.EncodeToString(sum[:16])
}

func (h *Hasher) file(p string) string {
	if v, ok := h.snap[p]; ok {
		return v
	}
	if h.root == "" || pathutil.IsVendored(p) {
		return ""
	}
	if v, ok := h.disk[p]; ok {
		return v
	}
	v, _ := gitx.HashFile(filepath.Join(h.root, filepath.FromSlash(p)))
	if h.disk == nil {
		h.disk = map[string]string{}
	}
	h.disk[p] = v
	return v
}

// exists hashes an existence check of p, a path outside the snapshot, from disk:
// "exists", "absent", or Unsettled when p was created or modified while the tests ran.
func (h *Hasher) exists(p string) string {
	if h.root == "" {
		return "absent"
	}
	if v, ok := h.exist[p]; ok {
		return v
	}
	v := "absent"
	if fi, err := os.Lstat(filepath.Join(h.root, filepath.FromSlash(p))); err == nil {
		v = "exists"
		if h.modifiedDuringRun(fi) {
			v = Unsettled
		}
	}
	if h.exist == nil {
		h.exist = map[string]string{}
	}
	h.exist[p] = v
	return v
}

// Hash returns the hash of a dependency key, "" when absent.
func (h *Hasher) Hash(key string) string {
	kind, p := depkey.Parse(key)
	switch kind {
	case depkey.KindEnv:
		lookup := h.env
		if lookup == nil {
			lookup = (*runner.Suite)(nil).LookupEnv
		}
		return EnvHash(lookup(p))
	case depkey.KindTree:
		return h.tree(p)
	case depkey.KindSet:
		return h.set(key)
	case depkey.KindExistence:
		v := "exists"
		// A path outside the snapshot is checked on disk (and pinned, see PinBefore);
		// not through the listing, which reads directories git ignores from disk.
		if _, isFile := h.snap[p]; !isFile && p != "." {
			if _, isDir := h.snapEntries(depkey.Listing(p)); !isDir {
				v = h.exists(p)
			}
		}
		if blob, ok := h.snap[key]; ok {
			// A file whose name ends in "?" (see depkey): the key is its content or an
			// existence check of p, so it hashes as both.
			return blob + "+" + v
		}
		return v
	case depkey.KindConfig:
		if !h.isConfig(key) {
			return h.file(key) // another configuration file's settings: hashed as a path
		}
		if h.suite == nil {
			return ""
		}
		return ConfigHash(h.cfg, configured(h.suite))
	case depkey.KindContent:
		return h.file(key)
	case depkey.KindDecls:
		return h.decls(p)
	}
	return h.listing(key)
}

// snapEntries returns the entry names of directory key ("dir/") in the snapshot
// (subdirectories with a trailing "/"), sorted; ok is false when no file of the
// snapshot is below it.
func (h *Hasher) snapEntries(key string) (names []string, ok bool) {
	if h.dirs == nil {
		h.dirs = map[string][]string{}
		for f := range h.snap {
			for d := path.Dir(f); ; d = path.Dir(d) {
				child := f
				if d != "." {
					child = strings.TrimPrefix(f, d+"/")
				}
				if i := strings.IndexByte(child, '/'); i >= 0 {
					child = child[:i+1]
				}
				h.dirs[d+"/"] = append(h.dirs[d+"/"], child)
				if d == "." {
					break
				}
			}
		}
		for k, v := range h.dirs {
			sort.Strings(v)
			h.dirs[k] = uniq(v)
		}
	}
	names, ok = h.dirs[key]
	return names, ok
}

// listing hashes the entry names of a directory (key "dir/"). A directory the snapshot
// has files below hashes as the snapshot's entries alone: entries git ignores inside it
// (a log a test writes into its package, a test binary, editor and Finder files) are
// not part of its listing, or one of them would rerun every test listing the directory
// (with granularity: func, every test function of a Go package), and on every fresh
// checkout where a test creates it. A directory the snapshot has no file below (one git
// ignores entirely: generated fixtures, build output) is listed from disk, with a root,
// except vendored entries and those the ignore globs match; it hashes as "" when it
// does not exist. Like the content of files outside the snapshot, it is listed when
// first hashed, which for a recording is after the run: a test that adds a file to the
// directory it lists (a timestamped report) then finds the listing it left.
func (h *Hasher) listing(key string) string {
	if v, ok := h.lists[key]; ok {
		return v
	}
	names, ok := h.snapEntries(key)
	if !ok && h.root != "" {
		dir := strings.TrimSuffix(key, "/")
		if entries, err := os.ReadDir(filepath.Join(h.root, filepath.FromSlash(dir))); err == nil {
			ok = true
			for _, e := range entries {
				n := e.Name()
				if p := path.Join(dir, n); pathutil.IsVendored(p) || pathutil.Match(h.ignore, p) {
					continue
				}
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			sort.Strings(names)
		}
	}
	v := ""
	if ok {
		sum := sha1.Sum([]byte(strings.Join(names, "\n")))
		v = "dir:" + hex.EncodeToString(sum[:])
	}
	if h.lists == nil {
		h.lists = map[string]string{}
	}
	h.lists[key] = v
	return v
}

// declsCache maps a Go file's blob hash to the hash of its declarations.
var declsCache sync.Map

// decls hashes the declarations of a Go file (see golang.DeclsHash): "" when the file
// is absent, its content hash when the declarations cannot be told apart from the
// rest (it does not parse, or is not on disk as the snapshot has it), so that any
// change counts.
func (h *Hasher) decls(p string) string {
	blob := h.file(p)
	if blob == "" || h.root == "" {
		return blob
	}
	if v, ok := declsCache.Load(blob); ok {
		return v.(string)
	}
	src, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(p)))
	if err != nil || gitx.HashBytes(src) != gitx.Blob(blob) {
		return blob
	}
	v, ok := golang.DeclsHash(src, strings.HasSuffix(p, "_test.go"))
	if !ok {
		v = blob
	}
	declsCache.Store(blob, v)
	return v
}

// tree hashes every file below dir ("." for the repository): a dependency recorded for
// commands smates cannot see into. Files git ignores below a directory the snapshot
// lists are left out (build output, caches). A directory the snapshot has no file below
// (one git ignores entirely: a command ran in build/) is read from disk instead, as the
// run left it (see diskTree).
func (h *Hasher) tree(dir string) string {
	if v, ok := h.trees[dir]; ok {
		return v
	}
	var v string
	if _, listed := h.snapEntries(depkey.Listing(dir)); !listed && h.root != "" && dir != "." && !pathutil.IsVendored(dir) {
		v = h.diskTree(dir)
	} else {
		var files []string
		for f := range h.snap {
			if (dir == "." || pathutil.Under(f, dir)) && !pathutil.IsVendored(f) && !pathutil.Match(h.ignore, f) {
				files = append(files, f)
			}
		}
		sort.Strings(files)
		sum := sha1.New()
		for _, f := range files {
			sum.Write([]byte(f + "\x00" + h.snap[f] + "\n"))
		}
		v = "tree:" + hex.EncodeToString(sum.Sum(nil))
	}
	if h.trees == nil {
		h.trees = map[string]string{}
	}
	h.trees[dir] = v
	return v
}

// maxDiskTree caps the entries diskTree walks (a variable for tests).
var maxDiskTree = 20000

// diskTree hashes a directory outside the snapshot from disk: the paths and sizes of
// the files below it, without vendored ones and those the ignore globs match. Contents
// are not read: such trees are build output (build/, target/, dist/), gigabytes to hash
// on every selection, and a build that is not reproducible would rerun the tests
// forever. A tree of more than maxDiskTree entries hashes as a constant (it stays the
// same while the tree is that large: its changes select nothing).
func (h *Hasher) diskTree(dir string) string {
	type file struct {
		p    string
		size int64
	}
	var files []file
	n := 0
	_ = filepath.WalkDir(filepath.Join(h.root, filepath.FromSlash(dir)), func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: what can be read still counts
		}
		if n++; n > maxDiskTree {
			return filepath.SkipAll
		}
		rel, rerr := filepath.Rel(h.root, abs)
		if rerr != nil {
			return nil
		}
		p := filepath.ToSlash(rel)
		if pathutil.IsVendored(p) || pathutil.Match(h.ignore, p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			var size int64
			if fi, err := d.Info(); err == nil {
				size = fi.Size()
			}
			files = append(files, file{p, size})
		}
		return nil
	})
	if n > maxDiskTree {
		return "tree:over:" + strconv.Itoa(maxDiskTree)
	}
	sum := sha1.New()
	for _, f := range files { // WalkDir walks in lexical order
		sum.Write([]byte(f.p + "\x00" + strconv.FormatInt(f.size, 10) + "\n"))
	}
	return "tree:" + hex.EncodeToString(sum.Sum(nil))
}

func uniq(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// EnvIgnored reports whether key is an environment variable c ignores for su.
func EnvIgnored(c *config.Config, su *runner.Suite, key string) bool {
	kind, name := depkey.Parse(key)
	if kind != depkey.KindEnv {
		return false
	}
	var cs *config.Suite
	if su != nil {
		cs = &su.Suite
	}
	return c.EnvIgnored(cs, name)
}

// DropIgnoredEnv removes the environment variables c ignores for su from what a run
// observed, so they are neither recorded nor offered as failure causes.
func DropIgnoredEnv(c *config.Config, su *runner.Suite, r *runner.RunResult) {
	drop := func(m map[string]bool) {
		for k := range m {
			if EnvIgnored(c, su, k) {
				delete(m, k)
			}
		}
	}
	for _, o := range r.Observations {
		drop(o.Files)
	}
	for _, m := range r.ReadsBy {
		drop(m)
	}
}

// RecordOptions control how observations are written to the state.
type RecordOptions struct {
	Commit string
	Now    time.Time
	// Full replaces every entry of the suite (the run covered all its tests).
	Full bool
	// ReadsBy maps source files to what they read or imported when they ran (once per
	// process); a unit depending on such a file depends on those reads too.
	ReadsBy map[string]map[string]bool
	// Retried holds the first attempt of units that were run again in the same run
	// (see config flaky.retry); obs then holds the retry.
	Retried map[string]*runner.Observation
	// Unreliable: the outcomes are recorded but never count as flake evidence (runner
	// arguments were passed through and may change how tests behave).
	Unreliable bool
}

// Record writes the observations of one suite run into the state. Each observed unit
// gets deps = observed files ∪ known deps (static closure, inputs, rules), hashed with
// the snapshot taken before the run. Units without an observation keep their old entry
// (and therefore stay selected next time if they were selected now) unless Full is set,
// in which case they are dropped.
//
// Record also keeps each unit's recent outcomes and returns the units whose outcome
// flipped between pass and fail while their inputs stayed the same (see flips).
func Record(st *state.State, sr *Suite, obs map[string]*runner.Observation, h *Hasher, opt RecordOptions) []FlakeEvent {
	h = h.WithSets(sr.Services) // depends_on/serves: one key per declaration
	su := st.Suite(sr.Name)
	old := su.Tests
	if opt.Full {
		su.Tests = map[string]*state.Entry{}
	}
	var events []FlakeEvent
	for unit, o := range obs {
		deps := map[string]string{}
		// Observed paths are recorded even when absent from the snapshot (hash ""): a
		// test that tried to read a missing file must run again once the file exists.
		// Known (static/config) deps only matter while they exist.
		add := func(p string, observed bool) {
			if pathutil.IsVendored(p) {
				return
			}
			if v := h.Hash(p); v != "" || observed {
				deps[p] = v
			}
		}
		for p := range o.Files {
			add(p, true)
		}
		for _, p := range sr.Known[unit] {
			add(p, false)
		}
		// Reads made by files the unit depends on, transitively: a module imported by
		// another test first still read its data for this one.
		if len(opt.ReadsBy) > 0 {
			queue := make([]string, 0, len(o.Files)+len(sr.Known[unit]))
			for p := range o.Files {
				queue = append(queue, p)
			}
			queue = append(queue, sr.Known[unit]...)
			seen := map[string]bool{}
			for len(queue) > 0 {
				f := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				if seen[f] {
					continue
				}
				seen[f] = true
				for k := range opt.ReadsBy[f] {
					add(k, true)
					queue = append(queue, k)
				}
			}
		}
		status := o.Status
		if status == "" {
			status = state.Unknown
		}
		e := &state.Entry{Deps: deps, Status: status, DurationMs: o.DurationMs, Commit: opt.Commit, At: opt.Now}
		events = append(events, noteOutcomes(e, old[unit], st.Files, h, opt, unit, o)...)
		su.Tests[unit] = e
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Unit < events[j].Unit })
	return events
}

// FlakeEvent is a unit whose outcome flipped while its inputs stayed the same.
type FlakeEvent struct {
	Unit  string
	Flip  string // e.g. "pass->fail"
	Count int    // flips recorded for the unit so far
	// Retried: the flip was between a failed attempt and its retry in this run.
	Retried bool
}

// noteOutcomes appends this run's outcome(s) of a unit to its history (carried over
// from prev) and records flake evidence on e.
func noteOutcomes(e, prev *state.Entry, files map[string]string, h *Hasher, opt RecordOptions, unit string, o *runner.Observation) []FlakeEvent {
	type attempt struct {
		status   string
		inferred bool
	}
	var attempts []attempt
	if first := opt.Retried[unit]; first != nil {
		attempts = append(attempts, attempt{first.Status, first.Inferred})
	}
	attempts = append(attempts, attempt{e.Status, o.Inferred})
	var hist []state.Outcome
	if prev != nil {
		hist = append(hist, prev.History...)
		if prev.Flaky != nil {
			f := *prev.Flaky
			e.Flaky = &f
		}
	}
	fp := state.Fingerprint(e.Deps)
	prevSame := prev != nil && sameInputs(prev, e.Deps, files, h)
	var events []FlakeEvent
	for i, a := range attempts {
		cur := state.Outcome{Status: a.status, Fingerprint: fp, At: opt.Now, Inferred: a.inferred || opt.Unreliable}
		// The latest outcome that can serve as evidence, and whether its inputs are
		// known to equal this run's: it ran earlier in this run (a retry), it saw the
		// same fingerprint, or it is prev's own outcome and none of prev's inputs changed.
		if decisive(cur) {
			for j := len(hist) - 1; j >= 0; j-- {
				ref := hist[j]
				if !decisive(ref) {
					continue
				}
				thisRun := i > 0 && j == len(hist)-1
				same := thisRun || (len(e.Deps) > 0 && ref.Fingerprint == fp) || (prevSame && j == len(prev.History)-1)
				if same && ref.Status != cur.Status {
					flip := ref.Status + "->" + cur.Status
					if e.Flaky == nil {
						e.Flaky = &state.Flake{First: opt.Now}
					}
					e.Flaky.Count++
					e.Flaky.Last, e.Flaky.Commit, e.Flaky.Flip = opt.Now, opt.Commit, flip
					events = append(events, FlakeEvent{Unit: unit, Flip: flip, Count: e.Flaky.Count, Retried: thisRun})
				}
				break
			}
		}
		hist = append(hist, cur)
	}
	if len(hist) > state.HistoryLen {
		hist = hist[len(hist)-state.HistoryLen:]
	}
	e.History = hist
	return events
}

// decisive reports whether an outcome can prove flakiness: a pass or fail reported for
// the unit itself.
func decisive(o state.Outcome) bool {
	return !o.Inferred && (o.Status == state.Pass || o.Status == state.Fail)
}

// sameInputs reports whether the unit ran on the same inputs as its previous entry:
// every dependency recorded then still has the same hash, and every file it depends on
// now but did not record then is unchanged since the recorded snapshot (files), so a
// dependency the previous trace missed cannot hide a real change. Entries written
// before outcome histories existed are never compared (their status may have been
// inferred).
func sameInputs(prev *state.Entry, deps, files map[string]string, h *Hasher) bool {
	if len(prev.History) == 0 || len(prev.Deps) == 0 {
		return false
	}
	for k, v := range prev.Deps {
		if h.Hash(k) != v {
			return false
		}
	}
	for k, v := range deps {
		if _, ok := prev.Deps[k]; ok {
			continue
		}
		if depkey.IsEnv(k) {
			return false // no recorded environment to compare a newly read variable with
		}
		if kind, f := depkey.Parse(k); kind == depkey.KindDecls {
			// The declarations of a Go file: unchanged if the file is.
			if was, ok := files[f]; len(files) > 0 && (!ok && v != "" || ok && was != h.Hash(f)) {
				return false
			}
			continue
		}
		if depkey.IsPseudo(k) {
			continue
		}
		if was, ok := files[k]; len(files) > 0 && (!ok && v != "" || ok && was != v) {
			return false
		}
	}
	return true
}
