package trace

import (
	"path/filepath"
	"slices"
	"testing"
)

// The index answers which process held a pid at a time: a reference names the last
// one started by then (also one that ended since), a parent pid only one still running.
func TestProcIndex(t *testing.T) {
	mk := func(pid, tid int, start, end int64) *Proc {
		p := newProc(pid, tid)
		p.Start, p.End = start, end
		return p
	}
	first := mk(100, 0, 10, 50)
	second := mk(100, 0, 3000, 0)
	thread := mk(100, 2, 3100, 0)
	unknownEnd := mk(200, 0, 10, 0)
	ix := newProcIndex([]*Proc{second, thread, first, unknownEnd})
	for _, tc := range []struct {
		name string
		got  *Proc
		want *Proc
	}{
		{"named: before any start, the first", ix.named(100, 0, 5, nil), first},
		{"named: the last started", ix.named(100, 0, 3500, nil), second},
		{"named: one that ended since", ix.named(100, 0, 2000, nil), first},
		{"named: not self", ix.named(100, 0, 3500, second), first},
		{"running: during its life", ix.running(100, 0, 30, nil), first},
		{"running: within the slack after its end", ix.running(100, 0, 50+startSlack, nil), first},
		{"running: ended before (reused pid)", ix.running(100, 0, 2000, nil), nil},
		{"running: before any start", ix.running(100, 0, 5, nil), nil},
		{"running: the later one", ix.running(100, 0, 3500, nil), second},
		{"running: end unknown", ix.running(200, 0, 99999, nil), unknownEnd},
		{"running: no start time", ix.running(100, 0, 0, nil), first},
		{"main: the main thread", ix.main(100, 3500, nil), second},
		{"main: none running", ix.main(100, 2000, nil), nil},
		{"main: not self", ix.main(100, 3500, second), thread},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
	if got := ix.resolve("100-0", 3500, nil); got != second.Key() {
		t.Errorf("resolve: %q", got)
	}
	for _, ref := range []string{"", "x", "100", "100-x", "999-0"} {
		if got := ix.resolve(ref, 3500, nil); got != ref {
			t.Errorf("resolve(%q) = %q", ref, got)
		}
	}
}

// A process whose parent pid an ended traced process had is not that process's child:
// an untraced program (sh) started later by another test got the pid, and what its
// children read belongs to the traced ancestor they name. The parent pid is trusted
// only while its holder ran and agrees with that ancestor.
func TestAttributePidReuse(t *testing.T) {
	type tc struct {
		name string
		// the process that had pid 100 before: its language, end and traced ancestor
		lang string
		end  int64
		anc  string
		// the helper's traced ancestor
		helperAnc string
		// where the helper's read must go: a unit, or "global"
		want    string
		notWant string
	}
	for _, c := range []tc{
		{name: "ended long before", end: 50, anc: "10-0", helperAnc: "11-0", want: "a.test.ts", notWant: "b.test.ts"},
		{name: "end unknown, other ancestor named", anc: "10-0", helperAnc: "11-0", want: "a.test.ts", notWant: "b.test.ts"},
		{name: "ended within the slack, other ancestor named", end: 1500, anc: "10-0", helperAnc: "11-0", want: "a.test.ts", notWant: "b.test.ts"},
		{name: "running and named", anc: "10-0", helperAnc: "100-0", want: "b.test.ts", notWant: "a.test.ts"},
		{name: "running, no ancestor named (must keep)", anc: "10-0", want: "b.test.ts", notWant: "a.test.ts"},
		{name: "ended, no ancestor named (must keep)", end: 50, anc: "10-0", want: "global", notWant: "b.test.ts"},
		{name: "ancestor named is not traced (must keep)", anc: "10-0", helperAnc: "77-0", want: "b.test.ts", notWant: "a.test.ts"},
		// Bun.spawn without env: the child names Bun's own ancestor.
		{name: "bun parent passing on its environment", lang: "bun", anc: "10-0", helperAnc: "10-0", want: "b.test.ts", notWant: "a.test.ts"},
		{name: "node parent with the same ancestor", lang: "node", anc: "10-0", helperAnc: "10-0", want: "global", notWant: "b.test.ts"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runner := mkProc(10, 1, 0).times(1, 0)
			t1 := mkProc(11, 10, 0).code("a.test.ts").times(300, 0)
			t1.p.Anc = "10-0"
			t2 := mkProc(100, 10, 0).code("b.test.ts").times(10, c.end)
			t2.p.Anc, t2.p.Lang = c.anc, c.lang
			helper := mkProc(300, 100, 0).code("tool.js").read("x.txt").times(2000, 2100)
			helper.p.Anc = c.helperAnc
			a := attr(Options{}, runner, t1, t2, helper)
			has := func(where string) bool {
				if where == "global" {
					return a.Global["x.txt"]
				}
				return slices.Contains(unitFiles(a, where), "x.txt")
			}
			if !has(c.want) {
				t.Errorf("x.txt not in %s: a %v, b %v, global %v", c.want, unitFiles(a, "a.test.ts"), unitFiles(a, "b.test.ts"), keys(a.Global))
			}
			if has(c.notWant) {
				t.Errorf("x.txt in %s", c.notWant)
			}
		})
	}
}

