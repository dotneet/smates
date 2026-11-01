package state

import (
	"sort"
	"time"
)

// Limits on State.Runs, so the state does not grow with every run: the newest MaxRuns
// records are kept, and none older than MaxRunAge before the newest one.
const (
	MaxRuns   = 1000
	MaxRunAge = 180 * 24 * time.Hour
)

// Run summarizes one recorded `smates run` (see `smates stats`).
type Run struct {
	// ID identifies the record across merged states.
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Commit string    `json:"commit,omitempty"`
	Branch string    `json:"branch,omitempty"`
	// CI is the CI system that ran it ("" locally).
	CI    string `json:"ci,omitempty"`
	Watch bool   `json:"watch,omitempty"`
	// Mode is the selection mode (state, diff or all).
	Mode   string `json:"mode"`
	Shadow bool   `json:"shadow,omitempty"`
	// Shard is "I/N" for a sharded run, whose counts cover the units this shard owns.
	Shard string `json:"shard,omitempty"`
	// Selected and Total count test units (in shadow mode: what the selection chose).
	Selected int `json:"selected"`
	Total    int `json:"total"`
	// SkippedMs is the recorded duration of the skipped units; Untimed counts the
	// skipped units that have no recorded duration.
	SkippedMs int64 `json:"skipped_ms,omitempty"`
	Untimed   int   `json:"untimed,omitempty"`
	ElapsedMs int64 `json:"elapsed_ms"`
	ExitCode  int   `json:"exit_code"`
	Failed    int   `json:"failed,omitempty"`
	// Escapes and Possible are the shadow-mode misses (see `smates run --shadow`).
	Escapes  int `json:"escapes,omitempty"`
	Possible int `json:"possible,omitempty"`
	// Orphans counts the unattributed changes by how they were handled.
	Orphans map[string]int `json:"orphans,omitempty"`
	Agent   *AgentCall     `json:"agent,omitempty"`
	// Flaky counts the units whose outcome flipped with unchanged inputs in this run.
	Flaky int `json:"flaky,omitempty"`
	// NotRun counts the selected units --fail-fast did not start after a failure.
	NotRun int `json:"not_run,omitempty"`
}

// AgentCall is the agent's part in a run. Cached decisions cost nothing.
type AgentCall struct {
	Model            string  `json:"model,omitempty"`
	Cached           bool    `json:"cached,omitempty"`
	Turns            int     `json:"turns,omitempty"`
	InputTokens      int     `json:"input_tokens,omitempty"`
	OutputTokens     int     `json:"output_tokens,omitempty"`
	CacheReadTokens  int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"` // when the backend reports it
}

// AddRun appends a run record, dropping the oldest beyond the limits.
func (s *State) AddRun(r Run) {
	s.Runs = boundRuns(append(s.Runs, r))
}

// mergeRuns unions two run lists by ID (states merged from shards or branches usually
// share their older records).
func mergeRuns(a, b []Run) []Run {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a))
	out := append([]Run{}, a...)
	for _, r := range a {
		seen[r.ID] = true
	}
	for _, r := range b {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return boundRuns(out)
}

// boundRuns orders runs by time and applies MaxRuns and MaxRunAge.
func boundRuns(runs []Run) []Run {
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].At.Before(runs[j].At) })
	if len(runs) > MaxRuns {
		runs = runs[len(runs)-MaxRuns:]
	}
	if len(runs) > 0 {
		cut := runs[len(runs)-1].At.Add(-MaxRunAge)
		i := sort.Search(len(runs), func(i int) bool { return !runs[i].At.Before(cut) })
		runs = runs[i:]
	}
	return runs
}
