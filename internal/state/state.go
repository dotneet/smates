// Package state stores what smates learned about each test: the inputs it depends on,
// their content hashes when it last ran, and the outcome.
package state

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Version of the on-disk format.
const Version = 1

// ErrUnreadable wraps state files that cannot be decoded (corrupt, or written by an
// incompatible version); callers may start from an empty state instead.
var ErrUnreadable = errors.New("unreadable state")

// Status values.
const (
	Pass    = "pass"
	Fail    = "fail"
	Unknown = "unknown"
)

// State is the in-memory form.
type State struct {
	Commit  string
	Created time.Time
	Tool    string
	// Files is the full-tree snapshot (path -> blob) taken by the last full run.
	Files  map[string]string
	Suites map[string]*Suite
	// AgentCache maps a cache key to a stored agent decision.
	AgentCache map[string]json.RawMessage
}

// Suite holds the entries of one suite.
type Suite struct {
	Tests map[string]*Entry
}

// Entry is what we know about one test unit.
type Entry struct {
	Deps       map[string]string // path -> blob at the time of the run ("" = absent)
	Status     string
	DurationMs int64
	Commit     string
	At         time.Time
}

// New returns an empty state.
func New() *State {
	return &State{Files: map[string]string{}, Suites: map[string]*Suite{}, AgentCache: map[string]json.RawMessage{}}
}

// Suite returns the suite, creating it.
func (s *State) Suite(name string) *Suite {
	su := s.Suites[name]
	if su == nil {
		su = &Suite{Tests: map[string]*Entry{}}
		s.Suites[name] = su
	}
	return su
}

// Empty reports whether nothing has been recorded.
func (s *State) Empty() bool {
	for _, su := range s.Suites {
		if len(su.Tests) > 0 {
			return false
		}
	}
	return true
}

// ---- disk format ----

type diskState struct {
	Version    int                        `json:"version"`
	Tool       string                     `json:"tool,omitempty"`
	Commit     string                     `json:"commit,omitempty"`
	Created    time.Time                  `json:"created"`
	Paths      []string                   `json:"paths"`
	Blobs      []string                   `json:"blobs"`
	Files      [][2]int                   `json:"files"`
	Suites     map[string]diskSuite       `json:"suites"`
	AgentCache map[string]json.RawMessage `json:"agent_cache,omitempty"`
}

type diskSuite struct {
	Tests map[string]diskEntry `json:"tests"`
}

type diskEntry struct {
	Deps       [][2]int  `json:"deps"`
	Status     string    `json:"status"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	At         time.Time `json:"at"`
}

type interner struct {
	idx  map[string]int
	list []string
}

func (in *interner) id(s string) int {
	if i, ok := in.idx[s]; ok {
		return i
	}
	if in.idx == nil {
		in.idx = map[string]int{}
	}
	in.idx[s] = len(in.list)
	in.list = append(in.list, s)
	return len(in.list) - 1
}

func encodeMap(m map[string]string, paths, blobs *interner) [][2]int {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]int, len(keys))
	for i, k := range keys {
		out[i] = [2]int{paths.id(k), blobs.id(m[k])}
	}
	return out
}

func (s *State) toDisk() *diskState {
	var paths, blobs interner
	// Intern snapshot paths first in sorted order for better compression.
	d := &diskState{Version: Version, Tool: s.Tool, Commit: s.Commit, Created: s.Created, Suites: map[string]diskSuite{}, AgentCache: s.AgentCache}
	d.Files = encodeMap(s.Files, &paths, &blobs)
	for name, su := range s.Suites {
		ds := diskSuite{Tests: map[string]diskEntry{}}
		for id, e := range su.Tests {
			ds.Tests[id] = diskEntry{Deps: encodeMap(e.Deps, &paths, &blobs), Status: e.Status, DurationMs: e.DurationMs, Commit: e.Commit, At: e.At}
		}
		d.Suites[name] = ds
	}
	d.Paths, d.Blobs = paths.list, blobs.list
	if d.Paths == nil {
		d.Paths = []string{}
	}
	if d.Blobs == nil {
		d.Blobs = []string{}
	}
	return d
}

func fromDisk(d *diskState) (*State, error) {
	if d.Version != Version {
		return nil, fmt.Errorf("%w: unsupported state version %d (this smates reads %d)", ErrUnreadable, d.Version, Version)
	}
	decode := func(pairs [][2]int) (map[string]string, error) {
		m := make(map[string]string, len(pairs))
		for _, p := range pairs {
			if p[0] < 0 || p[0] >= len(d.Paths) || p[1] < 0 || p[1] >= len(d.Blobs) {
				return nil, fmt.Errorf("%w: index out of range", ErrUnreadable)
			}
			m[d.Paths[p[0]]] = d.Blobs[p[1]]
		}
		return m, nil
	}
	s := New()
	s.Tool, s.Commit, s.Created = d.Tool, d.Commit, d.Created
	var err error
	if s.Files, err = decode(d.Files); err != nil {
		return nil, err
	}
	for name, ds := range d.Suites {
		su := s.Suite(name)
		for id, de := range ds.Tests {
			deps, err := decode(de.Deps)
			if err != nil {
				return nil, err
			}
			su.Tests[id] = &Entry{Deps: deps, Status: de.Status, DurationMs: de.DurationMs, Commit: de.Commit, At: de.At}
		}
	}
	if d.AgentCache != nil {
		s.AgentCache = d.AgentCache
	}
	return s, nil
}

// Load reads a state file. A missing file yields an empty state and no error.
func Load(path string) (*State, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f, strings.HasSuffix(path, ".gz"))
}

// Read decodes a state from r.
func Read(r io.Reader, gz bool) (*State, error) {
	if gz {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnreadable, err)
		}
		defer zr.Close()
		r = zr
	}
	var d diskState
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	return fromDisk(&d)
}

// Save writes the state atomically.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := s.Write(tmp, strings.HasSuffix(path, ".gz")); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Write encodes the state to w.
func (s *State) Write(w io.Writer, gz bool) error {
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(w)
		w = zw
	}
	if err := json.NewEncoder(w).Encode(s.toDisk()); err != nil {
		return err
	}
	if zw != nil {
		return zw.Close()
	}
	return nil
}

// Merge copies entries from other that are newer than ours (by At). The snapshot and
// commit are taken from whichever state has the newer Created time.
func (s *State) Merge(other *State) {
	if other.Created.After(s.Created) {
		s.Files, s.Commit, s.Created = other.Files, other.Commit, other.Created
	}
	for name, osu := range other.Suites {
		su := s.Suite(name)
		for id, e := range osu.Tests {
			if cur, ok := su.Tests[id]; !ok || e.At.After(cur.At) {
				su.Tests[id] = e
			}
		}
	}
	for k, v := range other.AgentCache {
		s.AgentCache[k] = v
	}
}
