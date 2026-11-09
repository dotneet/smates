package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/dotneet/smates/internal/selector"
)

const (
	// defaultShardTime is the recorded test time `select --max-shards` aims for per
	// shard (--shard-time).
	defaultShardTime = 10 * time.Minute
	// maxShardsLimit caps --max-shards: a GitHub Actions matrix has at most 256 jobs.
	maxShardsLimit = 256
)

// shardCount sizes the shards of a selection for a CI matrix (`select --max-shards`):
// 0 when nothing is selected, else the estimated time of the selected units divided by
// target, rounded up and kept within 1..maxShards. A selected unit without a recorded
// duration counts as the median recorded duration of its suite; when a suite with
// selected units has no recorded duration at all, nothing tells how long they take and
// the count is maxShards. The count only sets how many jobs share the selection: every
// selected unit runs in one of them, whatever the count.
func shardCount(res *selector.Result, maxShards int, target time.Duration) int {
	selected, _ := res.Counts()
	if selected == 0 {
		return 0
	}
	if maxShards <= 1 || target <= 0 {
		return 1
	}
	var total int64 // ms
	for _, sr := range res.Suites {
		ids := sr.Selected()
		if len(ids) == 0 {
			continue
		}
		var recorded []int64
		for _, t := range sr.Tests {
			if t.Duration > 0 {
				recorded = append(recorded, t.Duration)
			}
		}
		if len(recorded) == 0 {
			return maxShards
		}
		slices.Sort(recorded)
		median := recorded[len(recorded)/2]
		// The shards record: units that run at once while recording overlap.
		jobs := int64(recordParallelism(sr))
		for _, t := range sr.Tests {
			switch {
			case !t.Selected:
			case t.Duration > 0:
				total += t.Duration / jobs
			default:
				total += median / jobs
			}
		}
	}
	per := max(target.Milliseconds(), 1)
	n := int((total + per - 1) / per)
	return min(max(n, 1), maxShards)
}

// shardOutputs are the GitHub outputs of a shard count: `shards` and `shard-matrix`, a
// strategy.matrix whose "shard" values ("1/3", "2/3", "3/3") go to --shard as they are,
// so that every job of the matrix uses the same count.
func shardOutputs(n int) []string {
	shards := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		shards = append(shards, fmt.Sprintf("%d/%d", i, n))
	}
	m, _ := json.Marshal(map[string][]string{"shard": shards})
	return []string{fmt.Sprintf("shards=%d", n), "shard-matrix=" + string(m)}
}
