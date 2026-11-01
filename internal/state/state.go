// Package state stores what smates learned about each test: the inputs it depends on,
// their content hashes when it last ran, and the outcome.
package state

import (
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
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
	// Runs summarizes the recent runs, oldest first (bounded, see AddRun).
	Runs []Run
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
	// History holds the most recent outcomes, oldest first (at most HistoryLen).
	History []Outcome
	// Flaky is set once the unit changed outcome while its inputs stayed the same.
	Flaky *Flake
}

// HistoryLen bounds Entry.History.
const HistoryLen = 8

// Outcome is one recorded run of a unit.
type Outcome struct {
	Status string
	// Fingerprint identifies the dependency hashes the run saw (see Fingerprint).
	Fingerprint string
	At          time.Time
	// Inferred: the status was derived from a shared exit code, not reported for the
	// unit; such outcomes never count as evidence of flakiness.
	Inferred bool
}

// Flake is the evidence that a unit is flaky: its outcome flipped between pass and
// fail although every input smates tracks for it was identical.
type Flake struct {
	Count  int       // flips observed
	First  time.Time // first flip
	Last   time.Time // most recent flip
	Commit string    // commit of the most recent flip
	Flip   string    // most recent flip, e.g. "pass->fail"
}

// Fingerprint hashes a dependency map: equal fingerprints mean equal inputs.
func Fingerprint(deps map[string]string) string {
	keys := make([]string, 0, len(deps))
	for k := range deps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha1.New()
	for _, k := range keys {
		h.Write([]byte(k + "\x00" + deps[k] + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
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
	Runs       []Run                      `json:"runs,omitempty"`
}

type diskSuite struct {
	Tests map[string]diskEntry `json:"tests"`
}

type diskEntry struct {
	Deps       [][2]int      `json:"deps"`
	Status     string        `json:"status"`
	DurationMs int64         `json:"duration_ms,omitempty"`
	Commit     string        `json:"commit,omitempty"`
	At         time.Time     `json:"at"`
	History    []diskOutcome `json:"history,omitempty"`
	Flaky      *diskFlake    `json:"flaky,omitempty"`
}

// diskOutcome is kept short: there is one per recent run of every unit.
type diskOutcome struct {
	Status      string `json:"s"`
	Fingerprint string `json:"f,omitempty"`
	At          int64  `json:"t"` // unix seconds
	Inferred    bool   `json:"i,omitempty"`
}

type diskFlake struct {
	Count  int       `json:"count"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	Commit string    `json:"commit,omitempty"`
	Flip   string    `json:"flip,omitempty"`
}

func historyToDisk(h []Outcome) []diskOutcome {
	if len(h) == 0 {
		return nil
	}
	out := make([]diskOutcome, len(h))
	for i, o := range h {
		out[i] = diskOutcome{Status: o.Status, Fingerprint: o.Fingerprint, At: o.At.Unix(), Inferred: o.Inferred}
	}
	return out
}

func historyFromDisk(h []diskOutcome) []Outcome {
	if len(h) == 0 {
		return nil
	}
	out := make([]Outcome, len(h))
	for i, o := range h {
		out[i] = Outcome{Status: o.Status, Fingerprint: o.Fingerprint, At: time.Unix(o.At, 0).UTC(), Inferred: o.Inferred}
	}
	return out
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
	d := &diskState{Version: Version, Tool: s.Tool, Commit: s.Commit, Created: s.Created, Suites: map[string]diskSuite{}, AgentCache: s.AgentCache, Runs: s.Runs}
	d.Files = encodeMap(s.Files, &paths, &blobs)
	for name, su := range s.Suites {
		ds := diskSuite{Tests: map[string]diskEntry{}}
		for id, e := range su.Tests {
			de := diskEntry{Deps: encodeMap(e.Deps, &paths, &blobs), Status: e.Status, DurationMs: e.DurationMs, Commit: e.Commit, At: e.At, History: historyToDisk(e.History)}
			if f := e.Flaky; f != nil {
				de.Flaky = &diskFlake{Count: f.Count, First: f.First, Last: f.Last, Commit: f.Commit, Flip: f.Flip}
			}
			ds.Tests[id] = de
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
	s.Tool, s.Commit, s.Created, s.Runs = d.Tool, d.Commit, d.Created, d.Runs
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
			e := &Entry{Deps: deps, Status: de.Status, DurationMs: de.DurationMs, Commit: de.Commit, At: de.At, History: historyFromDisk(de.History)}
			if f := de.Flaky; f != nil {
				e.Flaky = &Flake{Count: f.Count, First: f.First, Last: f.Last, Commit: f.Commit, Flip: f.Flip}
			}
			su.Tests[id] = e
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
// commit are taken from whichever state has the newer Created time. Outcome histories
// and flake evidence of a unit known to both are combined, so a flake seen on one
// branch or shard is not forgotten. Run records are unioned.
func (s *State) Merge(other *State) {
	if other.Created.After(s.Created) {
		s.Files, s.Commit, s.Created = other.Files, other.Commit, other.Created
	}
	for name, osu := range other.Suites {
		su := s.Suite(name)
		for id, e := range osu.Tests {
			su.Tests[id] = mergeEntry(su.Tests[id], e)
		}
	}
	for k, v := range other.AgentCache {
		s.AgentCache[k] = v
	}
	s.Runs = mergeRuns(s.Runs, other.Runs)
}

// mergeEntry combines two records of one unit: the newer one (by At) wins, outcome
// histories and flake evidence are combined.
func mergeEntry(cur, e *Entry) *Entry {
	if cur == nil {
		return e
	}
	if e == nil {
		return cur
	}
	newer, older := cur, e
	if e.At.After(cur.At) {
		newer, older = e, cur
	}
	merged := *newer
	merged.History = mergeHistory(newer.History, older.History)
	merged.Flaky = mergeFlake(newer.Flaky, older.Flaky)
	return &merged
}

// MergeChanges applies to s (typically the state just re-read from disk) the changes
// ours made to the per-test entries since it was loaded as base (nil: everything in
// ours counts as a change). An entry ours changed replaces the one in s unless s's
// entry changed too since base, a concurrent update: then the two are combined like
// Merge does (newer wins, histories and flake evidence combined). Entries ours left
// alone keep s's version (or are added when s lacks them). Agent cache entries and run
// records are unioned. The snapshot (Files, Commit, Created) is left to the caller: it
// describes one working tree, not the shared entries.
func (s *State) MergeChanges(base, ours *State) {
	for name, osu := range ours.Suites {
		su := s.Suite(name)
		var bsu *Suite
		if base != nil {
			bsu = base.Suites[name]
		}
		for id, o := range osu.Tests {
			var b *Entry
			if bsu != nil {
				b = bsu.Tests[id]
			}
			d := su.Tests[id]
			switch {
			case b != nil && SameEntry(b, o): // unchanged by ours
				if d == nil {
					su.Tests[id] = o
				}
			case d == nil || (b != nil && SameEntry(d, b)): // changed by ours only
				su.Tests[id] = o
			default: // changed on both sides
				su.Tests[id] = mergeEntry(d, o)
			}
		}
	}
	for k, v := range ours.AgentCache {
		s.AgentCache[k] = v
	}
	s.Runs = mergeRuns(s.Runs, ours.Runs)
	if ours.Tool != "" {
		s.Tool = ours.Tool
	}
}

// SameEntry reports whether two entries record the same thing (times compared to the
// precision the disk format keeps).
func SameEntry(a, b *Entry) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Status != b.Status || a.DurationMs != b.DurationMs || a.Commit != b.Commit || !a.At.Equal(b.At) ||
		len(a.Deps) != len(b.Deps) || len(a.History) != len(b.History) || (a.Flaky == nil) != (b.Flaky == nil) {
		return false
	}
	for k, v := range a.Deps {
		if w, ok := b.Deps[k]; !ok || w != v {
			return false
		}
	}
	for i, x := range a.History {
		y := b.History[i]
		if x.Status != y.Status || x.Fingerprint != y.Fingerprint || x.At.Unix() != y.At.Unix() || x.Inferred != y.Inferred {
			return false
		}
	}
	if f, g := a.Flaky, b.Flaky; f != nil {
		if f.Count != g.Count || !f.First.Equal(g.First) || !f.Last.Equal(g.Last) || f.Commit != g.Commit || f.Flip != g.Flip {
			return false
		}
	}
	return true
}

// mergeHistory unions two outcome histories (both usually share a common past),
// ordered by time and bounded to HistoryLen.
func mergeHistory(a, b []Outcome) []Outcome {
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		return b
	}
	// Outcomes are stored with second precision; compare them the same way.
	type key struct {
		status, fp string
		at         int64
		inferred   bool
	}
	seen := map[key]bool{}
	var out []Outcome
	for _, o := range append(append([]Outcome{}, a...), b...) {
		k := key{o.Status, o.Fingerprint, o.At.Unix(), o.Inferred}
		if !seen[k] {
			seen[k] = true
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if len(out) > HistoryLen {
		out = out[len(out)-HistoryLen:]
	}
	return out
}

// mergeFlake combines flake evidence. Counts are not added up: both states usually
// descend from the same recorded run and would count its flips twice.
func mergeFlake(a, b *Flake) *Flake {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	m := *a
	m.Count = max(a.Count, b.Count)
	if b.First.Before(m.First) {
		m.First = b.First
	}
	if b.Last.After(m.Last) {
		m.Last, m.Commit, m.Flip = b.Last, b.Commit, b.Flip
	}
	return &m
}
