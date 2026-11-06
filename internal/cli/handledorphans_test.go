package cli

import (
	"io"
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// A selective run that leaves suites out records the orphans it handled as seen by the
// suites it ran, but only by those the snapshot already holds a view of: a marker
// naming a suite no full recording covered would make it count as snapshotted
// (selector.Snapshotted), so that every marker it has not seen turns back into a change.
func TestMarkHandledOrphansScoped(t *testing.T) {
	web := &runner.Suite{Suite: config.Suite{Name: "web", Runner: "vitest", Root: "."}}
	api := &runner.Suite{Suite: config.Suite{Name: "api", Runner: "pytest", Root: "."}}
	tests := []struct {
		name    string
		commit  string
		files   map[string]string // the state's snapshot before the run
		left    []*runner.Suite
		want    string // Dockerfile's entry after
		apiSnap bool   // api counts as snapshotted after
	}{
		// Must keep api unsnapshotted: only web's full recording named it in markers.
		{name: "a suite no full recording covered", files: map[string]string{"Dockerfile": "?h0@web", "web/a.ts": "?h1@web"},
			left: []*runner.Suite{web}, want: "?h0@web"},
		{name: "a suite a full recording covered", files: map[string]string{"Dockerfile": "?h0@web", "web/a.ts": "?h1@api,web"},
			left: []*runner.Suite{web}, want: "h0", apiSnap: true}, // every owner saw it: a plain hash
		{name: "a full snapshot", commit: "c0", files: map[string]string{"Dockerfile": "h9", "web/a.ts": "h1"},
			left: []*runner.Suite{web}, want: "?h0@api", apiSnap: true},
		{name: "no suite left out", files: map[string]string{"Dockerfile": "?h9@web", "web/a.ts": "?h1@web"},
			want: "h0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := state.New()
			st.Commit = tt.commit
			st.Files = map[string]string{}
			for f, v := range tt.files {
				st.Files[f] = v
			}
			picked := []*runner.Suite{api}
			if tt.left == nil {
				picked = []*runner.Suite{api, web}
			}
			res := &selector.Result{Orphans: []string{"Dockerfile"}, OrphanHandling: map[string]string{"Dockerfile": "run"}}
			c := &cycle{p: &project{state: st, snap: map[string]string{"Dockerfile": "h0", "web/a.ts": "h1"}, log: io.Discard}, sel: &selection{res: res}}
			c.markHandledOrphans(picked, slices.Clone(tt.left))
			if got := st.Files["Dockerfile"]; got != tt.want {
				t.Errorf("Dockerfile %q, want %q", got, tt.want)
			}
			if got := selector.Snapshotted(st)("api"); got != tt.apiSnap {
				t.Errorf("api snapshotted %v, want %v", got, tt.apiSnap)
			}
		})
	}
}
