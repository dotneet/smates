package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"gopkg.in/yaml.v3"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func treeDir(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	writeTree(t, dir, files)
	return dir
}

func TestMiseTools(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
		from  []string
	}{
		{"none", map[string]string{"go.mod": "module x"}, nil, nil},
		{"mise.toml", map[string]string{"mise.toml": "[env]\nNODE_ENV = \"test\"\n\n[tools]\nnode = \"22\"\n\"npm:pnpm\" = \"9\"\ngolang = \"1.23\"\n\n[tasks.x]\nrun = \"y\"\n"},
			[]string{"go", "node", "pnpm"}, []string{"mise.toml"}},
		{"subtables", map[string]string{".mise.toml": "[tools.python]\nversion = \"3.12\"\n[tools]\nuv = \"latest\"\n"}, []string{"python", "uv"}, []string{".mise.toml"}},
		{"asdf", map[string]string{".tool-versions": "# pins\nnodejs 20.11.0\nbun 1.1.0\n\n"}, []string{"bun", "node"}, []string{".tool-versions"}},
	}
	for _, c := range cases {
		tools, from := miseTools(treeDir(t, c.files))
		var got []string
		for k := range tools {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, c.want) || !reflect.DeepEqual(from, c.from) {
			t.Errorf("%s: %v from %v, want %v from %v", c.name, got, from, c.want, c.from)
		}
	}
}

func TestNodeVersion(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		root  string
		want  string
	}{
		{"default", map[string]string{"web/package.json": "{}"}, "web", "22"},
		{"root nvmrc", map[string]string{".nvmrc": "v20.11.0\n"}, "web", "20.11.0"},
		{"nearest wins", map[string]string{".nvmrc": "18", "web/.node-version": "20\n"}, "web", "20"},
		{"volta", map[string]string{"web/package.json": `{"volta":{"node":"20.10.0"},"engines":{"node":">=18"}}`}, "web", "20.10.0"},
		{"engines", map[string]string{"package.json": `{"engines":{"node":"^22"}}`, "web/package.json": `{}`}, "web", "^22"},
		{"nvmrc beats package.json", map[string]string{".nvmrc": "lts/*", "web/package.json": `{"engines":{"node":">=18"}}`}, "web", "lts/*"},
	}
	for _, c := range cases {
		if got := nodeVersion(treeDir(t, c.files), c.root); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPythonVersionFile(t *testing.T) {
	cases := []struct {
		files map[string]string
		root  string
		want  string
	}{
		{map[string]string{"tools/pyproject.toml": "[project]\nname = \"x\"\n"}, "tools", ""},
		{map[string]string{"tools/pyproject.toml": "[project]\nrequires-python = \">=3.11\"\n"}, "tools", "tools/pyproject.toml"},
		{map[string]string{".python-version": "3.12\n", "tools/pyproject.toml": "[project]\nrequires-python = \">=3.11\"\n"}, "tools", ".python-version"},
	}
	for i, c := range cases {
		if got := pythonVersionFile(treeDir(t, c.files), c.root); got != c.want {
			t.Errorf("%d: %q, want %q", i, got, c.want)
		}
	}
}

func TestComposeServices(t *testing.T) {
	dir := treeDir(t, map[string]string{"compose.yaml": `
services:
  app:
    build: .
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: ${DB_PASSWORD}
      POSTGRES_DB: app_test
  cache:
    image: docker.io/library/redis:7
  search:
    image: elasticsearch:8
  my:
    image: mariadb:11
    environment:
      - MARIADB_ROOT_PASSWORD=secret
`})
	got := composeServices(dir)
	var names []string
	for _, h := range got {
		names = append(names, h.Name+"="+h.Kind)
	}
	if want := []string{"cache=redis", "db=postgres", "my=mysql"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("services %v, want %v", names, want)
	}
	db := got[1]
	if !reflect.DeepEqual(db.Env, [][2]string{{"POSTGRES_USER", "app"}, {"POSTGRES_DB", "app_test"}}) {
		t.Errorf("db env %v", db.Env)
	}
	if k, v := serviceURL(db); k != "DATABASE_URL" || v != "postgres://app:postgres@localhost:5432/app_test?sslmode=disable" {
		t.Errorf("db url %s=%s", k, v)
	}
	if k, v := serviceURL(got[2]); k != "DATABASE_URL" || v != "mysql://root:secret@localhost:3306/" {
		t.Errorf("mysql url %s=%s", k, v)
	}
}

func TestInspect(t *testing.T) {
	files := map[string]string{
		"api/db_test.go":        `func TestX(t *testing.T) { dsn := os.Getenv("DATABASE_URL"); _ = os.Getenv("UNLISTED") }`,
		"web/src/api.test.ts":   `const r = await fetch("http://localhost:8080/api"); const k = process.env.API_TOKEN;`,
		"web/src/cache.ts":      `connect("redis://127.0.0.1:6379/0"); import.meta.env.VITE_MODE`,
		"web/node_modules/x.js": `fetch("http://localhost:9999")`,
		"README.md":             `see http://localhost:3000`,
		".env.example":          "# local\nDATABASE_URL=postgres://u:p@localhost:5432/db\nexport API_TOKEN=changeme # yours\nNOT_READ=1\n",
		"web/.env.example":      "VITE_MODE=dev\n",
	}
	dir := treeDir(t, files)
	var names []string
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	suites := []config.Suite{{Name: "go", Runner: "go", Root: "api"}, {Name: "web", Runner: "vitest", Root: "web"}}
	in := inspect(dir, suites, names)
	if len(in.Local) != 1 || in.Local[0].URL != "http://localhost:8080" || in.Local[0].File != "web/src/api.test.ts" {
		t.Errorf("local %+v", in.Local)
	}
	if len(in.Services) != 1 || in.Services[0].Kind != "redis" || !strings.Contains(in.Services[0].Source, "web/src/cache.ts") {
		t.Errorf("services %+v", in.Services)
	}
	want := []envReq{
		{Name: "DATABASE_URL", Example: "postgres://u:p@localhost:5432/db", Source: ".env.example", ReadBy: "api/db_test.go"},
		{Name: "API_TOKEN", Example: "changeme", Source: ".env.example", ReadBy: "web/src/api.test.ts"},
		{Name: "VITE_MODE", Example: "dev", Source: "web/.env.example", ReadBy: "web/src/cache.ts"},
	}
	if !reflect.DeepEqual(in.Env, want) {
		t.Errorf("env %+v\nwant %+v", in.Env, want)
	}
	var b strings.Builder
	renderServices(&b, in)
	out := b.String()
	for _, s := range []string{"    # services:\n", "    #   redis:\n", `--health-cmd "redis-cli ping"`, "    #   API_TOKEN: ${{ secrets.API_TOKEN }}\n", "    #   REDIS_URL: redis://localhost:6379\n", `#   DATABASE_URL: postgres://u:p@localhost:5432/db`} {
		if !strings.Contains(out, s) {
			t.Errorf("services block lacks %q:\n%s", s, out)
		}
	}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(l, "    #") {
			t.Errorf("live line in services block: %q", l)
		}
	}
}

