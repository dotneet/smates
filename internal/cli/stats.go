package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/summary"
)

// ---- recording ----

// runRecord summarizes this cycle for `smates stats`. A sharded run counts the units
// its shard owns, so the records of all shards add up to the whole selection.
func (c *cycle) runRecord(elapsed time.Duration, watch bool) state.Run {
	p, res := c.p, c.sel.res
	r := state.Run{
		ID: newRunID(), At: c.now, Commit: p.repo.Head(), Branch: first(os.Getenv("SMATES_BRANCH"), p.ci.Branch, p.repo.CurrentBranch()),
		CI: p.ci.Name, Watch: watch, Mode: string(res.Mode), Shadow: c.f.shadow,
		ElapsedMs: elapsed.Milliseconds(), ExitCode: c.worst,
		Escapes: len(c.escapes), Possible: len(c.possible), Flaky: len(c.flakes), NotRun: countUnits(c.notRun),
	}
	if c.f.shardN > 1 {
		r.Shard = fmt.Sprintf("%d/%d", c.f.shardI, c.f.shardN)
	}
	countSelection(&r, res, c.f.shardI, c.f.shardN)
	for _, statuses := range c.ran {
		for _, st := range statuses {
			if st == state.Fail {
				r.Failed++
			}
		}
	}
	if dec := c.sel.agentDec; dec != nil {
		a := &state.AgentCall{Model: dec.Model, Cached: c.sel.agentCache}
		if !a.Cached {
			u := dec.Usage
			a.Turns, a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens, a.CostUSD = u.Turns, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD
		}
		r.Agent = a
	}
	return r
}

// countSelection counts the selected and skipped units of a selection (only those shard
// i of n owns when sharded), the recorded time of the skipped ones, and the orphans by
// handling. Every shard sees the same orphans: only the first shard records them.
func countSelection(r *state.Run, res *selector.Result, shardI, shardN int) {
	if shardN <= 1 || shardI == 1 {
		for _, how := range res.OrphanHandling {
			if r.Orphans == nil {
				r.Orphans = map[string]int{}
			}
			r.Orphans[how]++
		}
	}
	for _, sr := range res.Suites {
		for _, t := range sr.Tests {
			if shardN > 1 && t.Shard != 0 && t.Shard != shardI {
				continue
			}
			r.Total++
			switch {
			case t.Selected:
				r.Selected++
			case t.Duration > 0:
				r.SkippedMs += t.Duration
			default:
				r.Untimed++
			}
		}
	}
}

func newRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// cumulativeSavings is the summary line on the time the recorded runs saved. A sharded
// run is one run: its shards' records add up its savings, its first shard counts it.
func cumulativeSavings(runs []state.Run) *summary.Savings {
	s := &summary.Savings{}
	for _, r := range runs {
		if r.Shadow {
			continue
		}
		if s.Since.IsZero() {
			s.Since = r.At
		}
		if r.Shard == "" || strings.HasPrefix(r.Shard, "1/") {
			s.Runs++
		}
		s.SavedMs += r.SkippedMs
	}
	if s.Runs < 2 || s.SavedMs == 0 {
		return nil
	}
	return s
}

// ---- smates stats ----

type statsFilter struct {
	since  time.Time // zero: no lower bound
	branch string
	ciOnly bool
}

func (f statsFilter) keep(r state.Run) bool {
	return !r.At.Before(f.since) && (f.branch == "" || r.Branch == f.branch) && (!f.ciOnly || r.CI != "")
}

// statsReport is the aggregate `smates stats` prints.
type statsReport struct {
	State  string    `json:"state"`
	Since  time.Time `json:"since,omitzero"`
	Branch string    `json:"branch,omitempty"`
	From   time.Time `json:"from,omitzero"` // first run in the period
	To     time.Time `json:"to,omitzero"`   // last run in the period

	Runs       int            `json:"runs"`
	ShardJobs  int            `json:"shard_jobs"`
	ByMode     map[string]int `json:"by_mode"`   // state, diff, all, shadow
	BySource   map[string]int `json:"by_source"` // ci, local, watch
	FailedRuns int            `json:"failed_runs"`

	// Selection covers the runs that were not in shadow mode.
	Selected  int     `json:"selected_units"`
	Total     int     `json:"total_units"`
	SkipRate  float64 `json:"skip_rate"`
	SavedMs   int64   `json:"saved_ms"`
	Untimed   int     `json:"skipped_untimed_units"`
	ElapsedMs int64   `json:"elapsed_ms"` // time the non-shadow runs took
	// SavedPer30d extrapolates SavedMs to 30 days over the period (0 when the period
	// is shorter than a day).
	SavedPer30d int64 `json:"saved_per_30d_ms,omitempty"`

	Shadow  shadowStats `json:"shadow"`
	Orphans orphanStats `json:"orphans"`
	Agent   agentStats  `json:"agent"`
	Flaky   flakyStats  `json:"flaky"`
	runs    []state.Run // the runs in the period, oldest first
}

