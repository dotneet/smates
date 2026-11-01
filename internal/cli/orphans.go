package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
)

// handleOrphans applies the orphan policy to the changed files no test depends on
// (res.Orphans): tests that name them are selected; what is left is ignored, handed to
// the agent or makes the affected suites run in full, as smates.yml says.
func (p *project) handleOrphans(ctx context.Context, sel *selection, opt selectOptions, base string) {
	res := sel.res
	if len(res.Orphans) == 0 && len(res.LeftOut) == 0 {
		return
	}

	// 1. Deterministic: tests that mention an orphan by name. Changes owned by the
	// suites --suite left out are searched too: a test of this run that names one is
	// selected, as in a run over every suite.
	mentions, explained := res.Mentions(p.graph, append(slices.Clip(res.Orphans), res.LeftOut...))
	sel.mentions = mentions
	res.Apply(res.ReferenceAdditions(mentions))
	explained = intersect(explained, res.Orphans)
	res.HandleOrphans(explained, selector.OrphanReference)
	remaining := subtract(res.Orphans, explained)
	if opt.orphanScope != nil {
		var in []string
		for _, o := range remaining {
			if opt.orphanScope[o] {
				in = append(in, o)
			}
		}
		if n := len(remaining) - len(in); n > 0 {
			p.debugf("%d unattributed change(s) predate this cycle: orphan policy not applied to them", n)
		}
		remaining = in
	}
	if len(remaining) == 0 {
		return
	}

	// 2. Source code no test reaches: no test can be affected.
	if untested := res.Untested(remaining); len(untested) > 0 {
		if p.cfg.Untested == config.UntestedSkip {
			res.HandleOrphans(untested, selector.OrphanUntested)
			res.Notes = append(res.Notes, fmt.Sprintf("%d changed source file(s) no test reaches (untested code): no tests to run for them", len(untested)))
			remaining = subtract(remaining, untested)
			if len(remaining) == 0 {
				return
			}
		} else {
			p.debugf("%d unattributed change(s) are source files no test reaches; `untested: skip` would skip them", len(untested))
		}
	}

	// 3. Policy for what is left (--orphans overrides smates.yml for this run).
	policy := p.cfg.Orphans
	switch opt.orphans {
	case orphansAgent:
		policy = config.OrphansAgent
	case orphansFull:
		policy = config.OrphansAll
	case orphansSkip:
		// An explicit opt-out by the caller, which judged these changes itself.
		res.Notes = append(res.Notes, fmt.Sprintf("skipped by caller: orphan policy not applied to %d unattributed change(s) (--orphans=skip)", len(remaining)))
		res.HandleOrphans(remaining, selector.OrphanSkipped)
		return
	}
	if policy == config.OrphansIgnore {
		res.Notes = append(res.Notes, fmt.Sprintf("ignored %d unattributed change(s) (orphans: ignore)", len(remaining)))
		res.HandleOrphans(remaining, selector.OrphanIgnored)
		return
	}
	if policy == config.OrphansAgent && !opt.noAgent && p.cfg.Agent.Enabled != config.AgentNever {
		ok, why := agent.Available(p.cfg.Agent)
		if ok {
			// A sharded run only reuses a cached decision: each shard asking on its
			// own may get a different answer, and a test only one shard's agent picked
			// would run nowhere when another shard owns it.
			dec, cached, err := p.runAgent(ctx, res, remaining, base, opt.sharded)
			if err == nil {
				sel.agentDec, sel.agentCache = dec, cached
				applyDecision(res, dec)
				if !dec.RunAll {
					res.HandleOrphans(intersect(remaining, dec.NoImpact), selector.OrphanNoImpact)
				}
				res.HandleOrphans(remaining, selector.OrphanAgent)
				return
			}
			if errors.Is(err, errNoCachedDecision) {
				sel.agentNote = "sharded run: the agent is not asked (shards could get different answers) and no unsharded run cached its decision; running affected suites in full"
			} else {
				sel.agentNote = "agent failed (" + err.Error() + "); running affected suites in full"
			}
		} else {
			sel.agentNote = "agent unavailable (" + why + "); running affected suites in full"
		}
	}
	res.Apply(res.OrphanAll(remaining))
	res.HandleOrphans(remaining, selector.OrphanPolicy)
	// Whole suites run for these changes: a calling agent that knows what it changed
	// can decide instead (unless it already did, with --orphans).
	sel.hint = opt.orphans == ""
}

