package trace

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

// A stream that lost events (the shim said so, or a line cannot be read) marks its
// process: whatever was lost may have been a read, a context switch or a child.
func TestCollectLostEvents(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "a.txt", "b.txt")
	read := func(r string) string {
		data, _ := json.Marshal(ev{"k": "read", "p": filepath.Join(root, r)})
		return string(data) + "\n"
	}
	start := `{"k":"start","pid":1,"ppid":0,"t":1,"lang":"node"}` + "\n"
	for _, tc := range []struct {
		name   string
		stream string
		lost   bool
	}{
		{"clean", start + read("a.txt") + read("b.txt"), false},
		{"blank lines", start + "\n" + read("a.txt") + "  \r\n" + read("b.txt"), false},
		{"lost marker", start + read("a.txt") + `{"k":"lost","n":2}` + "\n" + read("b.txt"), true},
		// The shims end a line a failed write cut short before the next one.
		{"line cut short by a failed write", start + read("a.txt")[:20] + "\n" + read("b.txt"), true},
		{"line cut short, run into the next", start + read("a.txt")[:20] + read("a.txt") + read("b.txt"), true},
		{"garbled line", start + "\x00\x00garbage\n" + read("b.txt"), true},
		{"last line cut short", start + read("b.txt") + read("a.txt")[:25], true},
		{"overlong line", start + `{"k":"read","p":"` + strings.Repeat("x", maxLine) + `"}` + "\n" + read("b.txt"), true},
		// A whole last line without its newline is a whole event.
		{"last line without newline", start + strings.TrimSuffix(read("b.txt"), "\n"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			td := t.TempDir()
			testutil.Write(t, td, "proc-1-0-x.jsonl", tc.stream)
			procs, err := Collect(td, root)
			if err != nil {
				t.Fatal(err)
			}
			if len(procs) != 1 {
				t.Fatalf("procs %v", procs)
			}
			if procs[0].Lost != tc.lost {
				t.Errorf("lost %v, want %v", procs[0].Lost, tc.lost)
			}
			if !procs[0].Reads["b.txt"] {
				t.Errorf("the rest of the stream is not read: %v", keys(procs[0].Reads))
			}
		})
	}
}

// A process that lost events depends on the whole repository, for every unit it serves:
// all units of a process with contexts (the lost event may have been a context switch),
// all units of the process a helper belongs to, every unit when it belongs to none (or
// is the runner).
func TestAttributeLostProcess(t *testing.T) {
	lost := func(b *pb) *pb { b.p.Lost = true; return b }
	ctxProc := func() *pb {
		b := mkProc(20, 10, 0).times(100, 900).
			ctx(110, "test:test_a.py", "test_a.py").
			ctx(200, "test:test_b.py", "test_b.py")
		// test_c.py only reported a result: its context switch was lost.
		b.p.Results["test_c.py"] = Result{Status: state.Pass}
		return b
	}
	helper := func(ppid int) *pb {
		h := mkProc(30, ppid, 0).read("h.txt").times(150, 160)
		h.p.Anc = "20-0"
		return h
	}
	for _, tc := range []struct {
		name   string
		procs  func() []*pb
		want   []string // units (or "global") depending on the whole repository
		noLost []string // units that must not
	}{
		{
			name:   "nothing lost",
			procs:  func() []*pb { return []*pb{mkProc(10, 1, 0), ctxProc(), helper(20)} },
			noLost: []string{"test_a.py", "test_b.py", "test_c.py", "global"},
		},
		{
			name:  "process with contexts",
			procs: func() []*pb { return []*pb{mkProc(10, 1, 0), lost(ctxProc())} },
			want:  []string{"test_a.py", "test_b.py", "test_c.py"},
		},
		{
			name:  "helper of a process with contexts",
			procs: func() []*pb { return []*pb{mkProc(10, 1, 0), ctxProc(), lost(helper(20))} },
			want:  []string{"test_a.py", "test_b.py", "test_c.py"},
		},
		{
			name: "process of one unit",
			procs: func() []*pb {
				return []*pb{mkProc(10, 1, 0), lost(mkProc(11, 10, 0).code("a.test.ts")), mkProc(12, 10, 0).code("b.test.ts")}
			},
			want:   []string{"a.test.ts"},
			noLost: []string{"b.test.ts", "global"},
		},
		{
			name: "helper of a unit's process",
			procs: func() []*pb {
				h := lost(mkProc(30, 11, 0).read("h.txt"))
				return []*pb{mkProc(10, 1, 0), mkProc(11, 10, 0).code("a.test.ts"), h}
			},
			want: []string{"a.test.ts"},
		},
		{
			name: "helper of no unit that ran no code",
			procs: func() []*pb {
				return []*pb{mkProc(10, 1, 0), mkProc(11, 10, 0).code("a.test.ts"), lost(mkProc(40, 1, 0).read("h.txt"))}
			},
			want: []string{"global"},
		},
		{
			name: "the runner",
			procs: func() []*pb {
				return []*pb{lost(mkProc(10, 1, 0).code("vitest.config.ts")), mkProc(11, 10, 0).code("a.test.ts")}
			},
			want: []string{"global"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := attr(Options{}, tc.procs()...)
			has := func(where string) bool {
				if where == "global" {
					return a.Global[lostKey]
				}
				return slices.Contains(unitFiles(a, where), lostKey)
			}
			for _, w := range tc.want {
				if !has(w) {
					t.Errorf("%s does not depend on the whole repository: units %v, global %v", w, a.Units, keys(a.Global))
				}
			}
			for _, w := range tc.noLost {
				if has(w) {
					t.Errorf("%s depends on the whole repository", w)
				}
			}
		})
	}
}

