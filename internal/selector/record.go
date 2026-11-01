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
	disk  map[string]string // memoized hashes of files outside the snapshot
	exist map[string]string // memoized existence checks of paths outside the snapshot
	dirs  map[string][]string
	trees map[string]string
	setv  map[*ServiceDeps]string // memoized hashes of sets
	// since is shortly before the tests started (see PinBefore); zero when not pinned.
	since time.Time
}

// Unsettled is the hash of a path outside the snapshot that changed while the tests ran
// and whose state before the run is not known: it matches no hash, so the tests that
// depend on it run again (and then record the state it had before that run).
const Unsettled = "unsettled"

// settleMargin widens the window in which a modification time counts as during the
// run, for file systems with coarse timestamps (FAT: two seconds).
const settleMargin = 2 * time.Second

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

// PinBefore makes the hasher hash the paths outside the snapshot as they were before
// the tests ran, like the snapshot itself: Record hashes after the run, and tests may
// write git-ignored files (caches, generated code, .env) or create paths they probed.
// It is called before the first test starts (at start) and reads now what the entries
// of the given suites depend on outside the snapshot. A path first read later that was
// modified at or after start (give or take settleMargin) may have been written by the
// tests and hashes as Unsettled; one absent when read is taken to have been absent.
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
				h.pin(k)
			}
		}
	}
	h.since = start.Add(-settleMargin)
}

// pin reads what key depends on outside the snapshot, if anything, into the caches.
func (h *Hasher) pin(key string) {
	if _, ok := h.snap[key]; ok {
		return // most keys: a file of the snapshot
	}
	switch kind, p := depkey.Parse(key); kind {
	case depkey.KindContent:
		h.file(key)
	case depkey.KindConfig:
		if !h.isConfig(key) {
			h.file(key)
		}
	case depkey.KindDecls:
		h.file(p)
	case depkey.KindExistence:
		h.Hash(key)
	}
}

// modifiedDuringRun reports whether a path outside the snapshot, read for the first
// time, was modified since the tests started (PinBefore).
func (h *Hasher) modifiedDuringRun(fi os.FileInfo) bool {
	return !h.since.IsZero() && !fi.ModTime().Before(h.since)
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

// ConfigHash fingerprints the settings of smates.yml that can change how the tests of
// su run or what they are recorded to depend on: the suite's own entry and the
// top-level env_track and env_ignore (a variable env_ignore lists is not recorded, so
// taking it out of the list must run the tests again to record who reads it). The rest
// (ignore, orphans, agent, flaky, state, inputs, full_run_branches, other suites)
// cannot change a test's outcome: input globs and rules add dependency keys of their
// own, and ignore globs change the "dir/**" hashes they affect.
func ConfigHash(c *config.Config, su *config.Suite) string {
	data, err := json.Marshal(struct {
		Suite     *config.Suite
		EnvTrack  []string
		EnvIgnore []string `json:",omitempty"`
	}{su, c.EnvTrack, c.EnvIgnore})
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
	abs := filepath.Join(h.root, filepath.FromSlash(p))
	v, _ := gitx.HashFile(abs)
	if fi, err := os.Lstat(abs); v != "" && err == nil && h.modifiedDuringRun(fi) {
		v = Unsettled
	}
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
		if _, isFile := h.snap[p]; !isFile && h.Hash(depkey.Listing(p)) == "" && p != "." {
			v = h.exists(p)
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
		return ConfigHash(h.cfg, &h.suite.Suite)
	case depkey.KindContent:
		return h.file(key)
	case depkey.KindDecls:
		return h.decls(p)
	}
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
	list, ok := h.dirs[key]
	if !ok {
		return ""
	}
	sum := sha1.Sum([]byte(strings.Join(list, "\n")))
	return "dir:" + hex.EncodeToString(sum[:])
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
	if err != nil || gitx.HashBytes(src) != blob {
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
// commands smates cannot see into.
func (h *Hasher) tree(dir string) string {
	if v, ok := h.trees[dir]; ok {
		return v
	}
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
	v := "tree:" + hex.EncodeToString(sum.Sum(nil))
	if h.trees == nil {
		h.trees = map[string]string{}
	}
	h.trees[dir] = v
	return v
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
