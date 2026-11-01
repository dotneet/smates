package runner

import (
	"bytes"
	"context"
	"os/exec"
	"reflect"
	"runtime"
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
	shimDir := t.TempDir()
	if _, err := trace.ExtractShims(shimDir); err != nil {
		t.Fatal(err)
	}
	s := &Suite{Suite: config.Suite{Name: "u", Root: ".", Runner: "unittest", Command: "python3 -m unittest discover -s lib/tests -t lib", Env: map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}}, Adapter: unittestRunner{}}
	s.Tests = []string{"lib/tests/**/test*.py"}
	units := []string{"lib/tests/test_0_raises.py", "lib/tests/test_a.py", "lib/tests/test_b.py", "lib/tests/test_broken.py", "lib/tests/test_skipped.py"}
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
		"lib/tests/test_0_raises.py": state.Fail,
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
}
