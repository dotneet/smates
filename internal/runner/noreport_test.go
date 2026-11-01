package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// stubNodeRunner is a jest / vitest stand-in (a script, so that smates does not take
// its text for user configuration): it fails (exit 9) when traced, appends a line to
// $COUNT per invocation ("1" with smates' reporter on its command line), and writes
// $RESULTS as reporter output when it has the reporter (and $RESULTS is set).
const stubNodeRunner = `rep=
for a; do case $a in --reporters=*jest-reporter.cjs|--reporter=*vitest-reporter.mjs) rep=1;; esac; done
[ -z "$NODE_OPTIONS$NODE_V8_COVERAGE" ] || exit 9
echo "x$rep" >> "$COUNT"
if [ -n "$rep" ] && [ -n "$RESULTS" ]; then printf "%s\n" "$RESULTS" > "$SMATES_TRACE_DIR/results-stub-1.jsonl"; fi
exit 1
`

// Without recording, jest and vitest get smates' reporter but no tracing, and the run
// reports per-file status.
func TestNodeRunnersReportWithoutRecording(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	t.Setenv("NODE_OPTIONS", "")
	t.Setenv("NODE_V8_COVERAGE", "")
	units := []string{"a.test.js", "b.test.js"}
	setup := func(t *testing.T, results bool) (*RunContext, string) {
		repo := tempRepo(t, append([]string{"node_modules/"}, units...)...)
		sh, err := trace.ExtractShims(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		stub := filepath.Join(t.TempDir(), "stub.sh")
		if err := os.WriteFile(stub, []byte(stubNodeRunner), 0o644); err != nil {
			t.Fatal(err)
		}
		count := filepath.Join(t.TempDir(), "count")
		env := map[string]string{"COUNT": count}
		if results {
			env["RESULTS"] = mustJSON(map[string]any{"k": "result", "id": filepath.Join(repo.Root, "a.test.js"), "status": "fail", "ms": 3, "msg": "boom"}) + "\n" +
				mustJSON(map[string]any{"k": "result", "id": filepath.Join(repo.Root, "b.test.js"), "status": "pass", "ms": 4})
		}
		var out bytes.Buffer
		rc := &RunContext{
			Repo: repo, Suite: &Suite{Suite: config.Suite{Root: ".", Command: "sh " + stub, Env: env}}, Units: units,
			TraceDir: t.TempDir(), ShimDir: sh.Dir, Stdout: &out, Stderr: &out,
			UnitOf: func(f string) string {
				if slices.Contains(units, f) {
					return f
				}
				return ""
			},
		}
		return rc, count
	}
	invocations := func(count string) []string {
		data, _ := os.ReadFile(count)
		return strings.Fields(string(data))
	}
	for _, a := range []Adapter{jest{}, vitest{}} {
		t.Run(a.Name(), func(t *testing.T) {
			rc, count := setup(t, true)
			res, err := a.Run(context.Background(), rc)
			if err != nil || res.ExitCode != 1 {
				t.Fatalf("run: %+v %v\n%s", res, err, rc.Stdout)
			}
			if got := invocations(count); !slices.Equal(got, []string{"x1"}) {
				t.Errorf("invocations (x1 = with the reporter) %v", got)
			}
			if a := res.Observations["a.test.js"]; a == nil || a.Status != state.Fail || a.Inferred || a.Failure != "boom" || len(a.Files) != 0 {
				t.Errorf("a: %+v", a)
			}
			if b := res.Observations["b.test.js"]; b == nil || b.Status != state.Pass || b.Inferred {
				t.Errorf("b: %+v", b)
			}
		})
		// A reporter that leaves no result in a failing run may have been rejected: the
		// run is repeated without it.
		t.Run(a.Name()+"/fallback", func(t *testing.T) {
			rc, count := setup(t, false)
			res, err := a.Run(context.Background(), rc)
			if err != nil || res.ExitCode != 1 {
				t.Fatalf("run: %+v %v", res, err)
			}
			if got := invocations(count); !slices.Equal(got, []string{"x1", "x"}) {
				t.Errorf("invocations (x1 = with the reporter) %v", got)
			}
			if len(res.Observations) != 0 || len(res.Warnings) != 1 {
				t.Errorf("observations %v warnings %v", res.Observations, res.Warnings)
			}
		})
		// Without a results directory nothing is injected.
		t.Run(a.Name()+"/no-results-dir", func(t *testing.T) {
			rc, count := setup(t, true)
			rc.TraceDir = ""
			res, err := a.Run(context.Background(), rc)
			if err != nil || res.ExitCode != 1 || len(res.Observations) != 0 {
				t.Fatalf("run: %+v %v", res, err)
			}
			if got := invocations(count); !slices.Equal(got, []string{"x"}) {
				t.Errorf("invocations (x1 = with the reporter) %v", got)
			}
		})
	}
}

// Without recording, go test's summary lines give each package's status.
func TestGoStatusWithoutRecording(t *testing.T) {
	repo := tempRepo(t, "calc/c.go", "util/u.go", "broken/b.go", "empty/e.go", "silent/s.go")
	os.WriteFile(filepath.Join(repo.Root, "go.mod"), []byte("module example.com/m\n"), 0o644)
	b := &buildFailures{}
	b.Write([]byte("--- FAIL: TestAdd (0.00s)\n    add_test.go:9: got 3, want 4\nFAIL\nFAIL\texample.com/m/calc\t0.012s\n" +
		"ok  \texample.com/m/util\t(cached)\n" +
		"# example.com/m/broken\nbroken/b.go:3:2: undefined: x\nFAIL\texample.com/m/broken [build failed]\n" +
		"?   \texample.com/m/empty\t[no test files]\nFAIL\n"))
	rc := &RunContext{Repo: repo, Suite: &Suite{Suite: config.Suite{Root: "."}}, Units: []string{"calc", "util", "broken", "empty", "silent"}}
	obs := b.observations(rc)
	for u, want := range map[string]string{"calc": state.Fail, "util": state.Pass, "broken": state.Fail} {
		if o := obs[u]; o == nil || o.Status != want || o.Inferred {
			t.Errorf("%s: %+v", u, o)
		}
	}
	if o := obs["calc"]; o == nil || !strings.Contains(o.Failure, "got 3, want 4") {
		t.Errorf("calc failure: %+v", o)
	}
	if obs["empty"] != nil || obs["silent"] != nil {
		t.Errorf("packages without a status: %v %v", obs["empty"], obs["silent"])
	}
}

// Without recording, custom and bun suites get per-file status from a JUnit report.
func TestReportedJUnitWithoutRecording(t *testing.T) {
	repo := tempRepo(t, "a_test.sh", "b_test.sh", "junit.xml")
	os.WriteFile(filepath.Join(repo.Root, "junit.xml"), []byte(`<testsuite><testcase file="a_test.sh" name="x"><failure message="boom"/></testcase><testcase file="b_test.sh" name="y"/></testsuite>`), 0o644)
	rc := &RunContext{
		Repo: repo, Suite: &Suite{Suite: config.Suite{Root: ".", Junit: "junit.xml"}}, TraceDir: t.TempDir(),
		UnitOf: func(f string) string {
			if strings.HasSuffix(f, "_test.sh") {
				return f
			}
			return ""
		},
	}
	obs, warns := reported(rc)
	if a := obs["a_test.sh"]; a == nil || a.Status != state.Fail || !strings.Contains(a.Failure, "boom") {
		t.Errorf("a: %+v", a)
	}
	if b := obs["b_test.sh"]; b == nil || b.Status != state.Pass {
		t.Errorf("b: %+v", b)
	}
	if len(warns) != 0 {
		t.Errorf("warnings %v", warns)
	}
	rc.TraceDir = ""
	if obs, _ := reported(rc); obs != nil {
		t.Errorf("no results directory: %v", obs)
	}
}