const ciWorkflow = `name: ci
on: [push]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: golangci-lint run
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
      - name: Go tests
        run: |
          go vet ./...
          go test -race ./...
        env:
          DATABASE_URL: postgres://postgres:postgres@localhost:5432/test
          TZ: UTC

      - name: Web tests
        working-directory: web
        run: pnpm test
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: make test-e2e
      - uses: dotneet/smates@v1
`

func TestWorkflowTestSteps(t *testing.T) {
	dir := treeDir(t, map[string]string{".github/workflows/ci.yml": ciWorkflow, ".github/workflows/other.yaml": "on: push\njobs:\n  x:\n    steps:\n      - run: npm run build\n"})
	steps := workflowTestSteps(dir)
	type row struct {
		job, step, cmd string
		line, end      int
	}
	var got []row
	for _, s := range steps {
		got = append(got, row{s.Job, s.Step, s.Command, s.Line, s.End})
	}
	want := []row{
		{"test", "Go tests", "go test -race ./...", 14, 20},
		{"test", "Web tests", "pnpm test", 22, 24},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("steps %+v\nwant %+v", got, want)
	}
	if steps[0].Blocks["env"] != [2]int{18, 20} || steps[0].TestLine != 17 || steps[1].WorkDir != "web" {
		t.Errorf("env block %v, test line %d, working-directory %q", steps[0].Blocks["env"], steps[0].TestLine, steps[1].WorkDir)
	}
}

