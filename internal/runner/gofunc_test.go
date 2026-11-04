package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
)

func TestRunRegex(t *testing.T) {
	re := runRegex([]string{"TestB", "TestA", "TestB", "ExampleX"})
	if re != "^(ExampleX|TestA|TestB)$" {
		t.Fatalf("runRegex = %q", re)
	}
	// go test matches the pattern against each top-level name; subtests are not
	// constrained by it.
	r := regexp.MustCompile(re)
	for name, want := range map[string]bool{"TestA": true, "TestB": true, "TestAB": false, "XTestA": false, "ExampleX": true} {
		if r.MatchString(name) != want {
			t.Errorf("%s matched %v", name, !want)
		}
	}
	if got := runRegex([]string{"Test_a.b"}); got != `^(Test_a\.b)$` {
		t.Errorf("quoting: %q", got)
	}
}

func TestPlainInvocations(t *testing.T) {
	funcs := map[string][]string{"a": {"TestA1", "TestA2"}, "b": {"TestB"}}
	inv := plainInvocations([]string{"a", "b"}, funcs)
	if len(inv) != 1 || !slices.Equal(inv[0].dirs, []string{"a", "b"}) || inv[0].run != "^(TestA1|TestA2|TestB)$" {
		t.Fatalf("invocations %+v", inv)
	}
	// A package whose pattern is too long runs whole; others are split to fit.
	var many []string
	for i := 0; i < maxRunRegex*3/36; i++ { // about 1.5 times the limit
		many = append(many, fmt.Sprintf("TestLongName%05d", i))
	}
	funcs["big"] = many
	half := many[:len(many)/2]
	funcs["c"], funcs["d"] = half, slices.Clone(half)
	for i := range funcs["d"] {
		funcs["d"][i] += "D"
	}
	inv = plainInvocations([]string{"a", "big", "c", "d"}, funcs)
	var whole, runs int
	for _, v := range inv {
		if v.run == "" {
			whole++
			if !slices.Equal(v.dirs, []string{"big"}) {
				t.Errorf("whole %v", v.dirs)
			}
			continue
		}
		runs++
		if len(v.run) > maxRunRegex {
			t.Errorf("pattern of %d bytes", len(v.run))
		}
	}
	if whole != 1 || runs != 2 {
		t.Errorf("invocations %d whole, %d with -run", whole, runs)
	}
}

func TestSplitFuncUnit(t *testing.T) {
	if d, n, ok := SplitFuncUnit("pkg/x#TestA"); !ok || d != "pkg/x" || n != "TestA" {
		t.Errorf("split %q %q %v", d, n, ok)
	}
	if d, n, ok := SplitFuncUnit(FuncUnit(".", "TestA")); !ok || d != "." || n != "TestA" {
		t.Errorf("root %q %q", d, n)
	}
	if _, _, ok := SplitFuncUnit("pkg/x"); ok {
		t.Error("a package is not a function")
	}
	pkgs, dirs, funcs, checks := splitUnits([]string{"a#T1", "b", "a#T2", "c#T", "a#(build)"})
	if !slices.Equal(pkgs, []string{"b"}) || !slices.Equal(dirs, []string{"a", "c"}) || !slices.Equal(funcs["a"], []string{"T1", "T2"}) || !slices.Equal(checks, []string{"a"}) {
		t.Errorf("splitUnits %v %v %v %v", pkgs, dirs, funcs, checks)
	}
}

func feed(w io.Writer, out string) {
	for i := 0; i < len(out); i += 5 {
		io.WriteString(w, out[i:min(i+5, len(out))])
	}
}

