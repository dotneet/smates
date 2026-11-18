package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/hints"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/summary"
)

// unstartedSection lists, for the summary, the suites whose runner could not start or
// whose requirements are not met.
func (c *cycle) unstartedSection() (sec summary.Section, ok bool) {
	n := c.units.count(unitUnstarted, unitUnmet)
	if n == 0 {
		return sec, false
	}
	var b strings.Builder
	if c.units.count(unitUnstarted) > 0 {
		b.WriteString("The runner could not start (see the log); these units did not run and stay selected:\n")
		for _, sr := range c.sel.res.Suites {
			if n := c.units.countIn(sr.Name, unitUnstarted); n > 0 {
				fmt.Fprintf(&b, "- `%s`: %d test unit(s)\n", sr.Name, n)
			}
		}
	}
	if c.units.count(unitUnmet) > 0 {
		b.WriteString("Requirements (`requires`) are not met here; these units did not run and stay selected:\n")
		for _, sr := range c.sel.res.Suites {
			if n := c.units.countIn(sr.Name, unitUnmet); n > 0 {
				fmt.Fprintf(&b, "- `%s`: %d test unit(s), need %s\n", sr.Name, n, strings.Join(c.unmetWhy[sr.Name], ", "))
			}
		}
	}
	return summary.Section{Title: fmt.Sprintf(":warning: %d test unit(s) not run", n), Body: b.String()}, true
}

// configStopSection lists, for the summary, the units a configuration error (a suite
// command that cannot be parsed) left out by stopping the run. summary.Run.NotRun
// would call them left out by --fail-fast.
func (c *cycle) configStopSection() (sec summary.Section, ok bool) {
	n := c.units.count(unitNotRun)
	if c.stop != stopConfig || n == 0 {
		return sec, false
	}
	var b strings.Builder
	b.WriteString("A configuration error stopped the run (see the log); these units did not run and stay selected:\n")
	for _, sr := range c.sel.res.Suites {
		for _, u := range c.units.in(sr.Name, unitNotRun) {
			fmt.Fprintf(&b, "- `%s: %s`\n", sr.Name, u)
		}
	}
	return summary.Section{Title: fmt.Sprintf(":x: %d test unit(s) not run after a configuration error", n), Body: b.String()}, true
}

// notStartedBy names what left the units of notStartedUnits not started: what stopped
// the run (--fail-fast, --max-time, a configuration error, an interrupt), unmet
// requirements, a runner that could not start (or exited 126/127 before any test
// reported a result).
func (c *cycle) notStartedBy() string {
	var by []string
	if c.units.count(unitNotRun) > 0 && c.stopped() {
		by = append(by, c.stop.String())
	}
	if c.units.count(unitUnmet) > 0 {
		by = append(by, "unmet requirements")
	}
	if c.units.count(unitUnstarted) > 0 {
		by = append(by, "a runner that could not start")
	}
	return joinAnd(by)
}

// notStartedUnits counts the planned units that did not run: not run, not started,
// left out by unmet requirements. They stay selected.
func (c *cycle) notStartedUnits() int {
	return c.units.count(unitNotRun, unitUnstarted, unitUnmet)
}

// skippedNote is what the outcome line adds for the units that skipped themselves in
// this run (state.Skip): they did not fail, but they are not proven to pass, so they
// stay selected. "" when none did.
func (c *cycle) skippedNote() string {
	n := c.units.countStatus(state.Skip)
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; %d skipped (not proven: they run again next time)", n)
}

// logShadowAndFlakes logs, in failures mode, what logOutcome logs besides the outcome
// line: shadow escapes and flaky tests.
func (c *cycle) logShadowAndFlakes() {
	if c.f.shadow && len(c.escapes) > 0 {
		c.p.logf("SHADOW: %d failing test(s) would NOT have been selected: %s", len(c.escapes), strings.Join(limit(c.escapes, 10), ", "))
	}
	logFlakes(c.p, c.stderr, c.flakes)
}

// logOutcome logs how the run went.
func (c *cycle) logOutcome(elapsed time.Duration) {
	p := c.p
	selected, total := c.sel.res.Counts()
	if c.f.shadow {
		if len(c.escapes) > 0 {
			p.logf("SHADOW: %d failing test(s) would NOT have been selected:", len(c.escapes))
			for _, e := range c.escapes {
				fmt.Fprintf(c.stderr, "  - %s\n", e)
			}
		} else {
			if c.shadowShard() {
				// Out of the units this shard owns, all of which it ran.
				var r state.Run
				countSelection(&r, c.sel.res, c.f.shardI, c.f.shardN)
				selected, total = r.Selected, r.Total
			}
			p.logf("shadow: every failing test was selected (%d/%d would have run)", selected, total)
		}
		if len(c.possible) > 0 {
			p.logf("shadow: %d unselected test(s) are marked failed only because the run failed (the runner reports no per-file status): %s", len(c.possible), strings.Join(limit(c.possible, 10), ", "))
		}
	} else if n := c.notStartedUnits(); n > 0 && c.interrupted() {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d; interrupted, %d selected unit(s) not run (they stay selected)", elapsed, selected-n, total, c.exit, n)
	} else if n := c.notStartedUnits(); n > 0 {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d; %s left %d selected unit(s) not started (they stay selected)%s", elapsed, selected-n, total, c.exit, c.notStartedBy(), n, c.skippedNote())
	} else {
		p.logf("done in %s: ran %d of %d test unit(s), exit code %d%s", elapsed, selected, total, c.exit, c.skippedNote())
	}
	logFlakes(p, c.stderr, c.flakes)
}

