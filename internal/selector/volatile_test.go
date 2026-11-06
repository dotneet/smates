package selector

import (
	"reflect"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// A unit that skipped itself last time is selected again (unproven); volatile units
// run every time; max_age expires a recorded run. None of them hides another reason.
func TestRunAnyway(t *testing.T) {
	recorded := time.Unix(1, 0) // fixture.record's time
	for _, c := range []struct {
		name     string
		volatile []string
		maxAge   time.Duration
		now      time.Time
		status   map[string]string // recorded status (default pass)
		edit     bool              // change b.test.js's source
		all      bool
		want     map[string]string
	}{
		{name: "nothing", want: map[string]string{"test/a.test.js": "-unaffected", "test/b.test.js": "-unaffected", "test/c.test.js": "-unaffected"}},
		{name: "skipped last run", status: map[string]string{"test/a.test.js": state.Skip},
			want: map[string]string{"test/a.test.js": ReasonUnproven, "test/b.test.js": "-unaffected", "test/c.test.js": "-unaffected"}},
		{name: "skipped, then changed", status: map[string]string{"test/b.test.js": state.Skip}, edit: true,
			want: map[string]string{"test/a.test.js": "-unaffected", "test/b.test.js": ReasonChanged, "test/c.test.js": "-unaffected"}},
		{name: "volatile file", volatile: []string{"test/a.test.js"},
			want: map[string]string{"test/a.test.js": ReasonVolatile, "test/b.test.js": "-unaffected", "test/c.test.js": "-unaffected"}},
		{name: "volatile glob", volatile: []string{"**/[ab].test.js"},
			want: map[string]string{"test/a.test.js": ReasonVolatile, "test/b.test.js": ReasonVolatile, "test/c.test.js": "-unaffected"}},
		{name: "volatile, changed", volatile: []string{"test/b.test.js"}, edit: true,
			want: map[string]string{"test/a.test.js": "-unaffected", "test/b.test.js": ReasonChanged, "test/c.test.js": "-unaffected"}},
		{name: "volatile, failed", volatile: []string{"test/a.test.js"}, status: map[string]string{"test/a.test.js": state.Fail},
			want: map[string]string{"test/a.test.js": ReasonFailed, "test/b.test.js": "-unaffected", "test/c.test.js": "-unaffected"}},
		{name: "volatile, all", volatile: []string{"test/a.test.js"}, all: true,
			want: map[string]string{"test/a.test.js": ReasonAll, "test/b.test.js": ReasonAll, "test/c.test.js": ReasonAll}},
		{name: "max_age not reached", maxAge: 2 * time.Hour, now: recorded.Add(time.Hour),
			want: map[string]string{"test/a.test.js": "-unaffected", "test/b.test.js": "-unaffected", "test/c.test.js": "-unaffected"}},
		{name: "max_age reached", maxAge: 30 * time.Minute, now: recorded.Add(time.Hour), edit: true,
			want: map[string]string{"test/a.test.js": ReasonExpired, "test/b.test.js": ReasonChanged, "test/c.test.js": ReasonExpired}},
		{name: "max_age, current time", maxAge: time.Hour,
			want: map[string]string{"test/a.test.js": ReasonExpired, "test/b.test.js": ReasonExpired, "test/c.test.js": ReasonExpired}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{
				"test/a.test.js": `1`,
				"test/b.test.js": `2`,
				"test/c.test.js": `3`,
			})
			f.suites = []*runner.Suite{suite(t, config.Suite{Name: "node", Runner: "node", Tests: []string{"test/*.test.js"}, Volatile: c.volatile, MaxAge: c.maxAge})}
			st := f.record(nil, c.status)
			if c.edit {
				f.write("test/b.test.js", `22`)
			}
			res := f.sel(st, func(in *Input) { in.Now, in.All = c.now, c.all })
			if got := decisions(res); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%v, want %v", got, c.want)
			}
		})
	}
}

// volatile names Go test functions by unit, package or file; their compile checks are
// not volatile. A unit with no recorded run is new, whatever max_age says.
func TestRunAnywayGoFunc(t *testing.T) {
	for _, c := range []struct {
		volatile []string
		want     []string
	}{
		{[]string{"pkg/app/repo#TestRepo"}, []string{"pkg/app/repo#TestRepo"}},
		{[]string{"pkg/app/repo"}, []string{"pkg/app/repo#TestRepo"}},
		{[]string{"**/svc_test.go"}, []string{"pkg/app/svc#TestSvc"}},
		{[]string{"pkg/app/*#Test*"}, []string{"pkg/app/envdb#TestEnvDB", "pkg/app/pure#TestPure", "pkg/app/repo#TestRepo", "pkg/app/split#TestSplit", "pkg/app/svc#TestSvc"}},
	} {
		f := reachesFixture(t, config.GranularityFunc)
		f.suites = f.suites[:1]
		f.suites[0].Volatile = c.volatile
		res := f.sel(reachesRecord(f, nil), nil)
		var got []string
		for _, tt := range res.Suites[0].Tests {
			if tt.Selected {
				if tt.Reason != ReasonVolatile {
					t.Errorf("%v: %s selected: %s", c.volatile, tt.ID, tt.Reason)
				}
				got = append(got, tt.ID)
			}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: %v, want %v", c.volatile, got, c.want)
		}
	}
	f := envFixture(t)
	f.suites[0].MaxAge = time.Nanosecond
	st := f.record(nil, nil)
	f.write("test/new.test.js", `4`)
	if d := decisions(f.sel(st, nil)); d["test/new.test.js"] != ReasonNew || d["test/plain.test.js"] != ReasonExpired {
		t.Errorf("max_age with a unit not recorded yet: %v", d)
	}
}
