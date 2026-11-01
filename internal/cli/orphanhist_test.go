package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/state"
)

// orphanFixture is a repository whose history has every kind of orphan: plain files to
// ignore, a directory to ignore as a whole, execution-relevant files, a file a test
// names, and files next to a dependency (which must not be ignored as a directory).
func orphanFixture(t *testing.T) (*gitRepo, string) {
	t.Helper()
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("smates.yml", `version: 1
orphans: all
ignore: ["design/**"]
suites:
  - name: a
    runner: custom
    root: a
    tests: ["*_check.sh"]
    command: "true"
    rules: [{tests: ["*"], deps: ["src/**", "tools/keep.sh"]}]
`)
	write(".gitignore", ".smates/\n")
	write("a/x_check.sh", "true\n")
	write("a/y_check.sh", "cat fixtures/users.json\n")
	write("src/lib.txt", "1")
	write("fixtures/users.json", "[]")
	write(".env.example", "A=1")
	write("Makefile", "test:\n")
	write(".github/workflows/ci.yml", "on: push")
	write("deploy/Dockerfile", "FROM x")
	write("deploy/k8s/app.yaml", "1")
	write("tools/keep.sh", "1")
	write("tools/x.sh", "1")
	write("tools/y.sh", "1")
	write("design/logo.svg", "1")
	g.git("add", "-A")
	base := g.commit("fixture")
	step := func(msg string, files ...string) {
		for _, f := range files {
			write(f, msg)
		}
		g.git("add", "-A")
		g.commit(msg)
	}
	step("c1", ".env.example")
	step("c2", ".env.example", "Makefile")
	step("c3", "deploy/k8s/app.yaml", "deploy/k8s/svc.yaml")
	step("c4", ".github/workflows/ci.yml", "design/logo.svg", "README.md")
	step("c5", "src/lib.txt")
	step("c6", "fixtures/users.json")
	step("c7", "tools/x.sh", "tools/y.sh")
	return g, base
}

func runOrphans(t *testing.T, g *gitRepo, args ...string) (*orphanReport, string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Main(append([]string{"orphans", "-C", g.dir, "--format", "json"}, args...), &out, &errb); code != 0 {
		t.Fatalf("orphans: %d %s", code, errb.String())
	}
	var rep orphanReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return &rep, errb.String()
}