// writeSummaries writes the Markdown summaries and the GitHub outputs.
func (c *cycle) writeSummaries(elapsed time.Duration) {
	p, f := c.p, c.f
	sum := summaryInput(c.sel, f.shardI, f.shardN)
	sum.Run = &summary.Run{ExitCode: c.exit, Elapsed: elapsed, Shadow: f.shadow, Escapes: c.escapes, Possible: c.possible, Interrupted: c.interrupted()}
	if c.capped() {
		sum.Run.MaxTime = f.maxTime
	}
	sum.Savings = cumulativeSavings(p.state.Runs)
	for _, sr := range c.sel.res.Suites {
		for _, t := range sr.Tests {
			if c.units.status(sr.Name, t.ID) == state.Fail {
				sum.Run.Failed = append(sum.Run.Failed, sr.Name+": "+t.ID)
			}
		}
		if c.stop != stopConfig { // a configuration error has its own section
			for _, u := range c.units.in(sr.Name, unitNotRun) {
				sum.Run.NotRun = append(sum.Run.NotRun, sr.Name+": "+u)
			}
		}
	}
	if sec, ok := c.configStopSection(); ok {
		sum.Sections = append(sum.Sections, sec)
	}
	if sec, ok := c.unstartedSection(); ok {
		sum.Sections = append(sum.Sections, sec)
	}
	if sec, ok := flakySection(c.flakes); ok {
		sum.Sections = append(sum.Sections, sec)
	}
	if sec, ok := c.failures.section(); ok {
		sum.Sections = append(sum.Sections, sec)
	}
	writeRunSummary(p, f.summary, sum)
	writeMarkdown(p, f.markdown, sum)
	if os.Getenv("GITHUB_OUTPUT") != "" {
		if err := writeGitHubOutputs(c.stdout, c.sel, append([]string{fmt.Sprintf("exit-code=%d", c.exit)}, flakyOutputs(c.flakes)...)...); err != nil {
			p.logf("could not write GitHub outputs: %v", err)
		}
	}
}

type runTimings struct {
	Discovery int64 `json:"discovery"`
	StateLoad int64 `json:"state_load"`
	Selection int64 `json:"selection"`
	Execution int64 `json:"execution"`
	Record    int64 `json:"record"`
	StateSave int64 `json:"state_save"`
}

// writeReport writes the JSON report (--report).
func (c *cycle) writeReport() error {
	rep := struct {
		jsonOut
		Results  map[string]map[string]string `json:"results"`
		Escapes  []string                     `json:"escapes"`
		Possible []string                     `json:"possible_escapes"`
		ExitCode int                          `json:"exit_code"`
		Shadow   bool                         `json:"shadow"`
		Flaky    []flakeReport                `json:"flaky"`
		Failures []*hints.Hint                `json:"failure_hints"`
		FailFast bool                         `json:"fail_fast"`
		// NotRun: the selected units that did not run (they stay selected): left out when
		// the run stopped (stopped_by), not started by their runner, or left out by unmet
		// requires.
		NotRun map[string][]string `json:"not_run"`
		// StoppedBy: what stopped the run before every batch started ("" when nothing
		// did): "--fail-fast", "--max-time", "an interrupt" or "a configuration error".
		StoppedBy string `json:"stopped_by"`
		// Interrupted: the run was cancelled (Ctrl-C); what did not finish is in not_run.
		Interrupted bool `json:"interrupted"`
		// MaxTimeReached: --max-time left the units of not_run out (except unmet_requires).
		MaxTimeReached bool `json:"max_time_reached"`
		// UnmetRequires: suite -> the requirements not met (the units are in not_run).
		UnmetRequires map[string][]string `json:"unmet_requires"`
		Timings       runTimings          `json:"timings_ms"`
	}{jsonSelection(c.sel), c.units.statuses(), c.escapes, c.possible, c.exit, c.f.shadow, c.flakes, c.failures.all(), c.f.failFast,
		c.units.bySuite(unitNotRun, unitUnstarted, unitUnmet), c.stop.String(), c.interrupted(), c.capped(), c.unmetWhy, c.timings}
	if rep.Flaky == nil {
		rep.Flaky = []flakeReport{}
	}
	if rep.Escapes == nil {
		rep.Escapes = []string{}
	}
	if rep.Possible == nil {
		rep.Possible = []string{}
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if c.f.report == "-" {
		_, err := c.reportOut.Write(append(data, '\n'))
		return err
	}
	return os.WriteFile(c.f.report, data, 0o644)
}