func TestWorkflowPatchApplies(t *testing.T) {
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{".github/workflows/ci.yml": ciWorkflow})
	g.git("add", ".")
	g.commit("ci")
	suites := []config.Suite{{Name: "go", Runner: "go", Root: ".", Command: "go test -race"}, {Name: "web", Runner: "vitest", Root: "web"}}
	patch, _ := workflowPatch(g.dir, workflowTestSteps(g.dir), suites, "main")
	for _, s := range []string{
		"--- a/.github/workflows/ci.yml\n+++ b/.github/workflows/ci.yml\n",
		"-          go test -race ./...\n",
		"+        uses: dotneet/smates@v1\n",
		"+          args: --suite go\n",
		"+          cache-key: go\n",
		"+          args: --suite web\n",
		"+          cache-key: web\n",
	} {
		if !strings.Contains(patch, s) {
			t.Errorf("patch lacks %q:\n%s", s, patch)
		}
	}
	pf := filepath.Join(t.TempDir(), "p.patch")
	if err := os.WriteFile(pf, []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	g.git("apply", pf)
	data, _ := os.ReadFile(filepath.Join(g.dir, ".github/workflows/ci.yml"))
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("patched workflow does not parse: %v\n%s", err, data)
	}
	if steps := workflowTestSteps(g.dir); len(steps) != 0 {
		t.Errorf("patched workflow still runs tests: %+v\n%s", steps, data)
	}
	if !strings.Contains(string(data), "        env:\n          DATABASE_URL: postgres://postgres:postgres@localhost:5432/test\n          TZ: UTC\n") {
		t.Errorf("step env not kept:\n%s", data)
	}
	if !strings.Contains(string(data), "      - run: golangci-lint run\n") {
		t.Errorf("unrelated step changed:\n%s", data)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n"
	b := "1\n2\nthree\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\nfourteen\n15\n"
	want := "--- a/f\n+++ b/f\n@@ -1,6 +1,6 @@\n 1\n 2\n-3\n+three\n 4\n 5\n 6\n@@ -11,5 +11,5 @@\n 11\n 12\n 13\n-14\n+fourteen\n 15\n"
	if got := unifiedDiff("f", a, b); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMakefileHints(t *testing.T) {
	dir := treeDir(t, map[string]string{
		"Makefile":       "GO ?= go\n.PHONY: test test-web lint\n\ntest: ## run tests\n\t@TZ=UTC $(GO) test -race -tags integration \\\n\t\t-count=1 ./...\n\ntest-web:\n\tcd web && pnpm exec vitest run --coverage\n\ntest-plain:\n\tgo test ./...\n\nlint:\n\tgolangci-lint run\n",
		"tools/Makefile": "test:\n\texport PYTHONHASHSEED=0; uv run pytest -x tests\n",
	})
	suites := []config.Suite{
		{Name: "go", Runner: "go", Root: "."},
		{Name: "web", Runner: "vitest", Root: "web"},
		{Name: "other", Runner: "vitest", Root: "other"},
		{Name: "tools", Runner: "pytest", Root: "tools"},
	}
	got := makefileHints(dir, suites)
	type row struct{ suite, target, cmd string }
	var rows []row
	for _, h := range got {
		rows = append(rows, row{h.Suite, h.Target, h.Command})
	}
	want := []row{
		{"go", "test", "$(GO) test -race -tags integration -count=1"},
		{"tools", "test", "uv run pytest -x tests"},
		{"web", "test-web", "pnpm exec vitest run --coverage"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("hints %+v\nwant %+v", rows, want)
	}
	if !reflect.DeepEqual(got[0].Env, [][2]string{{"TZ", "UTC"}}) || !reflect.DeepEqual(got[1].Env, [][2]string{{"PYTHONHASHSEED", "0"}}) {
		t.Errorf("env %v %v", got[0].Env, got[1].Env)
	}
	doc := renderConfig(suites, configHints(got, nil, nil))
	if _, err := config.Parse([]byte(doc)); err != nil {
		t.Fatalf("config with hints does not parse: %v\n%s", err, doc)
	}
	if !strings.Contains(doc, "    # command: \"$(GO) test -race -tags integration -count=1\"\n    # env:\n    #   TZ: UTC\n") {
		t.Errorf("hint missing:\n%s", doc)
	}
}

func TestRenderWorkflowToolchains(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		suites  []string
		want    []string
		notWant []string
	}{
		{"mise covers go and node",
			map[string]string{"mise.toml": "[tools]\ngo = \"1.23\"\nnode = \"22\"\n", "web/pnpm-lock.yaml": "", "go.mod": "module x"},
			[]string{"go", "vitest:web"},
			[]string{"      - uses: actions/checkout@v5\n      - uses: jdx/mise-action@v2   # installs the tools pinned in mise.toml\n", "pnpm/action-setup@v4", "pnpm install --frozen-lockfile"},
			[]string{"setup-go", "setup-node"}},
		{"asdf covers node only",
			map[string]string{".tool-versions": "nodejs 20.1.0\n", "go.mod": "module x", "package-lock.json": "{}"},
			[]string{"go", "jest:."},
			[]string{"jdx/mise-action@v2", "actions/setup-go@v5", "go-version-file: go.mod"},
			[]string{"setup-node"}},
		{"node version per suite root",
			map[string]string{"web/.nvmrc": "20.11\n", "web/package-lock.json": "{}", "web/package.json": `{"engines":{"node":">=18"}}`},
			[]string{"vitest:web"},
			[]string{"actions/setup-node@v4\n        with:\n          node-version: \"20.11\"\n", "      - run: npm ci\n        working-directory: web\n"},
			[]string{"mise"}},
		{"engines.node",
			map[string]string{"package.json": `{"engines":{"node":"^22.3"}}`},
			[]string{"node:."},
			[]string{"node-version: \"^22.3\""}, nil},
		{"python version file",
			map[string]string{"tools/.python-version": "3.12\n", "tools/uv.lock": ""},
			[]string{"pytest:tools"},
			[]string{"actions/setup-python@v5\n        with:\n          python-version-file: tools/.python-version\n", "astral-sh/setup-uv@v6"}, nil},
		{"mise python and uv",
			map[string]string{"mise.toml": "[tools]\npython = \"3.12\"\nuv = \"latest\"\n"},
			[]string{"pytest:."},
			[]string{"jdx/mise-action@v2"}, []string{"setup-python", "setup-uv"}},
		{"gradle with a toolchain, twice",
			map[string]string{"settings.gradle": "", "gradlew": "", "app/build.gradle": "java { toolchain { languageVersion = JavaLanguageVersion.of(21) } }", "lib/build.gradle.kts": "JavaLanguageVersion.of(17)", "build/x.gradle": "JavaLanguageVersion.of(99)"},
			[]string{"gradle:app", "gradle:lib"},
			[]string{"      - uses: actions/setup-java@v4\n        with:\n          distribution: temurin\n          java-version: \"21\"   # the toolchain app/build.gradle requests\n      - uses: gradle/actions/setup-gradle@v4\n"},
			[]string{"setup-node", "npm ci"}},
		{"gradle with a .java-version, a toolchain below 17",
			map[string]string{"svc/.java-version": "17\n", "svc/settings.gradle": "", "other/settings.gradle": "", "other/build.gradle": "JavaLanguageVersion.of(11)"},
			[]string{"gradle:svc", "gradle:other"},
			[]string{"java-version-file: svc/.java-version\n", "java-version: \"21\"   # recording the tests needs Java 17 or later\n"},
			nil},
		{"mise java",
			map[string]string{".tool-versions": "java temurin-21\n", "settings.gradle": ""},
			[]string{"gradle:."},
			[]string{"jdx/mise-action@v2", "gradle/actions/setup-gradle@v4\n        with:\n          gradle-version: current"}, []string{"setup-java"}},
		{"compose services",
			map[string]string{"docker-compose.yml": "services:\n  db:\n    image: postgres:16\n", "go.mod": "module x"},
			[]string{"go"},
			[]string{"    # services:\n    #   db:\n    #     image: postgres:16\n", "#   DATABASE_URL: postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable", `--health-cmd "pg_isready -U postgres"`}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := treeDir(t, c.files)
			var suites []config.Suite
			for _, s := range c.suites {
				r, root, ok := strings.Cut(s, ":")
				if !ok {
					root = "."
				}
				suites = append(suites, config.Suite{Name: s, Runner: r, Root: root})
			}
			var files []string
			for f := range c.files {
				files = append(files, f)
			}
			out := renderWorkflow(&gitx.Repo{Root: dir}, suites, inspect(dir, suites, files))
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("lacks %q:\n%s", w, out)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(out, w) {
					t.Errorf("has %q:\n%s", w, out)
				}
			}
			var doc map[string]any
			if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("workflow does not parse: %v\n%s", err, out)
			}
		})
	}
}

