//go:build e2e

package e2e

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestEnv changes environment variables between runs of fixtures/env and checks that
// exactly the tests that read a tracked variable (Node, Go and Python) are selected,
// that ignored variables select nothing, and that --explain, `smates deps` and the
// failure hints name the variable.
func TestEnv(t *testing.T) {
	if !fixtureSelected("env") {
		t.Skip("SMATES_E2E_FIXTURE excludes env")
	}
	t.Parallel()
	r := prepare(t, "env", map[string]string{"APP_REGION": "eu", "BUILD_LABEL": "a", "GITHUB_RUN_ID": "1"})
	readers := []string{"go/region", "node/region.test.js", "py/test_region.py"}
	run := func(step string, want []string, args ...string) (*report, string) {
		t.Helper()
		out, _ := r.smates(append([]string{"run", "--no-agent", "--explain", "--report", r.reportPath()}, args...)...)
		rep := r.readReport()
		if got := slices.Sorted(maps.Keys(rep.selected())); !slices.Equal(got, want) {
			t.Errorf("%s: selected %v, want %v\n%s", step, got, want, tail(out, 40))
		}
		return rep, out
	}

	if _, out := run("same environment", nil); strings.Contains(out, "env changed") {
		t.Errorf("same environment:\n%s", tail(out, 40))
	}

	r.spec.Env["APP_REGION"] = "us"
	if _, out := run("changed value", readers); !strings.Contains(out, "node/region.test.js  [env changed: APP_REGION]") {
		t.Errorf("--explain does not name the variable:\n%s", tail(out, 40))
	}
	run("recorded again", nil)

	delete(r.spec.Env, "APP_REGION")
	run("unset", readers)
	r.spec.Env["APP_REGION"] = ""
	run("set to empty", readers)

	// env_ignore (BUILD_LABEL) and the default list (GITHUB_*) select nothing.
	r.spec.Env["BUILD_LABEL"], r.spec.Env["GITHUB_RUN_ID"] = "b", "2"
	run("ignored variables", nil)

	out, code := r.smates("deps", "node/region.test.js")
	if code != 0 || !strings.Contains(out, "$APP_REGION\trecorded") {
		t.Errorf("smates deps (exit %d):\n%s", code, out)
	}
	out, _ = r.smates("why", "$APP_REGION")
	for _, id := range readers {
		if !strings.Contains(out, "\t"+id+"\t(recorded)") {
			t.Errorf("smates why $APP_REGION does not list %s:\n%s", id, out)
		}
	}
	if strings.Contains(out, "plain") {
		t.Errorf("smates why $APP_REGION lists a test that does not read it:\n%s", out)
	}
	if out, _ = r.smates("deps", "node/labels.test.js"); strings.Contains(out, "$BUILD_LABEL") || strings.Contains(out, "$GITHUB_RUN_ID") {
		t.Errorf("ignored variables recorded:\n%s", out)
	}

	// A failure after the variable changed points at it.
	r.spec.Env["APP_REGION"] = "invalid"
	rep, out := run("failing", readers)
	if rep.Exit == 0 {
		t.Fatalf("APP_REGION=invalid must fail node/region.test.js:\n%s", tail(out, 40))
	}
	h := rep.hint("node/region.test.js")
	if h == nil || len(h.Candidates) == 0 || h.Candidates[0].Path != "$APP_REGION" || h.Candidates[0].Kind != "env" {
		t.Errorf("hint %+v\n%s", h, tail(out, 40))
	}
	if !strings.Contains(out, "$APP_REGION  (environment variable, read in its last run, changed)") {
		t.Errorf("run log does not show the hint:\n%s", tail(out, 60))
	}
}