func TestGoOutcomesPlain(t *testing.T) {
	g := &goOutcomes{}
	feed(g, "--- FAIL: TestMul (0.00s)\n    mul_test.go:9: got 7, want 6\n    --- FAIL: TestMul/sub (0.00s)\nFAIL\nFAIL\texample.com/m/calc\t0.012s\n"+
		"ok  \texample.com/m/util\t0.003s\n"+
		"# example.com/m/broken\nbroken/b.go:3:2: undefined: x\nFAIL\texample.com/m/broken [build failed]\n"+
		"panic: test timed out after 1s\n\trunning tests:\n\t\tTestSlow (1s)\nFAIL\texample.com/m/slow\t1.002s\n"+
		"=== RUN   TestV\n--- PASS: TestV (0.00s)\n=== RUN   TestSkip\n--- SKIP: TestSkip (0.00s)\nPASS\nok  \texample.com/m/verbose\t0.01s\n")
	for _, c := range []struct {
		ip, name, status string
		inferred, ran    bool
	}{
		{"example.com/m/calc", "TestMul", state.Fail, false, true},
		{"example.com/m/calc", "TestAdd", state.Unknown, true, true}, // passing tests print nothing
		{"example.com/m/util", "TestId", state.Pass, false, true},
		{"example.com/m/broken", "TestB", state.Fail, false, true},
		{"example.com/m/slow", "TestSlow", state.Unknown, true, true},
		{"example.com/m/verbose", "TestV", state.Pass, false, true},
		{"example.com/m/verbose", "TestSkip", state.Pass, false, true},
		{"example.com/m/other", "TestX", "", false, false},
	} {
		st, inf, ran := g.status(c.ip, c.name)
		if st != c.status || inf != c.inferred || ran != c.ran {
			t.Errorf("%s %s: %q %v %v, want %q %v %v", c.ip, c.name, st, inf, ran, c.status, c.inferred, c.ran)
		}
	}
	if f := g.failure("example.com/m/calc", "TestMul"); !strings.Contains(f, "got 7, want 6") || !strings.Contains(f, "TestMul/sub") {
		t.Errorf("failure %q", f)
	}
	if f := g.failure("example.com/m/util", "TestId"); f != "" {
		t.Errorf("failure of a passing test %q", f)
	}
}

func TestGoOutcomesJSON(t *testing.T) {
	g := &goOutcomes{}
	feed(g, `{"Action":"run","Package":"m/a","Test":"TestA"}
{"Action":"output","Package":"m/a","Test":"TestA","Output":"    a_test.go:3: boom\n"}
{"Action":"fail","Package":"m/a","Test":"TestA/sub"}
{"Action":"fail","Package":"m/a","Test":"TestA"}
{"Action":"pass","Package":"m/a","Test":"TestB"}
{"Action":"fail","Package":"m/a"}
{"Action":"pass","Package":"m/b"}
{"Action":"fail","Package":"m/c","FailedBuild":"m/c"}
`)
	for _, c := range []struct{ ip, name, status string }{
		{"m/a", "TestA", state.Fail}, {"m/a", "TestB", state.Pass}, {"m/a", "TestC", state.Unknown},
		{"m/b", "TestX", state.Pass}, {"m/c", "TestY", state.Fail},
	} {
		if st, _, _ := g.status(c.ip, c.name); st != c.status {
			t.Errorf("%s %s: %q, want %q", c.ip, c.name, st, c.status)
		}
	}
	if f := g.failure("m/a", "TestA"); !strings.Contains(f, "boom") {
		t.Errorf("failure %q", f)
	}
}

// With -count=N a test that fails in one iteration and passes in a later one failed.
func TestGoOutcomesCount(t *testing.T) {
	plain := &goOutcomes{}
	feed(plain, "=== RUN   TestA\n--- FAIL: TestA (0.00s)\n    a_test.go:5: first\n=== RUN   TestC\n    a_test.go:9: always\n--- FAIL: TestC (0.00s)\n"+
		"=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestC\n    a_test.go:9: always\n--- FAIL: TestC (0.00s)\nFAIL\nFAIL\tm/a\t0.01s\n")
	js := &goOutcomes{}
	feed(js, `{"Action":"run","Package":"m/a","Test":"TestA"}
{"Action":"output","Package":"m/a","Test":"TestA","Output":"    a_test.go:5: first\n"}
{"Action":"fail","Package":"m/a","Test":"TestA"}
{"Action":"run","Package":"m/a","Test":"TestA"}
{"Action":"output","Package":"m/a","Test":"TestA","Output":"--- PASS: TestA (0.00s)\n"}
{"Action":"pass","Package":"m/a","Test":"TestA"}
{"Action":"fail","Package":"m/a"}
`)
	for name, g := range map[string]*goOutcomes{"plain": plain, "json": js} {
		if st, inf, _ := g.status("m/a", "TestA"); st != state.Fail || inf {
			t.Errorf("%s: TestA %q (inferred %v), want fail", name, st, inf)
		}
		if f := g.failure("m/a", "TestA"); !strings.Contains(f, "first") || strings.Contains(f, "PASS") {
			t.Errorf("%s: failure %q", name, f)
		}
	}
}

func goRC(t *testing.T, command string, env map[string]string) *RunContext {
	t.Helper()
	repo := tempRepo(t)
	s := &Suite{Suite: config.Suite{Name: "go", Root: ".", Runner: "go", Command: command, Env: env, Granularity: config.GranularityFunc}}
	return &RunContext{Repo: repo, Suite: s}
}