func TestInitGitHubExistingWorkflow(t *testing.T) {
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".github/workflows/ci.yml": ciWorkflow,
		"go.mod":                   "module example.com/x\n\ngo 1.22\n",
		"x_test.go":                "package x\n",
		"Makefile":                 "test:\n\tgo test -race ./...\n",
	})
	g.git("add", ".")
	g.commit("files")
	var out, errb bytes.Buffer
	if code := Main([]string{"init", "--github", "-C", g.dir}, &out, &errb); code != 0 {
		t.Fatalf("init: %d %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(g.dir, ".github/workflows/smates.yml")); err == nil {
		t.Error("wrote a second workflow running the same tests")
	}
	for _, s := range []string{`.github/workflows/ci.yml: job "test", step "Go tests": go test -race ./...`, "+        uses: dotneet/smates@v1", "not writing .github/workflows/smates.yml", `Makefile target "test" runs "go test -race ./..."`} {
		if !strings.Contains(errb.String(), s) {
			t.Errorf("stderr lacks %q:\n%s", s, errb.String())
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(g.dir, "smates.yml"))
	if !strings.Contains(string(cfg), `# command: "go test -race"`) {
		t.Errorf("smates.yml lacks the Makefile hint:\n%s", cfg)
	}
	// --patch writes the patch instead, and it applies.
	pf := filepath.Join(t.TempDir(), "smates.patch")
	errb.Reset()
	if code := Main([]string{"init", "--github", "--force", "--patch", pf, "-C", g.dir}, &out, &errb); code != 0 {
		t.Fatalf("init --patch: %d %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(g.dir, ".github/workflows/smates.yml")); err != nil {
		t.Error("--force did not write the workflow")
	}
	cmd := exec.Command("git", "apply", "--check", pf)
	cmd.Dir = g.dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("patch does not apply: %v %s", err, b)
	}
}

func TestNodeSatisfies(t *testing.T) {
	cases := []struct {
		v, rng    string
		ok, known bool
	}{
		{"v20.11.1", ">=18", true, true},
		{"v16.20.0", ">=18", false, true},
		{"v20.11.1", ">= 18.17.0", true, true},
		{"v22.1.0", "^20 || ^22", true, true},
		{"v21.0.0", "^20 || ^22", false, true},
		{"v20.5.0", "20.x", true, true},
		{"v20.4.9", "~20.4.1", true, true},
		{"v20.5.0", "~20.4.1", false, true},
		{"v20.3.0", "~20.4.1", false, true},
		{"v19.0.0", "18 - 20", true, true},
		{"v21.0.0", "18 - 20", false, true},
		{"v18.0.0", ">18", false, true},
		{"v19.0.0", ">18", true, true},
		{"v18.19.0", ">=18 <19", true, true},
		{"v22.0.0", "*", true, true},
		{"v22.0.0", "lts/*", false, false},
		{"v22.0.0", "", false, false},
	}
	for _, c := range cases {
		if ok, known := nodeSatisfies(c.v, c.rng); ok != c.ok || known != c.known {
			t.Errorf("%s %q: %t %t, want %t %t", c.v, c.rng, ok, known, c.ok, c.known)
		}
	}
}

func TestDoctorReportsProjectNeeds(t *testing.T) {
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".github/workflows/ci.yml": "on: push\njobs:\n  t:\n    steps:\n      - run: npx vitest run\n",
		"package.json":             `{"engines":{"node":">=999"}}`,
		"package-lock.json":        "{}",
		"src/a.test.ts":            "const u = process.env.SMATES_TEST_REQUIRED_VAR;",
		".env.example":             "SMATES_TEST_REQUIRED_VAR=x\n",
		"smates.yml":               "version: 1\nsuites:\n  - name: web\n    runner: vitest\n    root: .\n",
	})
	g.git("add", ".")
	g.commit("files")
	var out, errb bytes.Buffer
	Main([]string{"doctor", "-C", g.dir}, &out, &errb)
	for _, s := range []string{
		"FAIL  suite web: no node_modules",
		`job "t", step "#1" runs the tests without smates (npx vitest run)`,
		"warn  SMATES_TEST_REQUIRED_VAR is not set",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("doctor output lacks %q:\n%s", s, out.String())
		}
	}
	if _, err := exec.LookPath("node"); err == nil && !strings.Contains(out.String(), `does not satisfy engines.node ">=999"`) {
		t.Errorf("no engines.node warning:\n%s", out.String())
	}
}

