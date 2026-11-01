package trace

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/state"
)

// Unit is the attributed usage of one test unit.
type Unit struct {
	Files map[string]bool
	// Status: state.Pass, state.Fail or "" (unknown) from runner plugins or process exit codes.
	Status string
	Ms     int64
	// Message is the failure output a runner plugin reported for the unit.
	Message string
	// Shared is set when the unit ran in a process together with other units, so its
	// dependencies are the union of all of them (correct but less precise).
	Shared bool
}

// Attribution is the result of Attribute.
type Attribution struct {
	Units map[string]*Unit
	// Global are files used by helper processes that belong to no test (e.g. a server
	// started by global setup); callers should treat them as dependencies of every unit.
	Global map[string]bool
	// SharedProcs counts processes that executed more than one unit.
	SharedProcs int
}

// Options tune attribution.
type Options struct {
	// UnitsFromArgv identifies the unit a process runs from test files named on its
	// command line instead of from executed code (runners without code coverage that
	// are started once per test file).
	UnitsFromArgv bool
	// Rel converts an absolute path to a repo-relative one (required with UnitsFromArgv).
	Rel func(string) (string, bool)
}

// Attribute maps traced processes to units. unitOf returns the unit of a repo-relative
// test file ("" when the file is not a test of the suite).
func Attribute(procs []*Proc, unitOf func(string) string, opt Options) *Attribution {
	res := &Attribution{Units: map[string]*Unit{}, Global: map[string]bool{}}
	byKey := map[string]*Proc{}
	byPid := map[int]*Proc{}
	for _, p := range procs {
		byKey[p.Key()] = p
		if cur, ok := byPid[p.Pid]; !ok || p.Tid < cur.Tid {
			byPid[p.Pid] = p
		}
	}
	parent := func(p *Proc) *Proc {
		if p.Tid != 0 {
			if m := byPid[p.Pid]; m != nil && m != p {
				return m
			}
		}
		if p.Ppid != 0 && p.Ppid != p.Pid {
			if a := byPid[p.Ppid]; a != nil {
				return a
			}
		}
		// The direct parent was not traced (a shell, npx, uv...): use the nearest traced
		// ancestor recorded in the environment.
		if a := byKey[p.Anc]; a != nil && a != p {
			return a
		}
		return nil
	}

	// A process started inside a test of a process with contexts (a script, a pool
	// worker, a pytest session the test runs in a subprocess) is that test's helper,
	// also when it executes test files itself (a test running pytest on files of the
	// suite) or has contexts of its own that belong to no unit (the session's tests are
	// temporary files).
	helper := map[*Proc]bool{}
	for _, p := range procs {
		sp := byKey[p.SpawnKey]
		if sp != nil && sp != p && sp.HasContexts() && testContext(p.SpawnCtx) && !(p.HasContexts() && hasUnitContexts(p, unitOf)) {
			helper[p] = true
		}
	}

	// Units each process executed itself.
	own := map[*Proc][]string{}
	for _, p := range procs {
		if p.HasContexts() || helper[p] {
			continue
		}
		seen := map[string]bool{}
		addUnit := func(f string) {
			if u := unitOf(f); u != "" && !seen[u] {
				seen[u] = true
				own[p] = append(own[p], u)
			}
		}
		if opt.UnitsFromArgv {
			for _, a := range p.Argv {
				if !filepath.IsAbs(a) {
					a = filepath.Join(p.Cwd, a)
				}
				if r, ok := opt.Rel(a); ok {
					addUnit(r)
				}
			}
		} else {
			for f := range p.Code {
				addUnit(f)
			}
		}
		sort.Strings(own[p])
	}
	owner := func(p *Proc) bool { return !helper[p] && (len(own[p]) > 0 || p.HasContexts()) }

	// Processes that are ancestors of a unit-owning process are runner infrastructure.
	infra := map[*Proc]bool{}
	for _, p := range procs {
		if !owner(p) {
			continue
		}
		for a, n := parent(p), 0; a != nil && n < 64; a, n = parent(a), n+1 {
			infra[a] = true
		}
	}

	extra := map[*Proc]map[string]bool{} // files from helper children, per owning process
	for _, p := range procs {
		if owner(p) {
			continue
		}
		files := p.allFiles()
		var target *Proc
		for a, n := parent(p), 0; a != nil && n < 64; a, n = parent(a), n+1 {
			if owner(a) {
				target = a
				break
			}
		}
		switch {
		case target != nil && target.HasContexts():
			for ctx, files := range p.contextsIn(target, files) {
				m := target.Ctx[ctx]
				if m == nil {
					m = map[string]bool{}
					target.Ctx[ctx] = m
				}
				for f := range files {
					m[f] = true
				}
			}
		case target != nil:
			if extra[target] == nil {
				extra[target] = map[string]bool{}
			}
			for f := range files {
				extra[target][f] = true
			}
		case !infra[p] && len(p.Code) > 0:
			// A helper that runs repository code (a server started by global setup) is
			// something every test may talk to. Helpers that only read files (the
			// runner's own crawlers and transformers) are not.
			for f := range files {
				res.Global[f] = true
			}
		}
	}

	// Code the runner itself executed (its config, global setup and what they import,
	// custom reporters or resolvers) can affect every test. Its reads are not counted:
	// runners read every test and source file to transform or crawl them.
	for p := range infra {
		if p.HasContexts() {
			continue
		}
		for f := range p.Code {
			if unitOf(f) == "" {
				res.Global[f] = true
			}
		}
	}
	// Global setup (marked by runner integrations) affects every test, even when it ran
	// in the runner's own process.
	for _, p := range procs {
		for f := range p.Ctx[GlobalContext] {
			if k := depkey.KindOf(f); unitOf(f) == "" && k != depkey.KindExistence && k != depkey.KindListing {
				res.Global[f] = true
			}
		}
	}

	unit := func(id string) *Unit {
		u := res.Units[id]
		if u == nil {
			u = &Unit{Files: map[string]bool{}}
			res.Units[id] = u
		}
		return u
	}
	for _, p := range procs {
		if helper[p] {
			continue
		}
		if p.HasContexts() {
			attributeContexts(p, unitOf, unit)
			continue
		}
		units := own[p]
		if len(units) == 0 {
			continue
		}
		if len(units) > 1 {
			res.SharedProcs++
		}
		files := p.allFiles()
		for f := range extra[p] {
			files[f] = true
		}
		named := namedUnit(p, units, unitOf, opt.Rel)
		for _, id := range units {
			u := unit(id)
			for f := range files {
				u.Files[f] = true
			}
			u.Shared = u.Shared || len(units) > 1
			if len(units) == 1 && p.End > p.Start && p.Start > 0 {
				u.Ms += p.End - p.Start
			}
			// A failing process that ran several units says which one failed only when its
			// command line names exactly one of them (node --test's process for
			// test/a.test.js, which also loaded the unit test/common.js). Otherwise
			// (node --test --test-isolation=none names every file) they are left to the
			// overall exit code: an inferred failure, run again next time.
			if p.Exit != nil && (*p.Exit == 0 || len(units) == 1 || id == named) {
				st := state.Pass
				if *p.Exit != 0 {
					st = state.Fail
				}
				u.Status = worse(u.Status, st)
			}
		}
	}
	return res
}