func TestParseGoFlags(t *testing.T) {
	g := parseGoFlags(goRC(t, "go test -race -tags 'a b' -v -count=1 -timeout 5m -short -json -coverprofile c.out -ldflags=-s ./... -args -x 1", nil))
	if g.why != "" {
		t.Fatal(g.why)
	}
	if !slices.Equal(g.build, []string{"-race", "-tags", "a b", "-ldflags=-s"}) {
		t.Errorf("build %q", g.build)
	}
	if !slices.Equal(g.test, []string{"-test.v", "-test.count=1", "-test.timeout=5m", "-test.short"}) {
		t.Errorf("test %q", g.test)
	}
	if !slices.Equal(g.tail, []string{"-x", "1"}) {
		t.Errorf("tail %q", g.tail)
	}
	g = parseGoFlags(goRC(t, "go test -exec 'sudo -E'", map[string]string{"GOFLAGS": "-mod=mod"}))
	if !slices.Equal(g.exec, []string{"sudo", "-E"}) || !slices.Equal(g.test, []string{"-test.timeout=10m0s"}) {
		t.Errorf("exec %q test %q", g.exec, g.test)
	}
	if g := parseGoFlags(goRC(t, "go test", map[string]string{"GOFLAGS": "-exec=wrap"})); !slices.Equal(g.exec, []string{"wrap"}) {
		t.Errorf("GOFLAGS exec %q", g.exec)
	}
	// Test flags in GOFLAGS reach the binary, before the command's own (which win);
	// build flags there are left to the go command.
	g = parseGoFlags(goRC(t, "go test -count=1", map[string]string{"GOFLAGS": "-mod=mod -timeout=3m -short -count=2 -v -failfast -parallel=2 -cpu=1,2"}))
	if g.why != "" || len(g.build) != 0 {
		t.Errorf("GOFLAGS: why %q build %q", g.why, g.build)
	}
	if want := []string{"-test.timeout=3m", "-test.short", "-test.count=2", "-test.v", "-test.failfast", "-test.parallel=2", "-test.cpu=1,2", "-test.count=1"}; !slices.Equal(g.test, want) {
		t.Errorf("GOFLAGS test flags %q, want %q", g.test, want)
	}
	if g := parseGoFlags(goRC(t, "go test", map[string]string{"GOFLAGS": "-run=TestX"})); !strings.Contains(g.why, "GOFLAGS") {
		t.Errorf("GOFLAGS -run: why %q", g.why)
	}
	// -p bounds the packages recorded at once, from the command or GOFLAGS.
	for cmd, env := range map[string]string{"go test -p 1": "", "go test": "-p=1", "go test -p=1": "-p=8"} {
		rc := goRC(t, cmd, map[string]string{"GOFLAGS": env})
		fr := &funcRecorder{rc: rc, flags: parseGoFlags(rc)}
		if fr.packages() != 1 {
			t.Errorf("%q GOFLAGS=%q: %d packages at once, want 1", cmd, env, fr.packages())
		}
	}
	if fr := (&funcRecorder{flags: parseGoFlags(goRC(t, "go test", nil))}); fr.packages() < 1 {
		t.Errorf("default packages %d", fr.packages())
	}
	if g := parseGoFlags(goRC(t, "go test -p 2 -race", nil)); !slices.Equal(g.build, []string{"-p", "2", "-race"}) || g.p != 2 {
		t.Errorf("-p build %q p %d", g.build, g.p)
	}
	for _, cmd := range []string{"go test -run TestX", "go test -c", "go test -test.run=X", "gotestsum --", "make test", "go vet"} {
		if g := parseGoFlags(goRC(t, cmd, nil)); g.why == "" {
			t.Errorf("%q: per-function recording should not be possible", cmd)
		}
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFuncUnitsAndMapStatic(t *testing.T) {
	repo := tempRepo(t)
	writeFiles(t, repo.Root, map[string]string{
		"p/a_test.go":      "package p\nimport \"testing\"\nfunc TestA(t *testing.T) { helper(t) }\nfunc TestA2(t *testing.T) {}\nfunc BenchmarkA(b *testing.B) {}\n",
		"p/b_test.go":      "package p\nimport \"testing\"\nfunc TestB(t *testing.T) { other() }\n",
		"p/helper_test.go": "package p\nimport \"testing\"\nfunc helper(t *testing.T) { deep() }\n",
		"p/deep_test.go":   "package p\nfunc deep() {}\n",
		"p/other_test.go":  "package p\nfunc other() {}\n",
		"p/main_test.go":   "package p\nimport (\"os\"; \"testing\")\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n",
		"p/ext_test.go":    "package p_test\nimport \"testing\"\nfunc ExampleX() {}\n",
		"q/bad_test.go":    "package q\nfunc {",
	})
	s := &Suite{Suite: config.Suite{Runner: "go", Granularity: config.GranularityFunc}, RepoRoot: repo.Root}
	a := goTest{}
	units := a.Units(s, []string{"p/a_test.go", "p/b_test.go", "p/helper_test.go", "p/ext_test.go", "q/bad_test.go"})
	// Then the compile check of each package.
	if !slices.Equal(units, []string{"p#TestA", "p#TestA2", "p#TestB", "p#ExampleX", "q", "p#(build)", "q#(build)"}) {
		t.Errorf("units %v", units)
	}
	// A test file without test functions still belongs to its package's compile check.
	if got := a.Units(s, []string{"p/helper_test.go"}); !slices.Equal(got, []string{"p#(build)"}) {
		t.Errorf("helper units %v", got)
	}
	closure := []string{"go.mod", "p/a.go", "p/a_test.go", "p/b_test.go", "p/deep_test.go", "p/ext_test.go", "p/helper_test.go", "p/main_test.go", "p/other_test.go", "p/static/x.txt", "r/r.go"}
	got := a.MapStatic(s, "p#TestA", closure)
	// Every Go file's declarations; in the test files, the declarations TestA reaches:
	// itself, helper and what it calls, TestMain (it runs for every test).
	want := []string{"go.mod", "p/a.go#decls", "p/a_test.go#decls", "p/b_test.go#decls", "p/deep_test.go#decls", "p/ext_test.go#decls", "p/helper_test.go#decls", "p/main_test.go#decls", "p/other_test.go#decls", "p/static/x.txt", "r/r.go#decls",
		"p/a_test.go#decl:TestA", "p/deep_test.go#decl:deep", "p/helper_test.go#decl:helper", "p/main_test.go#decl:TestMain", "p/"}
	if !slices.Equal(got, want) {
		t.Errorf("MapStatic\n got %v\nwant %v", got, want)
	}
	// Package granularity keeps the closure as it is, and so does a compile check.
	if got := a.MapStatic(&Suite{Suite: config.Suite{Runner: "go"}, RepoRoot: repo.Root}, "p", closure); !slices.Equal(got, closure) {
		t.Errorf("package granularity: %v", got)
	}
	if got := a.MapStatic(s, "p#(build)", closure); !slices.Equal(got, closure) {
		t.Errorf("compile check: %v", got)
	}
}

func TestAddCoverage(t *testing.T) {
	repo := tempRepo(t, "m/calc/add.go", "m/calc/mul.go", "m/util/util.go", "m/gen/gen.go", "m/gen/other.go", "m/gen/gen_test.go")
	gp := &goPackage{dirs: map[string]string{
		"ex.com/m/calc": filepath.Join(repo.Root, "m/calc"),
		"ex.com/m/util": filepath.Join(repo.Root, "m/util"),
		"ex.com/m/gen":  filepath.Join(repo.Root, "m/gen"),
	}}
	profile := filepath.Join(t.TempDir(), "cover.out")
	os.WriteFile(profile, []byte("mode: set\nex.com/m/calc/add.go:3.24,3.40 1 0\nex.com/m/calc/mul.go:7.24,7.58 1 1\nex.com/m/util/util.go:3.20,3.32 1 1\n"+
		"ex.com/m/gen/parser.y:10.1,12.2 2 1\nexample.org/dep/x.go:1.1,2.2 1 1\n"), 0o644)
	o := &Observation{}
	if !addCoverage(o, repo.Root, gp, profile) {
		t.Fatal("profile not read")
	}
	var got []string
	for f := range o.Files {
		got = append(got, f)
	}
	slices.Sort(got)
	// A block placed by a //line directive in a file that is not there: the whole
	// package's Go files (not its tests).
	if want := []string{"m/calc/mul.go", "m/gen/gen.go", "m/gen/other.go", "m/util/util.go"}; !slices.Equal(got, want) {
		t.Errorf("coverage deps %v, want %v", got, want)
	}
	if addCoverage(&Observation{}, repo.Root, gp, filepath.Join(t.TempDir(), "missing")) {
		t.Error("a missing profile is not certain")
	}
}

// --dry-run prints what Run executes: a narrowed suite runs explicit -run invocations
// even when every unit is selected.
func TestGoFuncCommandsNarrowed(t *testing.T) {
	rc := goRC(t, "go test", nil)
	rc.Units, rc.All = []string{"a#TestA", "b#TestB"}, true
	cmds, err := goTest{}.Commands(rc)
	if err != nil || len(cmds) != 1 || !slices.Contains(cmds[0], "./...") {
		t.Fatalf("whole suite: %q %v", cmds, err)
	}
	rc.Suite.Narrowed = true
	cmds, err = goTest{}.Commands(rc)
	if err != nil || len(cmds) != 1 || slices.Contains(cmds[0], "./...") || !slices.Contains(cmds[0], "^(TestA|TestB)$") || !slices.Contains(cmds[0], "./a") {
		t.Errorf("narrowed: %q %v", cmds, err)
	}
}

// The test-file declarations a test function reaches: names resolved within the
// package, not by text (a selector, a local or a method of the same name does not
// count), types with their methods, what runs for every test, and in an external test
// package the declarations of the package's own test files it selects (p.Name).
func TestTestFileDeps(t *testing.T) {
	repo := tempRepo(t)
	files := map[string]string{
		"p/p.go":              "package p\ntype Server struct{}\nfunc (s *Server) Render() {}\n",
		"p/a_test.go":         "package p\nimport \"testing\"\nfunc TestA(t *testing.T) { name := 1; _ = name; f := newFake(); f.Render() }\nfunc TestA2(t *testing.T) { run() }\n",
		"p/fake_test.go":      "package p\ntype fake struct{ cfg config }\nfunc newFake() *fake { return &fake{} }\nfunc (f *fake) Render() { f.cfg.load() }\n",
		"p/cfg_test.go":       "package p\ntype config struct{}\nfunc (config) load() {}\n",
		"p/name_test.go":      "package p\nfunc name() {}\nfunc run() {}\nfunc (u unrelated) Render() {}\ntype unrelated int\n",
		"p/init_test.go":      "package p\nimport \"regexp\"\nvar re = regexp.MustCompile(\"x\")\nvar raw = []byte(\"x\")\nvar _ = (*fake)(nil)\nfunc TestInit(t *testing.T) { _ = raw }\n",
		"p/srv_test.go":       "package p\nfunc (s *Server) testOnly() {}\n",
		"p/export_test.go":    "package p\nfunc Exported() { run() }\n",
		"p/x_test.go":         "package p_test\nimport (\"testing\"; \"example.com/m/p\")\nfunc TestX(t *testing.T) { p.Exported() }\n",
		"p/dot_test.go":       "package p_test\nimport (\"testing\"; . \"example.com/m/p\")\nfunc TestDot(t *testing.T) { DotHelper() }\n",
		"p/dothelper_test.go": "package p\nfunc DotHelper() { name() }\n",
		"p/bad_test.go":       "package p\nfunc {",
	}
	writeFiles(t, repo.Root, files)
	var all []string
	for f := range files {
		all = append(all, f)
	}
	slices.Sort(all)
	every := []string{"p/bad_test.go", "p/init_test.go#decl:re", "p/srv_test.go#decl:Server.testOnly"}
	for name, want := range map[string][]string{
		// Not name (a local), unrelated.Render (a method of another type), run.
		"TestA":  {"p/a_test.go#decl:TestA", "p/cfg_test.go#decl:config", "p/cfg_test.go#decl:config.load", "p/fake_test.go#decl:fake", "p/fake_test.go#decl:fake.Render", "p/fake_test.go#decl:newFake"},
		"TestA2": {"p/a_test.go#decl:TestA2", "p/name_test.go#decl:run"},
		// raw is a conversion, not initialization code: only the test using it depends on it.
		"TestInit": {"p/init_test.go#decl:TestInit", "p/init_test.go#decl:raw"},
		"TestX":    {"p/export_test.go#decl:Exported", "p/name_test.go#decl:run", "p/x_test.go#decl:TestX"},
		// A dot import of the package: its test helpers by their bare names.
		"TestDot": {"p/dot_test.go#decl:TestDot", "p/dothelper_test.go#decl:DotHelper", "p/name_test.go#decl:name"},
	} {
		want = append(want, every...)
		slices.Sort(want)
		if got := testFileDeps(repo.Root, "p", name, all); !slices.Equal(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", name, got, want)
		}
	}
}
