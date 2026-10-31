package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// repoDir returns a temporary repository root with symlinks resolved (as Collect expects).
func repoDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func touch(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, r := range rels {
		p := filepath.Join(root, filepath.FromSlash(r))
		if strings.HasSuffix(r, "/") {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// stream writes a proc-<pid>-<tid>.jsonl file from events (maps or raw strings).
func stream(t *testing.T, dir string, pid, tid int, events ...any) {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		if s, ok := e.(string); ok {
			b.WriteString(s)
			continue
		}
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("proc-%d-%d.jsonl", pid, tid)), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

type ev = map[string]any

func v8File(t *testing.T, traceDir string, pid int, ts int64, tid int, content string) {
	t.Helper()
	os.MkdirAll(V8Dir(traceDir), 0o755)
	if err := os.WriteFile(filepath.Join(V8Dir(traceDir), fmt.Sprintf("coverage-%d-%d-%d.json", pid, ts, tid)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func v8JSON(urls ...string) string {
	type script struct {
		ScriptID  string `json:"scriptId"`
		URL       string `json:"url"`
		Functions []any  `json:"functions"`
	}
	var res []script
	for i, u := range urls {
		res = append(res, script{ScriptID: fmt.Sprint(i), URL: u, Functions: []any{map[string]any{"functionName": "", "ranges": []any{}}}})
	}
	data, _ := json.Marshal(map[string]any{"result": res})
	return string(data)
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func byKey(procs []*Proc) map[string]*Proc {
	m := map[string]*Proc{}
	for _, p := range procs {
		m[p.Key()] = p
	}
	return m
}

func TestCollectStreams(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "src/a.ts", "src/data.json", "node_modules/x/index.js", "src/sub/")
	td := t.TempDir()
	abs := func(r string) string { return filepath.Join(root, filepath.FromSlash(r)) }
	stream(t, td, 100, 0,
		ev{"k": "start", "pid": 100, "ppid": 1, "tid": 0, "argv": []string{"node", "x"}, "cwd": root, "t": 1000, "lang": "node"},
		ev{"k": "read", "p": abs("src/data.json")},
		ev{"k": "read", "p": abs("node_modules/x/index.js")},
		ev{"k": "read", "p": "/definitely/outside/repo.txt"},
		ev{"k": "read", "p": abs("src/missing.json")},
		ev{"k": "dir", "p": abs("src/sub")},
		ev{"k": "dir", "p": root},
		ev{"k": "code", "p": abs("src/a.ts")},
		ev{"k": "unknown-kind", "p": abs("src/a.ts")},
		ev{"k": "exit", "code": 3, "t": 1500},
		`{"k":"read","p":"`+abs("src/partial")+``, // truncated last line of a killed process
	)
	// A second file for another process; a non-matching file is ignored.
	stream(t, td, 200, 0, ev{"k": "start", "pid": 200, "ppid": 100, "t": 1100}, ev{"k": "exit", "code": nil, "t": 1200})
	os.WriteFile(filepath.Join(td, "proc-x-0.jsonl"), []byte("garbage"), 0o644)
	os.WriteFile(filepath.Join(td, "other.txt"), []byte("garbage"), 0o644)

	procs, err := Collect(td, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 2 || procs[0].Pid != 100 || procs[1].Pid != 200 {
		t.Fatalf("procs %v", procs)
	}
	p := procs[0]
	if p.Ppid != 1 || p.Lang != "node" || p.Cwd != root || p.Start != 1000 || p.End != 1500 || p.Exit == nil || *p.Exit != 3 || !reflect.DeepEqual(p.Argv, []string{"node", "x"}) {
		t.Errorf("header %+v", p)
	}
	if got, want := keys(p.Reads), []string{"./", "src/data.json", "src/missing.json", "src/sub/"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reads %v, want %v", got, want)
	}
	if got := keys(p.Code); !reflect.DeepEqual(got, []string{"src/a.ts"}) {
		t.Errorf("code %v", got)
	}
	if got := keys(p.Ctx[""]); len(got) != 5 {
		t.Errorf("default context %v", got)
	}
	if procs[1].Exit != nil || procs[1].Ppid != 100 {
		t.Errorf("exit without code: %+v", procs[1])
	}
	if p.HasContexts() {
		t.Error("no contexts")
	}

	if _, err := Collect(filepath.Join(td, "nope"), root); err == nil {
		t.Error("missing trace dir must fail")
	}
}

func TestCollectV8(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "src/a.ts", "src/b.js", "src/my file.ts", "src/é.ts", "lib/w.js", "node_modules/dep/index.js", "src/q.ts")
	td := t.TempDir()
	abs := func(r string) string { return filepath.Join(root, filepath.FromSlash(r)) }
	v8File(t, td, 300, 2000, 0, v8JSON(
		"file://"+abs("src/a.ts"),
		abs("src/b.js"),
		"file://"+strings.ReplaceAll(abs("src/my file.ts"), " ", "%20"),
		"file://"+abs("src/é.ts"),
		"file://"+abs("node_modules/dep/index.js"),
		"node:internal/fs",
		"evalmachine.<anonymous>",
		"",
		abs("src/q.ts")+"?v=123",
	))
	// Worker thread 2 of the same process; plus a later file for the main thread.
	v8File(t, td, 300, 2100, 2, v8JSON("file://"+abs("lib/w.js")))
	v8File(t, td, 300, 2500, 0, v8JSON())
	// Truncated (killed) process: skipped without failing the collection.
	v8File(t, td, 400, 2000, 0, `{"result":[{"scriptId":"1","url":"file://`+abs("src/a.ts")+`","functions":[`)
	// Not coverage file names.
	os.WriteFile(filepath.Join(V8Dir(td), "coverage-x.json"), []byte("{}"), 0o644)

	procs, err := Collect(td, root)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(procs)
	main := m["300-0"]
	if main == nil || !main.Coverage || main.End != 2500 {
		t.Fatalf("main %+v", main)
	}
	want := []string{"src/a.ts", "src/b.js", "src/my file.ts", "src/q.ts", "src/é.ts"}
	if got := keys(main.Code); !reflect.DeepEqual(got, want) {
		t.Errorf("code %v, want %v", got, want)
	}
	if w := m["300-2"]; w == nil || !reflect.DeepEqual(keys(w.Code), []string{"lib/w.js"}) || w.Tid != 2 {
		t.Errorf("worker %+v", w)
	}
	if k := m["400-0"]; k == nil || len(k.Code) != 0 {
		t.Errorf("truncated coverage must add no code: %+v", k)
	}
}

func TestCollectSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root := repoDir(t)
	touch(t, root, "src/a.ts", "data/x.json")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	td := t.TempDir()
	stream(t, td, 1, 0,
		ev{"k": "start", "pid": 1},
		ev{"k": "read", "p": filepath.Join(link, "data", "x.json")},
		ev{"k": "dir", "p": link},
	)
	v8File(t, td, 1, 5, 0, v8JSON("file://"+filepath.Join(link, "src", "a.ts")))
	procs, err := Collect(td, root)
	if err != nil {
		t.Fatal(err)
	}
	p := procs[0]
	if !p.Code["src/a.ts"] || !p.Reads["data/x.json"] || !p.Reads["./"] {
		t.Errorf("paths through a symlinked root: code %v reads %v", keys(p.Code), keys(p.Reads))
	}
}

func TestCollectPythonContexts(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "tests/test_a.py", "tests/test_b.py", "src/m.py", "data/a.json", "data/db.sql", "tests/conftest.py")
	td := t.TempDir()
	abs := func(r string) string { return filepath.Join(root, filepath.FromSlash(r)) }
	stream(t, td, 500, 0,
		ev{"k": "start", "pid": 500, "ppid": 1, "tid": 0, "lang": "python", "t": 10},
		ev{"k": "code", "p": abs("tests/conftest.py")},
		ev{"k": "ctx", "id": "collect:" + abs("tests/test_a.py"), "t": 11},
		ev{"k": "code", "p": abs("src/m.py")},
		ev{"k": "ctx", "id": "", "t": 12},
		ev{"k": "ctx", "id": "test:" + abs("tests/test_a.py"), "t": 13},
		ev{"k": "read", "p": abs("data/a.json")},
		ev{"k": "ctx", "id": "fixture:db@tests", "t": 14},
		ev{"k": "read", "p": abs("data/db.sql")},
		ev{"k": "ctx", "id": "test:" + abs("tests/test_a.py"), "t": 15},
		ev{"k": "ctx", "id": "", "t": 16},
		ev{"k": "uses", "id": abs("tests/test_a.py"), "ctx": []string{"fixture:db@tests"}},
		ev{"k": "uses", "id": "/outside/test.py", "ctx": []string{"fixture:x@y"}},
		ev{"k": "result", "id": abs("tests/test_a.py"), "status": "pass", "ms": 5},
		ev{"k": "result", "id": abs("tests/test_b.py"), "status": "fail", "ms": 1},
		ev{"k": "result", "id": abs("tests/test_b.py"), "status": "pass", "ms": 2},
		ev{"k": "ctx", "id": "test:/outside/x.py", "t": 17},
		ev{"k": "exit", "code": nil, "t": 20},
	)
	procs, err := Collect(td, root)
	if err != nil {
		t.Fatal(err)
	}
	p := procs[0]
	if !p.HasContexts() {
		t.Fatal("contexts expected")
	}
	if got := keys(p.Ctx["collect:tests/test_a.py"]); !reflect.DeepEqual(got, []string{"src/m.py"}) {
		t.Errorf("collect ctx %v", got)
	}
	if got := keys(p.Ctx["test:tests/test_a.py"]); !reflect.DeepEqual(got, []string{"data/a.json"}) {
		t.Errorf("test ctx %v", got)
	}
	if got := keys(p.Ctx["fixture:db@tests"]); !reflect.DeepEqual(got, []string{"data/db.sql"}) {
		t.Errorf("fixture ctx %v", got)
	}
	if got := keys(p.Ctx[""]); !reflect.DeepEqual(got, []string{"tests/conftest.py"}) {
		t.Errorf("default ctx %v", got)
	}
	if !reflect.DeepEqual(p.Uses, map[string][]string{"tests/test_a.py": {"fixture:db@tests"}}) {
		t.Errorf("uses %v", p.Uses)
	}
	if p.Results["tests/test_a.py"] != (Result{"pass", 5}) || p.Results["tests/test_b.py"] != (Result{"fail", 3}) {
		t.Errorf("results %v (a failure is sticky, durations add up)", p.Results)
	}
	if n := len(p.CtxSpans); n != 7 || p.CtxSpans[6].ID != "test:/outside/x.py" {
		t.Errorf("spans %v (contexts outside the repo keep their raw id)", p.CtxSpans)
	}
	if p.contextAt(9) != "" || p.contextAt(13) != "test:tests/test_a.py" || p.contextAt(14) != "fixture:db@tests" || p.contextAt(16) != "" {
		t.Errorf("contextAt")
	}
}

func TestRelCtx(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "t.py")
	rel := relCache(root)
	cases := map[string]string{
		"test:" + filepath.Join(root, "t.py"):    "test:t.py",
		"collect:" + filepath.Join(root, "t.py"): "collect:t.py",
		"fixture:db@tests":                       "fixture:db@tests",
		"test:/outside.py":                       "test:/outside.py",
		"plain":                                  "plain",
		"":                                       "",
	}
	for in, want := range cases {
		if got := relCtx(in, rel); got != want {
			t.Errorf("relCtx(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestV8URLs(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, fmt.Sprintf("f%d.json", len(s)))
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	cases := []struct {
		name string
		data string
		want []string
		err  bool
	}{
		{"plain", `{"result":[{"url":"file:///a/b.js"},{"url":"/c/d.ts"},{"url":"node:fs"},{"url":""}]}`, []string{"file:///a/b.js", "/c/d.ts"}, false},
		{"windows drive", `{"result":[{"url":"C:\\x\\y.js"}]}`, []string{`C:\x\y.js`}, false},
		{"escaped quote and unicode", `{"result":[{"url":"/a/q\"x.js"},{"url":"/a/\u00e9.js"}]}`, []string{`/a/q"x.js`, "/a/é.js"}, false},
		{"trailing whitespace ok", "{\"result\":[{\"url\":\"/a.js\"}]}\n\n", []string{"/a.js"}, false},
		{"truncated", `{"result":[{"url":"/a.js"},{"url":"/b`, nil, true},
		{"unterminated escape at end", `{"url":"/a\`, nil, true},
		{"no urls", `{"result":[]}`, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := v8URLs(write(c.data))
			if (err != nil) != c.err {
				t.Fatalf("err %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	if _, err := v8URLs(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file")
	}
}

func TestResults(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "a.test.ts", "b.test.ts")
	td := t.TempDir()
	a, b := filepath.Join(root, "a.test.ts"), filepath.Join(root, "b.test.ts")
	os.WriteFile(filepath.Join(td, "results-vitest-1.jsonl"), []byte(
		`{"k":"result","id":"`+a+`","status":"pass","ms":5}`+"\n"+
			`{"k":"result","id":"`+b+`","status":"fail","ms":1}`+"\n"+
			`{"k":"read","id":"`+a+`","status":"fail"}`+"\n"+
			`not json`+"\n"+
			`{"k":"result","id":"/outside/x.test.ts","status":"fail"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(td, "results-jest-2.jsonl"), []byte(
		`{"k":"result","id":"`+b+`","status":"pass","ms":2}`+"\n"+
			`{"k":"result","id":"`+a+`","status":"pass","ms":1}`+"\n"), 0o644)
	got := Results(td, root)
	want := map[string]Result{"a.test.ts": {"pass", 6}, "b.test.ts": {"fail", 3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if len(Results(filepath.Join(td, "none"), root)) != 0 {
		t.Error("missing dir")
	}
}

func TestJUnitVitest(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "web/src/a.test.ts", "web/src/b.test.ts", "web/src/c.test.ts")
	base := filepath.Join(root, "web")
	report := filepath.Join(base, "junit.xml")
	os.WriteFile(report, []byte(`<?xml version="1.0" encoding="UTF-8" ?>
<testsuites name="vitest tests" tests="4" failures="1" errors="0" time="0.5">
    <testsuite name="src/a.test.ts" timestamp="2026-01-01T00:00:00" hostname="h" tests="2" failures="1" errors="0" skipped="0" time="0.2">
        <testcase classname="src/a.test.ts" name="adds" time="0.1"></testcase>
        <testcase classname="src/a.test.ts" name="fails" time="0.1">
            <failure message="expected 1 to be 2" type="AssertionError">trace</failure>
        </testcase>
    </testsuite>
    <testsuite name="src/b.test.ts" tests="1" failures="0" errors="0" time="0.25">
        <testcase classname="src/b.test.ts" name="ok" time="0.25"></testcase>
    </testsuite>
    <testsuite name="src/c.test.ts" tests="1" failures="0" errors="1" time="0">
        <testcase classname="src/c.test.ts" name="boom" time="0"><error message="x"/></testcase>
    </testsuite>
    <testsuite name="src/gone.test.ts" tests="1"><testcase classname="src/gone.test.ts" name="x" time="1"/></testsuite>
</testsuites>`), 0o644)
	got, err := JUnit(report, base, root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Result{"web/src/a.test.ts": {"fail", 200}, "web/src/b.test.ts": {"pass", 250}, "web/src/c.test.ts": {"fail", 0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestJUnitJestStyle(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "app/__tests__/sum.test.js", "app/lib/util.test.js", "app/x.test.js")
	base := filepath.Join(root, "app")
	report := filepath.Join(base, "reports", "junit.xml")
	os.MkdirAll(filepath.Dir(report), 0o755)
	// jest-junit: suite names are describe titles; files come from the "file" attribute
	// (addFileAttribute) on test cases or suites; nested suites inherit the file.
	os.WriteFile(report, []byte(`<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="jest tests" tests="4" failures="1" errors="0" time="1.2">
  <testsuite name="sum" errors="0" failures="1" skipped="0" timestamp="2026-01-01T00:00:00" time="0.5" tests="2">
    <testcase classname="sum adds" name="sum adds" time="0.2" file="__tests__/sum.test.js"></testcase>
    <testcase classname="sum breaks" name="sum breaks" time="0.3" file="`+filepath.Join(base, "__tests__", "sum.test.js")+`">
      <failure>Error: nope</failure>
    </testcase>
  </testsuite>
  <testsuite name="util" file="lib/util.test.js" tests="1" time="0.1">
    <testcase classname="util works" name="util works" time="0.1"></testcase>
    <testsuite name="nested" tests="1">
      <testcase classname="nested" name="n" time="0.05"/>
    </testsuite>
  </testsuite>
  <testsuite name="unknown describe" tests="1"><testcase classname="unknown describe" name="z" time="9"/></testsuite>
</testsuites>`), 0o644)
	got, err := JUnit(report, base, root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Result{"app/__tests__/sum.test.js": {"fail", 500}, "app/lib/util.test.js": {"pass", 150}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}

	// A single <testsuite> root element.
	single := filepath.Join(base, "single.xml")
	os.WriteFile(single, []byte(`<testsuite name="x.test.js" tests="1"><testcase classname="x.test.js" name="a" time="0.001"/></testsuite>`), 0o644)
	got, err = JUnit(single, base, root)
	if err != nil || !reflect.DeepEqual(got, map[string]Result{"app/x.test.js": {"pass", 1}}) {
		t.Errorf("single root: %v %v", got, err)
	}
	if _, err := JUnit(filepath.Join(base, "missing.xml"), base, root); err == nil {
		t.Error("missing report")
	}
	bad := filepath.Join(base, "bad.xml")
	os.WriteFile(bad, []byte(`<testsuites><testsuite`), 0o644)
	if _, err := JUnit(bad, base, root); err == nil {
		t.Error("malformed report")
	}
}

func TestParseTestLog(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "pkg/a/data.txt", "pkg/a/testdata/x.golden", "pkg/top.txt", "pkg/a/dir/", "other/abs.txt")
	pkg := filepath.Join(root, "pkg", "a")
	log := filepath.Join(t.TempDir(), "test.log")
	os.WriteFile(log, []byte(strings.Join([]string{
		"# test log",
		"getenv HOME",
		"open data.txt",
		"open data.txt", // duplicate
		"stat ../top.txt",
		"open dir",
		"stat dir",
		"open missing.txt",
		"chdir testdata",
		"open x.golden",
		"chdir " + filepath.Join(root, "other"),
		"open abs.txt",
		"open " + filepath.Join(root, "pkg", "a", "data.txt"), // same file, absolute: deduplicated
		"malformed",
		"",
	}, "\n")), 0o644)
	got := parseTestLog(log, pkg)
	sep := string(filepath.Separator)
	want := []string{
		filepath.Join(pkg, "data.txt"),
		filepath.Join(root, "pkg", "top.txt") + "?",
		filepath.Join(pkg, "dir") + sep,
		filepath.Join(pkg, "dir") + "?",
		filepath.Join(pkg, "missing.txt") + "?",
		filepath.Join(pkg, "testdata", "x.golden"),
		filepath.Join(root, "other", "abs.txt"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n %q\nwant\n %q", got, want)
	}
	if parseTestLog(filepath.Join(root, "nope.log"), pkg) != nil {
		t.Error("missing log")
	}
}

func TestCollectGo(t *testing.T) {
	td := t.TempDir()
	os.WriteFile(filepath.Join(td, "go-1.json"), []byte(`{"dir":"/r/pkg","files":["/r/pkg/a"],"exit":1,"ms":5}`), 0o644)
	os.WriteFile(filepath.Join(td, "go-2.json"), []byte(`{broken`), 0o644)
	os.WriteFile(filepath.Join(td, "go-3.testlog"), []byte(`x`), 0o644)
	runs, err := CollectGo(td)
	if err != nil || len(runs) != 1 || runs[0].Dir != "/r/pkg" || runs[0].Exit != 1 || runs[0].Ms != 5 || runs[0].Files[0] != "/r/pkg/a" {
		t.Fatalf("%+v %v", runs, err)
	}
	if _, err := CollectGo(filepath.Join(td, "none")); err == nil {
		t.Error("missing dir")
	}
}

func TestEnv(t *testing.T) {
	sh := &Shims{Dir: "/s", Node: "/s/node.cjs", Python: "/s/py"}
	t.Setenv("NODE_OPTIONS", "")
	t.Setenv("PYTHONPATH", "")
	t.Setenv("PYTEST_PLUGINS", "")
	env := NodeEnv("/tr", "/repo", sh, []string{"NODE_OPTIONS=--max-old-space-size=4096"})
	want := []string{"SMATES_TRACE_DIR=/tr", "SMATES_REPO_ROOT=/repo", "NODE_V8_COVERAGE=" + V8Dir("/tr"), "NODE_OPTIONS=--require=/s/node.cjs --max-old-space-size=4096"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("NodeEnv %v", env)
	}
	// Already present: not added twice.
	env = NodeEnv("/tr", "/repo", sh, []string{"NODE_OPTIONS=--require=/s/node.cjs"})
	if env[3] != "NODE_OPTIONS=--require=/s/node.cjs" {
		t.Errorf("%v", env[3])
	}
	// Paths with spaces are quoted for NODE_OPTIONS.
	env = NodeEnv("/tr", "/repo", &Shims{Node: "/my dir/node.cjs"}, nil)
	if env[3] != `NODE_OPTIONS=--require="/my dir/node.cjs"` {
		t.Errorf("%v", env[3])
	}
	// The process environment is the fallback.
	t.Setenv("NODE_OPTIONS", "--inspect")
	env = NodeEnv("/tr", "/repo", sh, nil)
	if env[3] != "NODE_OPTIONS=--require=/s/node.cjs --inspect" {
		t.Errorf("%v", env[3])
	}

	py := PythonEnv("/tr", "/repo", sh, []string{"PYTHONPATH=/a", "PYTEST_PLUGINS=xdist"})
	wantPy := []string{"SMATES_TRACE_DIR=/tr", "SMATES_REPO_ROOT=/repo", "PYTHONPATH=/s/py" + string(os.PathListSeparator) + "/a", "PYTEST_PLUGINS=xdist,smates_pytest"}
	if !reflect.DeepEqual(py, wantPy) {
		t.Errorf("PythonEnv %v", py)
	}
	py = PythonEnv("/tr", "/repo", sh, nil)
	if py[2] != "PYTHONPATH=/s/py" || py[3] != "PYTEST_PLUGINS=smates_pytest" {
		t.Errorf("%v", py)
	}
}

func TestExtractShims(t *testing.T) {
	dir := t.TempDir()
	sh, err := ExtractShims(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{sh.Node, filepath.Join(sh.Python, "smates_trace.py"), filepath.Join(sh.Python, "sitecustomize.py"), filepath.Join(sh.Python, "smates_pytest.py"), filepath.Join(dir, "vitest-reporter.mjs"), filepath.Join(dir, "jest-reporter.cjs")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestGoExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh as the test binary")
	}
	td := t.TempDir()
	t.Setenv("SMATES_TRACE_DIR", td)
	t.Setenv(GoExecEnv, "1")
	cwd, _ := os.Getwd()
	os.WriteFile(filepath.Join(td, "data.txt"), []byte("x"), 0o644)
	logFile := filepath.Join(td, "given.log")
	// The "test binary" writes a test log and checks that the wrapper's marker is gone.
	script := `f="${0#-test.testlogfile=}"; printf '# test log\nopen ` + filepath.Join(td, "data.txt") + `\nstat nope\n' > "$f"; [ -z "$` + GoExecEnv + `" ] || exit 9; exit 3`
	code := GoExec([]string{"/bin/sh", "-c", script, "-test.testlogfile=" + logFile})
	if code != 3 {
		t.Fatalf("exit code %d", code)
	}
	runs, err := CollectGo(td)
	if err != nil || len(runs) != 1 {
		t.Fatalf("%v %v", runs, err)
	}
	r := runs[0]
	want := []string{filepath.Join(td, "data.txt"), filepath.Join(cwd, "nope") + "?"}
	if r.Exit != 3 || r.Dir != cwd || !reflect.DeepEqual(r.Files, want) {
		t.Errorf("run %+v, want files %v", r, want)
	}
	if GoExec(nil) != 2 {
		t.Error("missing binary argument")
	}
	if GoExec([]string{filepath.Join(td, "does-not-exist")}) != 1 {
		t.Error("unstartable binary")
	}
	if !IsGoExec() {
		t.Error("IsGoExec")
	}
}