func TestDoctorYarnPnP(t *testing.T) {
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		"package.json":  `{}`,
		"yarn.lock":     "",
		".pnp.cjs":      "",
		"src/a.test.ts": "",
		"smates.yml":    "version: 1\nsuites:\n  - name: web\n    runner: vitest\n    root: .\n    command: yarn vitest run\n",
	})
	g.git("add", ".")
	g.commit("files")
	var out, errb bytes.Buffer
	Main([]string{"doctor", "-C", g.dir}, &out, &errb)
	if strings.Contains(out.String(), "no node_modules") {
		t.Errorf("Yarn PnP reported as not installed:\n%s", out.String())
	}
}

// patchWorkflow writes ci.yml (and files) into a repository, applies the patch
// workflowPatch proposes with git apply and returns the patch, its notes and the
// patched workflow.
func patchWorkflow(t *testing.T, workflow string, files map[string]string, suites []config.Suite) (string, []string, string) {
	t.Helper()
	g := newGitRepo(t)
	files[".github/workflows/ci.yml"] = workflow
	writeTree(t, g.dir, files)
	g.git("add", ".")
	g.commit("ci")
	patch, notes := workflowPatch(g.dir, workflowTestSteps(g.dir), suites, "main")
	if patch != "" {
		pf := filepath.Join(t.TempDir(), "p.patch")
		if err := os.WriteFile(pf, []byte(patch), 0o644); err != nil {
			t.Fatal(err)
		}
		g.git("apply", pf)
	}
	data, _ := os.ReadFile(filepath.Join(g.dir, ".github/workflows/ci.yml"))
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("patched workflow does not parse: %v\n%s", err, data)
	}
	return patch, notes, string(data)
}

