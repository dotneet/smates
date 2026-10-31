package trace

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// proc builds a synthetic process. Files ending in .test.ts / test_*.py are tests.
type pb struct{ p *Proc }

func mkProc(pid, ppid, tid int) *pb {
	p := newProc(pid, tid)
	p.Ppid = ppid
	return &pb{p}
}

func (b *pb) code(fs ...string) *pb {
	for _, f := range fs {
		b.p.add("", "code", f)
	}
	return b
}

func (b *pb) read(fs ...string) *pb {
	for _, f := range fs {
		b.p.add("", "read", f)
	}
	return b
}

func (b *pb) dir(fs ...string) *pb {
	for _, f := range fs {
		b.p.add("", "dir", f)
	}
	return b
}

func (b *pb) times(start, end int64) *pb { b.p.Start, b.p.End = start, end; return b }

func (b *pb) exit(c int) *pb { b.p.Exit = &c; return b }

// ctx switches context at time t and records files in it (code for .py/.ts, else read).
func (b *pb) ctx(t int64, id string, files ...string) *pb {
	b.p.CtxSpans = append(b.p.CtxSpans, Span{At: t, ID: id})
	for _, f := range files {
		kind := "read"
		if strings.HasSuffix(f, ".py") || strings.HasSuffix(f, ".ts") {
			kind = "code"
		}
		b.p.add(id, kind, f)
	}
	return b
}

func unitOfTests(f string) string {
	base := filepath.Base(f)
	if strings.HasSuffix(f, ".test.ts") || strings.HasPrefix(base, "test_") {
		return f
	}
	return ""
}

func attr(opt Options, ps ...*pb) *Attribution {
	var procs []*Proc
	for _, b := range ps {
		procs = append(procs, b.p)
	}
	return Attribute(procs, unitOfTests, opt)
}

func unitFiles(a *Attribution, id string) []string {
	u := a.Units[id]
	if u == nil {
		return nil
	}
	return keys(u.Files)
}

func TestAttributeSingleUnitWorkers(t *testing.T) {
	a := attr(Options{},
		mkProc(10, 1, 0).code("node_modules_free/vitest.mjs").read("vitest.config.ts"), // runner main
		mkProc(11, 10, 0).code("a.test.ts", "src/a.ts").read("data/a.json").times(100, 150).exit(0),
		mkProc(12, 10, 0).code("b.test.ts", "src/b.ts").dir("fixtures").times(100, 120).exit(1),
	)
	if got := unitFiles(a, "a.test.ts"); !reflect.DeepEqual(got, []string{"a.test.ts", "data/a.json", "src/a.ts"}) {
		t.Errorf("a: %v", got)
	}
	if got := unitFiles(a, "b.test.ts"); !reflect.DeepEqual(got, []string{"b.test.ts", "fixtures/", "src/b.ts"}) {
		t.Errorf("b: %v", got)
	}
	if a.Units["a.test.ts"].Status != "pass" || a.Units["b.test.ts"].Status != "fail" {
		t.Errorf("statuses %+v %+v", a.Units["a.test.ts"], a.Units["b.test.ts"])
	}
	if a.Units["a.test.ts"].Ms != 50 || a.Units["a.test.ts"].Shared || a.SharedProcs != 0 {
		t.Errorf("ms/shared %+v %d", a.Units["a.test.ts"], a.SharedProcs)
	}
	// Code the runner process executed itself (config, global setup) affects every
	// test; what it only read (transforms, crawling) does not.
	if got := keys(a.Global); !reflect.DeepEqual(got, []string{"node_modules_free/vitest.mjs"}) {
		t.Errorf("runner code should be global, reads not: %v", got)
	}
}

func TestAttributeSharedProcess(t *testing.T) {
	a := attr(Options{},
		mkProc(10, 1, 0),
		mkProc(11, 10, 0).code("a.test.ts", "b.test.ts", "src/a.ts", "src/b.ts").read("x.json").times(1, 100).exit(0),
	)
	want := []string{"a.test.ts", "b.test.ts", "src/a.ts", "src/b.ts", "x.json"}
	for _, id := range []string{"a.test.ts", "b.test.ts"} {
		u := a.Units[id]
		if u == nil || !reflect.DeepEqual(keys(u.Files), want) || !u.Shared || u.Ms != 0 {
			t.Errorf("%s: %+v", id, u)
		}
	}
	if a.SharedProcs != 1 {
		t.Errorf("shared procs %d", a.SharedProcs)
	}
}

