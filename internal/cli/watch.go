package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// runHooks let `smates watch` drive a `smates run` cycle (nil for `smates run`).
type runHooks struct {
	// orphans, when non-nil, limits the orphan policy to these files.
	orphans map[string]bool
	// selected is called with the selection before it is printed and run; it may
	// change it. The project's state is updated in place by the run.
	selected func(*project, *selection)
	// started is set by the run to the units it started (suite -> units); nil when it
	// started none (a dry run, an error before running).
	started map[string][]string
}

func (h *runHooks) scope() map[string]bool {
	if h == nil {
		return nil
	}
	return h.orphans
}

func (h *runHooks) onSelected(p *project, sel *selection) {
	if h != nil && h.selected != nil {
		h.selected(p, sel)
	}
}

func (h *runHooks) onRan(started map[string][]string) {
	if h != nil {
		h.started = started
	}
}

type watchFlags struct {
	commonFlags
	interval time.Duration
	debounce time.Duration
	agent    bool
	clear    bool
	failFast bool
}

// Kinds of watch cycles.
const (
	cycleInitial = "start"
	cycleChange  = "change"
	cycleAll     = "all"
	cycleFailed  = "failed"
	cycleRerun   = "rerun"
)

type cycleReq struct {
	kind    string
	changed []string
}

const watchKeysHelp = "a: run all · f: rerun failed · enter: rerun · q: quit"