// A child started by a worker thread has the process's pid as its parent pid but names
// the thread as its traced ancestor: it belongs to the thread (the test running there),
// not to the main thread.
func TestAttributeChildOfWorkerThread(t *testing.T) {
	child := mkProc(40, 30, 0).code("tool.js").read("x.txt").times(500, 600)
	child.p.Anc = "30-1"
	a := attr(Options{},
		mkProc(30, 1, 0).code("runner.js").times(1, 0),
		mkProc(30, 1, 1).code("c.test.ts").times(100, 0),
		child,
	)
	if got := unitFiles(a, "c.test.ts"); !slices.Contains(got, "x.txt") {
		t.Errorf("c %v, global %v", got, keys(a.Global))
	}
}

// A process with the pid of one that ran an exec event is the program it exec'd only
// when it started while that process ran and names it (or its ancestor) as its traced
// ancestor; another one that got the pid later does not verify the command, which then
// stays opaque.
func TestCollectExecReusedPid(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "next.py")
	abs := func(r string) string { return filepath.Join(root, r) }
	for _, tc := range []struct {
		name   string
		exit   bool // the exec'ing process recorded its end (at 130)
		anc    string
		start  int64
		opaque bool
	}{
		{name: "exec'd program", anc: "80-0", start: 120},
		{name: "exec'd by a forked child (its parent's ancestor)", anc: "5-0", start: 120},
		{name: "later process, other ancestor (must keep)", anc: "99-0", start: 120, opaque: true},
		{name: "same ancestor, started after the end (must keep)", exit: true, anc: "5-0", start: 130 + startSlack + 1, opaque: true},
		{name: "same ancestor, started before (must keep)", anc: "5-0", start: 90, opaque: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			td := t.TempDir()
			evs := []ev{
				{"k": "start", "pid": 80, "ppid": 1, "anc": "5-0", "t": 100, "lang": "python"},
				{"k": "exec", "args": []string{"python3", "next.py"}, "cwd": root, "t": 110},
			}
			if tc.exit {
				evs = append(evs, ev{"k": "exit", "code": 0, "t": 130})
			}
			streamAs(t, td, "proc-80-0-a.jsonl", evs...)
			streamAs(t, td, "proc-80-0-b.jsonl",
				ev{"k": "start", "pid": 80, "ppid": 1, "anc": tc.anc, "argv": []string{abs("next.py")}, "t": tc.start, "lang": "python"})
			procs, err := Collect(td, root)
			if err != nil {
				t.Fatal(err)
			}
			var p *Proc
			for _, q := range procs {
				if q.Start == 100 {
					p = q
				}
			}
			if p == nil {
				t.Fatalf("procs %v", procs)
			}
			if got := keys(p.Reads); slices.Contains(got, "./**") != tc.opaque {
				t.Errorf("reads %v, want opaque %v", got, tc.opaque)
			}
		})
	}
}