type shadowStats struct {
	Runs            int `json:"runs"`
	RunsWithEscapes int `json:"runs_with_escapes"`
	// Failed counts the failing units of shadow runs, Escapes those the selection
	// would have skipped; MissRate is Escapes/Failed.
	Failed   int     `json:"failed_units"`
	Escapes  int     `json:"escapes"`
	Possible int     `json:"possible_escapes"`
	MissRate float64 `json:"miss_rate"`
}

type orphanStats struct {
	Files      int            `json:"files"`
	Runs       int            `json:"runs"`
	ByHandling map[string]int `json:"by_handling"`
}

type agentStats struct {
	Calls            int     `json:"calls"`
	Cached           int     `json:"cached"`
	Turns            int     `json:"turns"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// CostReported counts the uncached calls whose backend reported a cost.
	CostReported int `json:"cost_reported_calls"`
}

type flakyStats struct {
	// Flips counts the flips detected by the runs in the period; Units the units the
	// state currently marks flaky.
	Flips int `json:"flips"`
	Units int `json:"units"`
}

func aggregateStats(st *state.State, f statsFilter, now time.Time) *statsReport {
	rep := &statsReport{
		Since: f.since, Branch: f.branch, ByMode: map[string]int{}, BySource: map[string]int{},
		Orphans: orphanStats{ByHandling: map[string]int{}},
	}
	for _, r := range st.Runs {
		if !f.keep(r) {
			continue
		}
		if rep.Runs == 0 {
			rep.From = r.At
		}
		rep.To = r.At
		rep.Runs++
		if r.Shard != "" {
			rep.ShardJobs++
		}
		switch {
		case r.Watch:
			rep.BySource["watch"]++
		case r.CI != "":
			rep.BySource["ci"]++
		default:
			rep.BySource["local"]++
		}
		if r.ExitCode != 0 {
			rep.FailedRuns++
		}
		if r.Shadow {
			rep.ByMode["shadow"]++
			s := &rep.Shadow
			s.Runs++
			s.Failed += r.Failed
			s.Escapes += r.Escapes
			s.Possible += r.Possible
			if r.Escapes > 0 {
				s.RunsWithEscapes++
			}
		} else {
			rep.ByMode[r.Mode]++
			rep.Selected += r.Selected
			rep.Total += r.Total
			rep.SavedMs += r.SkippedMs
			rep.Untimed += r.Untimed
			rep.ElapsedMs += r.ElapsedMs
		}
		if len(r.Orphans) > 0 {
			rep.Orphans.Runs++
		}
		for how, n := range r.Orphans {
			rep.Orphans.Files += n
			rep.Orphans.ByHandling[how] += n
		}
		if a := r.Agent; a != nil {
			g := &rep.Agent
			g.Calls++
			if a.Cached {
				g.Cached++
			}
			g.Turns += a.Turns
			g.InputTokens += a.InputTokens
			g.OutputTokens += a.OutputTokens
			g.CacheReadTokens += a.CacheReadTokens
			g.CacheWriteTokens += a.CacheWriteTokens
			if a.CostUSD > 0 {
				g.CostUSD += a.CostUSD
				g.CostReported++
			}
		}
		rep.Flaky.Flips += r.Flaky
		rep.runs = append(rep.runs, r)
	}
	if rep.Total > 0 {
		rep.SkipRate = float64(rep.Total-rep.Selected) / float64(rep.Total)
	}
	if rep.Shadow.Failed > 0 {
		rep.Shadow.MissRate = float64(rep.Shadow.Escapes) / float64(rep.Shadow.Failed)
	}
	// The period starts with the first recorded run: --since may reach further back
	// than the history (adoption, records trimmed by the limits).
	start := rep.From
	if f.since.After(start) {
		start = f.since
	}
	if span := now.Sub(start); rep.Runs > 0 && span >= 24*time.Hour {
		rep.SavedPer30d = int64(float64(rep.SavedMs) * float64(30*24*time.Hour) / float64(span))
	}
	for _, su := range st.Suites {
		for _, e := range su.Tests {
			if e.Flaky != nil {
				rep.Flaky.Units++
			}
		}
	}
	return rep
}

func cmdStats(args []string, stdout, stderr io.Writer) error {
	c := newCommand("stats", stderr)
	dir := c.String("C", "", "run as if started in `dir`")
	statePath := c.String("state", "", "state file (default: .smates/state.json.gz)")
	c.withFormat("text", "json")
	c.bareFormatError = true
	since := c.String("since", "", "only runs since `when`: a date (2026-01-02), an RFC 3339 time, or an age such as 30d, 2w or 12h")
	branch := c.String("branch", "", "only runs on `branch`")
	ciOnly := c.Bool("ci", false, "only runs on CI")
	recent := c.Int("recent", 5, "text: list the last `n` runs")
	if err := c.parse(args); err != nil {
		return err
	}
	if err := c.noArgs(); err != nil {
		return err
	}
	now := time.Now().UTC()
	f := statsFilter{branch: *branch, ciOnly: *ciOnly}
	if *since != "" {
		t, err := parseSince(*since, now)
		if err != nil {
			return usageError{err}
		}
		f.since = t
	}
	store, err := resolveStore(*dir, *statePath, stderr)
	if err != nil {
		return err
	}
	path := store.path
	st, err := store.Load()
	if err != nil {
		return err
	}
	rep := aggregateStats(st, f, now)
	rep.State = path
	if c.asJSON() {
		// Unlike writeJSON, this escapes HTML characters.
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printStats(stdout, rep, *recent)
	return nil
}

// parseSince parses --since: a date, an RFC 3339 time, or an age (30d, 2w, 12h, 90m).
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	unit := map[byte]time.Duration{'d': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	if n, err := strconv.Atoi(s[:len(s)-1]); err == nil && n >= 0 && unit[s[len(s)-1]] > 0 {
		return now.Add(-time.Duration(n) * unit[s[len(s)-1]]), nil
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want a date such as 2026-01-02, or an age such as 30d, 2w or 12h)", s)
}

func printStats(w io.Writer, rep *statsReport, recent int) {
	scope := ""
	if !rep.Since.IsZero() {
		scope += " since " + rep.Since.Local().Format("2006-01-02 15:04")
	}
	if rep.Branch != "" {
		scope += " on " + rep.Branch
	}
	if rep.Runs == 0 {
		fmt.Fprintf(w, "smates stats: no recorded runs%s in %s\n", scope, rep.State)
		fmt.Fprintln(w, "  (runs are recorded by `smates run`, except with --no-record or --dry-run)")
		return
	}
	fmt.Fprintf(w, "smates stats: %s%s, %s to %s\n", plural(rep.Runs, "run"), scope, day(rep.From), day(rep.To))
	fmt.Fprintf(w, "  state: %s\n\n", rep.State)

	row := func(label, format string, args ...any) {
		fmt.Fprintf(w, "  %-14s %s\n", label+":", fmt.Sprintf(format, args...))
	}
	fmt.Fprintln(w, "Runs")
	row("total", "%d%s, %d failed (exit code ≠ 0)", rep.Runs, shardNote(rep.ShardJobs), rep.FailedRuns)
	row("by mode", "%s", breakdown(rep.ByMode, []string{"state", "diff", "all", "shadow"}, map[string]string{"state": "selective", "diff": "static (no state)", "all": "full"}))
	row("by source", "%s", breakdown(rep.BySource, []string{"ci", "local", "watch"}, nil))

	fmt.Fprintln(w, "\nSelection")
	if rep.Total > 0 {
		row("test units", "ran %d of %d (%.0f%% skipped)", rep.Selected, rep.Total, 100*rep.SkipRate)
	}
	saved := "about " + summary.Duration(rep.SavedMs) + " of recorded test time skipped"
	if rep.SavedMs == 0 {
		saved = "none recorded"
	}
	if rep.Untimed > 0 {
		saved += fmt.Sprintf(" (+%d skipped units without a recorded duration)", rep.Untimed)
	}
	row("time saved", "%s", saved)
	if rep.SavedPer30d > 0 {
		row("per 30 days", "about %s at this pace", summary.Duration(rep.SavedPer30d))
	}
	row("time spent", "%s running the selected tests", summary.Duration(rep.ElapsedMs))

	fmt.Fprintln(w, "\nShadow mode (--shadow)")
	if s := rep.Shadow; s.Runs == 0 {
		row("runs", "none (run `smates run --shadow` now and then to check the selection)")
	} else {
		row("runs", "%d, %d with misses", s.Runs, s.RunsWithEscapes)
		miss := fmt.Sprintf("%d of %d failing units would not have been selected", s.Escapes, s.Failed)
		if s.Failed > 0 {
			miss += fmt.Sprintf(" (%.1f%%)", 100*s.MissRate)
		}
		row("misses", "%s", miss)
		if s.Possible > 0 {
			row("possible", "%d more marked failed only by the run's exit code", s.Possible)
		}
	}

	fmt.Fprintln(w, "\nOrphans (changes no recorded test depends on)")
	if o := rep.Orphans; o.Files == 0 {
		row("files", "none")
	} else {
		row("files", "%d in %s", o.Files, plural(o.Runs, "run"))
		row("handled by", "%s", breakdown(o.ByHandling, []string{selector.OrphanReference, selector.OrphanAgent, selector.OrphanNoImpact, selector.OrphanPolicy, selector.OrphanIgnored, selector.OrphanSkipped, selector.OrphanUntested},
			map[string]string{selector.OrphanReference: "reference scan", selector.OrphanNoImpact: "agent: no impact", selector.OrphanPolicy: "full suite run", selector.OrphanSkipped: "skipped by caller", selector.OrphanUntested: "untested code"}))
	}
	if a := rep.Agent; a.Calls > 0 {
		row("agent", "%s (%d cached), %d turns, %s in / %s out tokens", plural(a.Calls, "call"), a.Cached, a.Turns,
			tokens(a.InputTokens+a.CacheReadTokens+a.CacheWriteTokens), tokens(a.OutputTokens))
		if uncached := a.Calls - a.Cached; a.CostReported > 0 {
			row("agent cost", "$%.2f (reported for %d of %d uncached calls)", a.CostUSD, a.CostReported, uncached)
		}
	}

	fmt.Fprintln(w, "\nFlaky tests")
	row("flips", "%d detected in these runs; %d unit(s) currently marked flaky (see `smates flaky`)", rep.Flaky.Flips, rep.Flaky.Units)

	if recent > 0 {
		fmt.Fprintln(w, "\nRecent runs")
		runs := rep.runs[max(len(rep.runs)-recent, 0):]
		for i := len(runs) - 1; i >= 0; i-- {
			r := runs[i]
			mode := r.Mode
			if r.Shadow {
				mode = "shadow"
			}
			extra := ""
			if r.Shard != "" {
				extra += ", shard " + r.Shard
			}
			skipped := "-"
			if r.SkippedMs > 0 {
				skipped = summary.Duration(r.SkippedMs)
			}
			if r.Escapes > 0 {
				extra += fmt.Sprintf(", %d missed", r.Escapes)
			}
			fmt.Fprintf(w, "  %s  %-8.8s %-20.20s %-6s %d/%d units, skipped %s, exit %d%s\n",
				r.At.Local().Format("2006-01-02 15:04"), first(r.Commit, "-"), first(r.Branch, "-"), mode, r.Selected, r.Total, skipped, r.ExitCode, extra)
		}
	}
}

func day(t time.Time) string { return t.Local().Format("2006-01-02") }

func shardNote(n int) string {
	if n == 0 {
		return ""
	}
	return " (" + plural(n, "shard job") + ")"
}

// breakdown renders counts as "a 3, b 1" in the given order, then any other keys.
func breakdown(m map[string]int, order []string, labels map[string]string) string {
	var parts []string
	keys := append([]string{}, order...)
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(order, k) {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if m[k] == 0 {
			continue
		}
		label := k
		if l, ok := labels[k]; ok {
			label = l
		}
		parts = append(parts, fmt.Sprintf("%s %d", label, m[k]))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func tokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.Itoa(n)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