func cmdWatch(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f watchFlags
	f.register(fs)
	fs.DurationVar(&f.interval, "interval", 500*time.Millisecond, "how often to poll the working tree for changes")
	fs.DurationVar(&f.debounce, "debounce", 300*time.Millisecond, "wait until no file changed for this long before running")
	fs.BoolVar(&f.agent, "agent", false, "ask the agent about unattributed changes (default: apply the orphan policy without it)")
	fs.BoolVar(&f.clear, "clear", false, "clear the screen before each run")
	fs.BoolVar(&f.failFast, "fail-fast", false, "in each run, start no further test batch after one fails")
	if err := fs.Parse(args); err != nil {
		return 2, usageError{err}
	}
	extra := fs.Args()
	if f.interval <= 0 || f.debounce < 0 {
		return 2, usageError{errors.New("--interval must be positive and --debounce not negative")}
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// Stopping a run must stop everything the runner started.
	ctx = runner.WithProcessGroup(ctx, 5*time.Second)

	p, err := load(&f.commonFlags, stderr)
	if err != nil {
		return 1, err
	}
	if len(p.suites) == 0 {
		return 1, configError{fmt.Errorf("no test suites configured or detected; run `smates init` or write smates.yml")}
	}
	repo := p.repo
	ignored := watchIgnore(p)

	var keys <-chan rune
	if stdin != nil {
		if restore, perKey, ok := keyMode(stdin); ok {
			defer restore()
			keys = readKeys(stdin, perKey)
		}
	}

	c := &watchCycles{f: &f, extra: extra, stdout: stdout, stderr: stderr, hasKeys: keys != nil, logf: p.logf}
	p.logf("watching %s for changes (%s); orphan policy: %s%s", repo.Root, suiteNames(p.suites), p.cfg.Orphans, agentNote(p, f.agent))
	w := &watchLoop{
		scan:     repo.ScanTree,
		changes:  repo.Changes,
		ignored:  ignored,
		interval: f.interval,
		quiet:    f.debounce,
		maxWait:  max(10*f.debounce, 5*time.Second),
		keys:     keys,
		cycle:    c.run,
		logf:     p.logf,
	}
	w.run(ctx)
	p.logf("stopped watching")
	return 0, nil
}

func agentNote(p *project, agent bool) string {
	if p.cfg.Orphans != config.OrphansAgent {
		return ""
	}
	if agent {
		return " (agent enabled)"
	}
	return " (without the agent: affected suites run in full; --agent to ask it)"
}

// watchIgnore decides which changes never start a cycle: smates' own state, dependency
// trees and caches, runner reports, and editor temp files. Everything else does, even
// files the orphan check ignores (a test may read them; selection decides).
func watchIgnore(p *project) func(string) bool {
	globs := []string{
		"**/*.swp", "**/*.swx", "**/*.swo", "**/*~", "**/.#*", "**/#*#", "**/4913", "**/.DS_Store",
		"**/*___jb_tmp___", "**/*___jb_old___", "**/.vitest-cache/**", "**/.vite/**",
		"**/.nyc_output/**", "**/*.pyc",
		// Coverage reports: the report directory of the repository or of a suite (a
		// coverage/ elsewhere may be source code, Go's internal/coverage), and the
		// reports' own files wherever a tool writes them.
		"coverage/**", "**/lcov.info", "**/lcov-report/**", "**/coverage-final.json", "**/coverage-summary.json",
		"**/clover.xml", "**/cobertura-coverage.xml",
	}
	statePaths := []string{p.statePath}
	if p.store != nil && p.store.snapPath != "" {
		statePaths = append(statePaths, p.store.snapPath) // shared mode: this worktree's snapshot
	}
	for _, sp := range statePaths {
		if rel, err := filepath.Rel(p.repo.Root, sp); err == nil && !strings.HasPrefix(rel, "..") {
			rel = filepath.ToSlash(rel)
			globs = append(globs, rel, path.Join(path.Dir(rel), ".state-*")) // and state.Save's temp file
		}
	}
	for _, s := range p.suites {
		if s.Junit != "" {
			globs = append(globs, filepath.ToSlash(filepath.Join(s.Root, s.Junit)))
		}
		if s.Root != "" && s.Root != "." {
			globs = append(globs, path.Join(s.Root, "coverage/**"))
		}
	}
	return func(p string) bool {
		return pathutil.IsVendored(p) || pathutil.Match(globs, p)
	}
}

// readKeys delivers key presses ('\n' for Enter). In line mode every line counts as its
// first character. The channel is closed at end of input.
func readKeys(r io.Reader, perKey bool) <-chan rune {
	ch := make(chan rune, 8)
	go func() {
		defer close(ch)
		br := bufio.NewReader(r)
		for {
			if perKey {
				b, err := br.ReadByte()
				if err != nil {
					return
				}
				if b == '\r' {
					b = '\n'
				}
				ch <- rune(b)
				continue
			}
			line, err := br.ReadString('\n')
			if err != nil && line == "" {
				return
			}
			if line = strings.TrimSpace(line); line == "" {
				ch <- '\n'
			} else {
				ch <- rune(line[0])
			}
		}
	}()
	return ch
}

// keyCycle maps a key to the cycle it requests (nil: none; quit for q). Upper-case
// letters are not keys: arrow keys send ESC [ A..D.
func keyCycle(k rune) (req *cycleReq, quit bool) {
	switch k {
	case 'q':
		return nil, true
	case 'a':
		return &cycleReq{kind: cycleAll}, false
	case 'f':
		return &cycleReq{kind: cycleFailed}, false
	case '\n', 'r':
		return &cycleReq{kind: cycleRerun}, false
	}
	return nil, false
}

// watchLoop polls the working tree and runs cycles. Changes are settled (debounced)
// before a cycle starts; changes made while a cycle runs start exactly one follow-up
// cycle once it ends. Key presses during a run queue one cycle (the last one wins).
type watchLoop struct {
	scan     func(also map[string]gitx.FileSig) (*gitx.TreeState, error)
	changes  func(a, b *gitx.TreeState) ([]string, error)
	ignored  func(string) bool
	interval time.Duration
	quiet    time.Duration
	maxWait  time.Duration
	keys     <-chan rune
	cycle    func(context.Context, cycleReq)
	logf     func(string, ...any)

	scanCost time.Duration // duration of the last scan, to back off on huge trees
}

// repeatLimit is how many runs in a row may change the same files before those changes
// are taken to be the tests' own output and no longer start runs by themselves.
const repeatLimit = 3

func (w *watchLoop) run(ctx context.Context) {
	base, _, _, ok := w.observe(ctx, nil, nil)
	if !ok {
		return
	}
	req := &cycleReq{kind: cycleInitial}
	var lastDuring []string
	repeats := 0
	for {
		if req != nil {
			next, quit := w.execute(ctx, *req)
			if quit || ctx.Err() != nil {
				return
			}
			req = next
			if req != nil {
				continue
			}
			// Changes made during the run (base is the tree the run started from).
			cur, ch, _, ok := w.observe(ctx, base, nil)
			if !ok {
				return
			}
			if len(ch) > 0 {
				if slices.Equal(ch, lastDuring) {
					repeats++
				} else {
					repeats = 1
				}
				lastDuring = ch
				if repeats >= repeatLimit {
					w.logf("%s changed during each of the last %d runs; taking it for test output and not rerunning (add it to .gitignore; edit it again to rerun)", strings.Join(limit(ch, 5), ", "), repeats)
					base = cur
					continue
				}
				w.logf("changed during the run: %s", strings.Join(limit(ch, 8), ", "))
				req, base = w.settle(ctx, base, cur, ch)
				continue
			}
			lastDuring, repeats = nil, 0
		}
		t := time.NewTimer(max(w.interval, 2*w.scanCost))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case k, ok := <-w.keys:
			t.Stop()
			if !ok {
				w.keys = nil
				continue
			}
			r, quit := keyCycle(k)
			if quit {
				return
			}
			req = r
		case <-t.C:
			cur, ch, _, ok := w.observe(ctx, base, nil)
			if !ok {
				return
			}
			if len(ch) > 0 {
				req, base = w.settle(ctx, base, cur, ch)
			}
		}
	}
}