func TestAttributeChildrenAndThreads(t *testing.T) {
	a := attr(Options{},
		mkProc(10, 1, 0), // runner
		mkProc(11, 10, 0).code("a.test.ts"),
		mkProc(20, 11, 0).code("scripts/gen.js").read("templates/t.txt"),  // child of a test
		mkProc(21, 20, 0).code("scripts/deep.js"),                         // grandchild
		mkProc(30, 10, 0).code("b.test.ts"),                               // test in the main thread...
		mkProc(30, 10, 1).code("src/worker.js").read("w.json"),            // ...with a worker thread
		mkProc(40, 10, 0).code("server/main.js").read("server/conf.json"), // helper started by the runner
		mkProc(41, 40, 0).read("server/child.txt"),                        // helper's child
		mkProc(50, 10, 0).read("crawler-only.txt"),                        // reads but runs no repo code
		mkProc(60, 999, 0).code("orphan-parent.js"),                       // parent not traced
	)
	if got := unitFiles(a, "a.test.ts"); !reflect.DeepEqual(got, []string{"a.test.ts", "scripts/deep.js", "scripts/gen.js", "templates/t.txt"}) {
		t.Errorf("a (children attributed to the test): %v", got)
	}
	if got := unitFiles(a, "b.test.ts"); !reflect.DeepEqual(got, []string{"b.test.ts", "src/worker.js", "w.json"}) {
		t.Errorf("b (worker thread attributed to its main thread): %v", got)
	}
	for _, f := range []string{"server/main.js", "server/conf.json", "orphan-parent.js"} {
		if !a.Global[f] {
			t.Errorf("%s should be global: %v", f, keys(a.Global))
		}
	}
	if len(a.Units) != 2 {
		t.Errorf("units %v", a.Units)
	}
}

func TestAttributeWorkerThreadsUnderRunner(t *testing.T) {
	// Vitest threads pool: the main thread is the runner, each worker thread one file.
	a := attr(Options{},
		mkProc(10, 1, 0).code("vitest.config.ts"),
		mkProc(10, 1, 1).code("a.test.ts", "src/a.ts"),
		mkProc(10, 1, 2).code("b.test.ts"),
	)
	if got := unitFiles(a, "a.test.ts"); !reflect.DeepEqual(got, []string{"a.test.ts", "src/a.ts"}) {
		t.Errorf("a %v", got)
	}
	if got := unitFiles(a, "b.test.ts"); !reflect.DeepEqual(got, []string{"b.test.ts"}) {
		t.Errorf("b %v", got)
	}
	if got := keys(a.Global); !reflect.DeepEqual(got, []string{"vitest.config.ts"}) {
		t.Errorf("config executed by the main thread is global: %v", got)
	}
}

func TestAttributeAncestorThroughUntracedShell(t *testing.T) {
	// The test spawns `sh -c "node tool.js"`: sh is untraced, so ppid does not resolve,
	// but the environment-inherited ancestor key links the child to the test.
	child := mkProc(21, 20, 0).code("tool.js")
	child.p.Anc = "11-0"
	a := attr(Options{},
		mkProc(10, 1, 0),
		mkProc(11, 10, 0).code("a.test.ts"),
		child,
	)
	if got := unitFiles(a, "a.test.ts"); !reflect.DeepEqual(got, []string{"a.test.ts", "tool.js"}) {
		t.Errorf("a %v (global %v)", got, keys(a.Global))
	}
}

func TestAttributeCycleSafe(t *testing.T) {
	// Corrupt ppid data forming a cycle must not hang.
	a := attr(Options{},
		mkProc(1, 2, 0).read("x"),
		mkProc(2, 1, 0).code("a.test.ts"),
	)
	if a.Units["a.test.ts"] == nil {
		t.Fatal("unit missing")
	}
}

