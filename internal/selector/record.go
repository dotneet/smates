package selector

import (
	"sort"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

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
		// A unit recorded from its known dependencies (StaticOnly) passes only when they
		// can be trusted, and are known at all: an empty list is one never computed. A
		// pass reported without any trace (NoTrace) proves nothing about what it used.
		if status == "" || o.StaticOnly && (sr.Untrusted[unit] != "" || len(sr.Known[unit]) == 0) || o.NoTrace && status == state.Pass {
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
		if !h.Unchanged(k, v) {
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
		if kind, f := depkey.Parse(k); kind == depkey.KindDecls || kind == depkey.KindDecl || kind == depkey.KindSig {
			// The declarations of a Go file, or one of them: unchanged if the file is.
			if was, ok := files[f]; len(files) > 0 && (!ok && v != "" || ok && !gitx.SameHash(was, h.Hash(f))) {
				return false
			}
			continue
		}
		if depkey.IsPseudo(k) {
			continue
		}
		if was, ok := files[k]; len(files) > 0 && (!ok && v != "" || ok && !gitx.SameHash(was, v)) {
			return false
		}
	}
	return true
}