// observe scans the tree and returns it with the relevant changes since base and since
// prev (either may be nil). A failing scan (git busy with a rebase, say) is retried; ok
// is false only when ctx is done.
func (w *watchLoop) observe(ctx context.Context, base, prev *gitx.TreeState) (cur *gitx.TreeState, sinceBase, sincePrev []string, ok bool) {
	lastErr := ""
	for {
		var err error
		if cur, err = w.scanFrom(base, prev); err == nil {
			if sinceBase, err = w.relevant(base, cur); err == nil {
				if sincePrev, err = w.relevant(prev, cur); err == nil {
					return cur, sinceBase, sincePrev, true
				}
			}
		}
		if msg := err.Error(); msg != lastErr {
			w.logf("watching: %s (retrying)", msg)
			lastErr = msg
		}
		select {
		case <-ctx.Done():
			return nil, nil, nil, false
		case <-time.After(max(w.interval, time.Second)):
		}
	}
}

// scanFrom scans with the dirty files of the given states stat'ed too.
func (w *watchLoop) scanFrom(states ...*gitx.TreeState) (*gitx.TreeState, error) {
	also := map[string]gitx.FileSig{}
	for _, s := range states {
		if s != nil {
			for p, sig := range s.Dirty() {
				if !also[p].Racy {
					also[p] = sig // a racy signature makes the scan hash the file
				}
			}
		}
	}
	start := time.Now()
	ts, err := w.scan(also)
	w.scanCost = time.Since(start)
	return ts, err
}