// Values of --orphans: what to do with the changes no test depends on, overriding
// `orphans` in smates.yml for one run.
const (
	orphansAgent = "agent" // the orphans: agent policy
	orphansFull  = "full"  // the orphans: all policy (affected suites run in full)
	orphansSkip  = "skip"  // the caller opts out of the orphan policy
)

// orphanHint tells a calling agent how to decide about orphans itself.
const orphanHint = "a calling agent can decide instead: `smates select --format json` lists orphan_candidates; " +
	"then `smates run --add <suite>:<unit|test file>` (repeatable) and/or `--orphans=skip`"

// callerFlags let the caller (e.g. a coding agent) return its own decision about the
// changes no test depends on: tests to add, and how to apply the orphan policy.
type callerFlags struct {
	adds    stringList
	orphans string
}

func (c *callerFlags) register(fs *flag.FlagSet) {
	fs.Var(&c.adds, "add", "also run test `suite:unit` (or a test file path; repeatable); never removes tests")
	fs.StringVar(&c.orphans, "orphans", "", "for changes no test depends on: agent, full (run the affected suites) or skip (explicit opt-out); overrides smates.yml")
}

func (c *callerFlags) validate() error {
	switch c.orphans {
	case "", orphansAgent, orphansFull, orphansSkip:
		return nil
	}
	return usageError{fmt.Errorf("invalid --orphans %q (want agent, full or skip)", c.orphans)}
}

// applyCallerAdds selects the tests --add names (reason "caller"). It only ever adds
// tests; an argument that names no test of this run is an error.
func (p *project) applyCallerAdds(res *selector.Result, args []string) error {
	var adds []selector.Addition
	for _, a := range args {
		suite, ids, err := p.resolveAdd(res, a)
		if err != nil {
			return usageError{fmt.Errorf("--add %s: %w", a, err)}
		}
		for _, id := range ids {
			adds = append(adds, selector.Addition{Suite: suite, Test: id, Reason: selector.ReasonCaller, Detail: "added by caller"})
		}
	}
	if len(adds) == 0 {
		return nil
	}
	before, _ := res.Counts()
	res.Apply(adds)
	after, _ := res.Counts()
	res.Notes = append(res.Notes, fmt.Sprintf("added by caller: %d test unit(s) requested, %d newly selected (--add)", len(adds), after-before))
	return nil
}

// resolveAdd maps an --add argument to a suite and a unit: "suite:unit",
// "suite:path/to/test_file", or a unit or test file without the suite when only one
// suite has it. Paths are repository-relative, relative to the current directory (or
// -C), or after "suite:" relative to the suite's root. A test file declaring several
// units (Go with granularity: func) resolves to all of them.
func (p *project) resolveAdd(res *selector.Result, arg string) (string, []string, error) {
	scope := res.Suites
	spec := arg
	// An absolute path (C:\... on Windows) is not "suite:unit".
	if name, rest, ok := strings.Cut(arg, ":"); ok && name != "" && !strings.HasPrefix(rest, ":") && !filepath.IsAbs(arg) && filepath.VolumeName(arg) == "" {
		if su := res.SuiteByName(name); su != nil {
			scope, spec = []*selector.Suite{su}, rest
		} else if slices.ContainsFunc(p.others, func(s *runner.Suite) bool { return s.Name == name }) {
			return "", nil, fmt.Errorf("suite %q is not part of this run (--suite)", name)
		} else if !strings.ContainsAny(name, "/.") {
			return "", nil, fmt.Errorf("unknown suite %q (have: %s)", name, resultSuiteNames(res))
		}
	}
	if i := strings.Index(spec, "::"); i > 0 { // pytest node IDs: keep the file
		spec = spec[:i]
	}
	if spec == "" {
		return "", nil, fmt.Errorf("no test unit given (want suite:unit)")
	}
	type hit struct {
		suite string
		ids   []string
	}
	for _, cand := range p.addPaths(spec) {
		var hits []hit
		for _, su := range scope {
			ids := unitFor(su, cand)
			if len(ids) == 0 && len(scope) == 1 && su.Suite != nil {
				// "suite:path" may be relative to the suite's root.
				ids = unitFor(su, path.Join(su.Suite.Root, cand))
			}
			if len(ids) > 0 {
				hits = append(hits, hit{su.Name, ids})
			}
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			return hits[0].suite, hits[0].ids, nil
		default:
			var names []string
			for _, h := range hits {
				names = append(names, h.suite+":"+strings.Join(h.ids, ","))
			}
			return "", nil, fmt.Errorf("ambiguous: %s (prefix it with the suite)", strings.Join(names, ", "))
		}
	}
	return "", nil, fmt.Errorf("no test unit or test file %q in suite(s) %s (see `smates list`)", spec, resultSuiteNames(res))
}

