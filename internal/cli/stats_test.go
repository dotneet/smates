package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"30d":                  now.AddDate(0, 0, -30),
		"2w":                   now.AddDate(0, 0, -14),
		"12h":                  now.Add(-12 * time.Hour),
		"2026-10-01T00:00:00Z": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"2026-10-01":           time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"d", "-3d", "yesterday", "3x"} {
		if _, err := parseSince(in, now); err == nil {
			t.Errorf("parseSince(%q): no error", in)
		}
	}
}

func TestCountSelection(t *testing.T) {
	res := &selector.Result{OrphanHandling: map[string]string{"x": selector.OrphanAgent, "y": selector.OrphanReference}, Suites: []*selector.Suite{{Name: "s", Tests: []*selector.Test{
		{ID: "a", Selected: true, Duration: 100, Shard: 1},
		{ID: "b", Duration: 2000, Shard: 1},
		{ID: "c", Shard: 1},
		{ID: "d", Duration: 5000, Shard: 2}, // another shard's: not counted by shard 1
	}}}}
	var r state.Run
	countSelection(&r, res, 1, 2)
	if r.Total != 3 || r.Selected != 1 || r.SkippedMs != 2000 || r.Untimed != 1 || len(r.Orphans) != 2 {
		t.Errorf("shard 1/2: %+v", r)
	}
	// The orphans of a sharded run are recorded by its first shard only.
	r = state.Run{}
	countSelection(&r, res, 2, 2)
	if r.Total != 1 || r.SkippedMs != 5000 || r.Orphans != nil {
		t.Errorf("shard 2/2: %+v", r)
	}
	r = state.Run{}
	countSelection(&r, res, 0, 0)
	if r.Total != 4 || r.SkippedMs != 7000 || r.Orphans[selector.OrphanAgent] != 1 {
		t.Errorf("unsharded: %+v", r)
	}
}

func statsSample() *state.State {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	st := state.New()
	st.Runs = []state.Run{
		{ID: "1", At: t0, Branch: "main", CI: "github", Mode: "all", Selected: 10, Total: 10, ElapsedMs: 60_000},
		{ID: "2", At: t0.AddDate(0, 0, 1), Branch: "feat", CI: "github", Mode: "state", Selected: 2, Total: 10, SkippedMs: 40_000, Untimed: 1, ElapsedMs: 9_000,
			Orphans: map[string]int{selector.OrphanReference: 1, selector.OrphanAgent: 2}, Agent: &state.AgentCall{Turns: 3, InputTokens: 1500, OutputTokens: 200, CostUSD: 0.02}},
		{ID: "3", At: t0.AddDate(0, 0, 2), Branch: "feat", CI: "github", Mode: "state", Shard: "1/2", Selected: 1, Total: 5, SkippedMs: 20_000, ExitCode: 1, Failed: 1,
			Orphans: map[string]int{selector.OrphanAgent: 1}, Agent: &state.AgentCall{Cached: true}, Flaky: 1},
		{ID: "4", At: t0.AddDate(0, 0, 3), Branch: "feat", Mode: "state", Shadow: true, Selected: 1, Total: 10, SkippedMs: 50_000, ElapsedMs: 300_000, ExitCode: 1, Failed: 4, Escapes: 1, Possible: 1},
		{ID: "5", At: t0.AddDate(0, 0, 4), Branch: "feat", Watch: true, Mode: "state", Selected: 0, Total: 10, SkippedMs: 30_000},
	}
	st.Suite("s").Tests["x"] = &state.Entry{Flaky: &state.Flake{Count: 2}}
	return st
}

func TestAggregateStats(t *testing.T) {
	st := statsSample()
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	rep := aggregateStats(st, statsFilter{}, now)
	if rep.Runs != 5 || rep.ShardJobs != 1 || rep.FailedRuns != 2 {
		t.Errorf("runs: %+v", rep)
	}
	if rep.ByMode["all"] != 1 || rep.ByMode["state"] != 3 || rep.ByMode["shadow"] != 1 {
		t.Errorf("by mode: %v", rep.ByMode)
	}
	if rep.BySource["ci"] != 3 || rep.BySource["local"] != 1 || rep.BySource["watch"] != 1 {
		t.Errorf("by source: %v", rep.BySource)
	}
	// Shadow runs save nothing: 40s + 20s + 30s.
	if rep.Selected != 13 || rep.Total != 35 || rep.SavedMs != 90_000 || rep.Untimed != 1 {
		t.Errorf("selection: %d/%d saved %d untimed %d", rep.Selected, rep.Total, rep.SavedMs, rep.Untimed)
	}
	// 90s over the 30 days since the first run; shadow runs take no part in the time spent.
	if rep.SavedPer30d != 90_000 || rep.ElapsedMs != 69_000 {
		t.Errorf("per 30 days: %d, elapsed %d", rep.SavedPer30d, rep.ElapsedMs)
	}
	// --since before the first recorded run: the pace is measured from that run.
	if r := aggregateStats(st, statsFilter{since: now.AddDate(0, 0, -90)}, now); r.SavedPer30d != 90_000 {
		t.Errorf("per 30 days with an early --since: %d", r.SavedPer30d)
	}
	if s := rep.Shadow; s.Runs != 1 || s.RunsWithEscapes != 1 || s.Escapes != 1 || s.Failed != 4 || s.MissRate != 0.25 {
		t.Errorf("shadow: %+v", s)
	}
	if o := rep.Orphans; o.Files != 4 || o.Runs != 2 || o.ByHandling[selector.OrphanAgent] != 3 {
		t.Errorf("orphans: %+v", o)
	}
	if a := rep.Agent; a.Calls != 2 || a.Cached != 1 || a.InputTokens != 1500 || a.CostReported != 1 || a.CostUSD != 0.02 {
		t.Errorf("agent: %+v", a)
	}
	if rep.Flaky.Flips != 1 || rep.Flaky.Units != 1 {
		t.Errorf("flaky: %+v", rep.Flaky)
	}

	rep = aggregateStats(st, statsFilter{branch: "feat", ciOnly: true, since: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}, now)
	if rep.Runs != 2 || rep.SavedMs != 60_000 {
		t.Errorf("filtered: %d runs, saved %d", rep.Runs, rep.SavedMs)
	}
}

