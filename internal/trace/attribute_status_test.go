package trace

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// A helper of the runner killed before it could tell what code it ran (Node writes its
// coverage at exit, Bun lists its modules at exit) still counts as one running
// repository code: what it read, and its script, are dependencies of every test. One
// that exited and showed no repository code (a crawler), a pool worker, and a Python
// process (its code is streamed) are judged by the code they showed.
func TestAttributeKilledHelper(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "server.js", "fixtures/data.json", "a.test.ts", "src/app.js")
	pool := filepath.Join(root, "node_modules", "tinypool", "dist", "entry", "process.js")
	for _, tc := range []struct {
		name     string
		lang     string
		argv     []string
		exited   bool
		coverage bool
		code     []string
		want     []string // Global
	}{
		{name: "node killed (must keep)", lang: "node", argv: []string{"node", "server.js"}, want: []string{"fixtures/data.json", "server.js"}},
		{name: "node killed, absolute script", lang: "node", argv: []string{"node", filepath.Join(root, "server.js")}, want: []string{"fixtures/data.json", "server.js"}},
		{name: "node killed, script outside", lang: "node", argv: []string{"node", "-e", "x"}, want: []string{"fixtures/data.json"}},
		{name: "bun killed", lang: "bun", argv: []string{"bun", "server.js"}, want: []string{"fixtures/data.json", "server.js"}},
		{name: "node killed with coverage of repo code", lang: "node", argv: []string{"node", "server.js"}, coverage: true, code: []string{"src/app.js"}, want: []string{"fixtures/data.json", "src/app.js"}},
		{name: "node killed with coverage, no repo code", lang: "node", argv: []string{"node", "x.js"}, coverage: true},
		{name: "node exited, no repo code", lang: "node", argv: []string{"node", "crawl.js"}, exited: true},
		{name: "bun exited, no repo code", lang: "bun", argv: []string{"bun", "crawl.js"}, exited: true},
		{name: "node pool worker killed", lang: "node", argv: []string{"node", pool}},
		{name: "python killed, no repo code", lang: "python", argv: []string{"x.py"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := mkProc(10, 1, 0).times(100, 900)
			runner.p.Argv, runner.p.Cwd = []string{"node", "runner.js"}, root
			worker := mkProc(11, 10, 0).times(110, 800).code("a.test.ts")
			helper := mkProc(20, 10, 0).times(120, 0).read("fixtures/data.json").code(tc.code...)
			helper.p.Lang, helper.p.Argv, helper.p.Cwd = tc.lang, tc.argv, root
			helper.p.Exited, helper.p.Coverage = tc.exited, tc.coverage
			a := attr(Options{Rel: RelFunc(root)}, runner, worker, helper)
			if got := keys(a.Global); !slices.Equal(got, tc.want) {
				t.Errorf("global %v, want %v", got, tc.want)
			}
		})
	}
}

// A unit's status from process exit codes: a process that died without an exit code
// keeps another one's pass from proving the unit ("" is unknown, settled by the run's
// exit code); a failure stays one.
func TestAttributeUnprovenStatus(t *testing.T) {
	type pr struct {
		code  *int
		units []string
	}
	exit := func(c int) *int { return &c }
	for _, tc := range []struct {
		name  string
		procs []pr
		want  string
	}{
		{name: "pass", procs: []pr{{exit(0), []string{"a.test.ts"}}}, want: state.Pass},
		{name: "pass twice", procs: []pr{{exit(0), []string{"a.test.ts"}}, {exit(0), []string{"a.test.ts"}}}, want: state.Pass},
		{name: "killed alone", procs: []pr{{nil, []string{"a.test.ts"}}}, want: ""},
		{name: "killed, then pass (must keep unknown)", procs: []pr{{nil, []string{"a.test.ts"}}, {exit(0), []string{"a.test.ts"}}}, want: ""},
		{name: "pass, then killed", procs: []pr{{exit(0), []string{"a.test.ts"}}, {nil, []string{"a.test.ts"}}}, want: ""},
		{name: "killed and fail", procs: []pr{{nil, []string{"a.test.ts"}}, {exit(1), []string{"a.test.ts"}}}, want: state.Fail},
		// A failing process that named no unit leaves them to the others.
		{name: "shared failing, then pass", procs: []pr{{exit(1), []string{"a.test.ts", "b.test.ts"}}, {exit(0), []string{"a.test.ts"}}}, want: state.Pass},
		{name: "shared killed, then pass", procs: []pr{{nil, []string{"a.test.ts", "b.test.ts"}}, {exit(0), []string{"a.test.ts"}}}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ps []*pb
			for i, p := range tc.procs {
				b := mkProc(10+i, 1, 0).code(p.units...)
				b.p.Exit = p.code
				ps = append(ps, b)
			}
			a := attr(Options{}, ps...)
			if u := a.Units["a.test.ts"]; u == nil || u.Status != tc.want {
				t.Errorf("a.test.ts: %+v, want status %q", u, tc.want)
			}
		})
	}
}

// pids are reused: a child goes to the process that had its parent's pid when it
// started, also for a worker thread and its process.
func TestAttributeReusedPid(t *testing.T) {
	first := mkProc(50, 1, 0).times(100, 200).code("a.test.ts").exit(0)
	second := mkProc(50, 1, 0).times(1000, 2000).code("b.test.ts").exit(0)
	second.p.Inst = 1
	early := mkProc(60, 50, 0).times(150, 160).read("early.txt")
	late := mkProc(61, 50, 0).times(1500, 1600).read("late.txt")
	thread := mkProc(50, 1, 1).times(1100, 1200).read("thread.txt")
	a := attr(Options{}, first, second, early, late, thread)
	if got, want := unitFiles(a, "a.test.ts"), []string{"a.test.ts", "early.txt"}; !slices.Equal(got, want) {
		t.Errorf("a: %v, want %v", got, want)
	}
	if got, want := unitFiles(a, "b.test.ts"), []string{"b.test.ts", "late.txt", "thread.txt"}; !slices.Equal(got, want) {
		t.Errorf("b: %v, want %v", got, want)
	}
}
