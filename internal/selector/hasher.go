package selector

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
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

// hashCache is shared by the hashers derived from one with ForSuite and WithSets. Its
// maps are made by NewHasher, so every copy fills the same ones.
type hashCache struct {
	disk  map[string]string         // memoized hashes of files outside the snapshot
	exist map[string]string         // memoized existence checks of paths outside the snapshot
	links map[string]string         // memoized existence checks of files of the snapshot (see snapLink)
	dirs  map[string][]string       // the snapshot's directories and their entries (nil: not indexed yet)
	files []string                  // the snapshot's paths, sorted (nil: not indexed yet)
	lists map[string]string         // memoized listing hashes (see listing)
	pkgs  map[string]*goPackageHash // memoized Go package hashes (see goPackage)
	trees map[string]string
	setv  map[*ServiceDeps]string // memoized hashes of sets
	// oversized are the directories outside the snapshot whose tree had more than
	// maxDiskTree entries (see diskTree, OversizedTrees).
	oversized map[string]bool
	// legacy and legacySets memoize tree and set hashes without the executable marker
	// (Unchanged). Sets are keyed like setv: one set key stands for different files in
	// the hashers of different suites.
	legacy     map[string]string
	legacySets map[*ServiceDeps]string
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
	return &Hasher{snap: snap, ignore: ignore, hashCache: &hashCache{
		disk:       map[string]string{},
		exist:      map[string]string{},
		links:      map[string]string{},
		lists:      map[string]string{},
		pkgs:       map[string]*goPackageHash{},
		trees:      map[string]string{},
		setv:       map[*ServiceDeps]string{},
		oversized:  map[string]bool{},
		legacy:     map[string]string{},
		legacySets: map[*ServiceDeps]string{},
	}}
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

// DepUnchanged reports whether dependency key still hashes as an entry recorded it
// (recorded: the entry's hashes, compared with Unchanged); it is the one comparison
// selection makes per key. A key the entry did not record is changed, except the
// settings key of an entry an older smates recorded, when every
// test depended on the configuration file's content: the entry then holds the file's
// hash instead, and the key counts as unchanged while the file is. A settings key an
// older smates recorded over the suite with the adapter's defaults applied (the
// command, the excludes and inputs, which depend on the machine: a virtualenv, GOWORK)
// counts as unchanged while those settings are the same.
func (h *Hasher) DepUnchanged(key string, recorded map[string]string) bool {
	old, ok := recorded[key]
	if !ok {
		if !h.isConfig(key) {
			return false
		}
		file, ok := recorded[h.cfg.Path]
		return ok && file != "" && file == h.file(h.cfg.Path)
	}
	if h.Unchanged(key, old) {
		return true
	}
	return h.isConfig(key) && h.suite != nil && old != "" && old == ConfigHash(h.cfg, &h.suite.Suite)
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
	v := h.setSum(d, false)
	h.setv[d] = v
	return v
}

// setSum hashes the files of a set; legacy leaves the executable marker out, as smates
// before it did (gitx.LegacyHash).
func (h *Hasher) setSum(d *ServiceDeps, legacy bool) string {
	sum := sha1.New()
	for _, f := range d.Files { // sorted
		v := h.file(f)
		if legacy {
			v = gitx.LegacyHash(v)
		}
		sum.Write([]byte(f + "\x00" + v + "\n"))
	}
	return "set:" + hex.EncodeToString(sum.Sum(nil))
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
	h.disk[p] = v
	return v
}

// Dangling is the hash of an existence check of a symlink that does not resolve (its
// target is missing, or it is part of a loop): a check that follows links (stat,
// fs.existsSync, Path.exists) finds nothing there, and finds something once the target
// appears, which "exists" for the link itself would hide.
const Dangling = "dangling"

// exists hashes an existence check of p, a path outside the snapshot, from disk:
// "exists", "absent", Dangling, or Unsettled when p was created or modified while the
// tests ran.
func (h *Hasher) exists(p string) string {
	if h.root == "" {
		return "absent"
	}
	if v, ok := h.exist[p]; ok {
		return v
	}
	v := "absent"
	abs := filepath.Join(h.root, filepath.FromSlash(p))
	if fi, err := os.Lstat(abs); err == nil {
		v = "exists"
		if fi.Mode()&os.ModeSymlink != 0 && !resolves(abs) {
			v = Dangling
		}
		if h.modifiedDuringRun(fi) {
			v = Unsettled
		}
	}
	h.exist[p] = v
	return v
}

// resolves reports whether the symlinks of path lead to something that exists.
func resolves(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// snapLink hashes an existence check of p, a file of the snapshot: "exists", Dangling
// for a symlink that does not resolve on disk (read from disk with a root), or "absent"
// for a skip-worktree file not on disk (gitx.OffDisk: the sparse cone leaves it out).
// A file the snapshot lists that is gone from disk was deleted after the snapshot was
// taken, during the run being recorded: it is hashed as it was before the run, as its
// content is, so that the next selection (which sees it absent) runs the test again.
func (h *Hasher) snapLink(p string) string {
	if gitx.IsOffDisk(h.snap[p]) {
		return "absent"
	}
	if h.root == "" {
		return "exists"
	}
	if v, ok := h.links[p]; ok {
		return v
	}
	v := "exists"
	abs := filepath.Join(h.root, filepath.FromSlash(p))
	if fi, err := os.Lstat(abs); err == nil && fi.Mode()&os.ModeSymlink != 0 && !resolves(abs) {
		v = Dangling
	}
	h.links[p] = v
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
		// not through the listing, which reads directories git ignores from disk. A
		// symlink of the snapshot exists for a check only while it resolves.
		if _, isFile := h.snap[p]; isFile {
			v = h.snapLink(p)
		} else if p != "." {
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
	case depkey.KindDecl, depkey.KindSig:
		return h.decl(key)
	case depkey.KindShape:
		return h.goPackage(p).shape
	case depkey.KindMethods:
		_, typ, _ := depkey.MethodsType(key)
		return h.goPackage(p).methodsOf(typ)
	}
	return h.listing(key)
}

// snapEntries returns the entry names of directory key ("dir/") in the snapshot
// (subdirectories with a trailing "/"), sorted; ok is false when no file of the
// snapshot is below it. Skip-worktree files not on disk (gitx.OffDisk) are no entries:
// a test listing the directory does not find them, and a directory holding only such
// files is not there.
func (h *Hasher) snapEntries(key string) (names []string, ok bool) {
	if h.dirs == nil {
		h.dirs = map[string][]string{}
		for f, v := range h.snap {
			if gitx.IsOffDisk(v) {
				continue
			}
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
// except vendored entries; it hashes as "" when it does not exist. The ignore globs do
// not apply: listing the directory is the read, so an added entry they match (a
// markdown fixture under DefaultIgnore's **/*.md) is a change to the test that listed
// it. Like the content of files outside the snapshot, it is listed when
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
				if pathutil.IsVendored(path.Join(dir, n)) {
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
	h.lists[key] = v
	return v
}

// declsCache maps a Go file's blob hash and whether it is a test file (declsKey) to
// the hash of its declarations: golang.DeclsHash depends on both.
var declsCache sync.Map

// declsKey is the key of declsCache.
type declsKey struct {
	blob  string
	tests bool
}

// decls hashes the declarations of a Go file (see golang.DeclsHash): "" when the file
// is absent, its content hash when the declarations cannot be told apart from the
// rest (it does not parse, or is not on disk as the snapshot has it), so that any
// change counts.
func (h *Hasher) decls(p string) string {
	blob := h.file(p)
	if blob == "" || h.root == "" {
		return blob
	}
	key := declsKey{blob, strings.HasSuffix(p, "_test.go")}
	if v, ok := declsCache.Load(key); ok {
		return v.(string)
	}
	src, readable := h.readGo(p, blob)
	if !readable {
		return blob
	}
	v, ok := golang.DeclsHash(src, key.tests)
	if !ok {
		v = blob
	}
	declsCache.Store(key, v)
	return v
}

// declCache maps a Go file's blob hash to its declHashes (nil: it does not parse):
// golang.DeclHashes depends on the content alone.
var declCache sync.Map

// declHashes are the full and signature hashes of the declarations of a Go file (see
// golang.DeclHashes).
type declHashes struct{ full, sig map[string]string }

// readGoIn is readGo, false without a repository root.
func (h *Hasher) readGoIn(p, blob string) ([]byte, bool) {
	if h.root == "" {
		return nil, false
	}
	return h.readGo(p, blob)
}

// readGo returns the content of Go file p as the snapshot has it (blob), or false when
// it cannot be read so (absent, off disk in the snapshot, or changed on disk since the
// snapshot).
func (h *Hasher) readGo(p, blob string) ([]byte, bool) {
	if gitx.IsOffDisk(blob) {
		return nil, false
	}
	src, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(p)))
	if err != nil || gitx.HashBytes(src) != gitx.Blob(blob) {
		return nil, false
	}
	return src, true
}

// decl hashes one top-level declaration of a Go file, with its function bodies
// (depkey.Decl) or without (depkey.Sig): "" when the file or the declaration is
// absent, the file's content hash when the declarations cannot be told apart (as for
// decls).
func (h *Hasher) decl(key string) string {
	kind, _ := depkey.Parse(key)
	p, name, _ := depkey.DeclName(key)
	blob := h.file(p)
	if blob == "" || h.root == "" {
		return blob
	}
	v, cached := declCache.Load(blob)
	if !cached {
		src, readable := h.readGo(p, blob)
		if !readable {
			return blob // not on disk as the snapshot has it
		}
		var dh *declHashes
		if full, sig, ok := golang.DeclHashes(src); ok {
			dh = &declHashes{full, sig}
		}
		declCache.Store(blob, dh)
		v = dh
	}
	dh := v.(*declHashes)
	switch {
	case dh == nil:
		return blob
	case kind == depkey.KindSig:
		return dh.sig[name]
	}
	return dh.full[name]
}

// partsCache maps a Go file's blob hash to its fileParts: golang.PackageParts depends
// on the content alone.
var partsCache sync.Map

// fileParts is what a Go file contributes to its package (see golang.PackageParts);
// unknown when it does not parse.
type fileParts struct {
	shape   string
	methods map[string][]string
	unknown bool
}

// goPackage hashes the shape and the method sets of the Go package in directory dir
// (depkey.Shape, depkey.Methods) from the Go files the snapshot lists there; the
// non-Go files go build compiles into the package count whole in the shape. A Go file
// that cannot be read as the snapshot has it or does not parse counts with its blob
// in the shape and in every method set, so that any change to it counts.
func (h *Hasher) goPackage(dir string) *goPackageHash {
	if v, ok := h.pkgs[dir]; ok {
		return v
	}
	ph := &goPackageHash{methods: map[string][]string{}}
	h.pkgs[dir] = ph
	names, ok := h.snapEntries(depkey.Listing(dir))
	if !ok {
		ph.shape = "shape:none" // no file of the snapshot there: nothing to hash
		return ph
	}
	var shapes []string
	for _, n := range names {
		if goCompiled[path.Ext(n)] {
			// Assembly, C and the like are compiled into the package: they count whole.
			f := path.Join(dir, n)
			shapes = append(shapes, n+"\x00"+h.file(f))
			continue
		}
		if !strings.HasSuffix(n, ".go") {
			continue
		}
		f := n
		if dir != "." {
			f = dir + "/" + n
		}
		blob := h.file(f)
		var fp *fileParts
		if v, ok := partsCache.Load(blob); ok {
			fp = v.(*fileParts)
		} else if src, ok := h.readGoIn(f, blob); ok {
			fp = &fileParts{unknown: true}
			if shape, methods, ok := golang.PackageParts(src); ok {
				fp = &fileParts{shape: shape, methods: methods}
			}
			partsCache.Store(blob, fp)
		} else {
			fp = &fileParts{unknown: true}
		}
		switch {
		case fp.unknown:
			ph.unknown = append(ph.unknown, n+"\x00"+blob)
			shapes = append(shapes, n+"\x00"+blob)
		case fp.shape != "":
			shapes = append(shapes, n+"\x00"+fp.shape)
		}
		for t, sigs := range fp.methods {
			ph.methods[t] = append(ph.methods[t], sigs...)
		}
	}
	sum := sha1.Sum([]byte(strings.Join(shapes, "\n")))
	ph.shape = "shape:" + hex.EncodeToString(sum[:])
	return ph
}

// goCompiled are the extensions of the non-Go files go build compiles or links into a
// package (cgo sources and headers, assembly, SWIG, system objects).
var goCompiled = map[string]bool{".c": true, ".h": true, ".s": true, ".S": true, ".sx": true, ".cc": true, ".cpp": true,
	".cxx": true, ".hh": true, ".hpp": true, ".hxx": true, ".m": true, ".f": true, ".F": true, ".for": true, ".f90": true,
	".swig": true, ".swigcxx": true, ".syso": true}

// goPackageHash is the hash of a Go package's shape and what its method sets hash
// from (see goPackage).
type goPackageHash struct {
	shape   string
	methods map[string][]string // type -> method signatures
	unknown []string            // files that count in every method set
}

// methodsOf hashes the method set of type typ.
func (ph *goPackageHash) methodsOf(typ string) string {
	sigs := append(slices.Clone(ph.methods[typ]), ph.unknown...)
	sort.Strings(sigs)
	sum := sha1.Sum([]byte(strings.Join(sigs, "\n")))
	return "methods:" + hex.EncodeToString(sum[:])
}

// tree hashes every file below dir ("." for the repository): a dependency recorded for
// commands smates cannot see into. Files git ignores below a directory the snapshot
// lists are left out (build output, caches). A directory the snapshot has no file below
// (one git ignores entirely: a command ran in build/; a submodule or a directory inside
// one) is read from disk instead, as the run left it (see diskTree). The pinned commit
// of a submodule at or above dir counts too (gitlinks): the files inside a submodule
// are not in the snapshot, and a new commit can leave their paths and sizes as they
// were (VERSION 1.2.3 -> 1.2.4).
func (h *Hasher) tree(dir string) string {
	if v, ok := h.trees[dir]; ok {
		return v
	}
	var v string
	if files, ok := h.treeFiles(dir); !ok {
		v = h.diskTree(dir)
	} else {
		v = h.treeSum(dir, files, false)
	}
	h.trees[dir] = v
	return v
}

// gitlinks returns the submodules of the snapshot at dir or above it, and not among
// files (the files a tree of dir sums, which hold a submodule below dir already),
// deepest first.
func (h *Hasher) gitlinks(dir string, files []string) []string {
	var out []string
	for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if gitx.IsGitlink(h.snap[d]) {
			if _, found := slices.BinarySearch(files, d); !found {
				out = append(out, d)
			}
		}
	}
	return out
}

// sumGitlinks writes gitlinks (see gitlinks) into the sum of a tree.
func (h *Hasher) sumGitlinks(sum io.Writer, links []string) {
	for _, d := range links {
		io.WriteString(sum, d+"\x00"+h.snap[d]+"\n")
	}
}

// treeFiles lists the snapshot's files a tree of dir hashes, sorted; false when the
// tree is read from disk instead (see tree).
func (h *Hasher) treeFiles(dir string) ([]string, bool) {
	if _, listed := h.snapEntries(depkey.Listing(dir)); !listed && h.root != "" && dir != "." && !pathutil.IsVendored(dir) {
		return nil, false
	}
	var files []string
	for _, f := range h.under(dir) {
		if !pathutil.IsVendored(f) && !pathutil.Match(h.ignore, f) {
			files = append(files, f)
		}
	}
	return files, true
}

// under returns the snapshot's paths that are dir or below it, sorted: a range of the
// sorted paths, found by binary search.
func (h *Hasher) under(dir string) []string {
	if h.files == nil {
		h.files = make([]string, 0, len(h.snap))
		for f := range h.snap {
			h.files = append(h.files, f)
		}
		sort.Strings(h.files)
	}
	if dir == "." || dir == "" {
		return h.files
	}
	// dir itself sorts first, then the paths with the prefix dir+"/" (paths sharing a
	// prefix are contiguous).
	var out []string
	if i, found := slices.BinarySearch(h.files, dir); found {
		out = append(out, h.files[i])
	}
	prefix := dir + "/"
	i, _ := slices.BinarySearch(h.files, prefix)
	j := i + sort.Search(len(h.files)-i, func(k int) bool { return !strings.HasPrefix(h.files[i+k], prefix) })
	return append(out, h.files[i:j]...)
}

// treeSum hashes the files of a tree of dir with their snapshot hashes, after the
// gitlinks at or above dir; legacy leaves the executable marker out, as smates before
// it did (gitx.LegacyHash).
func (h *Hasher) treeSum(dir string, files []string, legacy bool) string {
	sum := sha1.New()
	h.sumGitlinks(sum, h.gitlinks(dir, files))
	for _, f := range files {
		v := h.snap[f]
		if legacy {
			v = gitx.LegacyHash(v)
		}
		sum.Write([]byte(f + "\x00" + v + "\n"))
	}
	return "tree:" + hex.EncodeToString(sum.Sum(nil))
}

// Unchanged reports whether the dependency key, recorded with hash recorded, still has
// it (gitx.ByKey: also a hash recorded without the executable marker). A tree or set
// recorded by a smates without the marker hashed its files' blobs alone: it is
// unchanged while that hash is. DepUnchanged compares a key with an entry.
func (h *Hasher) Unchanged(key, recorded string) bool {
	cur := h.Hash(key)
	if gitx.ByKey.Same(recorded, cur) {
		return true
	}
	if recorded == "" || cur == "" {
		return false
	}
	kind, p := depkey.Parse(key)
	if kind != depkey.KindTree && kind != depkey.KindSet {
		return false
	}
	var v string
	if kind == depkey.KindTree {
		var ok bool
		if v, ok = h.legacy[key]; !ok {
			if files, listed := h.treeFiles(p); listed {
				v = h.treeSum(p, files, true)
			}
			h.legacy[key] = v
		}
	} else if d := h.sets[key]; d != nil {
		var ok bool
		if v, ok = h.legacySets[d]; !ok {
			v = h.setSum(d, true)
			h.legacySets[d] = v
		}
	}
	return v != "" && recorded == v
}

// maxDiskTree caps the entries diskTree walks (a variable for tests).
var maxDiskTree = 20000

// diskTree hashes a directory outside the snapshot from disk: the paths and sizes of
// the files below it, without vendored ones (the ignore globs do not apply, as for
// listing: the command ran there and may read anything below). Contents are not read:
// such trees are build output (build/, target/, dist/), gigabytes to hash on every
// selection, and a build that is not reproducible would rerun the tests forever. A
// tree of more than maxDiskTree entries hashes as a constant (it stays the same while
// the tree is that large: its changes select nothing); it is remembered for
// OversizedTrees, so the selection can say so. The pinned commit of a submodule at or
// above dir counts in both (see tree), so a new commit of the submodule is a change.
func (h *Hasher) diskTree(dir string) string {
	links := h.gitlinks(dir, nil)
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
		if pathutil.IsVendored(p) {
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
		h.oversized[dir] = true
		v := "tree:over:" + strconv.Itoa(maxDiskTree)
		if len(links) > 0 {
			sum := sha1.New()
			h.sumGitlinks(sum, links)
			v += ":" + hex.EncodeToString(sum.Sum(nil))
		}
		return v
	}
	sum := sha1.New()
	h.sumGitlinks(sum, links)
	for _, f := range files { // WalkDir walks in lexical order
		sum.Write([]byte(f.p + "\x00" + strconv.FormatInt(f.size, 10) + "\n"))
	}
	return "tree:" + hex.EncodeToString(sum.Sum(nil))
}

// OversizedTrees lists, sorted, the directories outside the snapshot whose "dir/**"
// tree the hasher (or one sharing its caches) found to have more than maxDiskTree
// entries: their hash is a constant, so changes below them select nothing.
func (h *Hasher) OversizedTrees() []string {
	out := make([]string, 0, len(h.oversized))
	for d := range h.oversized {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
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