func TestAttributePythonContexts(t *testing.T) {
	py := mkProc(100, 1, 0).
		ctx(10, "", "tests/conftest.py", "pyproject.toml", "settings.json", "tests/").
		ctx(11, "collect:tests/test_a.py", "tests/test_a.py", "src/m.py", "data/a_params.json").
		ctx(12, "collect:tests/test_b.py", "tests/test_b.py").
		ctx(13, "test:tests/test_a.py", "data/a.json").
		ctx(14, "fixture:db@tests", "data/db.sql").
		ctx(15, "fixture:dyn@tests", "data/dyn.json").
		ctx(16, "test:tests/test_b.py", "data/b.json").
		ctx(30, "", "tests/sub/conftest.py", "tests/sub?").
		ctx(40, "test:tests/test_c.py").
		ctx(50, "")
	py.p.Uses["tests/test_a.py"] = []string{"fixture:db@tests"}
	py.p.Uses["not_a_test.py"] = []string{"fixture:db@tests"}
	py.p.Results["tests/test_a.py"] = Result{"pass", 7}
	py.p.Results["tests/test_b.py"] = Result{"fail", 3}
	py.exit(1)
	server := mkProc(200, 100, 0).times(17, 0).code("server.py").read("server.cfg") // started during test_b
	early := mkProc(201, 100, 0).times(5, 0).code("bootstrap.py")                   // started before any context
	a := attr(Options{}, py, server, early)

	shared := []string{"bootstrap.py", "data/dyn.json", "pyproject.toml", "settings.json"}
	want := map[string][]string{
		"tests/test_a.py": append([]string{"data/a.json", "data/a_params.json", "data/db.sql", "src/m.py", "tests/test_a.py"}, shared...),
		"tests/test_b.py": append([]string{"data/b.json", "server.cfg", "server.py", "tests/test_b.py"}, shared...),
		"tests/test_c.py": nil,
	}
	for id, w := range want {
		got := unitFiles(a, id)
		if id == "tests/test_c.py" {
			// Only a context switch, no files and no result: no unit.
			if a.Units[id] != nil && len(a.Units[id].Files) > 0 {
				t.Errorf("%s: %v", id, got)
			}
			continue
		}
		ww := append([]string{}, w...)
		sortStrings(ww)
		if !reflect.DeepEqual(got, ww) {
			t.Errorf("%s:\n got %v\nwant %v", id, got, ww)
		}
	}
	if a.Units["tests/test_a.py"].Status != "pass" || a.Units["tests/test_a.py"].Ms != 7 || a.Units["tests/test_b.py"].Status != "fail" {
		t.Errorf("results %+v %+v", a.Units["tests/test_a.py"], a.Units["tests/test_b.py"])
	}
	if len(a.Global) != 0 {
		t.Errorf("children of a context process are attributed to it: %v", keys(a.Global))
	}
}

func TestAttributeUnitsFromArgv(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "web/src/a.test.ts", "web/src/b.test.ts", "web/src/a.ts")
	a := Attribute([]*Proc{
		{Pid: 1, Argv: []string{"bun", "test", "--preload", "/tmp/x.cjs", "./src/a.test.ts"}, Cwd: filepath.Join(root, "web"), Code: map[string]bool{}, Reads: map[string]bool{"web/src/a.ts": true}, Ctx: map[string]map[string]bool{}},
		{Pid: 2, Argv: []string{"bun", "test", filepath.Join(root, "web", "src", "b.test.ts")}, Cwd: "/elsewhere", Code: map[string]bool{}, Reads: map[string]bool{}, Ctx: map[string]map[string]bool{}},
		{Pid: 3, Argv: []string{"bun", "test"}, Cwd: root, Code: map[string]bool{"web/src/a.test.ts": true}, Reads: map[string]bool{}, Ctx: map[string]map[string]bool{}},
	}, unitOfTests, Options{UnitsFromArgv: true, Rel: RelFunc(root)})
	if got := unitFiles(a, "web/src/a.test.ts"); !reflect.DeepEqual(got, []string{"web/src/a.ts"}) {
		t.Errorf("a %v", got)
	}
	if a.Units["web/src/b.test.ts"] == nil {
		t.Error("absolute argv path")
	}
	if len(a.Units) != 2 {
		t.Errorf("with UnitsFromArgv executed code does not define units: %v", a.Units)
	}
}

func TestWorse(t *testing.T) {
	cases := [][3]string{
		{"", "pass", "pass"}, {"pass", "fail", "fail"}, {"fail", "pass", "fail"},
		{"pass", "unknown", "unknown"}, {"unknown", "pass", "unknown"}, {"unknown", "fail", "fail"}, {"", "", ""},
	}
	for _, c := range cases {
		if got := worse(c[0], c[1]); got != c[2] {
			t.Errorf("worse(%q,%q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
