package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/dotneet/smates/internal/hints"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// Output modes of `smates run` (--output, $SMATES_OUTPUT).
const (
	outputFull     = "full"     // stream the runner output (default)
	outputFailures = "failures" // print only the output of failing batches, then a short summary
)

// Bounds of the output printed in failures mode.
const (
	maxBatchHead    = 64 << 10  // bytes kept from the start of a batch's output
	maxBatchTail    = 256 << 10 // bytes kept from its end
	maxOutputLines  = 200       // lines of a failing batch's output printed
	maxOutputHead   = 30        // of which from its start (the rest from its end)
	maxUnitLines    = 80        // lines of one failing test's output printed
	maxSummaryTests = 20        // failing tests listed in the final summary
	maxSummaryFiles = 5         // changed inputs listed per failing test
)

// parseOutputMode returns the output mode of --output (flag) or $SMATES_OUTPUT (env;
// "agent" is an alias of failures). An invalid flag value is a usage error, an
// invalid variable is ignored with a warning.
func parseOutputMode(flagValue string, stderr io.Writer) (string, error) {
	norm := func(v string) string {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case outputFull:
			return outputFull
		case outputFailures, "agent":
			return outputFailures
		}
		return ""
	}
	if flagValue != "" {
		if m := norm(flagValue); m != "" {
			return m, nil
		}
		return "", usageError{fmt.Errorf("invalid --output %q (want full or failures)", flagValue)}
	}
	env := os.Getenv("SMATES_OUTPUT")
	if env == "" {
		return outputFull, nil
	}
	if m := norm(env); m != "" {
		return m, nil
	}
	fmt.Fprintf(stderr, "smates: ignoring SMATES_OUTPUT=%q (want full, failures or agent)\n", env)
	return outputFull, nil
}

// outputBuffer collects the output of one runner batch (stdout and stderr, from
// several goroutines) within bounds: the start and the end of it are kept.
type outputBuffer struct {
	mu      sync.Mutex
	head    []byte
	tail    []byte
	dropped int64 // bytes between head and tail that were not kept
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if room := maxBatchHead - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	b.tail = append(b.tail, p...)
	if len(b.tail) > 2*maxBatchTail {
		cut := len(b.tail) - maxBatchTail
		b.dropped += int64(cut)
		b.tail = append(b.tail[:0:0], b.tail[cut:]...)
	}
	return n, nil
}

// text returns the kept output, the head and the tail separated by a note when bytes
// were dropped in between.
func (b *outputBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	tail := b.tail
	dropped := b.dropped
	if len(tail) > maxBatchTail {
		dropped += int64(len(tail) - maxBatchTail)
		tail = tail[len(tail)-maxBatchTail:]
	}
	if dropped == 0 {
		return string(b.head) + string(tail)
	}
	return string(b.head) + fmt.Sprintf("\n… %d bytes of output omitted …\n", dropped) + string(tail)
}

// truncateLines keeps at most max lines of s: the first head ones and the last ones,
// with a note in between.
func truncateLines(s string, maxLines, head int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n")
	}
	tail := maxLines - head
	omitted := len(lines) - head - tail
	out := append(slices.Clip(lines[:head]), fmt.Sprintf("… %d line(s) omitted (rerun with --output full for everything) …", omitted))
	return strings.Join(append(out, lines[len(lines)-tail:]...), "\n")
}

// printFailedBatch prints the output of a batch that failed: for runners that report
// each failing test's own output (vitest, jest, pytest, go test, JUnit), that output
// only; otherwise the batch's output, truncated.
func (c *cycle) printFailedBatch(sr *selector.Suite, b *batch, buf *outputBuffer, obs map[string]*runner.Observation) {
	w := c.stdout
	var units []string
	perUnit := true
	for u, o := range obs {
		if o.Status != state.Fail {
			continue
		}
		units = append(units, u)
		if o.Inferred || strings.TrimSpace(hints.CleanOutput(o.Failure)) == "" {
			perUnit = false
		}
	}
	slices.Sort(units)
	if perUnit && len(units) > 0 {
		for _, u := range units {
			fmt.Fprintf(w, "── smates: FAIL %s: %s\n", sr.Name, u)
			fmt.Fprintln(w, truncateLines(hints.CleanOutput(obs[u].Failure), maxUnitLines, maxOutputHead))
		}
		return
	}
	what := fmt.Sprintf("%d test unit(s)", len(b.units))
	if b.full {
		what = "every test"
	}
	fmt.Fprintf(w, "── smates: FAIL %s (%s with %s); its output:\n", sr.Name, what, sr.Suite.Runner)
	if text := strings.TrimSpace(buf.text()); text != "" {
		fmt.Fprintln(w, truncateLines(text, maxOutputLines, maxOutputHead))
	}
}

