package selector

import (
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

func envFixture(t *testing.T) *fixture {
	f := newFixture(t, map[string]string{
		"test/region.test.js": `process.env.APP_REGION`,
		"test/plain.test.js":  `1`,
	})
	f.suites = []*runner.Suite{suite(t, config.Suite{Name: "node", Runner: "node", Tests: []string{"test/*.test.js"}})}
	return f
}

func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "") // restores the old value after the test
	os.Unsetenv(name)
}

func TestSelectEnvChanged(t *testing.T) {
	f := envFixture(t)
	t.Setenv("APP_REGION", "eu")
	st := f.record(map[string][]string{"test/region.test.js": {"$APP_REGION"}}, nil)
	if v := st.Suites["node"].Tests["test/region.test.js"].Deps["$APP_REGION"]; v != EnvHash("eu", true) || strings.Contains(v, "eu") {
		t.Fatalf("recorded %q", v)
	}
	if d := decisions(f.sel(st, nil)); d["test/region.test.js"] != "-unaffected" || d["test/plain.test.js"] != "-unaffected" {
		t.Fatalf("same value: %v", d)
	}
	for _, tc := range []struct {
		name string
		set  func()
	}{
		{"changed", func() { t.Setenv("APP_REGION", "us") }},
		{"empty", func() { t.Setenv("APP_REGION", "") }},
		{"unset", func() { unsetenv(t, "APP_REGION") }},
	} {
		tc.set()
		res := f.sel(st, nil)
		if d := decisions(res); d["test/region.test.js"] != ReasonChanged || d["test/plain.test.js"] != "-unaffected" {
			t.Errorf("%s: %v", tc.name, d)
		}
		tt := res.Suites[0].Tests[1]
		if tt.ID != "test/region.test.js" || !reflect.DeepEqual(tt.Changed, []string{"$APP_REGION"}) || tt.Via["$APP_REGION"] != ViaTraced {
			t.Errorf("%s: %+v", tc.name, tt)
		}
		if len(res.Orphans) != 0 || len(res.Changed) != 0 {
			t.Errorf("%s: an environment variable is not a changed file: %v %v", tc.name, res.Changed, res.Orphans)
		}
	}

	// Unset when recorded: setting it (even to "") selects the test.
	unsetenv(t, "APP_REGION")
	st = f.record(map[string][]string{"test/region.test.js": {"$APP_REGION"}}, nil)
	if v, ok := st.Suites["node"].Tests["test/region.test.js"].Deps["$APP_REGION"]; !ok || v != "" {
		t.Fatalf("unset variable recorded as %q, %v", v, ok)
	}
	if d := decisions(f.sel(st, nil)); d["test/region.test.js"] != "-unaffected" {
		t.Errorf("still unset: %v", d)
	}
	t.Setenv("APP_REGION", "")
	if d := decisions(f.sel(st, nil)); d["test/region.test.js"] != ReasonChanged {
		t.Errorf("set to empty: %v", d)
	}
}

func TestSelectEnvInputs(t *testing.T) {
	const name, key = "DB_IMAGE_DIGEST", "$DB_IMAGE_DIGEST"
	cases := []struct {
		name, value, want string
		editState         func(*state.State)
		unset, override   bool
	}{
		{name: "unchanged", value: "revision-a"},
		{name: "suite override", value: "revision-b", override: true},
		{name: "changed", value: "revision-b", want: "differs from the value recorded"},
		{name: "unset", unset: true, want: "is missing or empty"},
		{name: "empty", want: "is missing or empty"},
		{name: "blank", value: " \t", want: "is missing or empty"},
		{name: "one entry predates env_inputs", value: "revision-a", want: "has no recorded value", editState: func(st *state.State) {
			delete(st.Suites["node"].Tests["test/plain.test.js"].Deps, key)
		}},
		{name: "one test was never recorded", value: "revision-a", want: "has no recorded value", editState: func(st *state.State) {
			delete(st.Suites["node"].Tests, "test/plain.test.js")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := envFixture(t)
			f.suites[0].EnvInputs = []string{name}
			f.cfg.EnvIgnore = []string{"*"} // explicit inputs still apply
			f.write("other/example.test.js", "1")
			f.suites = append(f.suites, suite(t, config.Suite{Name: "other", Runner: "node", Root: "other", Tests: []string{"*.test.js"}}))
			t.Setenv(name, "revision-a")
			if c.override {
				f.suites[0].Env = map[string]string{name: "revision-a"}
			}
			st := f.record(nil, nil)
			for id, entry := range st.Suites["node"].Tests {
				if got := entry.Deps[key]; got != EnvHash("revision-a", true) {
					t.Errorf("%s: recorded %q, want the hash of the declared input", id, got)
				}
			}
			if c.editState != nil {
				c.editState(st)
			}
			if c.unset {
				unsetenv(t, name)
			} else {
				t.Setenv(name, c.value)
			}
			res := f.sel(st, nil)
			for _, unit := range res.Suites[0].Tests {
				if c.want == "" {
					if unit.Selected {
						t.Errorf("%s: unexpectedly selected: %+v", unit.ID, unit)
					}
				} else if !unit.Selected || unit.Via[key] != ViaInput || !strings.Contains(unit.Detail, c.want) {
					t.Errorf("%s: %+v, want selection because %s %s", unit.ID, unit, key, c.want)
				}
			}
			if d := decisions(res)["other/example.test.js"]; d != "-unaffected" {
				t.Errorf("other suite: %s, want unaffected", d)
			}
			if c.want != "" && (!res.Suites[0].All || !strings.Contains(strings.Join(res.Notes, "\n"), c.want)) {
				t.Errorf("want whole suite and a note: %+v", res)
			}
			if len(res.Changed) != 0 || len(res.Orphans) != 0 {
				t.Errorf("an external revision is not a changed file: %v %v", res.Changed, res.Orphans)
			}
		})
	}
}