// relevant lists the changes from a to b that are not ignored (none when a is nil).
func (w *watchLoop) relevant(a, b *gitx.TreeState) ([]string, error) {
	if a == nil {
		return nil, nil
	}
	all, err := w.changes(a, b)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range all {
		if !w.ignored(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// settle waits until the tree stops changing (editors' atomic saves, formatters,
// checkouts) and returns the cycle for the net changes since base (nil when a burst
// nets out, like a temp file come and gone), and the tree state it will run against.
func (w *watchLoop) settle(ctx context.Context, base, cur *gitx.TreeState, changed []string) (*cycleReq, *gitx.TreeState) {
	step := max(min(w.interval, w.quiet/3), 10*time.Millisecond)
	start, quietSince := time.Now(), time.Now()
	last := cur
	for time.Since(quietSince) < w.quiet && time.Since(start) < w.maxWait {
		select {
		case <-ctx.Done():
			return nil, last
		case <-time.After(max(step, w.scanCost)):
		}
		next, sinceBase, sinceLast, ok := w.observe(ctx, base, last)
		if !ok {
			return nil, last
		}
		if len(sinceLast) > 0 {
			quietSince = time.Now()
		}
		last, changed = next, sinceBase
	}
	if len(changed) == 0 {
		return nil, last
	}
	return &cycleReq{kind: cycleChange, changed: changed}, last
}

// execute runs one cycle, handling keys meanwhile: q (or ctx) stops it, others queue the
// next cycle.
func (w *watchLoop) execute(ctx context.Context, req cycleReq) (next *cycleReq, quit bool) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.cycle(cctx, req)
	}()
	for {
		select {
		case <-done:
			return next, ctx.Err() != nil
		case k, ok := <-w.keys:
			if !ok {
				w.keys = nil
				continue
			}
			r, q := keyCycle(k)
			if q {
				w.logf("stopping the run")
				cancel()
				<-done
				return nil, true
			}
			if r != nil {
				next = r
				w.logf("queued: %s after this run", r.kind)
			}
		}
	}
}

// watchCycles runs cycles through `smates run`.
type watchCycles struct {
	f       *watchFlags
	extra   []string
	stdout  io.Writer
	stderr  io.Writer
	hasKeys bool
	logf    func(string, ...any)
	// last is what the previous cycle ran (suite -> units), for rerun.
	last map[string][]string
}

func (c *watchCycles) run(ctx context.Context, req cycleReq) {
	if req.kind == cycleRerun && len(c.last) == 0 {
		req.kind = cycleFailed // nothing ran yet: rerun what has not passed
	}
	if c.f.clear {
		fmt.Fprint(c.stderr, "\033[H\033[2J")
	}
	c.header(req)
	f := c.runFlags(req)
	hooks := &runHooks{}
	switch req.kind {
	case cycleChange:
		hooks.orphans = map[string]bool{}
		for _, p := range req.changed {
			hooks.orphans[p] = true
		}
	case cycleFailed, cycleRerun:
		hooks.orphans = map[string]bool{} // the selection is replaced anyway
	}
	var p *project
	var sel *selection
	hooks.selected = func(pp *project, s *selection) {
		p, sel = pp, s
		switch req.kind {
		case cycleFailed:
			narrow(s.res, func(su *selector.Suite, t *selector.Test) bool {
				e := pp.state.Suite(su.Name).Tests[t.ID]
				return e != nil && e.Status != state.Pass
			}, selector.ReasonFailed, "rerun of failed tests")
		case cycleRerun:
			last := c.last
			narrow(s.res, func(su *selector.Suite, t *selector.Test) bool {
				return slices.Contains(last[su.Name], t.ID)
			}, selector.ReasonRerun, "rerun")
		}
	}
	start := time.Now()
	code, err := runCycle(ctx, f, c.stdout, c.stderr, hooks)
	switch {
	case ctx.Err() != nil:
		c.logf("run stopped")
		return
	case err != nil:
		var ue usageError
		if errors.As(err, &ue) {
			c.logf("%v", err)
		} else {
			c.logf("error: %v", err)
		}
	}
	if sel != nil {
		ran := ranUnits(sel.res)
		if len(ran) > 0 || req.kind != cycleChange {
			c.last = ran
		}
		// With --fail-fast, what ran is less than the selection.
		started, notStarted := ran, 0
		if hooks.started != nil {
			started = hooks.started
			notStarted = countUnits(ran) - countUnits(started)
		}
		c.footer(p, started, notStarted, code, err, time.Since(start))
	} else {
		c.footer(nil, nil, 0, code, err, time.Since(start))
	}
}

func (c *watchCycles) header(req cycleReq) {
	ts := time.Now().Format("15:04:05")
	var what string
	switch req.kind {
	case cycleInitial:
		what = "initial run"
	case cycleChange:
		what = fmt.Sprintf("%d changed: %s", len(req.changed), strings.Join(limit(req.changed, 6), ", "))
	case cycleAll:
		what = "running every test"
	case cycleFailed:
		what = "rerunning failed tests"
	case cycleRerun:
		what = "rerunning the last selection"
	}
	fmt.Fprintf(c.stderr, "\n── %s ── %s\n", ts, what)
}

// runFlags are the `smates run` options of a cycle.
func (c *watchCycles) runFlags(req cycleReq) *runFlags {
	return &runFlags{
		commonFlags: c.f.commonFlags, selectOn: true, explain: true, noAgent: !c.f.agent,
		all: req.kind == cycleAll, extra: c.extra, failFast: c.f.failFast,
	}
}

// footer prints the outcome of a cycle from the recorded statuses. notStarted counts
// the selected units --fail-fast did not start.
func (c *watchCycles) footer(p *project, ran map[string][]string, notStarted, code int, err error, took time.Duration) {
	var failed, unknown []string
	passed, skipped := 0, 0
	for suite, units := range ran {
		for _, u := range units {
			st := state.Unknown
			if e := p.state.Suite(suite).Tests[u]; e != nil {
				st = e.Status
			}
			switch st {
			case state.Pass:
				passed++
			case state.Fail:
				failed = append(failed, u)
			case state.Skip:
				skipped++
			default:
				unknown = append(unknown, u)
			}
		}
	}
	slices.Sort(failed)
	slices.Sort(unknown)
	var parts []string
	switch {
	case err != nil:
		parts = append(parts, "✗ error")
	case len(failed) > 0 || code != 0:
		parts = append(parts, fmt.Sprintf("✗ FAIL (exit %d)", code))
	case passed+skipped+len(unknown) == 0:
		parts = append(parts, "· nothing to run")
	default:
		parts = append(parts, "✓ PASS")
	}
	if passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", passed))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (not proven)", skipped))
	}
	if len(failed) > 0 {
		parts = append(parts, fmt.Sprintf("%d failed: %s", len(failed), strings.Join(limit(failed, 6), ", ")))
	}
	if len(unknown) > 0 {
		parts = append(parts, fmt.Sprintf("%d without a result: %s", len(unknown), strings.Join(limit(unknown, 4), ", ")))
	}
	if notStarted > 0 {
		parts = append(parts, fmt.Sprintf("%d not started (--fail-fast)", notStarted))
	}
	parts = append(parts, took.Round(100*time.Millisecond).String())
	fmt.Fprintf(c.stderr, "── %s\n", strings.Join(parts, " · "))
	if c.hasKeys {
		fmt.Fprintf(c.stderr, "   watching for changes (%s)\n", watchKeysHelp)
	} else {
		fmt.Fprintln(c.stderr, "   watching for changes")
	}
}

// ranUnits lists what a selection runs (every test of a full run).
func ranUnits(res *selector.Result) map[string][]string {
	out := map[string][]string{}
	for _, s := range res.Suites {
		var units []string
		if res.Mode == selector.ModeAll {
			for _, t := range s.Tests {
				units = append(units, t.ID)
			}
		} else {
			units = s.Selected()
		}
		if len(units) > 0 {
			out[s.Name] = units
		}
	}
	return out
}

func countUnits(m map[string][]string) int {
	n := 0
	for _, units := range m {
		n += len(units)
	}
	return n
}

// narrow replaces the selection with the tests keep accepts.
func narrow(res *selector.Result, keep func(*selector.Suite, *selector.Test) bool, reason, detail string) {
	res.Mode = selector.ModeState
	for _, s := range res.Suites {
		s.All = false
		for _, t := range s.Tests {
			t.Selected = keep(s, t)
			if t.Selected {
				t.Reason, t.Detail, t.Changed = reason, detail, nil
			} else {
				t.Reason, t.Detail, t.Changed = selector.ReasonUnaffected, "", nil
			}
		}
	}
	res.Orphans = nil
}