func TestWorkflowPatchCases(t *testing.T) {
	goWeb := []config.Suite{{Name: "go", Runner: "go", Root: ".", Command: "go test -race"}, {Name: "web", Runner: "vitest", Root: "web"}}
	head := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v5\n"
	cases := []struct {
		name     string
		steps    string
		files    map[string]string
		want     []string // substrings of the patched workflow
		notWant  []string
		note     string // substring of the notes ("" = none expected)
		noPatch  bool
		skipWant string // substring of the step's Skip
	}{
		{name: "single go step in a go+vitest repo selects its suite",
			steps:   "      - run: go test ./...\n",
			want:    []string{"        uses: dotneet/smates@v1\n        with:\n          args: --suite go\n          anthropic-api-key:"},
			notWant: []string{"cache-key", "go test"}},
		{name: "step keys kept",
			steps:   "      - name: unit\n        id: unit\n        if: github.event_name == 'push'\n        timeout-minutes: 15\n        continue-on-error: true\n        shell: bash\n        run: go test ./...\n        env:\n          TZ: UTC\n",
			want:    []string{"      - name: 'unit (smates: affected tests only)'\n        id: unit\n        if: github.event_name == 'push'\n        timeout-minutes: 15\n        continue-on-error: true\n        uses: dotneet/smates@v1\n", "          anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional (or openai-api-key / openrouter-api-key)\n        env:\n          TZ: UTC\n"},
			notWant: []string{"shell: bash", "go test"}},
		{name: "if on the dash line",
			steps: "      - if: always()\n        run: go test ./...\n",
			want:  []string{"      - name: 'tests (smates: affected tests only)'\n        if: always()\n        uses: dotneet/smates@v1\n"}},
		{name: "commands before the tests stay in the original step",
			steps:   "      - name: checks\n        id: checks\n        if: always()\n        run: |\n          go vet ./...\n          echo start\n          go test -race ./...\n      - run: echo after\n",
			want:    []string{"      - name: checks\n        id: checks\n        if: always()\n        run: |\n          go vet ./...\n          echo start\n      - name: 'checks (smates: affected tests only)'\n        if: always()\n        uses: dotneet/smates@v1\n", "      - run: echo after\n"},
			notWant: []string{"go test", "        id: checks\n        if: always()\n        uses"},
			note:    `only "go test -race ./..." moves`},
		{name: "working-directory picks the suites under it, without the action's working-directory",
			steps:   "      - working-directory: web\n        run: pnpm test\n",
			want:    []string{"          args: --suite web\n"},
			notWant: []string{"working-directory"}},
		{name: "a smates.yml in the working-directory does not move the action there",
			steps:   "      - working-directory: web\n        run: pnpm test\n",
			files:   map[string]string{"web/smates.yml": "version: 1\n"},
			want:    []string{"          args: --suite web\n"},
			notWant: []string{"working-directory"}},
		{name: "test:unit and make test-unit are unit tests, with a cache-key per set of suites",
			steps:   "      - run: pnpm run test:unit\n      - run: make test-unit\n",
			want:    []string{"cache-key: web\n", "cache-key: go-web\n"},
			notWant: []string{"test:unit", "make"}},
		{name: "steps running the same suites share their cache-key",
			steps: "      - run: go test ./...\n      - run: pnpm test\n      - name: again\n        run: go test -count=1 ./...\n",
			want:  []string{"          args: --suite go\n          cache-key: go\n", "          args: --suite web\n          cache-key: web\n"}, notWant: []string{"go-2"}},
		// Must keep apart: jobs may run in other environments (a runtime version, other
		// services), where a test passing in one fails in the other.
		{name: "jobs running the same suites get their own cache-key",
			steps:   "      - run: go test ./...\n  test-db2:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n",
			want:    []string{"          cache-key: go\n", "          cache-key: go-test-db2\n"},
			notWant: []string{"go-2"}},
		{name: "a matrix job's legs get a cache-key each",
			steps:   "  legs:\n    strategy:\n      matrix:\n        node: [18, 22]\n        include:\n          - db: mysql\n    runs-on: ubuntu-latest\n    steps:\n      - working-directory: web\n        run: pnpm test\n",
			want:    []string{"          cache-key: web-${{ matrix.db }}-${{ matrix.node }}\n"},
			notWant: []string{"cache-key: web\n"}},
		{name: "a matrix from an expression is reported, not patched",
			steps:    "  legs:\n    strategy:\n      matrix: ${{ fromJson(needs.plan.outputs.m) }}\n    steps:\n      - run: go test ./...\n",
			noPatch:  true,
			skipWant: "the job's matrix comes from an expression"},
		// Must refuse: legs built from an expression one level down cannot be named.
		{name: "a matrix include from an expression is reported, not patched",
			steps:    "  legs:\n    strategy:\n      matrix:\n        include: ${{ fromJSON(needs.plan.outputs.legs) }}\n    runs-on: ${{ matrix.os }}\n    steps:\n      - run: go test ./...\n",
			noPatch:  true,
			skipWant: "the job's matrix comes from an expression"},
		{name: "a matrix dimension from an expression is reported, not patched",
			steps:    "  legs:\n    strategy:\n      matrix:\n        os: ${{ fromJSON(needs.plan.outputs.oses) }}\n    steps:\n      - run: go test ./...\n",
			noPatch:  true,
			skipWant: `matrix dimension "os" comes from an expression`},
		{name: "a matrix value from an expression is reported, not patched",
			steps:    "  legs:\n    strategy:\n      matrix:\n        os: [ubuntu-latest, '${{ inputs.os }}']\n    steps:\n      - run: go test ./...\n",
			noPatch:  true,
			skipWant: `matrix dimension "os" has a value that is an expression`},
		{name: "an include entry from an expression is reported, not patched",
			steps:    "  legs:\n    strategy:\n      matrix:\n        include:\n          - os: ${{ fromJSON(needs.plan.outputs.os) }}\n    steps:\n      - run: go test ./...\n",
			noPatch:  true,
			skipWant: `the value of "os" in an include entry`},
		// GitHub turns an object into "Object" in a key: the legs are named by its
		// properties.
		{name: "a matrix of objects is keyed by their properties",
			steps:   "  legs:\n    strategy:\n      matrix:\n        platform:\n          - {os: ubuntu-latest, go: '1.22'}\n          - {os: macos-latest, go: '1.23'}\n    steps:\n      - run: go test ./...\n",
			want:    []string{"          cache-key: go-${{ matrix.platform.go }}-${{ matrix.platform.os }}\n"},
			notWant: []string{"${{ matrix.platform }}"}},
		{name: "a matrix of lists is reported, not patched",
			steps:    "  legs:\n    strategy:\n      matrix:\n        versions: [[1, 2], [3, 4]]\n    steps:\n      - run: go test ./...\n",
			noPatch:  true,
			skipWant: `matrix dimension "versions" has a value that is an expression, a list or a nested object`},
		{name: "a matrix of nested objects is reported, not patched",
			steps:    "  legs:\n    strategy:\n      matrix:\n        platform:\n          - {os: ubuntu-latest, tools: {go: '1.22'}}\n    steps:\n      - run: go test ./...\n",
			noPatch:  true,
			skipWant: `matrix dimension "platform" has a value that is an expression, a list or a nested object`},
		// Must keep apart: a smates job the workflows already have keeps its own state.
		{name: "a step patched next to an existing smates job gets a cache-key",
			steps: "      - working-directory: web\n        run: pnpm test\n",
			files: map[string]string{".github/workflows/pr.yml": "on: pull_request\njobs:\n  unit:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: dotneet/smates@v1\n        with:\n          args: --suite go\n"},
			want:  []string{"          args: --suite web\n          cache-key: web\n"}},
		{name: "a cache-key an existing smates job uses is not taken",
			steps:   "      - working-directory: web\n        run: pnpm test\n",
			files:   map[string]string{".github/workflows/pr.yml": "on: pull_request\njobs:\n  unit:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: dotneet/smates@v1\n        with:\n          args: --suite web\n          cache-key: web\n"},
			want:    []string{"          args: --suite web\n          cache-key: web-test\n"},
			notWant: []string{"          cache-key: web\n          anthropic"}},
		{name: "go test flags the suite command lacks are reported, not patched",
			steps: "      - run: go test -p 1 ./...\n", noPatch: true, skipWant: `with -p 1, which the command of suite "go" in smates.yml (go test -race) lacks: set ` + "`command: go test -p 1 -race`"},
		{name: "test:e2e is reported, not patched",
			steps: "      - run: npm run test:e2e\n", noPatch: true, skipWant: `"npm run test:e2e" is not the plain unit-test command`},
		{name: "make test-integration is reported, not patched",
			steps: "      - run: make test-integration\n", noPatch: true, skipWant: "not the plain unit-test command"},
		{name: "commands after the tests are reported, not patched",
			steps: "      - run: |\n          go test ./...\n          ./scripts/upload-results.sh\n", noPatch: true, skipWant: "commands after the test line use its output"},
		{name: "chained command is reported, not patched",
			steps: "      - run: cd web && pnpm test\n", noPatch: true, skipWant: "chained with other commands"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.files == nil {
				c.files = map[string]string{}
			}
			patch, notes, got := patchWorkflow(t, head+c.steps, c.files, goWeb)
			if c.noPatch {
				if patch != "" {
					t.Errorf("patched:\n%s", patch)
				}
				steps := parseWorkflowSteps("ci.yml", []byte(head+c.steps))
				if len(steps) != 1 {
					t.Fatalf("steps %+v", steps)
				}
				if _, why := stepPatch(steps[0], goWeb); why == "" || !strings.Contains(why, c.skipWant) {
					t.Errorf("not patched because %q, want %q", why, c.skipWant)
				}
				return
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("lacks %q:\n%s", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("has %q:\n%s", w, got)
				}
			}
			joined := strings.Join(notes, "\n")
			if (c.note == "") != (joined == "") || !strings.Contains(joined, c.note) {
				t.Errorf("notes %q, want %q", joined, c.note)
			}
		})
	}
}