// namedUnit returns the one unit among units that p's command line names (arguments
// resolved against its working directory), "" when none or several are named.
func namedUnit(p *Proc, units []string, unitOf func(string) string, rel func(string) (string, bool)) string {
	if len(units) < 2 || rel == nil {
		return ""
	}
	named := ""
	for _, a := range p.Argv {
		if !filepath.IsAbs(a) {
			a = filepath.Join(p.Cwd, a)
		}
		r, ok := rel(a)
		if !ok {
			continue
		}
		u := unitOf(r)
		if u == "" || u == named || !slices.Contains(units, u) {
			continue
		}
		if named != "" {
			return ""
		}
		named = u
	}
	return named
}

// testContext reports whether ctx is a context of a test file, of a test or a fixture.
func testContext(ctx string) bool {
	kind, _, _ := strings.Cut(ctx, ":")
	return kind == "test" || kind == "collect" || kind == "fixture"
}

// hasUnitContexts reports whether contexts, fixtures used or results of p belong to a
// unit.
func hasUnitContexts(p *Proc, unitOf func(string) string) bool {
	for ctx := range p.Ctx {
		if kind, file, ok := strings.Cut(ctx, ":"); ok && (kind == "test" || kind == "collect") && unitOf(file) != "" {
			return true
		}
	}
	for file := range p.Uses {
		if unitOf(file) != "" {
			return true
		}
	}
	for file := range p.Results {
		if unitOf(file) != "" {
			return true
		}
	}
	return false
}

