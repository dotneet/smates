package selector

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// TestUntracedBlind: every way a source may run code no trace sees without coverage
// makes an untraced test depending on it untrusted; reads through node:fs and the
// environment do not (the shim sees them), nor imports the analyzer resolves.
func TestUntracedBlind(t *testing.T) {
	for _, c := range []struct {
		name, src string
		blind     bool
	}{
		{"plain code", `import { sumGrid } from "./grid"; export const x = sumGrid(2);`, false},
		{"node:fs (the shim sees reads)", `import { readFileSync } from "node:fs"; readFileSync("a.json");`, false},
		{"fs/promises", `import { readFile } from "fs/promises";`, false},
		{"environment (the shim sees it)", `export const n = Number(process.env.SIZE ?? 10);`, false},
		{"literal dynamic import", `const m = await import("./grid");`, false},
		{"literal require", `const m = require('./grid');`, false},
		{"template the analyzer expands", "const m = await import(`./sizes/${name}.ts`);", false},
		{"require.resolve", `const p = require.resolve("./grid");`, false},
		{"type-only import of a blind builtin", `import type { ChildProcess } from "node:child_process";`, false},
		{"child_process", `import { execSync } from "child_process";`, true},
		{"node:child_process", `const cp = require("node:child_process");`, true},
		{"worker_threads", `import { Worker } from "node:worker_threads";`, true},
		{"cluster", `import cluster from "cluster";`, true},
		{"module (createRequire)", `import { createRequire } from "node:module";`, true},
		{"wasi", `import { WASI } from "node:wasi";`, true},
		{"sqlite", `import { DatabaseSync } from "node:sqlite";`, true},
		{"bun module", `import { $ } from "bun";`, true},
		{"Bun.file", `const t = await Bun.file("sizes.json").text();`, true},
		{"Bun.spawn", `Bun.spawn(["node", "x.js"]);`, true},
		{"Bun shell", "await Bun.$`ls`;", true},
		{"Deno read", `const t = await Deno.readTextFile("sizes.json");`, true},
		{"Deno command", `new Deno.Command("node");`, true},
		{"computed dynamic import", `const m = await import(name);`, true},
		{"computed require", `const m = require(path.join(dir, "grid"));`, true},
		{"computed import after a comment", `const m = await import(/* lazy */ "./grid");`, true}, // distrusted: more, never less
	} {
		t.Run(c.name, func(t *testing.T) {
			why := untracedBlind("src/x.ts", []byte(c.src))
			if (why != "") != c.blind {
				t.Errorf("untracedBlind = %q, want blind: %v", why, c.blind)
			}
		})
	}
	if why := untracedBlind("src/x.ts", nil); !strings.Contains(why, "could not read") {
		t.Errorf("unreadable source: %q", why)
	}
}

// untracedFixture is a node --test suite whose test/perf.test.js the suite's untraced
// lists; files override or add sources.
func untracedFixture(t *testing.T, cs config.Suite, files map[string]string) *fixture {
	t.Helper()
	src := map[string]string{
		"src/grid.js":       `export const sumGrid = (n) => n * n;`,
		"test/perf.test.js": `import { sumGrid } from "../src/grid.js";`,
		"test/grid.test.js": `import { sumGrid } from "../src/grid.js";`,
	}
	maps.Copy(src, files)
	f := newFixture(t, src)
	if cs.Runner == "" {
		cs.Runner = "node"
	}
	if cs.Tests == nil {
		cs.Tests = []string{"test/*.test.*"}
	}
	if cs.Untraced == nil {
		cs.Untraced = []string{"test/perf.test.*"}
	}
	f.suites = []*runner.Suite{suite(t, cs)}
	return f
}

// TestUntracedUntrusted: an untraced unit is trusted only when its known dependencies
// can stand for the code it ran; each case that cannot tell is untrusted (must keep:
// its pass stays unknown and it runs again).
func TestUntracedUntrusted(t *testing.T) {
	off := false
	for _, c := range []struct {
		name  string
		cs    config.Suite
		files map[string]string
		unit  string
		want  string // substring of the reason; "" = trusted
	}{
		{name: "trusted", unit: "test/perf.test.js"},
		{name: "trusted, reads files through node:fs", files: map[string]string{"src/grid.js": `import fs from "node:fs"; export const sizes = fs.readFileSync("sizes.json");`}, unit: "test/perf.test.js"},
		{name: "must keep: a runner that cannot trace without coverage", cs: config.Suite{Runner: "pytest", Tests: []string{"tests/test_*.py"}, Untraced: []string{"tests/test_perf.py"}},
			files: map[string]string{"tests/test_perf.py": "import time\n"}, unit: "tests/test_perf.py", want: "runs untraced tests without any tracing"},
		{name: "must keep: static analysis off", cs: config.Suite{Static: &off}, unit: "test/perf.test.js", want: "static analysis is off"},
		{name: "must keep: a test file the analyzer does not read", cs: config.Suite{Untraced: []string{"test/perf.test.*"}},
			files: map[string]string{"test/perf.test.py": "x"}, unit: "test/perf.test.py", want: "not JavaScript or TypeScript"},
		{name: "must keep: an import smates cannot resolve", files: map[string]string{"test/perf.test.js": `import sizes from "@/fixtures/sizes";`},
			unit: "test/perf.test.js", want: `"@/fixtures/sizes"`},
		{name: "must keep: a transitive import starts processes", files: map[string]string{"src/grid.js": `import { execSync } from "node:child_process";`},
			unit: "test/perf.test.js", want: `src/grid.js imports "node:child_process"`},
		{name: "must keep: a module named at run time", files: map[string]string{"test/perf.test.js": `const m = await import(process.argv[2]);`},
			unit: "test/perf.test.js", want: "names at run time"},
		{name: "traced units are not judged", unit: "test/grid.test.js"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := untracedFixture(t, c.cs, c.files)
			res := f.sel(state.New(), func(in *Input) { in.All = true })
			got := res.Suites[0].Untrusted[c.unit]
			if c.want == "" && got != "" || !strings.Contains(got, c.want) {
				t.Errorf("Untrusted[%s] = %q, want %q", c.unit, got, c.want)
			}
		})
	}
}