func TestOrphansHistory(t *testing.T) {
	g, base := orphanFixture(t)
	rep, _ := runOrphans(t, g, "--since", base)

	if rep.Commits != 7 || rep.RecordedState || rep.Policy != "all" || rep.Since != base {
		t.Errorf("header: %+v", rep)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "smates run --all") {
		t.Errorf("no-state warning: %v", rep.Warnings)
	}
	// Ranking: most commits first, then by path.
	var got []string
	for _, f := range rep.Files {
		got = append(got, f.Path+" "+itoa(f.Commits)+" "+f.Kind+" "+f.Glob)
	}
	want := []string{
		".env.example 2 ignore .env.example",
		".github/workflows/ci.yml 1 inputs ",
		"Makefile 1 inputs ",
		"deploy/k8s/app.yaml 1 ignore deploy/k8s/**",
		"deploy/k8s/svc.yaml 1 ignore deploy/k8s/**",
		"fixtures/users.json 1 reference ",
		"tools/x.sh 1 ignore tools/x.sh",
		"tools/y.sh 1 ignore tools/y.sh",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("files:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var globs []string
	for _, g := range rep.Ignore {
		globs = append(globs, g.Glob+" "+itoa(g.Commits))
	}
	if want := []string{".env.example 2", "deploy/k8s/** 1", "tools/x.sh 1", "tools/y.sh 1"}; !reflect.DeepEqual(globs, want) {
		t.Errorf("ignore globs %v, want %v", globs, want)
	}
	if len(rep.Inputs) != 2 || rep.Inputs[0].Why != "CI configuration" || rep.Inputs[1].Why != "build script" {
		t.Errorf("inputs: %+v", rep.Inputs)
	}
	// c1-c4 and c7 have orphans; c6 only a referenced one; c5 none.
	if rep.CommitsWithOrphans != 5 || rep.CommitsWithOrphansAfterIgnore != 2 || rep.CommitsWithOrphansAfterInputs != 0 {
		t.Errorf("counts: %d %d %d", rep.CommitsWithOrphans, rep.CommitsWithOrphansAfterIgnore, rep.CommitsWithOrphansAfterInputs)
	}
	if len(rep.History) != 6 || rep.History[1].Subject != "c6" || len(rep.History[1].Orphans) != 0 ||
		!reflect.DeepEqual(rep.History[1].Referenced, []string{"fixtures/users.json"}) {
		t.Errorf("history: %+v", rep.History)
	}
	if !reflect.DeepEqual(rep.ExistingIgnore, []string{"design/**"}) {
		t.Errorf("existing ignore: %v", rep.ExistingIgnore)
	}

	// --history limits the replay.
	if rep, _ := runOrphans(t, g, "--history", "1"); rep.Commits != 1 || rep.CommitsWithOrphans != 1 || len(rep.Files) != 2 {
		t.Errorf("--history 1: %+v", rep)
	}
}

func TestOrphansText(t *testing.T) {
	g, base := orphanFixture(t)
	var out, errb bytes.Buffer
	if code := Main([]string{"orphans", "-C", g.dir, "--since", base}, &out, &errb); code != 0 {
		t.Fatalf("orphans: %d %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"5 of 7 commit(s) changed a file no test depends on; the suites they may affect would have run in full",
		"Makefile",
		"build script: consider inputs:/rules: instead of ignore",
		"explained by reference",
		"With these globs, 2 of 7 commit(s) would still have orphans (0 once the files above are in inputs:/rules:).",
		"ignore:\n  - \"design/**\"\n  - \".env.example\"\n  - \"deploy/k8s/**\"\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if !strings.Contains(errb.String(), "warning: no recorded state") {
		t.Errorf("no-state warning on stderr: %q", errb.String())
	}
}

// TestOrphansRecordedState checks that recorded dependencies explain changes and that
// the no-state warning goes away.
func TestOrphansRecordedState(t *testing.T) {
	g, base := orphanFixture(t)
	st := state.New()
	st.Suite("a").Tests["a/x_check.sh"] = &state.Entry{Status: state.Pass, Deps: map[string]string{"Makefile": "x", ".env.example": "x"}}
	if err := st.Save(filepath.Join(g.dir, ".smates", "state.json.gz")); err != nil {
		t.Fatal(err)
	}
	rep, _ := runOrphans(t, g, "--since", base)
	if !rep.RecordedState || len(rep.Warnings) != 0 {
		t.Errorf("recorded state: %v %v", rep.RecordedState, rep.Warnings)
	}
	for _, f := range rep.Files {
		if f.Path == "Makefile" || f.Path == ".env.example" {
			t.Errorf("recorded dependency reported as orphan: %+v", f)
		}
	}
	if rep.CommitsWithOrphans != 3 {
		t.Errorf("commits with orphans: %d", rep.CommitsWithOrphans)
	}
}

func TestAffectsExecution(t *testing.T) {
	yes := []string{".github/workflows/ci.yml", "Makefile", "build/rules.mk", "Dockerfile", "api/Dockerfile.dev", "web.dockerfile",
		"docker-compose.yml", "compose.override.yaml", ".nvmrc", ".tool-versions", "mise.toml", "svc/go.mod", ".gitlab-ci.yml", "justfile"}
	no := []string{".env.example", "deploy/app.yaml", "web/src/App.tsx", "go.sum", "composer.json", ".github/CODEOWNERS"}
	for _, f := range yes {
		if affectsExecution(f) == "" {
			t.Errorf("%s should affect execution", f)
		}
	}
	for _, f := range no {
		if why := affectsExecution(f); why != "" {
			t.Errorf("%s: %s", f, why)
		}
	}
}

func TestSuggestGlobs(t *testing.T) {
	ranked := []orphanFile{
		{Path: "web/src/a.ts", Kind: orphanKindIgnore},
		{Path: "web/src/b.ts", Kind: orphanKindIgnore},
		{Path: "web/public/x.png", Kind: orphanKindIgnore},
		{Path: "ops/k8s/a.yaml", Kind: orphanKindIgnore},
		{Path: "ops/k8s/b.yaml", Kind: orphanKindIgnore},
		{Path: "ops/Makefile", Kind: orphanKindInputs},
		{Path: "lib/one.txt", Kind: orphanKindIgnore},
		{Path: "lib/two.txt", Kind: orphanKindIgnore},
		{Path: "solo/only.txt", Kind: orphanKindIgnore},
		{Path: "root.txt", Kind: orphanKindIgnore},
		{Path: "app/[id]/page.tsx", Kind: orphanKindIgnore},
		{Path: "app/[id]/layout.tsx", Kind: orphanKindIgnore},
		{Path: "pages/{slug}*?.tsx", Kind: orphanKindIgnore},
		{Path: `odd\name.txt`, Kind: orphanKindIgnore},
		{Path: "seed/one.sql", Kind: orphanKindIgnore},
		{Path: "seed/two.sql", Kind: orphanKindIgnore},
	}
	tree := []string{"web/src/a.ts", "web/src/b.ts", "web/public/x.png", "lib/one.txt", "lib/two.txt", "lib/dep.go", "solo/only.txt", "root.txt",
		"app/[id]/page.tsx", "app/[id]/layout.tsx", "app/dep.ts", "pages/{slug}*?.tsx", `odd\name.txt`,
		"seed/one.sql", "seed/two.sql", "seed/users.sql", "seed/other.sql"}
	covered := map[string]bool{"lib/dep.go": true, "app/dep.ts": true}
	var asked []string
	refs := func(files []string) []string {
		asked = append(asked, files...)
		var out []string
		for _, f := range files {
			if f == "seed/users.sql" { // a test names it
				out = append(out, f)
			}
		}
		return out
	}
	got := suggestGlobs(ranked, tree, covered, refs)
	want := map[string]string{
		"web/src/a.ts": "web/**", "web/src/b.ts": "web/**", "web/public/x.png": "web/**", // shallowest clean directory
		"ops/k8s/a.yaml": "ops/k8s/**", "ops/k8s/b.yaml": "ops/k8s/**", // ops/ holds an execution-relevant file
		"lib/one.txt": "lib/one.txt", "lib/two.txt": "lib/two.txt", // lib/ holds a dependency
		"solo/only.txt": "solo/only.txt", // a single file is not collapsed
		"root.txt":      "root.txt",
		// Metacharacters are escaped.
		"app/[id]/page.tsx": `app/\[id\]/**`, "app/[id]/layout.tsx": `app/\[id\]/**`,
		"pages/{slug}*?.tsx": `pages/\{slug\}\*\?.tsx`,
		`odd\name.txt`:       `odd\\name.txt`,
		// seed/ holds a file a test names, although it never changed.
		"seed/one.sql": "seed/one.sql", "seed/two.sql": "seed/two.sql",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	for f, g := range got {
		if !pathutil.Match([]string{g}, f) {
			t.Errorf("glob %q does not match %q", g, f)
		}
	}
	// Only unranked, uncovered files below a candidate directory go to the reference scan.
	sort.Strings(asked)
	if want := []string{"seed/other.sql", "seed/users.sql"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("reference scan asked about %v, want %v", asked, want)
	}
}

// TestOrphansEscapedAndReferencedTree replays a commit touching bracketed paths and a
// directory holding a file a test names (in the working tree, not in the history).
func TestOrphansEscapedAndReferencedTree(t *testing.T) {
	g, _ := orphanFixture(t)
	write := func(rel string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(g.dir+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("seed/users.sql")
	if err := os.WriteFile(filepath.Join(g.dir, "a", "z_check.sh"), []byte("psql < seed/users.sql\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.git("add", "-A")
	g.commit("setup")
	for _, f := range []string{"seed/one.sql", "seed/two.sql", "web/[id]/page.tsx", "web/[id]/layout.tsx", "web2/[slug].tsx"} {
		write(f)
	}
	g.git("add", "-A")
	g.commit("change")

	rep, _ := runOrphans(t, g, "--history", "1")
	globs := map[string]string{}
	for _, f := range rep.Files {
		globs[f.Path] = f.Glob
	}
	want := map[string]string{
		"seed/one.sql": "seed/one.sql", "seed/two.sql": "seed/two.sql",
		"web/[id]/page.tsx": "web/**", "web/[id]/layout.tsx": "web/**",
		"web2/[slug].tsx": `web2/\[slug\].tsx`,
	}
	if !reflect.DeepEqual(globs, want) {
		t.Errorf("globs %v, want %v", globs, want)
	}
	// Every suggestion matches its files, and the replay with them agrees.
	still := 0
	var all []string
	for _, ig := range rep.Ignore {
		all = append(all, ig.Glob)
		for _, f := range ig.Files {
			if !pathutil.Match([]string{ig.Glob}, f) {
				t.Errorf("%q does not match %q", ig.Glob, f)
			}
		}
	}
	for _, h := range rep.History {
		for _, f := range h.Orphans {
			if !pathutil.Match(all, f) {
				still++
				break
			}
		}
	}
	if rep.CommitsWithOrphans != 1 || rep.CommitsWithOrphansAfterIgnore != still || still != 0 {
		t.Errorf("counts %d %d (by matching: %d)", rep.CommitsWithOrphans, rep.CommitsWithOrphansAfterIgnore, still)
	}
}

func TestOrphansShallowClone(t *testing.T) {
	g, _ := orphanFixture(t)
	dir := filepath.Join(t.TempDir(), "clone")
	g.git("clone", "-q", "--depth", "3", "file://"+g.dir, dir)
	var out, errb bytes.Buffer
	if code := Main([]string{"orphans", "-C", dir, "--format", "json"}, &out, &errb); code != 0 {
		t.Fatalf("orphans: %d %s", code, errb.String())
	}
	var rep orphanReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	// c7, c6 replayed; c5 (the boundary) is not diffed as a root commit adding every file.
	if rep.Commits != 2 || len(rep.Warnings) != 2 || !strings.Contains(rep.Warnings[1], "shallow clone") {
		t.Errorf("commits %d, warnings %v", rep.Commits, rep.Warnings)
	}
	for _, f := range rep.Files {
		if f.Path == ".env.example" || f.Path == "Makefile" {
			t.Errorf("file from the boundary commit reported: %+v", f)
		}
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
