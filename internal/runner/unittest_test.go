package runner

import (
	"bytes"
	"context"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
	"github.com/dotneet/smates/internal/trace"
)

func TestParseUnittestCommand(t *testing.T) {
	cases := []struct {
		cmd                 string
		launcher            []string
		opts                []string
		start, pattern, top string
		glob                string
		err                 bool
	}{
		{cmd: "python3 -m unittest", launcher: []string{"python3", "-m"}, start: ".", pattern: "test*.py", top: ".", glob: "**/test*.py"},
		{cmd: "uv run python -m unittest -v", launcher: []string{"uv", "run", "python", "-m"}, opts: []string{"-v"}, start: ".", pattern: "test*.py", top: ".", glob: "**/test*.py"},
		{cmd: "python -m unittest discover -s tests -p '*_test.py' -v", launcher: []string{"python", "-m"}, opts: []string{"-v"}, start: "tests", pattern: "*_test.py", top: "tests", glob: "tests/**/*_test.py"},
		{cmd: "python -m unittest discover --start-directory=src/tests --top-level-directory src -k fast", launcher: []string{"python", "-m"}, opts: []string{"-k", "fast"}, start: "src/tests", pattern: "test*.py", top: "src", glob: "src/tests/**/test*.py"},
		{cmd: "python -m unittest discover -s ./tests -t .", launcher: []string{"python", "-m"}, start: "tests", pattern: "test*.py", top: ".", glob: "tests/**/test*.py"},
		{cmd: "python -m unittest discover tests 'check_*.py' .", launcher: []string{"python", "-m"}, start: "tests", pattern: "check_*.py", top: ".", glob: "tests/**/check_*.py"},
		{cmd: "python -munittest -stests -b", launcher: []string{"python"}, opts: []string{"-stests", "-b"}, start: ".", pattern: "test*.py", top: ".", glob: "**/test*.py"},
		{cmd: "python -munittest discover -stests -b", launcher: []string{"python"}, opts: []string{"-b"}, start: "tests", pattern: "test*.py", top: "tests", glob: "tests/**/test*.py"},
		// Test names in the command are dropped: smates passes the selection.
		{cmd: "python3 -m unittest tests.test_a --locals", launcher: []string{"python3", "-m"}, opts: []string{"--locals"}, start: ".", pattern: "test*.py", top: ".", glob: "**/test*.py"},
		{cmd: "python -m unittest discover -s ../shared", launcher: []string{"python", "-m"}, start: "../shared", pattern: "test*.py", top: "../shared", glob: "**/test*.py"},
		{cmd: "python3 -m pytest", err: true},
		{cmd: "python3 run_tests.py", err: true},
	}
	for _, c := range cases {
		uc, err := parseUnittestCommand(c.cmd)
		if (err != nil) != c.err {
			t.Errorf("%q: err %v", c.cmd, err)
			continue
		}
		if c.err {
			continue
		}
		if !reflect.DeepEqual(uc.launcher, c.launcher) || !reflect.DeepEqual(uc.opts, c.opts) || uc.start != c.start || uc.pattern != c.pattern || uc.top != c.top || uc.testGlob() != c.glob {
			t.Errorf("%q: launcher %q opts %q start %q pattern %q top %q glob %q", c.cmd, uc.launcher, uc.opts, uc.start, uc.pattern, uc.top, uc.testGlob())
		}
	}
}

func TestUnittestModuleName(t *testing.T) {
	root, _ := parseUnittestCommand("python3 -m unittest")
	src, _ := parseUnittestCommand("python3 -m unittest discover -s src/tests -t src")
	cases := []struct {
		uc   *unittestCommand
		rel  string
		want string
		ok   bool
	}{
		{root, "test_a.py", "test_a", true},
		{root, "tests/unit/test_b.py", "tests.unit.test_b", true},
		{root, "tests/test-c.py", "", false},  // not an identifier
		{root, "2tests/test_c.py", "", false}, // not an identifier
		{root, "tests/test_c.txt", "", false}, // not a module
		{src, "src/tests/test_d.py", "tests.test_d", true},
		{src, "other/test_e.py", "", false}, // outside the top-level directory
		{src, "srcx/test_e.py", "", false},
	}
	for _, c := range cases {
		got, ok := c.uc.moduleName(c.rel)
		if got != c.want || ok != c.ok {
			t.Errorf("moduleName(%q, top %q) = %q, %v; want %q, %v", c.rel, c.uc.top, got, ok, c.want, c.ok)
		}
	}
}

