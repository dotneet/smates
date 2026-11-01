package state

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunsRoundTrip(t *testing.T) {
	in := sample()
	in.AddRun(Run{ID: "r1", At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Commit: "abc", Branch: "main", CI: "github", Mode: "state", Selected: 3, Total: 10,
		SkippedMs: 4200, Untimed: 1, ElapsedMs: 900, Orphans: map[string]int{"agent": 2}, Agent: &AgentCall{Model: "m", InputTokens: 100, CostUSD: 0.01}, Flaky: 1})
	var buf bytes.Buffer
	if err := in.Write(&buf, true); err != nil {
		t.Fatal(err)
	}
	out, err := Read(&buf, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.Runs, out.Runs) {
		t.Errorf("runs: got %+v want %+v", out.Runs, in.Runs)
	}
}

func TestRunsOldFormat(t *testing.T) {
	// A state written before run records existed has no "runs" key.
	var buf bytes.Buffer
	if err := sample().Write(&buf, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"runs"`) {
		t.Fatalf("empty runs are written: %s", buf.String())
	}
	st, err := Read(&buf, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Runs) != 0 {
		t.Errorf("runs: %v", st.Runs)
	}
}

func TestAddRunBounds(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New()
	for i := range MaxRuns + 5 {
		s.AddRun(Run{ID: fmt.Sprint(i), At: t0.Add(time.Duration(i) * time.Minute)})
	}
	if len(s.Runs) != MaxRuns || s.Runs[0].ID != "5" || s.Runs[len(s.Runs)-1].ID != fmt.Sprint(MaxRuns+4) {
		t.Errorf("count bound: %d runs, %s..%s", len(s.Runs), s.Runs[0].ID, s.Runs[len(s.Runs)-1].ID)
	}

	s = New()
	s.AddRun(Run{ID: "old", At: t0})
	s.AddRun(Run{ID: "kept", At: t0.Add(MaxRunAge)})
	s.AddRun(Run{ID: "new", At: t0.Add(MaxRunAge + time.Hour)})
	if len(s.Runs) != 2 || s.Runs[0].ID != "kept" {
		t.Errorf("age bound: %+v", s.Runs)
	}
}

func TestMergeRuns(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	shared := Run{ID: "base", At: t0}
	a, b := New(), New()
	a.AddRun(shared)
	b.AddRun(shared)
	a.AddRun(Run{ID: "shard1", At: t0.Add(2 * time.Minute), Shard: "1/2"})
	b.AddRun(Run{ID: "shard2", At: t0.Add(time.Minute), Shard: "2/2"})
	a.Merge(b)
	var ids []string
	for _, r := range a.Runs {
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != "base,shard2,shard1" {
		t.Errorf("merged runs: %s", got)
	}
	// Merging an old state (no runs) keeps ours.
	a.Merge(New())
	if len(a.Runs) != 3 {
		t.Errorf("after merging an empty state: %d runs", len(a.Runs))
	}
}