func attributeContexts(p *Proc, unitOf func(string) string, unit func(string) *Unit) {
	touched := map[string]*Unit{}
	for ctx, files := range p.Ctx {
		kind, file, ok := strings.Cut(ctx, ":")
		if !ok || (kind != "test" && kind != "collect") {
			continue
		}
		id := unitOf(file)
		if id == "" {
			continue
		}
		u := unit(id)
		touched[id] = u
		for f := range files {
			u.Files[f] = true
		}
	}
	used := map[string]bool{}
	for file, ctxs := range p.Uses {
		for _, c := range ctxs {
			used[c] = true
		}
		id := unitOf(file)
		if id == "" {
			continue
		}
		u := unit(id)
		touched[id] = u
		for _, c := range ctxs {
			for f := range p.Ctx[c] {
				u.Files[f] = true
			}
		}
	}
	for file, r := range p.Results {
		id := unitOf(file)
		if id == "" {
			continue
		}
		u := unit(id)
		touched[id] = u
		u.Status = worse(u.Status, r.Status)
		u.Ms += r.Ms
		u.Message = JoinMessage(u.Message, r.Message)
	}

	// Activity no test context claims is shared by every unit of the process, like
	// Attribution.Global for helper processes: data read outside any context (e.g. by a
	// conftest at import time), child processes started outside any context (e.g. a
	// server started by a session hook), and fixtures no test declared (requested
	// dynamically with request.getfixturevalue). Directory listings and existence probes
	// outside contexts are test collection, and code executed there (conftest modules and
	// their imports) is covered by the static graph, so they are left out.
	shared := map[string]bool{}
	for ctx, files := range p.Ctx {
		kind, _, _ := strings.Cut(ctx, ":")
		switch {
		case ctx == "":
			for f := range files {
				if k := depkey.KindOf(f); k != depkey.KindListing && k != depkey.KindExistence && !p.Code[f] {
					shared[f] = true
				}
			}
		case kind == "fixture" && !used[ctx]:
			for f := range files {
				shared[f] = true
			}
		}
	}
	for _, u := range touched {
		for f := range shared {
			u.Files[f] = true
		}
	}
}

// worse combines statuses: fail beats unknown beats pass.
func worse(a, b string) string {
	rank := func(s string) int {
		switch s {
		case state.Fail:
			return 2
		case state.Pass:
			return 0
		}
		return 1
	}
	if a == "" {
		return b
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func (p *Proc) allFiles() map[string]bool {
	out := make(map[string]bool, len(p.Code)+len(p.Reads))
	for f := range p.Code {
		out[f] = true
	}
	for f := range p.Reads {
		out[f] = true
	}
	return out
}

// contextsIn splits files, what child p used, by the contexts of target they belong to:
// the ones target exported to p (the context p was started in, those a forked child
// followed), else the context active when p started. Files recorded outside the trace
// stream (V8 coverage) belong to the context p was started in.
func (p *Proc) contextsIn(target *Proc, files map[string]bool) map[string]map[string]bool {
	if p.SpawnKey != target.Key() {
		return map[string]map[string]bool{target.contextAt(p.Start): files}
	}
	out := map[string]map[string]bool{}
	rest := map[string]bool{}
	for f := range files {
		rest[f] = true
	}
	for ctx, fs := range p.SpawnFiles {
		out[ctx] = fs
		for f := range fs {
			delete(rest, f)
		}
	}
	if len(rest) > 0 {
		m := map[string]bool{}
		for f := range out[p.SpawnCtx] {
			m[f] = true
		}
		for f := range rest {
			m[f] = true
		}
		out[p.SpawnCtx] = m
	}
	return out
}

// contextAt returns the context active at time t (unix ms).
func (p *Proc) contextAt(t int64) string {
	ctx := ""
	for _, s := range p.CtxSpans {
		if s.At > t {
			break
		}
		ctx = s.ID
	}
	return ctx
}
