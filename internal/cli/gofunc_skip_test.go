package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// A test function that skips itself, or that its TestMain never runs, is recorded as
// skipped (not proven) and selected again; a subtest skip under a passing parent is a
// pass. Its suite still passes.
func TestGoFuncSkip(t *testing.T) {
	for _, command := range []string{"go test", "go test -v", "go test -count=2"} {
		t.Run(command, func(t *testing.T) {
			r := newGoFuncRepo(t, command)
			r.write("gate/gate_test.go", `package gate

import (
	"os"
	"testing"
)

func TestGated(t *testing.T) {
	if os.Getenv("GATE_SERVICE_UP") == "" {
		t.Skip("service unavailable")
	}
}

func TestSubSkip(t *testing.T) {
	t.Run("later", func(t *testing.T) { t.Skip("not yet") })
}

func TestPlain(t *testing.T) {}
`)
			r.write("early/main_test.go", `package early

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("EARLY_SERVICE_UP") == "" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestEarly(t *testing.T) {}
`)
			r.g.git("add", "-A")
			r.g.commit("skips")
			code, res, out := r.runReport("--all")
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			for u, want := range map[string]string{
				"gate#TestGated": state.Skip, "gate#TestSubSkip": state.Pass, "gate#TestPlain": state.Pass,
				"early#TestEarly": state.Skip, "calc#TestAdd": state.Pass,
			} {
				if res[u] != want {
					t.Errorf("%s: %q, want %q\n%s", u, res[u], want, out)
				}
			}
			if !strings.Contains(out, "2 skipped (not proven") {
				t.Errorf("no skipped count in the outcome:\n%s", out)
			}
			got := r.selected()
			if want := map[string]string{"gate#TestGated": "unproven", "early#TestEarly": "unproven"}; !mapsEqual(got, want) {
				t.Errorf("selected %v, want %v", got, want)
			}
			// Once the gate opens, they run, pass, and are proven.
			t.Setenv("GATE_SERVICE_UP", "1")
			t.Setenv("EARLY_SERVICE_UP", "1")
			if code, res, out := r.runReport(); code != 0 || res["gate#TestGated"] != state.Pass || res["early#TestEarly"] != state.Pass {
				t.Fatalf("gate open: %d %v\n%s", code, res, out)
			}
			if got := r.selected(); len(got) != 0 {
				t.Errorf("selected after passing: %v", got)
			}
		})
	}
}

// volatile units run every time; max_age expires recorded runs.
func TestGoFuncVolatileMaxAge(t *testing.T) {
	r := newGoFuncRepo(t, "go test")
	if code, _, out := r.runReport("--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, out)
	}
	base := "version: 1\norphans: ignore\nsuites:\n  - name: go\n    runner: go\n    granularity: func\n    command: go test\n"
	r.write("smates.yml", base+"    volatile: [\"calc#TestAdd\"]\n")
	if got := r.selected(); !mapsEqual(got, map[string]string{"calc#TestAdd": "volatile"}) {
		t.Errorf("volatile: %v", got)
	}
	r.write("smates.yml", base+"    max_age: 1ns\n")
	got := r.selected()
	if units := keys(got); !slices.Equal(units, []string{"calc#(build)", "calc#TestAdd", "calc#TestMul"}) || got["calc#TestAdd"] != "expired" {
		t.Errorf("max_age: %v", got)
	}
	r.write("smates.yml", base+"    max_age: 24h\n")
	if got := r.selected(); len(got) != 0 {
		t.Errorf("max_age not reached: %v", got)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