func TestInitGitHubTestsOutsideSuites(t *testing.T) {
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".github/workflows/ci.yml": "on: push\njobs:\n  misc:\n    steps:\n      - run: pytest scripts/\n      - run: npm test\n",
		"go.mod":                   "module example.com/x\n\ngo 1.22\n",
		"x_test.go":                "package x\n",
	})
	g.git("add", ".")
	g.commit("files")
	var out, errb bytes.Buffer
	if code := Main([]string{"init", "--github", "-C", g.dir}, &out, &errb); code != 0 {
		t.Fatalf("init: %d %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(g.dir, ".github/workflows/smates.yml")); err != nil {
		t.Errorf("tests outside every suite blocked the smates workflow:\n%s", errb.String())
	}
	for _, s := range []string{"not writing", "--- a/", "replace those steps"} {
		if strings.Contains(errb.String(), s) {
			t.Errorf("stderr has %q:\n%s", s, errb.String())
		}
	}
}

func TestInitGitHubOnlyOtherTestTargets(t *testing.T) {
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".github/workflows/e2e.yml": "on: push\njobs:\n  e2e:\n    steps:\n      - run: npm run test:e2e\n",
		"go.mod":                    "module example.com/x\n\ngo 1.22\n",
		"x_test.go":                 "package x\n",
	})
	g.git("add", ".")
	g.commit("files")
	var out, errb bytes.Buffer
	if code := Main([]string{"init", "--github", "-C", g.dir}, &out, &errb); code != 0 {
		t.Fatalf("init: %d %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(g.dir, ".github/workflows/smates.yml")); err != nil {
		t.Errorf("an e2e-only workflow blocked the smates workflow:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), `not patched: "npm run test:e2e" is not the plain unit-test command`) {
		t.Errorf("stderr:\n%s", errb.String())
	}
}