func TestUnittestArgv(t *testing.T) {
	rc := &RunContext{ExtraArgs: []string{"-f"}}
	uc, _ := parseUnittestCommand("uv run python -m unittest discover -s tests -v")
	mods := []string{"tests.test_a", "tests.test_b"}
	if got, want := uc.argv(rc, false, mods), []string{"uv", "run", "python", "-m", "unittest", "-v", "-f", "tests.test_a", "tests.test_b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("plain: %q", got)
	}
	if got, want := uc.argv(rc, true, mods), []string{"uv", "run", "python", "-m", "smates_unittest", "-v", "-f", "tests.test_a", "tests.test_b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("record: %q", got)
	}
	uc, _ = parseUnittestCommand("python -munittest -b {tests} --locals")
	if got, want := uc.argv(rc, true, mods[:1]), []string{"python", "-msmates_unittest", "-b", "-f", "tests.test_a", "--locals"}; !reflect.DeepEqual(got, want) {
		t.Errorf("placeholder: %q", got)
	}
}

func TestUnittestDefaultsAndUnits(t *testing.T) {
	repo := tempRepo(t, "svc/.venv/bin/python", "svc/setup.py")
	got, err := Resolve(repo, []config.Suite{
		{Name: "a", Root: "svc", Runner: "unittest"},
		{Name: "b", Root: ".", Runner: "unittest", Command: "python3 -m unittest discover -s lib/tests -t lib -p 'check*.py'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := got[0], got[1]
	if a.Command != "./.venv/bin/python -m unittest" || !reflect.DeepEqual(a.Tests, []string{"**/test*.py"}) {
		t.Errorf("a: %q %v", a.Command, a.Tests)
	}
	if !reflect.DeepEqual(b.Tests, []string{"lib/tests/**/check*.py"}) {
		t.Errorf("b: %v", b.Tests)
	}
	files := []string{"lib/tests/check_a.py", "lib/tests/sub/check_b.py", "lib/tests/check-c.py", "lib/tests/helper.py", "lib/tests/.venv/check_d.py"}
	if units := b.Adapter.Units(b, b.TestFiles(files)); !reflect.DeepEqual(units, []string{"lib/tests/check_a.py", "lib/tests/sub/check_b.py"}) {
		t.Errorf("units %v", units)
	}
	if _, err := Resolve(repo, []config.Suite{{Name: "u", Root: ".", Runner: "unittest", Command: "make test"}}); err != nil {
		t.Errorf("an unusual command only fails when run: %v", err)
	}
	// pytest's configuration (python_files) is not unittest's: its test files follow
	// unittest's discovery pattern.
	cfgRepo := tempRepo(t)
	testutil.Write(t, cfgRepo.Root, "pyproject.toml", "[tool.pytest.ini_options]\npython_files = [\"check_*.py\"]\n")
	us, err := NewSuite(cfgRepo, config.Suite{Name: "u", Root: ".", Runner: "unittest"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(us.Tests, []string{"**/test*.py"}) || len(us.Discovered) != 0 || !us.IsTestFile("tests/test_a.py") || us.IsTestFile("tests/check_a.py") {
		t.Errorf("unittest suite with a pytest config: tests %v discovered %v", us.Tests, us.Discovered)
	}
	uvRepo := tempRepo(t, "uv.lock")
	if d := (unittestRunner{}).Defaults(uvRepo, "."); d.Command != "uv run python -m unittest" {
		t.Errorf("uv: %q", d.Command)
	}
}

// TestUnittestRunRecords runs a small unittest project through the adapter with real
// python3 and the embedded shims.
func TestUnittestRunRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix paths")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	repo := tempRepo(t)
	write := func(rel, content string) { testutil.Write(t, repo.Root, rel, content) }
	write("lib/pkg/__init__.py", "")
	write("lib/pkg/a.py", "V = 1\n")
	write("lib/pkg/b.py", "W = 2\n")
	write("lib/data.txt", "hello\n")
	write("lib/tests/__init__.py", "")
	write("lib/tests/test_a.py", `import os, unittest
from pkg import a
def setUpModule():
    with open(os.path.join(os.path.dirname(__file__), "..", "data.txt")) as f:
        f.read()
class T(unittest.TestCase):
    def test_a(self):
        self.assertEqual(a.V, 1)
`)
	write("lib/tests/test_b.py", `import unittest
class T(unittest.TestCase):
    def test_b(self):
        from pkg import b
        self.assertEqual(b.W, 3)
    @unittest.skip("later")
    def test_skip(self):
        pass
`)
	write("lib/tests/test_skipped.py", `import unittest
@unittest.skip("all")
class T(unittest.TestCase):
    def test_x(self):
        pass
`)
	write("lib/tests/test_broken.py", "import does_not_exist\n")
	// unittest survives only an ImportError when it loads modules by name: the wrapper
	// keeps the run going after any exception (this module loads first).
	write("lib/tests/test_0_raises.py", "raise RuntimeError(\"boom at import\")\n")
	// pytest-style tests under unittest: the module loads and runs nothing, which must
	// not count as a pass.
	write("lib/tests/test_pytest_style.py", "def test_x():\n    assert False\n")
	// A helper matching test*.py, without tests: a pass, not a failure.
	write("lib/tests/test_support.py", "from pkg import a\n\ndef make():\n    return a.V\n")
	shimDir := t.TempDir()
	if _, err := trace.ExtractShims(shimDir); err != nil {
		t.Fatal(err)
	}
	s := &Suite{Suite: config.Suite{Name: "u", Root: ".", Runner: "unittest", Command: "python3 -m unittest discover -s lib/tests -t lib", Env: map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}}, Adapter: unittestRunner{}}
	s.Tests = []string{"lib/tests/**/test*.py"}
	units := []string{"lib/tests/test_0_raises.py", "lib/tests/test_a.py", "lib/tests/test_b.py", "lib/tests/test_broken.py", "lib/tests/test_pytest_style.py", "lib/tests/test_skipped.py", "lib/tests/test_support.py"}
	var out bytes.Buffer
	rc := &RunContext{
		Repo: repo, Suite: s, Units: units, All: true, Record: true,
		TraceDir: t.TempDir(), ShimDir: shimDir, Stdout: &out, Stderr: &out,
		IsTest: s.IsTestFile,
		UnitOf: func(f string) string {
			if s.IsTestFile(f) {
				return f
			}
			return ""
		},
	}
	res, err := unittestRunner{}.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.ExitCode == 0 {
		t.Errorf("exit code 0 with failing tests\n%s", out.String())
	}
	obs := res.Observations
	want := map[string]string{
		"lib/tests/test_a.py": state.Pass, "lib/tests/test_b.py": state.Fail,
		"lib/tests/test_broken.py": state.Fail, "lib/tests/test_skipped.py": state.Pass,
		"lib/tests/test_0_raises.py": state.Fail, "lib/tests/test_pytest_style.py": state.Unknown,
		"lib/tests/test_support.py": state.Pass,
	}
	for u, st := range want {
		o := obs[u]
		if o == nil {
			t.Errorf("%s: no observation\n%s", u, out.String())
			continue
		}
		if o.Status != st || o.Inferred {
			t.Errorf("%s: status %s (inferred %v), want %s", u, o.Status, o.Inferred, st)
		}
	}
	a, b := obs["lib/tests/test_a.py"], obs["lib/tests/test_b.py"]
	if a == nil || b == nil {
		t.FailNow()
	}
	// Each module gets what it used: test_a imports pkg.a and reads data.txt in
	// setUpModule; test_b imports pkg.b inside its test.
	if !a.Files["lib/pkg/a.py"] || !a.Files["lib/data.txt"] || a.Files["lib/pkg/b.py"] {
		t.Errorf("test_a files %v", a.Files)
	}
	if !b.Files["lib/pkg/b.py"] || b.Files["lib/pkg/a.py"] || b.Files["lib/data.txt"] {
		t.Errorf("test_b files %v", b.Files)
	}
	if !bytes.Contains([]byte(b.Failure), []byte("AssertionError")) || !bytes.Contains([]byte(obs["lib/tests/test_broken.py"].Failure), []byte("does_not_exist")) {
		t.Errorf("failure output: %q / %q", b.Failure, obs["lib/tests/test_broken.py"].Failure)
	}
	if o := obs["lib/tests/test_pytest_style.py"]; o == nil || !strings.Contains(o.Failure, "runner: pytest") {
		t.Errorf("module without unittest tests: %+v", o)
	}

	// isolate: true records each module in its own process, with the same outcome.
	isolate := true
	s.Isolate = &isolate
	rc.TraceDir = t.TempDir()
	res, err = unittestRunner{}.Run(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	for u, st := range want {
		if o := res.Observations[u]; o == nil || o.Status != st {
			t.Errorf("isolated %s: %+v, want %s", u, o, st)
		}
	}
	if o := res.Observations["lib/tests/test_a.py"]; o == nil || !o.Files["lib/pkg/a.py"] || o.Files["lib/pkg/b.py"] {
		t.Errorf("isolated test_a: %+v", o)
	}
	s.Isolate = nil

	// Without recording, a selection runs plain unittest on the selected modules.
	out.Reset()
	rc.Record, rc.All, rc.Units = false, false, []string{"lib/tests/test_a.py"}
	res, err = unittestRunner{}.Run(context.Background(), rc)
	if err != nil || res.ExitCode != 0 || !bytes.Contains(out.Bytes(), []byte("Ran 1 test")) {
		t.Errorf("selection: exit %d err %v\n%s", res.ExitCode, err, out.String())
	}

	// A run in which no selected module has a unittest test is not a clean pass.
	for _, record := range []bool{true, false} {
		out.Reset()
		rc.Record, rc.TraceDir, rc.Units = record, t.TempDir(), []string{"lib/tests/test_pytest_style.py"}
		res, err = unittestRunner{}.Run(context.Background(), rc)
		if err != nil {
			t.Fatal(err)
		}
		// unittest exits 5 when no test ran from Python 3.12 on (0 before).
		if res.ExitCode != unittestNoTests && res.ExitCode != 0 {
			t.Errorf("record=%v: exit %d\n%s", record, res.ExitCode, out.String())
		}
		if res.ExitCode == unittestNoTests && (len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "runner: pytest")) {
			t.Errorf("record=%v: warnings %q", record, res.Warnings)
		}
		if o := res.Observations["lib/tests/test_pytest_style.py"]; record && (o == nil || o.Status != state.Unknown) {
			t.Errorf("recorded status %+v", o)
		}
	}

	// A helper module matching the pattern (no tests, nothing pytest-style) passes, also
	// when it is the only module selected (`python -m unittest discover` accepts it too).
	for _, isolate := range []bool{false, true} {
		for _, record := range []bool{true, false} {
			out.Reset()
			s.Isolate = &isolate
			rc.Record, rc.TraceDir, rc.Units = record, t.TempDir(), []string{"lib/tests/test_support.py"}
			res, err = unittestRunner{}.Run(context.Background(), rc)
			if err != nil || res.ExitCode != 0 || len(res.Warnings) != 0 {
				t.Errorf("helper alone (record=%v isolate=%v): exit %d err %v warnings %q\n%s", record, isolate, res.ExitCode, err, res.Warnings, out.String())
			}
			if o := res.Observations["lib/tests/test_support.py"]; record && (o == nil || o.Status != state.Pass || !o.Files["lib/pkg/a.py"]) {
				t.Errorf("helper alone (isolate=%v): recorded %+v", isolate, o)
			}
		}
	}
	s.Isolate = nil
}

func TestPytestStyle(t *testing.T) {
	cases := map[string]bool{
		"import unittest\nclass T(unittest.TestCase):\n    def test_a(self): pass\n": false,
		"def test_suite():\n    return unittest.TestSuite()\n":                       false,
		"import pytest_mock\n":                         false,
		"helper = 1\n# def test_x(): commented\n":      false,
		"def test_x():\n    pass\n":                    true,
		"async def test_x():\n    pass\n":              true,
		"import pytest\n":                              true,
		"from pytest import raises\n":                  true,
		"def test_suite():\n    pass\ndef test_y(): 1": true,
	}
	for src, want := range cases {
		if got := PytestStyle([]byte(src)); got != want {
			t.Errorf("PytestStyle(%q) = %v, want %v", src, got, want)
		}
	}
}

// With --failfast, the modules whose tests never ran are not recorded as passing.
func TestUnittestFailfastStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix paths")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	repo := tempRepo(t)
	write := func(rel, content string) { testutil.Write(t, repo.Root, rel, content) }
	write("tests/__init__.py", "")
	write("tests/test_a.py", "import unittest\nclass T(unittest.TestCase):\n    def test_a(self):\n        self.fail('a')\n")
	write("tests/test_b.py", "import unittest\nclass T(unittest.TestCase):\n    def test_b(self):\n        raise RuntimeError('b')\n")
	shimDir := t.TempDir()
	if _, err := trace.ExtractShims(shimDir); err != nil {
		t.Fatal(err)
	}
	s := &Suite{Suite: config.Suite{Name: "u", Root: ".", Runner: "unittest", Command: "python3 -m unittest -f", Env: map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}}, Adapter: unittestRunner{}}
	s.Tests = []string{"**/test*.py"}
	var out bytes.Buffer
	rc := &RunContext{
		Repo: repo, Suite: s, Units: []string{"tests/test_a.py", "tests/test_b.py"}, All: true, Record: true,
		TraceDir: t.TempDir(), ShimDir: shimDir, Stdout: &out, Stderr: &out,
		IsTest: s.IsTestFile,
		UnitOf: func(f string) string {
			if s.IsTestFile(f) {
				return f
			}
			return ""
		},
	}
	res, err := unittestRunner{}.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.ExitCode == 0 {
		t.Errorf("exit code 0\n%s", out.String())
	}
	if o := res.Observations["tests/test_a.py"]; o == nil || o.Status != state.Fail {
		t.Errorf("test_a: %+v\n%s", o, out.String())
	}
	if o := res.Observations["tests/test_b.py"]; o != nil && o.Status == state.Pass {
		t.Errorf("test_b never ran but is recorded as passed: %+v\n%s", o, out.String())
	}
	if o := res.Observations["tests/test_b.py"]; o == nil || o.Status != state.Unknown || o.Inferred || !strings.Contains(o.Failure, "--failfast") {
		t.Errorf("test_b: %+v", o)
	}
	if strings.Contains(out.String(), "a helper matching") {
		t.Errorf("helper warning for a module that never ran:\n%s", out.String())
	}
}