// failedUnit is a failing test in the final summary of failures mode.
type failedUnit struct {
	suite, unit string
	inferred    bool // the status comes from the run's exit code (or the batch failed without per-test status)
}

// failedUnits lists the failing tests of the cycle: the units recorded as failed, or,
// when a failing batch reported none (--no-record, a runner without per-file status),
// the units of that batch.
func (c *cycle) failedUnits() []failedUnit {
	var out []failedUnit
	for _, sr := range c.sel.res.Suites {
		seen := map[string]bool{}
		for _, t := range sr.Tests {
			if c.ran[sr.Name][t.ID] == state.Fail {
				out = append(out, failedUnit{suite: sr.Name, unit: t.ID, inferred: c.inferred[sr.Name][t.ID]})
				seen[t.ID] = true
			}
		}
		for _, fb := range c.failedBatches {
			if fb.suite != sr.Name || fb.anyFail || fb.noResult {
				continue
			}
			for _, u := range fb.units {
				if !seen[u] {
					seen[u] = true
					out = append(out, failedUnit{suite: sr.Name, unit: u, inferred: true})
				}
			}
		}
	}
	return out
}

// failedBatch is a batch that exited non-zero.
type failedBatch struct {
	suite    string
	units    []string
	anyFail  bool // some unit of it was recorded as failed
	noResult bool // the runner exited 126/127 before any test reported a result (notStarted)
}

// printAgentSummary is the end of a run in failures mode: one line when it passed;
// otherwise the failing tests, the changed inputs of each (from the failure hints) and
// the command that reruns exactly them.
func (c *cycle) printAgentSummary(elapsedText string) {
	w := c.stderr
	selected, total := c.sel.res.Counts()
	started := countUnits(c.started)
	failed := c.failedUnits()
	noResult := c.noResultUnits()
	if len(failed) == 0 && !c.interrupted && (len(c.unstarted) > 0 || noResult > 0) {
		// Not a pass: smates exits 4 (the runner's error follows).
		fmt.Fprintf(w, "smates: NOT STARTED: %s left %d selected test unit(s) not started (they stay selected); %d of %d selected unit(s) ran in %s (%d in total)\n",
			c.notStartedBy(), c.notStartedUnits(), started-noResult, selected, elapsedText, total)
		if c.testsFailed {
			fmt.Fprintf(w, "smates: FAILED (exit code %d), but no test unit is known to have failed (see the output above)\n", c.worst)
		}
		return
	}
	if c.worst == 0 {
		note := ""
		if n := c.notStartedUnits(); n > 0 {
			note = fmt.Sprintf("; %s left %d selected unit(s) not started (they stay selected)", c.notStartedBy(), n)
		}
		fmt.Fprintf(w, "smates: PASSED %d of %d selected test unit(s) (%d in total) in %s%s%s\n", started, selected, total, elapsedText, note, c.skippedNote())
		return
	}
	if len(failed) == 0 && c.interrupted {
		fmt.Fprintf(w, "smates: INTERRUPTED after %s: %d selected test unit(s) not run (they stay selected)\n", elapsedText, countUnits(c.notRun))
		return
	}
	if len(failed) == 0 {
		fmt.Fprintf(w, "smates: FAILED (exit code %d) in %s, but no test unit is known to have failed (see the output above)\n", c.worst, elapsedText)
		return
	}
	fmt.Fprintf(w, "smates: FAILED %d of %d test unit(s) in %s%s:\n", len(failed), started, elapsedText, c.skippedNote())
	byTest := map[string]*hints.Hint{}
	for _, h := range c.failures.all() {
		byTest[h.Suite+"\x00"+h.Test] = h
	}
	for i, f := range failed {
		if i == maxSummaryTests {
			fmt.Fprintf(w, "  … %d more (see --report)\n", len(failed)-i)
			break
		}
		h := byTest[f.suite+"\x00"+f.unit]
		note := ""
		if f.inferred {
			note = "  [status inferred from the run's exit code: the failure may belong to another test]"
		}
		fmt.Fprintf(w, "  %s: %s%s\n", f.suite, f.unit, note)
		if h == nil {
			continue
		}
		if len(h.Candidates) == 0 {
			h := *h
			h.Inferred = false // said above
			fmt.Fprintf(w, "    %s\n", hintSummary(&h))
		} else {
			var files []string
			for j, cand := range h.Candidates {
				if j == maxSummaryFiles {
					break
				}
				files = append(files, cand.Path+" ("+cand.Change+")")
			}
			more := ""
			if n := len(h.Candidates) - len(files) + h.More; n > 0 {
				more = fmt.Sprintf(" and %d more", n)
			}
			fmt.Fprintf(w, "    changed inputs: %s%s\n", strings.Join(files, ", "), more)
		}
		if ex := h.Explanation; ex != nil {
			fmt.Fprintf(w, "    → %s\n", explanationLine(ex))
		}
	}
	if n := countUnits(c.notRun); n > 0 && c.interrupted {
		fmt.Fprintf(w, "  (interrupted: %d selected test unit(s) not run)\n", n)
	} else if n := c.notStartedUnits(); n > 0 {
		fmt.Fprintf(w, "  (%s left %d selected test unit(s) not started)\n", c.notStartedBy(), n)
	}
	fmt.Fprintf(w, "smates: rerun them: %s\n", c.rerunCommand(failed))
}

