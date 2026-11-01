package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// fakeTree is a working tree for the watch loop: every file is dirty, and its version
// stands for its metadata. Hooks run on scans so tests can change files at exact
// points of the loop instead of racing it.
type fakeTree struct {
	mu     sync.Mutex
	files  map[string]int64
	scans  int
	onScan []func(f *fakeTree) // consumed one per scan, once armed
	armed  bool
	fail   int // scans that fail from now on
}

func (f *fakeTree) set(path string, version int64) {
	if version < 0 {
		delete(f.files, path)
		return
	}
	f.files[path] = version
}

func (f *fakeTree) touch(path string) { f.files[path]++ }

func (f *fakeTree) do(fn func(*fakeTree)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// arm makes the next scans apply steps, one per scan.
func (f *fakeTree) arm(steps ...func(*fakeTree)) {
	f.do(func(f *fakeTree) { f.onScan, f.armed = steps, true })
}

func (f *fakeTree) scan(also map[string]gitx.FileSig) (*gitx.TreeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans++
	if f.fail > 0 {
		f.fail--
		return nil, errors.New("git status: index file corrupt")
	}
	if f.armed && len(f.onScan) > 0 {
		f.onScan[0](f)
		f.onScan = f.onScan[1:]
	}
	ts := &gitx.TreeState{Files: map[string]gitx.FileSig{}}
	for p, v := range f.files {
		ts.Files[p] = gitx.FileSig{Exists: true, Mtime: v, Dirty: true}
	}
	for p := range also {
		if _, ok := ts.Files[p]; !ok {
			ts.Files[p] = gitx.FileSig{} // gone
		}
	}
	return ts, nil
}

type loopHarness struct {
	t      *testing.T
	tree   *fakeTree
	loop   *watchLoop
	keys   chan rune
	mu     sync.Mutex
	cycles []cycleReq
	logs   []string
	// during runs inside a cycle (after it is recorded); block holds the cycle.
	during func(n int, req cycleReq)
	cancel context.CancelFunc
	done   chan struct{}
}

func newLoopHarness(t *testing.T) *loopHarness {
	h := &loopHarness{t: t, tree: &fakeTree{files: map[string]int64{"src/a.go": 1}}, keys: make(chan rune, 4)}
	h.loop = &watchLoop{
		scan:     h.tree.scan,
		changes:  (&gitx.Repo{}).Changes,
		ignored:  func(p string) bool { return strings.HasPrefix(p, "ignored/") },
		interval: 5 * time.Millisecond,
		quiet:    60 * time.Millisecond,
		maxWait:  10 * time.Second,
		keys:     h.keys,
		logf: func(format string, args ...any) {
			h.mu.Lock()
			h.logs = append(h.logs, fmt.Sprintf(format, args...))
			h.mu.Unlock()
		},
	}
	h.loop.cycle = func(ctx context.Context, req cycleReq) {
		h.mu.Lock()
		h.cycles = append(h.cycles, req)
		n := len(h.cycles)
		during := h.during
		h.mu.Unlock()
		if during != nil {
			during(n, req)
		}
	}
	return h
}

func (h *loopHarness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() { h.loop.run(ctx); close(h.done) }()
	h.t.Cleanup(func() { h.stop() })
}

func (h *loopHarness) stop() {
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
		<-h.done
	}
}

// waitFor polls until cond holds (or fails after a while).
func (h *loopHarness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ok := cond()
		h.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.t.Fatalf("timed out waiting for %s; cycles %+v; logs %q", what, h.cycles, h.logs)
}

func (h *loopHarness) cycleCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.cycles)
}

func (h *loopHarness) cycle(i int) cycleReq {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cycles[i]
}

// settleIdle lets the loop poll for a while (to show nothing more happens).
func (h *loopHarness) settleIdle() { time.Sleep(4 * h.loop.quiet) }

func TestWatchLoopDebouncesBurst(t *testing.T) {
	h := newLoopHarness(t)
	h.start()
	h.waitFor("initial cycle", func() bool { return len(h.cycles) == 1 })
	if got := h.cycle(0).kind; got != cycleInitial {
		t.Fatalf("first cycle %q, want %q", got, cycleInitial)
	}
	// A burst: something changes on every scan for a while (a formatter after a save,
	// a checkout), then the tree is quiet. It must be one cycle with every file.
	var steps []func(*fakeTree)
	for i := range 12 {
		steps = append(steps, func(f *fakeTree) {
			f.touch("src/a.go")
			if i == 3 {
				f.set("src/b.go", 1)
			}
			if i == 5 {
				f.set("ignored/cache.bin", 1)
			}
		})
	}
	h.tree.arm(steps...)
	h.waitFor("change cycle", func() bool { return len(h.cycles) == 2 })
	h.settleIdle()
	if n := h.cycleCount(); n != 2 {
		t.Fatalf("%d cycles, want 2 (one per burst)", n)
	}
	got := h.cycle(1)
	if got.kind != cycleChange || !slices.Equal(got.changed, []string{"src/a.go", "src/b.go"}) {
		t.Errorf("cycle %+v, want change of [src/a.go src/b.go]", got)
	}
}

