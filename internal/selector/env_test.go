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