func TestPrintStats(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	rep := aggregateStats(statsSample(), statsFilter{}, now)
	rep.State = ".smates/state.json.gz"
	printStats(&b, rep, 2)
	out := b.String()
	for _, want := range []string{
		"5 runs",
		"ran 13 of 35 (63% skipped)",
		"about 1m 30s of recorded test time skipped (+1 skipped units without a recorded duration)",
		"1 of 4 failing units would not have been selected (25.0%)",
		"reference scan 1, agent 3",
		"2 calls (1 cached), 3 turns, 1.5k in / 200 out tokens",
		"$0.02 (reported for 1 of 1 uncached calls)",
		"1 detected in these runs; 1 unit(s) currently marked flaky",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if n := strings.Count(out[strings.Index(out, "Recent runs"):], "\n  "); n != 2 {
		t.Errorf("recent runs: %d lines\n%s", n, out)
	}

	b.Reset()
	printStats(&b, aggregateStats(state.New(), statsFilter{}, now), 5)
	if !strings.Contains(b.String(), "no recorded runs") {
		t.Errorf("empty: %s", b.String())
	}
}

func TestCumulativeSavings(t *testing.T) {
	st := statsSample()
	s := cumulativeSavings(st.Runs)
	if s == nil || s.Runs != 4 || s.SavedMs != 90_000 || !s.Since.Equal(st.Runs[0].At) {
		t.Errorf("savings: %+v", s)
	}
	// The shards of one run count as one run.
	st.AddRun(state.Run{ID: "6", At: st.Runs[len(st.Runs)-1].At.Add(time.Hour), Mode: "state", Shard: "2/2", SkippedMs: 10_000})
	if s := cumulativeSavings(st.Runs); s.Runs != 4 || s.SavedMs != 100_000 {
		t.Errorf("with a second shard: %+v", s)
	}
	if cumulativeSavings(st.Runs[:1]) != nil {
		t.Error("one run without savings must not show a line")
	}
}

// TestShadowStatsSharded: the shards of a sharded shadow run count as one shadow run,
// which has misses once however many of its shards found some; their failures and
// escapes add up.
func TestShadowStatsSharded(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	shard := func(id, commit, shard string, failed, escapes int) state.Run {
		return state.Run{ID: id, At: t0, Commit: commit, Branch: "feat", CI: "github", Mode: "state", Shadow: true, Shard: shard, Failed: failed, Escapes: escapes}
	}
	cases := []struct {
		name                                string
		runs                                []state.Run
		runsN, withEscapes, escapes, failed int
	}{
		{"4 shards, an escape in shard 3",
			[]state.Run{shard("a", "c1", "1/4", 0, 0), shard("b", "c1", "2/4", 1, 0), shard("c", "c1", "3/4", 2, 1), shard("d", "c1", "4/4", 0, 0)},
			1, 1, 1, 3},
		{"escapes in two shards of one run",
			[]state.Run{shard("a", "c1", "1/2", 1, 1), shard("b", "c1", "2/2", 1, 1)},
			1, 1, 2, 2},
		{"two sharded runs",
			[]state.Run{shard("a", "c1", "1/2", 0, 0), shard("b", "c1", "2/2", 1, 1), shard("c", "c2", "1/2", 1, 1), shard("d", "c2", "2/2", 0, 0)},
			2, 2, 2, 2},
		{"unsharded runs count each",
			[]state.Run{shard("a", "c1", "", 1, 1), shard("b", "c1", "", 1, 1)},
			2, 2, 2, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := state.New()
			st.Runs = c.runs
			s := aggregateStats(st, statsFilter{}, t0.AddDate(0, 0, 1)).Shadow
			if s.Runs != c.runsN || s.RunsWithEscapes != c.withEscapes || s.Escapes != c.escapes || s.Failed != c.failed {
				t.Errorf("shadow %+v, want runs %d with misses %d escapes %d failed %d", s, c.runsN, c.withEscapes, c.escapes, c.failed)
			}
		})
	}
}