func TestWatchLoopIgnoresNetZeroBurstAndIgnoredFiles(t *testing.T) {
	h := newLoopHarness(t)
	h.start()
	h.waitFor("initial cycle", func() bool { return len(h.cycles) == 1 })
	// An editor's temp file comes and goes; an ignored file changes.
	h.tree.arm(
		func(f *fakeTree) { f.set("src/.a.go.tmp", 1) },
		func(f *fakeTree) { f.set("src/.a.go.tmp", -1) },
		func(f *fakeTree) { f.set("ignored/out.txt", 1) },
		func(f *fakeTree) { f.touch("ignored/out.txt") },
	)
	h.settleIdle()
	h.settleIdle()
	if n := h.cycleCount(); n != 1 {
		t.Fatalf("%d cycles, want only the initial one: %+v", n, h.cycles)
	}
}

func TestWatchLoopQueuesOneFollowUp(t *testing.T) {
	h := newLoopHarness(t)
	h.during = func(n int, req cycleReq) {
		if n == 2 {
			// Saves while the tests run: one follow-up cycle with all of them.
			h.tree.do(func(f *fakeTree) { f.touch("src/a.go") })
			time.Sleep(10 * time.Millisecond)
			h.tree.do(func(f *fakeTree) { f.set("src/c.go", 1) })
			h.tree.do(func(f *fakeTree) { f.touch("src/a.go") })
		}
	}
	h.start()
	h.waitFor("initial cycle", func() bool { return len(h.cycles) == 1 })
	h.tree.arm(func(f *fakeTree) { f.set("src/b.go", 1) })
	h.waitFor("follow-up cycle", func() bool { return len(h.cycles) == 3 })
	h.settleIdle()
	if n := h.cycleCount(); n != 3 {
		t.Fatalf("%d cycles, want 3", n)
	}
	if got := h.cycle(1).changed; !slices.Equal(got, []string{"src/b.go"}) {
		t.Errorf("second cycle changed %q", got)
	}
	if got := h.cycle(2).changed; !slices.Equal(got, []string{"src/a.go", "src/c.go"}) {
		t.Errorf("follow-up changed %q, want [src/a.go src/c.go]", got)
	}
}

func TestWatchLoopStopsRetriggeringOnTestOutput(t *testing.T) {
	h := newLoopHarness(t)
	// Every run rewrites a file nobody ignores (a report the tests write).
	h.during = func(n int, req cycleReq) {
		h.tree.do(func(f *fakeTree) { f.touch("report.txt") })
	}
	h.start()
	h.waitFor("runaway guard", func() bool {
		for _, l := range h.logs {
			if strings.Contains(l, "taking it for test output") {
				return true
			}
		}
		return false
	})
	h.settleIdle()
	if n := h.cycleCount(); n != repeatLimit {
		t.Fatalf("%d cycles, want %d", n, repeatLimit)
	}
	// Editing a real file still runs (and the report written by that run does not
	// start another one).
	h.tree.arm(func(f *fakeTree) { f.touch("src/a.go") })
	h.waitFor("cycle after edit", func() bool { return len(h.cycles) == repeatLimit+1 })
	h.settleIdle()
	if n := h.cycleCount(); n != repeatLimit+1 {
		t.Fatalf("%d cycles, want %d", n, repeatLimit+1)
	}
}

