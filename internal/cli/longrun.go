package cli

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/summary"
)

// Long runs: a caller that reads the output of `smates run` (a coding agent's tool call)
// may give up on a command that prints nothing for minutes, and an agent waits for the
// whole selection before it reads anything.
const (
	// defaultHeartbeat is how long failures mode stays silent before it prints a
	// progress line ($SMATES_HEARTBEAT overrides it; 0 turns it off).
	defaultHeartbeat = 30 * time.Second
	// defaultBudget is the estimated duration above which `smates run` prints how to
	// split the run (--budget).
	defaultBudget = 90 * time.Second
)

// heartbeatInterval is $SMATES_HEARTBEAT (a duration, or seconds), else the default.
func heartbeatInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("SMATES_HEARTBEAT"))
	if v == "" {
		return defaultHeartbeat
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	var secs int
	if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return defaultHeartbeat
}

// startHeartbeat prints a progress line to the log after every interval during which
// the batch of suite runs (its output is buffered in failures mode). started is the
// number of units started so far, of total. The returned function stops it.
func (c *cycle) startHeartbeat(suite string, started, total int, runStart time.Time) func() {
	if !c.quiet || c.heartbeat <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(c.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Fprintf(c.stderr, "smates: still running %s (%d of %d test unit(s) started, %s)\n",
					suite, started, total, summary.Duration(time.Since(runStart).Milliseconds()))
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// estimate is the recorded duration of what the batches run, per suite (in batch
// order), and the number of units without a recorded duration.
type estimate struct {
	total      time.Duration
	suites     []string
	bySuite    map[string]time.Duration
	unrecorded int
}

func (c *cycle) estimate(batches []*batch) estimate {
	e := estimate{bySuite: map[string]time.Duration{}}
	for _, b := range batches {
		dur := map[string]int64{}
		for _, t := range b.sr.Tests {
			dur[t.ID] = t.Duration
		}
		if _, ok := e.bySuite[b.sr.Name]; !ok {
			e.suites = append(e.suites, b.sr.Name)
		}
		jobs := time.Duration(1)
		if c.record {
			jobs = time.Duration(recordParallelism(b.sr))
		}
		for _, u := range b.units {
			d := time.Duration(dur[u]) * time.Millisecond / jobs
			if d == 0 {
				e.unrecorded++
			}
			e.bySuite[b.sr.Name] += d
			e.total += d
		}
	}
	return e
}

// recordParallelism is how many units of a suite overlap while recording, as far as
// their recorded durations add up: a Go suite with granularity func times each function
// as a process of its own and runs record_jobs of them at once. Other suites keep the
// sum of their durations, an upper bound.
func recordParallelism(sr *selector.Suite) int {
	if s := sr.Suite; s != nil && s.Runner == "go" && s.Granularity == config.GranularityFunc && s.RecordJobs > 1 {
		return s.RecordJobs
	}
	return 1
}

// hintLongRun prints how to split a selection whose recorded durations add up to more
// than --budget: one suite at a time, or in the background with a report.
func (c *cycle) hintLongRun(batches []*batch) {
	if c.f.budget <= 0 || len(c.f.onlySpecs) > 0 {
		return
	}
	e := c.estimate(batches)
	if e.total <= c.f.budget {
		return
	}
	w := c.stderr
	suites := slices.Clone(e.suites)
	slices.SortStableFunc(suites, func(a, b string) int { return cmp.Compare(e.bySuite[b], e.bySuite[a]) })
	var parts []string
	for _, s := range suites {
		parts = append(parts, fmt.Sprintf("%s ~%s", s, summary.Duration(e.bySuite[s].Milliseconds())))
	}
	note := ""
	if e.unrecorded > 0 {
		note = fmt.Sprintf(", plus %d test unit(s) never timed", e.unrecorded)
	}
	fmt.Fprintf(w, "smates: this selection takes about %s by recorded durations (%s%s), more than --budget %s.\n",
		summary.Duration(e.total.Milliseconds()), strings.Join(parts, ", "), note, c.f.budget)
	if len(suites) > 1 {
		fmt.Fprintf(w, "smates:   for faster feedback run one suite at a time: smates run --output failures --suite %s\n", suites[0])
	}
	fmt.Fprintf(w, "smates:   or run it in the background and read the report afterwards: smates run --output failures --report smates-report.json\n")
}