func TestSelectEnvInputsWithoutState(t *testing.T) {
	f := envFixture(t)
	f.suites[0].EnvInputs = []string{"DB_IMAGE_DIGEST"}
	t.Setenv("DB_IMAGE_DIGEST", "revision-a")
	repo, base := gitFixture(t, f)
	res := f.sel(state.New(), func(in *Input) { in.Repo, in.Base = repo, base })
	if res.Mode != ModeDiff || !res.Suites[0].All {
		t.Fatalf("want the whole suite with no recorded value in diff mode: %+v", res)
	}
}

func TestSelectEnvIgnore(t *testing.T) {
	f := envFixture(t)
	t.Setenv("APP_REGION", "eu")
	st := f.record(map[string][]string{"test/region.test.js": {"$APP_REGION"}}, nil)
	t.Setenv("APP_REGION", "us")
	// A variable listed in env_ignore after it was recorded no longer selects the test.
	f.cfg.EnvIgnore = []string{"APP_*"}
	if d := decisions(f.sel(st, nil)); d["test/region.test.js"] != "-unaffected" {
		t.Errorf("top-level env_ignore: %v", d)
	}
	f.cfg.EnvIgnore = nil
	f.suites[0].EnvIgnore = []string{"APP_REGION"}
	if d := decisions(f.sel(st, nil)); d["test/region.test.js"] != "-unaffected" {
		t.Errorf("suite env_ignore: %v", d)
	}
	f.suites[0].EnvIgnore = nil
	if d := decisions(f.sel(st, nil)); d["test/region.test.js"] != ReasonChanged {
		t.Errorf("tracked again: %v", d)
	}
}

// TestSelectEnvSuiteOverride: a variable the suite sets in smates.yml is what its tests
// see; the outer value does not matter.
func TestSelectEnvSuiteOverride(t *testing.T) {
	f := envFixture(t)
	f.suites[0].Env = map[string]string{"APP_REGION": "fixed"}
	t.Setenv("APP_REGION", "eu")
	st := state.New()
	res := f.sel(st, func(in *Input) { in.All = true })
	h := NewHasher(f.snap())
	obs := map[string]*runner.Observation{"test/region.test.js": {Files: map[string]bool{"$APP_REGION": true}, Status: state.Pass}}
	Record(st, res.Suites[0], obs, h.ForSuite(f.suites[0]), RecordOptions{Now: time.Unix(1, 0)})
	st.Files = map[string]string(f.snap())
	if got := st.Suites["node"].Tests["test/region.test.js"].Deps["$APP_REGION"]; got != EnvHash("fixed", true) {
		t.Fatalf("recorded %q", got)
	}
	t.Setenv("APP_REGION", "us")
	if d := decisions(f.sel(st, nil)); d["test/region.test.js"] != "-unaffected" {
		t.Errorf("outer value changed: %v", d)
	}
	f.suites[0].Env["APP_REGION"] = "other"
	if d := decisions(f.sel(st, nil)); d["test/region.test.js"] != ReasonChanged {
		t.Errorf("suite value changed: %v", d)
	}
}