func TestWatchLoopKeys(t *testing.T) {
	h := newLoopHarness(t)
	release := make(chan struct{})
	h.during = func(n int, req cycleReq) {
		if n == 2 {
			<-release
		}
	}
	h.start()
	h.waitFor("initial cycle", func() bool { return len(h.cycles) == 1 })
	h.keys <- 'a'
	h.waitFor("run-all cycle", func() bool { return len(h.cycles) == 2 })
	// Keys during a run queue one cycle; the last one wins. Unknown keys do nothing.
	h.keys <- 'f'
	h.keys <- 'x'
	h.keys <- '\n'
	time.Sleep(20 * time.Millisecond)
	close(release)
	h.waitFor("queued cycle", func() bool { return len(h.cycles) == 3 })
	h.settleIdle()
	if got := []string{h.cycle(1).kind, h.cycle(2).kind}; !slices.Equal(got, []string{cycleAll, cycleRerun}) {
		t.Errorf("cycles %q", got)
	}
	if n := h.cycleCount(); n != 3 {
		t.Fatalf("%d cycles, want 3", n)
	}
	h.keys <- 'q'
	select {
	case <-h.done:
		h.cancel = nil
	case <-time.After(5 * time.Second):
		t.Fatal("q did not stop the loop")
	}
}

func TestWatchLoopQuitStopsRun(t *testing.T) {
	h := newLoopHarness(t)
	stopped := make(chan struct{})
	h.loop.cycle = func(ctx context.Context, req cycleReq) {
		<-ctx.Done() // a long test run
		close(stopped)
	}
	h.start()
	h.keys <- 'q'
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("q did not cancel the running cycle")
	}
	select {
	case <-h.done:
		h.cancel = nil
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not return")
	}
}

func TestWatchLoopRetriesFailingScans(t *testing.T) {
	h := newLoopHarness(t)
	h.loop.interval = time.Millisecond // observe waits at least a second between retries
	h.start()
	h.waitFor("initial cycle", func() bool { return len(h.cycles) == 1 })
	h.tree.do(func(f *fakeTree) {
		f.fail = 2
		f.touch("src/a.go")
	})
	h.waitFor("change cycle", func() bool { return len(h.cycles) == 2 })
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, "index file corrupt (retrying)") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("logged the scan error %d times, want once: %q", n, h.logs)
	}
}

func TestReadKeys(t *testing.T) {
	collect := func(in string, perKey bool) string {
		var b strings.Builder
		for k := range readKeys(strings.NewReader(in), perKey) {
			b.WriteRune(k)
		}
		return b.String()
	}
	if got := collect("af\r\nq", true); got != "af\n\nq" {
		t.Errorf("per key: %q", got)
	}
	if got := collect("a\n\n  f  \nquit\n", false); got != "a\nfq" {
		t.Errorf("line mode: %q", got)
	}
}

func TestNarrowAndRanUnits(t *testing.T) {
	res := &selector.Result{Mode: "all", Suites: []*selector.Suite{{
		Name: "go", All: true,
		Tests: []*selector.Test{{ID: "a", Selected: true, Reason: "all"}, {ID: "b", Selected: true, Reason: "all"}, {ID: "c"}},
	}}}
	if got := ranUnits(res)["go"]; !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("full run units %q", got)
	}
	narrow(res, func(_ *selector.Suite, t *selector.Test) bool { return t.ID != "b" }, "failed", "rerun")
	if res.Mode == "all" || res.Suites[0].All {
		t.Error("narrow must leave a partial selection")
	}
	if got := ranUnits(res)["go"]; !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("narrowed units %q", got)
	}
	if r := res.Suites[0].Tests[2].Reason; r != "failed" {
		t.Errorf("reason %q", r)
	}
}

func TestWatchIgnore(t *testing.T) {
	p := &project{
		repo:      &gitx.Repo{Root: "/repo"},
		statePath: "/repo/.cache/smates.json.gz",
		suites:    []*runner.Suite{{Suite: config.Suite{Root: "web", Junit: "reports/junit.xml"}}},
		state:     state.New(),
	}
	ig := watchIgnore(p)
	for path, want := range map[string]bool{
		"src/a.go": false, "README.md": false, "data/x.json": false,
		".cache/smates.json.gz": true, ".cache/.state-123": true, ".smates/state.json.gz": true,
		"web/node_modules/x/index.js": true, "pkg/__pycache__/m.cpython-312.pyc": true,
		"web/reports/junit.xml": true, "src/.a.go.swp": true, "src/a.go~": true, "web/coverage/lcov.info": true,
		// Coverage reports, but not source code in a package named coverage.
		"coverage/index.html": true, "web/coverage/index.html": true, "pkg/a/coverage/lcov-report/index.html": true,
		"pkg/a/lcov.info": true, "pkg/a/coverage/coverage-final.json": true,
		"internal/coverage/profile.go": false, "src/coverage/merge.ts": false, "web/src/coverage/index.ts": false,
	} {
		if got := ig(path); got != want {
			t.Errorf("ignored(%q) = %v, want %v", path, got, want)
		}
	}
}
