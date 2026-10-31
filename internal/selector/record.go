package selector

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// Hasher returns content hashes of dependency keys. A key is a repo-relative file path;
// a directory path with a trailing "/" meaning "the list of entries of this directory"
// (recorded when a test lists a directory, so added or removed files are noticed); or a
// path with a trailing "?" meaning "whether this path exists" (recorded for existence
// checks, including of paths that do not exist yet).
type Hasher struct {
	snap   gitx.Snapshot
	ignore []string
	root   string            // repository root for files outside the snapshot ("" = none)
	disk   map[string]string // memoized hashes of files outside the snapshot
	dirs   map[string][]string
	trees  map[string]string
}

// NewHasher indexes a snapshot. Files matching ignore globs (docs, editor settings) are
// left out of "dir/**" tree hashes, so editing a README does not re-run every test that
// shells out to an opaque command.
func NewHasher(snap gitx.Snapshot, ignore ...string) *Hasher {
	return &Hasher{snap: snap, ignore: ignore}
}

// WithRoot makes the hasher read files git does not list (ignored files such as .env
// or generated code, files inside submodules) from disk under root, so a test that
// reads them notices when they change.
func (h *Hasher) WithRoot(root string) *Hasher {
	h.root = root
	return h
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

// Hash returns the hash of a dependency key, "" when absent.
func (h *Hasher) Hash(key string) string {
	if dir, ok := strings.CutSuffix(key, "/**"); ok {
		return h.tree(dir)
	}
	if p, ok := strings.CutSuffix(key, "?"); ok {
		if _, isFile := h.snap[p]; isFile || h.Hash(p+"/") != "" || p == "." {
			return "exists"
		}
		if h.root != "" {
			if _, err := os.Lstat(filepath.Join(h.root, filepath.FromSlash(p))); err == nil {
				return "exists"
			}
		}
		return "absent"
	}
	if !strings.HasSuffix(key, "/") {
		return h.file(key)
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

// RecordOptions control how observations are written to the state.
type RecordOptions struct {
	Commit string
	Now    time.Time
	// Full replaces every entry of the suite (the run covered all its tests).
	Full bool
	// ReadsBy maps source files to what they read or imported when they ran (once per
	// process); a unit depending on such a file depends on those reads too.
	ReadsBy map[string]map[string]bool
}

// Record writes the observations of one suite run into the state. Each observed unit
// gets deps = observed files ∪ known deps (static closure, inputs, rules), hashed with
// the snapshot taken before the run. Units without an observation keep their old entry
// (and therefore stay selected next time if they were selected now) unless Full is set,
// in which case they are dropped.
func Record(st *state.State, sr *Suite, obs map[string]*runner.Observation, h *Hasher, opt RecordOptions) {
	su := st.Suite(sr.Name)
	if opt.Full {
		su.Tests = map[string]*state.Entry{}
	}
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
		su.Tests[unit] = &state.Entry{Deps: deps, Status: status, DurationMs: o.DurationMs, Commit: opt.Commit, At: opt.Now}
	}
}
