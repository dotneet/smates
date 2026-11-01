package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunFlaky drives flake detection through Main: a test whose outcome depends on a
// file outside the repository (invisible to tracing) flips while every tracked input
// stays the same. The custom runner writes a JUnit report so outcomes are per file.
func TestRunFlaky(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	clearCI(t)
	noAgentCredentials(t)
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	tmp := t.TempDir()
	coin := filepath.Join(tmp, "coin")
	summary := filepath.Join(tmp, "summary.md")
	outputs := filepath.Join(tmp, "outputs")
	t.Setenv("SMATES_COIN", coin)
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	t.Setenv("GITHUB_OUTPUT", outputs)
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	config := `version: 1
orphans: ignore
suites:
  - name: py
    runner: custom
    tests: ["tests/*_check.py"]
    command: python3 run.py
    junit: .smates/junit.xml
`
	write("smates.yml", config)
	write(".gitignore", ".smates/\n")
	write("run.py", `import os, subprocess, sys
files = sys.argv[1:] or sorted("tests/" + f for f in os.listdir("tests") if f.endswith("_check.py"))
cases, failed = [], False
for f in files:
    ok = subprocess.call([sys.executable, f]) == 0
    failed = failed or not ok
    cases.append('<testcase file="%s" name="%s">%s</testcase>' % (f, f, "" if ok else "<failure/>"))
os.makedirs(".smates", exist_ok=True)
open(".smates/junit.xml", "w").write("<testsuite>" + "".join(cases) + "</testsuite>")
# An error no test file reports (e.g. an unhandled error after a's tests) while the
# marker exists.
leak = os.environ["SMATES_COIN"] + ".leak"
failed = failed or ("tests/a_check.py" in files and os.path.exists(leak))
sys.exit(1 if failed else 0)
`)
	write("tests/a_check.py", "assert 1 + 1 == 2\n")
	// c fails while the coin file says so; "fail-once" fails a single time.
	write("tests/c_check.py", `import os
p = os.environ["SMATES_COIN"]
v = open(p).read() if os.path.exists(p) else ""
if v == "fail-once":
    open(p, "w").write("pass")
assert not v.startswith("fail"), v
`)
	g.git("add", "-A")
	g.commit("fixture")

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		common := []string{"-C", g.dir}
		if args[0] == "run" {
			common = append(common, "--no-agent")
		}
		code := Main(append(args[:1:1], append(common, args[1:]...)...), &out, &errb)
		return code, out.String(), errb.String()
	}
	report := filepath.Join(tmp, "report.json")
	flakesIn := func() []flakeReport {
		t.Helper()
		var rep struct {
			Flaky []flakeReport `json:"flaky"`
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &rep); err != nil {
			t.Fatal(err)
		}
		return rep.Flaky
	}
	known := func() []flakyUnit {
		t.Helper()
		code, out, errs := run("flaky", "--format", "json")
		if code != 0 {
			t.Fatalf("flaky: %d %s", code, errs)
		}
		var js struct {
			Flaky []flakyUnit `json:"flaky"`
		}
		if err := json.Unmarshal([]byte(out), &js); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return js.Flaky
	}

	if code, _, errs := run("run", "--all", "--report", report); code != 0 || len(flakesIn()) != 0 {
		t.Fatalf("baseline: %d %v\n%s", code, flakesIn(), errs)
	}

	// The outcome flips on a full run with identical inputs: flaky, and still a failure.
	os.WriteFile(coin, []byte("fail-once"), 0o644)
	code, _, errs := run("run", "--all", "--report", report)
	if code == 0 {
		t.Fatalf("a flaky failure must fail the run\n%s", errs)
	}
	if f := flakesIn(); len(f) != 1 || f[0].Unit != "tests/c_check.py" || f[0].Flip != "pass->fail" || f[0].Count != 1 || f[0].Retried {
		t.Fatalf("flakes %+v\n%s", f, errs)
	}
	if !strings.Contains(errs, "FLAKY: 1 test unit(s)") {
		t.Errorf("run log:\n%s", errs)
	}
	if data, _ := os.ReadFile(summary); !strings.Contains(string(data), "1 flaky test unit(s)") || !strings.Contains(string(data), "`py: tests/c_check.py` — pass->fail") {
		t.Errorf("summary:\n%s", data)
	}
	if data, _ := os.ReadFile(outputs); !strings.Contains(string(data), "flaky=1\n") || !strings.Contains(string(data), `flaky-tests=["py: tests/c_check.py"]`) {
		t.Errorf("outputs:\n%s", data)
	}

	// The failure is selected again as usual; it passes now, a second flip.
	code, _, errs = run("run", "--report", report)
	if code != 0 {
		t.Fatalf("rerun: %d\n%s", code, errs)
	}
	if f := flakesIn(); len(f) != 1 || f[0].Flip != "fail->pass" || f[0].Count != 2 {
		t.Fatalf("flakes after rerun %+v\n%s", f, errs)
	}
	k := known()
	if len(k) != 1 || k[0].Unit != "tests/c_check.py" || k[0].Count != 2 || strings.Join(k[0].Recent, " ") != "pass fail pass" {
		t.Fatalf("smates flaky: %+v", k)
	}
	if _, out, _ := run("flaky"); !strings.Contains(out, "py\ttests/c_check.py\t2\tfail->pass\t") {
		t.Errorf("text output:\n%s", out)
	}

	// Opt-in retry: a known flaky unit that fails and then passes does not fail the run.
	write("smates.yml", config+"flaky:\n  retry: 1\n")
	os.WriteFile(coin, []byte("fail-once"), 0o644)
	code, _, errs = run("run", "--all", "--report", report)
	if code != 0 {
		t.Fatalf("retried flake must not fail the run: %d\n%s", code, errs)
	}
	// Editing flaky.retry does not change any test's inputs (it is not part of the
	// suite's configuration fingerprint), so the failure is a flip too.
	if f := flakesIn(); len(f) != 2 || f[0].Flip != "pass->fail" || f[0].Count != 3 || f[1].Flip != "fail->pass" || !f[1].Retried || f[1].Count != 4 {
		t.Fatalf("flakes after retry %+v\n%s", f, errs)
	}
	if !strings.Contains(errs, "retrying after 1 known flaky test unit(s) failed") {
		t.Errorf("run log:\n%s", errs)
	}
	// The retry re-runs the whole run: an error the units do not report is not hidden
	// by a flaky unit that passes on retry.
	os.WriteFile(coin, []byte("fail-once"), 0o644)
	os.WriteFile(coin+".leak", nil, 0o644)
	if code, _, errs = run("run", "--all", "--report", report); code == 0 {
		t.Fatalf("an error outside the flaky unit must fail the run\n%s", errs)
	}
	os.Remove(coin + ".leak")
	// A failure that persists through the retry still fails the run.
	os.WriteFile(coin, []byte("fail"), 0o644)
	if code, _, errs = run("run", "--all", "--report", report); code == 0 {
		t.Fatalf("persistent failure must fail the run\n%s", errs)
	}

	// A retry whose runner cannot start is an environment error; in failures mode the
	// first attempt's output is still printed.
	write("run.sh", "#!/bin/sh\n[ \"$(cat \"$SMATES_COIN\")\" = fail-break ] && chmod -x \"$0\"\nexec python3 run.py \"$@\"\n")
	os.Chmod(filepath.Join(g.dir, "run.sh"), 0o755)
	write("smates.yml", strings.Replace(config, "python3 run.py", "./run.sh", 1)+"flaky:\n  retry: 1\n")
	os.WriteFile(coin, []byte("fail-break"), 0o644)
	code, out, errs := run("run", "--all", "--output", "failures")
	if code != ExitEnv || !strings.Contains(errs, "py: retry: ") || strings.Contains(errs, "py: py:") || !strings.Contains(out, "AssertionError: fail-break") {
		t.Errorf("retry that cannot start: exit %d\n%s\n--- stderr\n%s", code, out, errs)
	}
	write("smates.yml", config)

	if code, _, errs := run("flaky", "--clear"); code != 0 || !strings.Contains(errs, "cleared flake evidence of 1") {
		t.Fatalf("clear: %d %s", code, errs)
	}
	if k := known(); len(k) != 0 {
		t.Errorf("after clear: %+v", k)
	}
}