// rerunCommand is the command line that reruns exactly the given tests.
func (c *cycle) rerunCommand(failed []failedUnit) string {
	args := []string{"smates", "run"}
	if c.f.dir != "" {
		args = append(args, "-C", shellQuote(c.f.dir))
	}
	if c.f.config != "" {
		args = append(args, "--config", shellQuote(c.f.config))
	}
	if c.f.state != "" {
		args = append(args, "--state", shellQuote(c.f.state))
	}
	if c.f.outputFlag {
		args = append(args, "--output", c.f.output)
	}
	if c.f.noAgent {
		args = append(args, "--no-agent")
	}
	if c.f.noRecord {
		args = append(args, "--no-record")
	}
	for _, f := range failed {
		args = append(args, "--only", shellQuote(f.suite+":"+f.unit))
	}
	if len(c.f.extra) > 0 {
		args = append(args, "--")
		for _, e := range c.f.extra {
			args = append(args, shellQuote(e))
		}
	}
	return strings.Join(args, " ")
}

// onlySpec is one --only argument.
type onlySpec struct{ suite, unit string }

// parseOnly parses the --only arguments (suite:unit).
func parseOnly(args []string) ([]onlySpec, error) {
	var out []onlySpec
	for _, a := range args {
		s, u, ok := strings.Cut(a, ":")
		u = strings.TrimSpace(u)
		if !ok || s == "" || u == "" {
			return nil, usageError{fmt.Errorf("invalid --only %q (want SUITE:UNIT, e.g. web:src/cart.test.ts)", a)}
		}
		if u != "./" && u != "." {
			u = strings.TrimSuffix(strings.TrimPrefix(u, "./"), "/")
		} else {
			u = "."
		}
		out = append(out, onlySpec{suite: s, unit: u})
	}
	return out, nil
}

// onlySuites returns the suites named by --only, in order and once each.
func onlySuites(specs []onlySpec) []string {
	var out []string
	for _, s := range specs {
		if !slices.Contains(out, s.suite) {
			out = append(out, s.suite)
		}
	}
	return out
}

// applyOnly narrows the selection to exactly the --only tests. A test the
// configuration does not define is a configuration error.
func applyOnly(res *selector.Result, specs []onlySpec) error {
	want := map[string]bool{}
	for _, sp := range specs {
		i := slices.IndexFunc(res.Suites, func(s *selector.Suite) bool { return s.Name == sp.suite })
		if i < 0 {
			var names []string
			for _, s := range res.Suites {
				names = append(names, s.Name)
			}
			return configError{fmt.Errorf("--only %s:%s: unknown suite %q (have: %s)", sp.suite, sp.unit, sp.suite, strings.Join(names, ", "))}
		}
		if !slices.ContainsFunc(res.Suites[i].Tests, func(t *selector.Test) bool { return t.ID == sp.unit }) {
			return configError{fmt.Errorf("--only %s:%s: suite %q has no test unit %q (see `smates list`)", sp.suite, sp.unit, sp.suite, sp.unit)}
		}
		want[sp.suite+"\x00"+sp.unit] = true
	}
	narrow(res, func(s *selector.Suite, t *selector.Test) bool { return want[s.Name+"\x00"+t.ID] }, selector.ReasonOnly, "--only")
	return nil
}