func TestHasherEnv(t *testing.T) {
	h := NewHasher(gitx.Snapshot{})
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	if got := h.Hash("$ANTHROPIC_API_KEY"); got != "" {
		t.Errorf("tests never see agent credentials: %q", got)
	}
	if got := h.ForSuite(suite(t, config.Suite{Runner: "node", Env: map[string]string{"ANTHROPIC_API_KEY": "k"}})).Hash("$ANTHROPIC_API_KEY"); got != EnvHash("k", true) {
		t.Errorf("unless the suite sets them: %q", got)
	}
	if EnvHash("", false) != "" || EnvHash("", true) == "" || EnvHash("a", true) == EnvHash("b", true) || EnvHash("a", true) != EnvHash("a", true) {
		t.Error("EnvHash")
	}
}

func TestDropIgnoredEnv(t *testing.T) {
	cfg := &config.Config{EnvIgnore: []string{"BUILD_TAG"}}
	su := suite(t, config.Suite{Runner: "node", EnvTrack: []string{"LANG"}})
	r := &runner.RunResult{
		Observations: map[string]*runner.Observation{"a.test.js": {Files: map[string]bool{"src/a.js": true, "$HOME": true, "$LANG": true, "$BUILD_TAG": true, "$TZ": true}}},
		ReadsBy:      map[string]map[string]bool{"src/config.js": {"$PATH": true, "$DATABASE_URL": true, "data.json": true}},
	}
	DropIgnoredEnv(cfg, su, r)
	if got := slices.Sorted(maps.Keys(r.Observations["a.test.js"].Files)); !reflect.DeepEqual(got, []string{"$LANG", "$TZ", "src/a.js"}) {
		t.Errorf("observed %v", got)
	}
	if got := slices.Sorted(maps.Keys(r.ReadsBy["src/config.js"])); !reflect.DeepEqual(got, []string{"$DATABASE_URL", "data.json"}) {
		t.Errorf("reads by %v", got)
	}
}

// TestRecordFlakeEnv: a flip after an environment variable changed is not a flake; a
// variable read for the first time cannot prove the inputs were the same.
func TestRecordFlakeEnv(t *testing.T) {
	const unit = "t.test.js"
	sr := &Suite{Name: "s"}
	snap := gitx.Snapshot{unit: "h1"}
	run := func(st *state.State, at int64, status string, files ...string) []FlakeEvent {
		o := &runner.Observation{Files: map[string]bool{unit: true}, Status: status}
		for _, f := range files {
			o.Files[f] = true
		}
		return Record(st, sr, map[string]*runner.Observation{unit: o}, NewHasher(snap), RecordOptions{Now: time.Unix(at, 0)})
	}
	t.Setenv("APP_REGION", "eu")
	st := state.New()
	st.Files = map[string]string{unit: "h1"}
	run(st, 1, state.Pass, "$APP_REGION")
	if ev := run(st, 2, state.Fail, "$APP_REGION"); len(ev) != 1 {
		t.Errorf("same value: events %+v", ev)
	}
	t.Setenv("APP_REGION", "us")
	if ev := run(st, 3, state.Pass, "$APP_REGION"); len(ev) != 0 {
		t.Errorf("changed value: events %+v", ev)
	}
	st = state.New()
	st.Files = map[string]string{unit: "h1"}
	run(st, 1, state.Pass)
	if ev := run(st, 2, state.Fail, "$APP_REGION"); len(ev) != 0 {
		t.Errorf("newly read variable: events %+v", ev)
	}
}

// TestRecordNoTrace: an outcome a runner reported without tracing the unit (gradle
// before recording) records a pass as unknown, so the unit runs again; a failure and
// an unknown stay as they are.
func TestRecordNoTrace(t *testing.T) {
	sr := &Suite{Name: "s", Known: map[string][]string{"A.java": {"A.java", "Lib.java"}}}
	snap := gitx.Snapshot{"A.java": "a", "Lib.java": "l"}
	for _, c := range []struct{ status, want string }{
		{state.Pass, state.Unknown},
		{state.Fail, state.Fail},
		{state.Unknown, state.Unknown},
	} {
		st := state.New()
		Record(st, sr, map[string]*runner.Observation{"A.java": {Status: c.status, NoTrace: true}}, NewHasher(snap), RecordOptions{Now: time.Unix(1, 0)})
		e := st.Suite("s").Tests["A.java"]
		if e.Status != c.want || e.Deps["Lib.java"] != "l" {
			t.Errorf("%s: recorded %+v, want status %s with the known deps", c.status, e, c.want)
		}
	}
}