// recordUntraced simulates a recorded run of every unit: untraced units pass as
// StaticOnly observations holding only what the shim saw (reads[unit]), others pass
// with their known dependencies.
func (f *fixture) recordUntraced(reads map[string][]string, known func(*Suite)) *state.State {
	f.t.Helper()
	st := state.New()
	res := f.sel(st, func(in *Input) { in.All = true })
	h := NewHasher(f.snap()).WithConfig(f.cfg)
	for _, sr := range res.Suites {
		if known != nil {
			known(sr)
		}
		obs := map[string]*runner.Observation{}
		for _, tt := range sr.Tests {
			o := &runner.Observation{Files: map[string]bool{}, Status: state.Pass}
			for _, r := range reads[tt.ID] {
				o.Files[r] = true
			}
			o.StaticOnly = UntracedUnit(sr.Suite, tt.ID, sr.Files[tt.ID])
			obs[tt.ID] = o
		}
		Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Commit: "c1", Now: time.Unix(1, 0), Full: true})
	}
	st.Files = map[string]string(f.snap())
	return st
}

// TestSelectUntraced: a trusted untraced pass is recorded with its static closure and
// what the shim saw, so changing either selects it; an untrusted one stays unknown and
// runs every time, as does one whose known dependencies were never computed.
func TestSelectUntraced(t *testing.T) {
	perf, grid, spawn := "test/perf.test.js", "test/grid.test.js", "test/spawn.test.js"
	files := map[string]string{
		"data/sizes.json":    `[100, 200]`,
		"src/spawn.js":       `import { execSync } from "node:child_process";`,
		"test/spawn.test.js": `import "../src/spawn.js";`,
	}
	cs := config.Suite{Untraced: []string{"test/perf.test.js", "test/spawn.test.js"}}
	reads := map[string][]string{perf: {"data/sizes.json"}}
	for _, c := range []struct {
		name   string
		change string
		want   map[string]string
	}{
		{name: "nothing changed", want: map[string]string{perf: "-unaffected", grid: "-unaffected", spawn: ReasonFailed}},
		{name: "a static import changed", change: "src/grid.js", want: map[string]string{perf: ReasonChanged, grid: ReasonChanged, spawn: ReasonFailed}},
		{name: "a file the shim saw read changed", change: "data/sizes.json", want: map[string]string{perf: ReasonChanged, grid: "-unaffected", spawn: ReasonFailed}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := untracedFixture(t, cs, files)
			st := f.recordUntraced(reads, nil)
			if e := st.Suites["node"].Tests[perf]; e == nil || e.Status != state.Pass {
				t.Fatalf("trusted untraced pass recorded as %+v", e)
			}
			if e := st.Suites["node"].Tests[spawn]; e == nil || e.Status != state.Unknown {
				t.Fatalf("untrusted untraced pass recorded as %+v", e)
			}
			if c.change != "" {
				f.write(c.change, f.files[c.change]+"\n// changed")
			}
			got := decisions(f.sel(st, nil))
			for u, want := range c.want {
				if got[u] != want {
					t.Errorf("%s: %s, want %s", u, got[u], want)
				}
			}
		})
	}
	t.Run("must keep: known dependencies never computed", func(t *testing.T) {
		f := untracedFixture(t, cs, files)
		st := f.recordUntraced(reads, func(sr *Suite) { delete(sr.Known, perf) })
		if e := st.Suites["node"].Tests[perf]; e == nil || e.Status != state.Unknown {
			t.Fatalf("a StaticOnly pass without known dependencies recorded as %+v", e)
		}
		if got := decisions(f.sel(st, nil))[perf]; strings.HasPrefix(got, "-") {
			t.Errorf("%s: %s, want selected", perf, got)
		}
	})
}

// TestUntracedUnit: the untraced globs match units like volatile does; a suite without
// them, or a Go compile check, is never untraced.
func TestUntracedUnit(t *testing.T) {
	node := suite(t, config.Suite{Runner: "node", Untraced: []string{"test/perf.*", "bench/**"}})
	gosu := suite(t, config.Suite{Runner: "go", Untraced: []string{"pkg/perf"}})
	for _, c := range []struct {
		name  string
		su    *runner.Suite
		unit  string
		files []string
		want  bool
	}{
		{"listed file", node, "test/perf.test.js", []string{"test/perf.test.js"}, true},
		{"glob below a directory", node, "bench/a/b.test.js", []string{"bench/a/b.test.js"}, true},
		{"not listed", node, "test/grid.test.js", []string{"test/grid.test.js"}, false},
		{"no untraced", suite(t, config.Suite{Runner: "node"}), "test/perf.test.js", []string{"test/perf.test.js"}, false},
		{"go package", gosu, "pkg/perf", []string{"pkg/perf/perf_test.go"}, true},
		{"go test function by its package", gosu, "pkg/perf#TestRatio", []string{"pkg/perf/perf_test.go"}, true},
		{"nil suite", nil, "test/perf.test.js", nil, false},
	} {
		if got := UntracedUnit(c.su, c.unit, c.files); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
