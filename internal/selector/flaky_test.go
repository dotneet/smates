package selector

import (
	"testing"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

func TestRecordDetectsFlakes(t *testing.T) {
	const unit = "src/a.test.ts"
	sr := &Suite{Name: "web", Known: map[string][]string{unit: {unit}}}
	base := gitx.Snapshot{"src/a.ts": "ha", unit: "ht", "other.ts": "ho"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	obs := func(status string) map[string]*runner.Observation {
		return map[string]*runner.Observation{unit: {Files: map[string]bool{"src/a.ts": true}, Status: status}}
	}
	type step struct {
		snap     gitx.Snapshot
		obs      map[string]*runner.Observation
		retried  map[string]*runner.Observation
		full     bool
		unrel    bool
		wantFlip string // "" = no flake event
	}
	changed := gitx.Snapshot{"src/a.ts": "ha2", unit: "ht", "other.ts": "ho"}
	cases := []struct {
		name  string
		pre   func(st *state.State)
		steps []step
		count int
	}{
		{"pass then fail on the same inputs", nil, []step{
			{snap: base, obs: obs(state.Pass)}, {snap: base, obs: obs(state.Fail), wantFlip: "pass->fail"},
		}, 1},
		{"fail then pass again (rerun of a failure)", nil, []step{
			{snap: base, obs: obs(state.Fail)}, {snap: base, obs: obs(state.Pass), wantFlip: "fail->pass"},
			{snap: base, obs: obs(state.Fail), wantFlip: "pass->fail"},
		}, 2},
		{"a changed dependency explains the new outcome", nil, []step{
			{snap: base, obs: obs(state.Pass)}, {snap: changed, obs: obs(state.Fail)},
		}, 0},
		{"same fingerprint across an unknown outcome", nil, []step{
			{snap: base, obs: obs(state.Pass)}, {snap: base, obs: obs(state.Unknown)},
			{snap: base, obs: obs(state.Fail), wantFlip: "pass->fail"},
		}, 1},
		// A skip is no outcome to flip from or to.
		{"skip then pass on the same inputs", nil, []step{
			{snap: base, obs: obs(state.Skip)}, {snap: base, obs: obs(state.Pass)},
		}, 0},
		{"pass, skip, pass", nil, []step{
			{snap: base, obs: obs(state.Pass)}, {snap: base, obs: obs(state.Skip)}, {snap: base, obs: obs(state.Pass)},
		}, 0},
		{"skip then fail on the same inputs", nil, []step{
			{snap: base, obs: obs(state.Skip)}, {snap: base, obs: obs(state.Fail)},
		}, 0},
		{"same fingerprint across a skip", nil, []step{
			{snap: base, obs: obs(state.Pass)}, {snap: base, obs: obs(state.Skip)},
			{snap: base, obs: obs(state.Fail), wantFlip: "pass->fail"},
		}, 1},
		{"back to the old inputs after a change", nil, []step{
			{snap: base, obs: obs(state.Pass)}, {snap: changed, obs: obs(state.Fail)},
			{snap: base, obs: obs(state.Pass)},
		}, 0},
		{"inferred failures are no evidence", nil, []step{
			{snap: base, obs: obs(state.Pass)},
			{snap: base, obs: map[string]*runner.Observation{unit: {Files: map[string]bool{"src/a.ts": true}, Status: state.Fail, Inferred: true}}},
			{snap: base, obs: obs(state.Pass)},
		}, 0},
		{"entries from before histories are not compared", func(st *state.State) {
			st.Suite("web").Tests[unit] = &state.Entry{Deps: map[string]string{"src/a.ts": "ha", unit: "ht"}, Status: state.Fail}
		}, []step{{snap: base, obs: obs(state.Pass)}}, 0},
		{"runs with runner arguments are no evidence", nil, []step{
			{snap: base, obs: obs(state.Pass)}, {snap: base, obs: obs(state.Fail), unrel: true}, {snap: base, obs: obs(state.Pass)},
		}, 0},
		{"failed attempt and passing retry in one run", nil, []step{
			{snap: base, obs: obs(state.Pass), retried: obs(state.Fail), wantFlip: "fail->pass"},
		}, 1},
		{"full runs keep history and evidence", nil, []step{
			{snap: base, obs: obs(state.Pass), full: true}, {snap: base, obs: obs(state.Fail), full: true, wantFlip: "pass->fail"},
			{snap: base, obs: obs(state.Fail), full: true},
		}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := state.New()
			if c.pre != nil {
				c.pre(st)
			}
			for i, s := range c.steps {
				now := t0.Add(time.Duration(i) * time.Hour)
				ev := Record(st, sr, s.obs, NewHasher(s.snap), RecordOptions{Commit: "c", Now: now, Full: s.full, Retried: s.retried, Unreliable: s.unrel})
				got := ""
				if len(ev) == 1 {
					got = ev[0].Flip
					if ev[0].Unit != unit || ev[0].Retried != (s.retried != nil) {
						t.Errorf("step %d: event %+v", i, ev[0])
					}
				} else if len(ev) > 1 {
					t.Fatalf("step %d: events %+v", i, ev)
				}
				if got != s.wantFlip {
					t.Errorf("step %d: flip %q, want %q", i, got, s.wantFlip)
				}
			}
			e := st.Suites["web"].Tests[unit]
			n := 0
			if e.Flaky != nil {
				n = e.Flaky.Count
			}
			if n != c.count {
				t.Errorf("flake count %d, want %d (%+v)", n, c.count, e.Flaky)
			}
			wantHist := len(c.steps)
			if c.steps[0].retried != nil {
				wantHist++
			}
			if len(e.History) != wantHist {
				t.Errorf("history %+v, want %d outcomes", e.History, wantHist)
			}
			// A failing unit must still be selected next time, flaky or not.
			if e.Status == state.Fail {
				tt := &Test{ID: unit}
				decide(tt, "state", e, []string{"src/a.ts", unit}, NewHasher(c.steps[len(c.steps)-1].snap), nil)
				if !tt.Selected || tt.Reason != ReasonFailed {
					t.Errorf("failing unit not selected: %+v", tt)
				}
			}
		})
	}
}

// TestRecordFlakeNewDependency: a dependency the previous trace did not record may have
// changed since; the flip is only evidence when the snapshot shows it did not.
func TestRecordFlakeNewDependency(t *testing.T) {
	const unit = "t.test.js"
	sr := &Suite{Name: "s"}
	snap := gitx.Snapshot{unit: "h1", "lib.js": "l2"}
	for _, c := range []struct {
		name     string
		recorded string // lib.js in the recorded snapshot
		want     int
	}{{"unchanged since the snapshot", "l2", 1}, {"changed since the snapshot", "l1", 0}} {
		t.Run(c.name, func(t *testing.T) {
			st := state.New()
			st.Files = map[string]string{unit: "h1", "lib.js": c.recorded}
			h := NewHasher(snap)
			Record(st, sr, map[string]*runner.Observation{unit: {Files: map[string]bool{unit: true}, Status: state.Pass}}, h, RecordOptions{Now: time.Unix(1, 0)})
			ev := Record(st, sr, map[string]*runner.Observation{unit: {Files: map[string]bool{unit: true, "lib.js": true}, Status: state.Fail}}, h, RecordOptions{Now: time.Unix(2, 0)})
			if len(ev) != c.want {
				t.Errorf("events %+v, want %d", ev, c.want)
			}
		})
	}
}

func TestRecordHistoryBounded(t *testing.T) {
	sr := &Suite{Name: "s"}
	st := state.New()
	h := NewHasher(gitx.Snapshot{"a.test.js": "h"})
	for i := range state.HistoryLen + 5 {
		Record(st, sr, map[string]*runner.Observation{"a.test.js": {Files: map[string]bool{"a.test.js": true}, Status: state.Pass}}, h, RecordOptions{Now: time.Unix(int64(i), 0)})
	}
	hist := st.Suites["s"].Tests["a.test.js"].History
	if len(hist) != state.HistoryLen || hist[len(hist)-1].At.Unix() != int64(state.HistoryLen+4) {
		t.Errorf("history %+v", hist)
	}
}