// A results file that lost a line records no pass; its failures stand, and other
// results files are not affected.
func TestResultsLostLines(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "a.test.ts", "b.test.ts", "c.test.ts")
	line := func(f, st string) string {
		data, _ := json.Marshal(ev{"k": "result", "id": filepath.Join(root, f), "status": st})
		return string(data) + "\n"
	}
	other := line("c.test.ts", "pass")
	for _, tc := range []struct {
		name  string
		file  string
		wantA string // a.test.ts passed in the file
		wantB string // b.test.ts failed in it
	}{
		{name: "clean", file: line("a.test.ts", "pass") + line("b.test.ts", "fail"), wantA: state.Pass, wantB: state.Fail},
		{name: "blank line", file: line("a.test.ts", "pass") + "\n" + line("b.test.ts", "fail"), wantA: state.Pass, wantB: state.Fail},
		{name: "lost marker", file: line("a.test.ts", "pass") + line("b.test.ts", "fail") + `{"k":"lost","n":1}` + "\n", wantA: state.Unknown, wantB: state.Fail},
		{name: "line cut short", file: line("a.test.ts", "pass") + line("x", "unknown")[:30] + "\n" + line("b.test.ts", "fail"), wantA: state.Unknown, wantB: state.Fail},
		{name: "last line cut short", file: line("a.test.ts", "pass") + line("b.test.ts", "fail") + line("x", "unknown")[:30], wantA: state.Unknown, wantB: state.Fail},
		{name: "overlong line", file: strings.Repeat("y", maxLine+1) + "\n" + line("a.test.ts", "pass") + line("b.test.ts", "fail"), wantA: state.Unknown, wantB: state.Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			td := t.TempDir()
			testutil.Write(t, td, "results-vitest-1.jsonl", tc.file)
			testutil.Write(t, td, "results-vitest-2.jsonl", other)
			got := Results(td, root)
			if got["a.test.ts"].Status != tc.wantA || got["b.test.ts"].Status != tc.wantB {
				t.Errorf("got %v, want a %s, b %s", got, tc.wantA, tc.wantB)
			}
			if got["c.test.ts"].Status != state.Pass {
				t.Errorf("another results file is affected: %v", got)
			}
		})
	}
}