// addPaths are the repository-relative paths an --add argument may mean: as given,
// then relative to the current directory and to -C.
func (p *project) addPaths(spec string) []string {
	out := []string{path.Clean(filepath.ToSlash(strings.TrimPrefix(spec, "./")))}
	try := func(abs string) {
		rel, err := filepath.Rel(p.repo.Root, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			// The repository root has its symlinks resolved (/var is /private/var
			// on macOS): resolve the argument's directory the same way.
			dir, derr := filepath.EvalSymlinks(filepath.Dir(abs))
			if derr != nil {
				return
			}
			if rel, err = filepath.Rel(p.repo.Root, filepath.Join(dir, filepath.Base(abs))); err != nil || strings.HasPrefix(rel, "..") {
				return
			}
		}
		if r := filepath.ToSlash(rel); !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	if filepath.IsAbs(spec) {
		try(spec)
		return out
	}
	if wd, err := os.Getwd(); err == nil {
		try(filepath.Join(wd, spec))
	}
	if p.dir != "" {
		try(filepath.Join(absPath(p.dir), spec))
	}
	return out
}

// unitFor returns the units of a suite that f names: a unit ID, or every unit whose
// test files include f (for Go, a test file of a package unit, or each test function
// it declares with granularity: func), or nil.
func unitFor(su *selector.Suite, f string) []string {
	has := func(id string) bool {
		return slices.ContainsFunc(su.Tests, func(t *selector.Test) bool { return t.ID == id })
	}
	if has(f) {
		return []string{f}
	}
	var out []string
	for _, t := range su.Tests {
		if slices.Contains(unitFiles(su, t.ID), f) {
			out = append(out, t.ID)
		}
	}
	if len(out) > 0 {
		return out
	}
	if su.Suite != nil && su.Suite.IsTestFile(f) && has(path.Dir(f)) {
		return []string{path.Dir(f)}
	}
	return nil
}

func resultSuiteNames(res *selector.Result) string {
	var n []string
	for _, s := range res.Suites {
		n = append(n, s.Name)
	}
	return strings.Join(n, ", ")
}

// orphanCandidate is what `smates select --format json` reports about an orphan, for a
// calling agent to decide which tests to add (--add) or to skip the policy.
type orphanCandidate struct {
	Path   string `json:"path"`
	Status string `json:"status"` // added, modified or deleted
	// ReferencedBy are the tests that name the file, with the file that names it.
	ReferencedBy []candidateRef `json:"referenced_by"`
	// Suites are the suites the orphan policy runs in full for it.
	Suites []string `json:"suites"`
	// Handling is how this run handled it (selector.Orphan*; "" = not handled).
	Handling string `json:"handling"`
}

type candidateRef struct {
	Suite    string `json:"suite"`
	Test     string `json:"test"`
	File     string `json:"file"`
	Selected bool   `json:"selected"`
}

// orphanCandidates describes the orphans of a selection for a calling agent.
func (p *project) orphanCandidates(sel *selection) []orphanCandidate {
	res := sel.res
	out := []orphanCandidate{}
	for _, o := range res.Orphans {
		c := orphanCandidate{Path: o, Status: p.orphanStatus(o), ReferencedBy: []candidateRef{}, Suites: []string{}, Handling: res.OrphanHandling[o]}
		for _, m := range sel.mentions {
			if m.Orphan != o {
				continue
			}
			ref := candidateRef{Suite: m.Suite, Test: m.Test, File: m.File}
			if su := res.SuiteByName(m.Suite); su != nil {
				i := slices.IndexFunc(su.Tests, func(t *selector.Test) bool { return t.ID == m.Test })
				ref.Selected = i >= 0 && su.Tests[i].Selected
			}
			c.ReferencedBy = append(c.ReferencedBy, ref)
		}
		for _, a := range res.OrphanAll([]string{o}) {
			c.Suites = append(c.Suites, a.Suite)
		}
		out = append(out, c)
	}
	return out
}

// orphanStatus says whether an orphan was added, modified or deleted.
func (p *project) orphanStatus(o string) string {
	_, hadOld := p.state.FileHash(o)
	_, has := p.snap[o]
	switch {
	case !has:
		return "deleted"
	case !hadOld && len(p.state.Files) > 0:
		return "added"
	}
	return "modified"
}

// subtract returns the elements of a that are not in b.
func subtract(a, b []string) []string {
	drop := map[string]bool{}
	for _, x := range b {
		drop[x] = true
	}
	var out []string
	for _, x := range a {
		if !drop[x] {
			out = append(out, x)
		}
	}
	return out
}

// intersect returns the elements of a that are in b.
func intersect(a, b []string) []string {
	return subtract(a, subtract(a, b))
}

func applyDecision(res *selector.Result, dec *agent.Decision) {
	if dec.RunAll {
		var adds []selector.Addition
		for _, s := range res.Suites {
			adds = append(adds, selector.Addition{Suite: s.Name, Reason: selector.ReasonAgent, Detail: "agent: " + dec.Summary})
		}
		res.Apply(adds)
		return
	}
	res.Apply(dec.Add)
}

// errNoCachedDecision: runAgent was asked for a cached decision only, and there is
// none.
var errNoCachedDecision = errors.New("no cached agent decision")

// runAgent asks the agent about the orphans, or reuses its cached answer to the same
// question (cached). With cachedOnly it never asks: without a cached answer it returns
// errNoCachedDecision.
func (p *project) runAgent(ctx context.Context, res *selector.Result, orphans []string, base string, cachedOnly bool) (*agent.Decision, bool, error) {
	req := &agent.Request{Repo: p.repo, Config: p.cfg.Agent, Selection: res, Log: io.Discard}
	if p.verbose {
		req.Log = p.log
	}
	for _, o := range orphans {
		old, _ := p.state.FileHash(o)
		req.Orphans = append(req.Orphans, agent.Orphan{Path: o, Status: p.orphanStatus(o), OldBlob: old, NewBlob: p.snap[o], Base: base})
	}
	key := agent.CacheKey(req)
	if raw, ok := p.state.AgentCache[key]; ok {
		var dec agent.Decision
		if err := json.Unmarshal(raw, &dec); err == nil && dec.Check() == nil {
			p.logf("agent: reusing cached decision for %d orphan file(s)", len(orphans))
			return &dec, true, nil
		}
	}
	if cachedOnly {
		return nil, false, errNoCachedDecision
	}
	p.logf("agent: asking %s about %d unattributed change(s): %s", agent.Describe(p.cfg.Agent), len(orphans), strings.Join(limit(orphans, 5), ", "))
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Agent.Timeout)
	defer cancel()
	start := time.Now()
	dec, err := agent.New(p.cfg.Agent).Decide(ctx, req)
	if err != nil {
		return nil, false, err
	}
	p.logf("agent: %s (%d turns, %d in / %d out tokens, %s)", oneLine(dec.Summary), dec.Usage.Turns, dec.Usage.InputTokens+dec.Usage.CacheReadTokens+dec.Usage.CacheWriteTokens, dec.Usage.OutputTokens, time.Since(start).Round(100*time.Millisecond))
	if raw, err := json.Marshal(dec); err == nil {
		p.state.AgentCache[key] = raw
	}
	return dec, false, nil
}
