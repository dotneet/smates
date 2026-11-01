package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

func tempRepo(t *testing.T, files ...string) *gitx.Repo {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		p := filepath.Join(d, filepath.FromSlash(f))
		if strings.HasSuffix(f, "/") {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &gitx.Repo{Root: d}
}

func TestSplitWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{"", nil, false},
		{"   ", nil, false},
		{"go test", []string{"go", "test"}, false},
		{"  a\tb\nc  ", []string{"a", "b", "c"}, false},
		{`a 'b c' d`, []string{"a", "b c", "d"}, false},
		{`a "b c" d`, []string{"a", "b c", "d"}, false},
		{`a "b \"c\" \\ \$x \n" d`, []string{"a", `b "c" \ $x \n`, "d"}, false},
		{`'it''s'`, []string{"its"}, false},
		{`a\ b`, []string{"a b"}, false},
		{`x'y'"z"w`, []string{"xyzw"}, false},
		{`''`, []string{""}, false},
		{`""`, []string{""}, false},
		{`a '' b`, []string{"a", "", "b"}, false},
		{`'$HOME' "*"`, []string{"$HOME", "*"}, false},
		{`trailing\`, []string{`trailing\`}, false},
		{`--opt='{"a": 1}'`, []string{`--opt={"a": 1}`}, false},
		{`a 'b`, nil, true},
		{`a "b`, nil, true},
		{`a "b\"`, nil, true},
	}
	for _, c := range cases {
		got, err := SplitWords(c.in)
		if (err != nil) != c.err {
			t.Errorf("%q: err %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitWords(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCommandLine(t *testing.T) {
	cases := []struct {
		cmd       string
		extraArgs []string
		extra     []string
		tests     []string
		want      []string
		err       bool
	}{
		{"vitest run", nil, nil, []string{"a.ts", "b.ts"}, []string{"vitest", "run", "a.ts", "b.ts"}, false},
		{"vitest run", []string{"--bail"}, []string{"--reporter=x"}, []string{"a.ts"}, []string{"vitest", "run", "--reporter=x", "--bail", "a.ts"}, false},
		{"pytest {tests} -q", []string{"-x"}, []string{"-p", "y"}, []string{"t1.py", "t 2.py"}, []string{"pytest", "-p", "y", "-x", "t1.py", "t 2.py", "-q"}, false},
		{"pytest {tests} -q", nil, nil, nil, []string{"pytest", "-q"}, false},
		{"make test TESTS='{tests}'", nil, nil, []string{"a"}, []string{"make", "test", "TESTS={tests}", "a"}, false},
		{"run 'unterminated", nil, nil, nil, nil, true},
	}
	for _, c := range cases {
		rc := &RunContext{Suite: &Suite{Suite: config.Suite{Name: "s", Command: c.cmd}}, ExtraArgs: c.extraArgs}
		got, err := rc.CommandLine(c.extra, c.tests)
		if (err != nil) != c.err {
			t.Errorf("%q: err %v", c.cmd, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q want %q", c.cmd, got, c.want)
		}
	}
}

func TestRelArgsAndDir(t *testing.T) {
	repo := &gitx.Repo{Root: "/r"}
	rc := &RunContext{Repo: repo, Suite: &Suite{Suite: config.Suite{Root: "web"}}, Units: []string{"web/a.test.ts", "web/sub/b.test.ts"}}
	if got := rc.RelArgs(""); !reflect.DeepEqual(got, []string{"a.test.ts", "sub/b.test.ts"}) {
		t.Errorf("%v", got)
	}
	if got := rc.RelArgs("./"); !reflect.DeepEqual(got, []string{"./a.test.ts", "./sub/b.test.ts"}) {
		t.Errorf("%v", got)
	}
	if rc.Dir() != filepath.Join("/r", "web") {
		t.Errorf("dir %s", rc.Dir())
	}
	rc.Suite.Root = "."
	if got := rc.RelArgs(""); !reflect.DeepEqual(got, []string{"web/a.test.ts", "web/sub/b.test.ts"}) {
		t.Errorf("%v", got)
	}
}

func TestChunk(t *testing.T) {
	if got := chunk(nil); len(got) != 1 || len(got[0]) != 0 {
		t.Errorf("empty input yields one empty batch: %v", got)
	}
	small := []string{"a", "b"}
	if got := chunk(small); len(got) != 1 || !reflect.DeepEqual(got[0], small) {
		t.Errorf("%v", got)
	}
	arg := strings.Repeat("x", 1000)
	var many []string
	for i := 0; i < 250; i++ {
		many = append(many, fmt.Sprintf("%s%03d", arg, i))
	}
	got := chunk(many)
	if len(got) < 3 {
		t.Fatalf("expected several batches, got %d", len(got))
	}
	var flat []string
	for _, b := range got {
		size := 0
		for _, a := range b {
			size += len(a) + 1
		}
		if size > maxArgBytes {
			t.Errorf("batch of %d bytes", size)
		}
		flat = append(flat, b...)
	}
	if !reflect.DeepEqual(flat, many) {
		t.Error("chunking must keep every argument in order")
	}
	// A single oversized argument still runs (alone).
	huge := strings.Repeat("y", maxArgBytes+10)
	got = chunk([]string{"a", huge, "b"})
	if len(got) != 3 || got[1][0] != huge {
		t.Errorf("oversized argument: %d batches", len(got))
	}
}

func TestMergeEnvAndGetenv(t *testing.T) {
	got := MergeEnv([]string{"A=1", "B=2", "NOVALUE"}, nil, []string{"B=3", "C=4", "A="}, []string{"NOVALUE=x"})
	want := []string{"A=", "B=3", "NOVALUE=x", "C=4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeEnv %v, want %v", got, want)
	}
	if MergeEnv() != nil {
		t.Error("empty")
	}
	t.Setenv("SMATES_TEST_GETENV", "from-process")
	env := []string{"X=1", "X=2", "XY=3"}
	if Getenv(env, "X") != "2" || Getenv(env, "XY") != "3" || Getenv(env, "SMATES_TEST_GETENV") != "from-process" || Getenv(nil, "SMATES_TEST_NOPE") != "" {
		t.Error("Getenv")
	}
	s := &Suite{Suite: config.Suite{Env: map[string]string{"B": "2", "A": "1"}}}
	if got := suiteEnv(s); !reflect.DeepEqual(got, []string{"A=1", "B=2"}) {
		t.Errorf("suiteEnv %v", got)
	}
}

func TestResolve(t *testing.T) {
	repo := tempRepo(t, "web/node_modules/.bin/vitest", "pnpm-lock.yaml", "app/package.json")
	got, err := Resolve(repo, []config.Suite{
		{Name: "web", Root: "web", Runner: "vitest"},
		{Name: "app", Root: "app", Runner: "jest", Tests: []string{"spec/**/*.js"}, Exclude: []string{"spec/slow/**"}, Inputs: []string{"/schema.graphql"}},
		{Name: "go", Root: ".", Runner: "go", Command: "go test -race"},
		{Name: "py", Root: ".", Runner: "pytest"},
		{Name: "c", Root: ".", Runner: "custom", Command: "make test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	web, app, gos, py := got[0], got[1], got[2], got[3]
	if web.Command != "./node_modules/.bin/vitest run" || !reflect.DeepEqual(web.Tests, []string{"**/*.{test,spec}." + jsTestExts}) || web.Adapter.Name() != "vitest" {
		t.Errorf("web %+v", web)
	}
	if app.Command != "pnpm exec jest" {
		t.Errorf("jest via pnpm: %q", app.Command)
	}
	if !reflect.DeepEqual(app.Tests, []string{"spec/**/*.js"}) {
		t.Errorf("configured tests replace defaults: %v", app.Tests)
	}
	if app.Exclude[len(app.Exclude)-1] != "spec/slow/**" || len(app.Exclude) < 2 {
		t.Errorf("configured excludes are added to defaults: %v", app.Exclude)
	}
	if app.Inputs[len(app.Inputs)-1] != "/schema.graphql" || !contains(app.Inputs, "jest.config.*") {
		t.Errorf("inputs %v", app.Inputs)
	}
	if gos.Command != "go test -race" || !contains(gos.Inputs, "go.mod") {
		t.Errorf("go %+v", gos)
	}
	if py.Command != "python3 -m pytest" {
		t.Errorf("py %q", py.Command)
	}
	if web.Narrowed || !app.Narrowed || gos.Narrowed {
		t.Errorf("narrowed when tests or exclude are configured: web=%v app=%v go=%v", web.Narrowed, app.Narrowed, gos.Narrowed)
	}
	// The configured suites are not modified in place (defaults go to copies).
	cs := []config.Suite{{Name: "x", Runner: "go", Exclude: make([]string, 0, 10)}}
	r1, _ := Resolve(repo, cs)
	r2, _ := Resolve(repo, cs)
	if len(r1[0].Exclude) != len(r2[0].Exclude) {
		t.Error("Resolve must not accumulate defaults across calls")
	}

	if _, err := Resolve(repo, []config.Suite{{Name: "x", Runner: "nope"}}); err == nil || !strings.Contains(err.Error(), "unknown runner") {
		t.Errorf("unknown runner: %v", err)
	}
	if _, err := Resolve(repo, []config.Suite{{Name: "x", Runner: "custom"}}); err == nil {
		t.Error("custom without a command")
	}
	if n := Names(); !contains(n, "go") || !contains(n, "vitest") || !contains(n, "jest") || !contains(n, "pytest") || !contains(n, "bun") || !contains(n, "node") || !contains(n, "custom") {
		t.Errorf("names %v", n)
	}
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func TestIsTestFileAndInputs(t *testing.T) {
	s := &Suite{Suite: config.Suite{
		Root:    "web",
		Tests:   []string{"**/*.test.ts", "e2e/*.spec.ts"},
		Exclude: []string{"**/fixtures/**"},
		Inputs:  []string{"vitest.config.*", "/package.json", "/shared/**/*.json", "src/setup.ts"},
	}}
	tests := map[string]bool{
		"web/a.test.ts":                 true,
		"web/src/deep/b.test.ts":        true,
		"web/e2e/x.spec.ts":             true,
		"web/e2e/sub/x.spec.ts":         false,
		"web/fixtures/c.test.ts":        false,
		"web/node_modules/p/d.test.ts":  false,
		"a.test.ts":                     false,
		"webapp/a.test.ts":              false,
		"web/a.ts":                      false,
		"web/src/__pycache__/x.test.ts": false,
	}
	for f, want := range tests {
		if got := s.IsTestFile(f); got != want {
			t.Errorf("IsTestFile(%q) = %v", f, got)
		}
	}
	files := []string{"web/z.test.ts", "web/a.test.ts", "web/a.ts"}
	if got := s.TestFiles(files); !reflect.DeepEqual(got, []string{"web/a.test.ts", "web/z.test.ts"}) {
		t.Errorf("TestFiles %v", got)
	}
	inputs := map[string]bool{
		"web/vitest.config.ts":     true,
		"vitest.config.ts":         false, // root-relative globs are relative to the suite root
		"web/sub/vitest.config.ts": false,
		"package.json":             true,
		"web/package.json":         false,
		"shared/a/b.json":          true,
		"shared/b.json":            true,
		"web/shared/b.json":        false,
		"web/src/setup.ts":         true,
		"webx/src/setup.ts":        false,
	}
	for f, want := range inputs {
		if got := s.InputMatch(f); got != want {
			t.Errorf("InputMatch(%q) = %v", f, got)
		}
	}
	root := &Suite{Suite: config.Suite{Root: ".", Tests: []string{"**/test_*.py"}, Inputs: []string{"pyproject.toml"}}}
	if !root.IsTestFile("test_a.py") || !root.IsTestFile("pkg/tests/test_b.py") || root.IsTestFile(".venv/lib/test_x.py") {
		t.Error("root suite tests")
	}
	if !root.InputMatch("pyproject.toml") || root.InputMatch("sub/pyproject.toml") {
		t.Error("root suite inputs")
	}
}

func TestGoUnits(t *testing.T) {
	a, _ := Get("go")
	s := &Suite{Suite: config.Suite{Root: "."}}
	got := a.Units(s, []string{"a_test.go", "pkg/x/a_test.go", "pkg/x/b_test.go", "pkg/y/c_test.go"})
	if !reflect.DeepEqual(got, []string{".", "pkg/x", "pkg/y"}) {
		t.Errorf("%v", got)
	}
	for _, name := range []string{"vitest", "jest", "node", "bun", "pytest", "custom"} {
		a, _ := Get(name)
		if got := a.Units(s, []string{"b", "a"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Errorf("%s units %v", name, got)
		}
	}
	d := goTest{}.Defaults(nil, ".")
	gs := &Suite{Suite: config.Suite{Root: ".", Tests: d.Tests, Exclude: d.Exclude}}
	for f, want := range map[string]bool{"a_test.go": true, "x/testdata/a_test.go": false, "vendor/m/a_test.go": false, "_tools/a_test.go": false, ".hidden/a_test.go": false, "a.go": false} {
		if gs.IsTestFile(f) != want {
			t.Errorf("go IsTestFile(%q) != %v", f, want)
		}
	}
}

func TestUserConfigures(t *testing.T) {
	repo := tempRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(repo.Root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	rc := func(cmd string, extra ...string) *RunContext {
		return &RunContext{Repo: repo, Suite: &Suite{Suite: config.Suite{Root: "web", Command: cmd}}, ExtraArgs: extra}
	}
	globs := []string{"vitest.config.*"}
	if userConfigures(rc("vitest run"), globs, "reporters", "--reporter") {
		t.Error("nothing configured")
	}
	if !userConfigures(rc("vitest run --reporter=dot"), globs, "reporters", "--reporter") {
		t.Error("flag in the command")
	}
	if !userConfigures(rc("vitest run", "--reporter", "json"), globs, "reporters", "--reporter") {
		t.Error("flag in extra args")
	}
	write("web/package.json", `{"name":"x","description":"no reporters here"}`)
	if userConfigures(rc("vitest run"), globs, "reporters", "--reporter") {
		t.Error("package.json mentions the word but not the key")
	}
	write("web/package.json", `{"jest":{"reporters":["default"]}}`)
	if !userConfigures(rc("vitest run"), globs, "reporters", "--reporter") {
		t.Error("package.json key")
	}
	os.Remove(filepath.Join(repo.Root, "web", "package.json"))
	write("web/vitest.config.ts", `export default { test: { reporters: ['verbose'] } }`)
	if !userConfigures(rc("vitest run"), globs, "reporters", "--reporter") {
		t.Error("config file")
	}
}

func TestReadLcov(t *testing.T) {
	repo := tempRepo(t, "web/src/a.ts", "web/src/b.ts", "lib/c.ts", "web/node_modules/d/i.js")
	cwd := filepath.Join(repo.Root, "web")
	lcov := filepath.Join(t.TempDir(), "lcov.info")
	os.WriteFile(lcov, []byte(strings.Join([]string{
		"TN:", "SF:src/a.ts", "DA:1,1", "end_of_record",
		"SF:" + filepath.Join(repo.Root, "web", "src", "b.ts"), "end_of_record",
		"SF:../lib/c.ts", "SF:node_modules/d/i.js", "SF:/outside/x.ts", "",
	}, "\n")), 0o644)
	got, err := readLcov(lcov, cwd, repo.Root)
	if err != nil || !reflect.DeepEqual(got, []string{"web/src/a.ts", "web/src/b.ts", "lib/c.ts"}) {
		t.Errorf("%v %v", got, err)
	}
	if _, err := readLcov(filepath.Join(cwd, "nope"), cwd, repo.Root); err == nil {
		t.Error("missing lcov")
	}
}

func TestOverallStatus(t *testing.T) {
	cases := []struct {
		unit string
		code int
		want string
	}{
		{state.Pass, 1, state.Pass}, {state.Fail, 0, state.Fail}, {"", 0, state.Pass}, {"", 2, state.Fail}, {"unknown", 0, state.Pass}, {"unknown", 1, state.Fail},
	}
	for _, c := range cases {
		if got := overallStatus(c.unit, c.code); got != c.want {
			t.Errorf("overallStatus(%q,%d) = %q", c.unit, c.code, got)
		}
	}
}

func TestJSCommands(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		dir   string
		want  string
	}{
		{"local bin", []string{"pkg/node_modules/.bin/vitest"}, "pkg", "./node_modules/.bin/vitest run"},
		{"hoisted bin", []string{"node_modules/.bin/vitest", "pkg/a/package.json"}, "pkg/a", "../../node_modules/.bin/vitest run"},
		{"pnpm", []string{"pnpm-lock.yaml", "pkg/package.json"}, "pkg", "pnpm exec vitest run"},
		{"yarn", []string{"yarn.lock", "pkg/package.json"}, "pkg", "yarn vitest run"},
		{"bun", []string{"bun.lock", "pkg/package.json"}, "pkg", "bunx vitest run"},
		{"bun lockb", []string{"bun.lockb"}, ".", "bunx vitest run"},
		{"npm", []string{"package-lock.json"}, ".", "npx --no-install vitest run"},
		{"nothing", []string{"pkg/package.json"}, "pkg", "npx --no-install vitest run"},
		{"nearest lockfile wins", []string{"pnpm-lock.yaml", "pkg/yarn.lock"}, "pkg", "yarn vitest run"},
	}
	for _, c := range cases {
		repo := tempRepo(t, c.files...)
		dir := filepath.Join(repo.Root, filepath.FromSlash(c.dir))
		if got := jsBinCommand(repo.Root, dir, "vitest", "run"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// A bin outside the repository is never used.
	outer := tempRepo(t, "node_modules/.bin/jest", "repo/pkg/package.json")
	root := filepath.Join(outer.Root, "repo")
	if got := jsBinCommand(root, filepath.Join(root, "pkg"), "jest", ""); got != "npx --no-install jest" {
		t.Errorf("bin above the repo root: %q", got)
	}
}

func TestPythonCommand(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		dir   string
		want  string
	}{
		{"uv lock", map[string]string{"uv.lock": ""}, ".", "uv run pytest"},
		{"uv in parent", map[string]string{"uv.lock": "", "svc/pyproject.toml": ""}, "svc", "uv run pytest"},
		{"tool.uv", map[string]string{"pyproject.toml": "[tool.uv]\ndev-dependencies = []\n"}, ".", "uv run pytest"},
		{"venv", map[string]string{"pyproject.toml": "[project]\n", ".venv/bin/python": ""}, ".", "./.venv/bin/python -m pytest"},
		{"parent venv", map[string]string{"venv/bin/python": "", "svc/pyproject.toml": ""}, "svc", "./../venv/bin/python -m pytest"},
		{"plain", map[string]string{"setup.py": ""}, ".", "python3 -m pytest"},
	}
	for _, c := range cases {
		var files []string
		for f := range c.files {
			files = append(files, f)
		}
		repo := tempRepo(t, files...)
		for f, content := range c.files {
			os.WriteFile(filepath.Join(repo.Root, filepath.FromSlash(f)), []byte(content), 0o755)
		}
		if got := pythonCommand(repo.Root, filepath.Join(repo.Root, filepath.FromSlash(c.dir))); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestExecAndBatches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	repo := tempRepo(t, "web/")
	var out bytes.Buffer
	rc := &RunContext{Repo: repo, Suite: &Suite{Suite: config.Suite{Root: "web", Env: map[string]string{"SUITE_VAR": "s"}}}, Stdout: &out, Stderr: &out}
	code, err := rc.Exec(context.Background(), []string{"sh", "-c", `echo "$PWD|$SUITE_VAR|$RUN_VAR"; exit 4`}, []string{"RUN_VAR=r"})
	if err != nil || code != 4 {
		t.Fatalf("%d %v", code, err)
	}
	if got := strings.TrimSpace(out.String()); got != filepath.Join(repo.Root, "web")+"|s|r" {
		t.Errorf("output %q", got)
	}
	if code, err := rc.Exec(context.Background(), []string{"/no/such/binary"}, nil); err == nil || code != 127 {
		t.Errorf("missing binary: %d %v", code, err)
	}
	if _, err := rc.Exec(context.Background(), nil, nil); err == nil {
		t.Error("empty argv")
	}

	// runBatches: worst exit code, accepted codes mapped to 0.
	rc.Suite.Command = `sh -c 'exit $1' sh`
	out.Reset()
	code, err = runBatches(context.Background(), rc, nil, []string{"5"}, nil, 5)
	if err != nil || code != 0 {
		t.Errorf("accepted code: %d %v", code, err)
	}
	code, err = runBatches(context.Background(), rc, nil, []string{"3"}, nil, 5)
	if err != nil || code != 3 {
		t.Errorf("other code: %d %v", code, err)
	}
	rc.All = true
	code, _ = runBatches(context.Background(), rc, nil, []string{"7"}, nil)
	if code != 0 {
		t.Errorf("All runs once without test arguments: %d", code)
	}
}

func TestCustomRunAllPassesFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	// A custom command has no discovery: a full run still passes every test file.
	repo := tempRepo(t, "a_test.sh", "b_test.sh")
	var out bytes.Buffer
	su := &Suite{Suite: config.Suite{Root: ".", Command: `sh -c 'echo "$#:$*"; exit 3' sh {tests}`}, Adapter: custom{}}
	rc := &RunContext{Repo: repo, Suite: su, Units: []string{"a_test.sh", "b_test.sh"}, All: true, Stdout: &out, Stderr: &out}
	res, err := custom{}.Run(context.Background(), rc)
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := strings.TrimSpace(out.String()); got != "2:a_test.sh b_test.sh" {
		t.Errorf("output %q", got)
	}
}

func TestObserve(t *testing.T) {
	repo := tempRepo(t, "a.test.ts", "b.test.ts", "src/a.ts", "src/b.ts", "fixtures/", "server.js", "junit.xml")
	td := t.TempDir()
	os.MkdirAll(trace.V8Dir(td), 0o755)
	abs := func(r string) string { return filepath.Join(repo.Root, filepath.FromSlash(r)) }
	writeStream := func(pid, ppid int, extra ...map[string]any) {
		lines := []string{mustJSON(map[string]any{"k": "start", "pid": pid, "ppid": ppid, "t": 1})}
		for _, e := range extra {
			lines = append(lines, mustJSON(e))
		}
		os.WriteFile(filepath.Join(td, fmt.Sprintf("proc-%d-0.jsonl", pid)), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	cov := func(pid int, files ...string) {
		var scripts []map[string]any
		for _, f := range files {
			scripts = append(scripts, map[string]any{"url": "file://" + abs(f)})
		}
		os.WriteFile(filepath.Join(trace.V8Dir(td), fmt.Sprintf("coverage-%d-1-0.json", pid)), []byte(mustJSON(map[string]any{"result": scripts})), 0o644)
	}
	writeStream(10, 1)
	writeStream(11, 10, map[string]any{"k": "dir", "p": abs("fixtures")}, map[string]any{"k": "exit", "code": 1, "t": 5})
	cov(11, "a.test.ts", "src/a.ts")
	writeStream(12, 10, map[string]any{"k": "exit", "code": 0, "t": 5})
	cov(12, "b.test.ts", "src/b.ts")
	writeStream(13, 10) // helper running repo code: global
	cov(13, "server.js")
	os.WriteFile(filepath.Join(td, "results-vitest-1.jsonl"), []byte(mustJSON(map[string]any{"k": "result", "id": abs("b.test.ts"), "status": "fail", "ms": 42})+"\n"), 0o644)

	unitOf := func(f string) string {
		if strings.HasSuffix(f, ".test.ts") {
			return f
		}
		return ""
	}
	rc := &RunContext{Repo: repo, Suite: &Suite{Suite: config.Suite{Root: "."}}, TraceDir: td, UnitOf: unitOf}

	obs, _, err := observe(rc, 0, traceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := obs["a.test.ts"], obs["b.test.ts"]
	if a == nil || b == nil {
		t.Fatalf("obs %v", obs)
	}
	if !a.Files["src/a.ts"] || !a.Files["fixtures/"] || !a.Files["server.js"] || a.Files["src/b.ts"] {
		t.Errorf("a files %v", a.Files)
	}
	if a.Status != state.Pass {
		t.Errorf("process exit codes are ignored unless trusted; overall exit 0: %s", a.Status)
	}
	if b.Status != state.Fail || b.DurationMs != 42 {
		t.Errorf("reporter results win: %+v", b)
	}

	obs, _, _ = observe(rc, 0, traceOptions{trustProcStatus: true, dropDirs: true})
	if obs["a.test.ts"].Status != state.Fail || obs["a.test.ts"].Files["fixtures/"] {
		t.Errorf("trusted exit code / dropped dirs: %+v", obs["a.test.ts"])
	}
	obs, _, _ = observe(rc, 1, traceOptions{})
	if obs["a.test.ts"].Status != state.Fail {
		t.Errorf("a failed run without per-unit status fails every unit: %+v", obs["a.test.ts"])
	}

	// JUnit results override, and a missing report is a warning, not an error.
	os.WriteFile(abs("junit.xml"), []byte(`<testsuites><testsuite name="a.test.ts"><testcase classname="a.test.ts" time="0.003"><failure/></testcase></testsuite></testsuites>`), 0o644)
	rc.Suite.Junit = "junit.xml"
	obs, warns, err := observe(rc, 0, traceOptions{})
	if err != nil || obs["a.test.ts"].Status != state.Fail || obs["a.test.ts"].DurationMs != 3 || len(warns) != 0 {
		t.Errorf("junit: %+v %v %v", obs["a.test.ts"], warns, err)
	}
	rc.Suite.Junit = "missing.xml"
	if _, warns, err := observe(rc, 0, traceOptions{}); err != nil || len(warns) != 1 {
		t.Errorf("missing junit: %v %v", warns, err)
	}
}

// A unit the runner reported but no traced process ran (its worker was killed before
// writing its coverage, or it failed to import) must not be recorded as a clean pass.
func TestObserveUntracedResults(t *testing.T) {
	repo := tempRepo(t, "a.test.ts", "b.test.ts")
	td := t.TempDir()
	os.MkdirAll(trace.V8Dir(td), 0o755)
	abs := func(r string) string { return filepath.Join(repo.Root, filepath.FromSlash(r)) }
	var lines []string
	for _, r := range []map[string]any{
		{"k": "result", "id": abs("a.test.ts"), "status": "pass", "ms": 5},
		{"k": "result", "id": abs("b.test.ts"), "status": "fail", "ms": 7},
	} {
		lines = append(lines, mustJSON(r))
	}
	os.WriteFile(filepath.Join(td, "results-vitest-1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	unitOf := func(f string) string {
		if strings.HasSuffix(f, ".test.ts") {
			return f
		}
		return ""
	}
	rc := &RunContext{Repo: repo, Suite: &Suite{Suite: config.Suite{Root: "."}}, TraceDir: td, UnitOf: unitOf}
	obs, warns, err := observe(rc, 0, traceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a := obs["a.test.ts"]; a == nil || a.Status != state.Unknown {
		t.Errorf("untraced pass must run again: %+v", a)
	}
	if b := obs["b.test.ts"]; b == nil || b.Status != state.Fail {
		t.Errorf("untraced failure stays a failure: %+v", b)
	}
	if len(warns) != 1 {
		t.Errorf("warnings %v", warns)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// In an isolated run each unit's status is its own process's exit code: a failing
// sibling must not leave the other units marked inferred (flake detection skips those).
// Both isolated paths (runIsolatedNode and bun.Run) run here with a stub runner that
// fails b.test.js; the traces name each unit on the command line of its process.
func TestIsolatedRunStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	units := []string{"a.test.js", "b.test.js"}
	setup := func(t *testing.T, command string) *RunContext {
		repo := tempRepo(t, units...)
		td := t.TempDir()
		os.MkdirAll(trace.V8Dir(td), 0o755)
		start := func(pid, ppid int, argv ...string) {
			ev := map[string]any{"k": "start", "pid": pid, "ppid": ppid, "t": 1, "argv": argv, "cwd": repo.Root}
			os.WriteFile(filepath.Join(td, fmt.Sprintf("proc-%d-0.jsonl", pid)), []byte(mustJSON(ev)+"\n"), 0o644)
		}
		start(10, 1)
		for i, u := range units {
			start(11+i, 10, "runner", filepath.Join(repo.Root, u))
		}
		var out bytes.Buffer
		return &RunContext{
			Repo: repo, Suite: &Suite{Suite: config.Suite{Root: ".", Command: command}}, Units: units, Record: true,
			TraceDir: td, Stdout: &out, Stderr: &out, UnitOf: func(f string) string {
				if slices.Contains(units, f) {
					return f
				}
				return ""
			},
		}
	}
	check := func(t *testing.T, res *RunResult, err error) {
		t.Helper()
		if err != nil || res.ExitCode != 1 {
			t.Fatalf("run: %+v %v", res, err)
		}
		for u, want := range map[string]string{"a.test.js": state.Pass, "b.test.js": state.Fail} {
			if o := res.Observations[u]; o == nil || o.Status != want || o.Inferred {
				t.Errorf("%s: %+v", u, o)
			}
		}
	}

	t.Run("node", func(t *testing.T) {
		rc := setup(t, `sh -c 'case $1 in *b.test*) exit 1;; esac' sh`)
		res, err := runIsolatedNode(context.Background(), rc, func(u string) []string { return []string{u} }, traceOptions{unitsFromArgv: true})
		check(t, res, err)
	})
	t.Run("bun", func(t *testing.T) {
		// Like bun test --coverage: write lcov to --coverage-dir; the test file comes last.
		rc := setup(t, `sh -c 'for a; do case $a in --coverage-dir=*) d=${a#--coverage-dir=};; esac; last=$a; done
mkdir -p "$d" && printf "SF:%s\\nend_of_record\\n" "$last" > "$d/lcov.info"
case $last in *b.test*) exit 1;; esac' sh`)
		res, err := bun{}.Run(context.Background(), rc)
		check(t, res, err)
	})

}

// A command line that cannot be built starts nothing.
func TestIsolatedRunCommandLineError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	repo := tempRepo(t)
	var out bytes.Buffer
	rc := &RunContext{Repo: repo, Suite: &Suite{Suite: config.Suite{Root: "."}}, Stdout: &out, Stderr: &out}
	units := []string{"a.test.js", "b.test.js"}
	marker := filepath.Join(repo.Root, "ran")
	_, err := isolatedRun(context.Background(), rc, units, 1, func(u string, i int) ([]string, []string, error) {
		if i == 1 {
			return nil, nil, fmt.Errorf("bad command")
		}
		return []string{"sh", "-c", "touch " + marker}, nil, nil
	})
	if err == nil {
		t.Error("want the command line error")
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Error("a unit ran although a command line failed")
	}
}
