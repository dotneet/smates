package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
)

// smatesCmd runs `smates <cmd> -C dir args...` and returns its exit code and output.
func smatesCmd(t *testing.T, dir, cmd string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	full := []string{cmd}
	if cmd == "config" || cmd == "state" {
		full = append(full, args[0], "-C", dir)
		args = args[1:]
	} else {
		full = append(full, "-C", dir)
	}
	code := Main(append(full, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func decodeJSON[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%v\n%s", err, s)
	}
	return v
}

const commentedConfig = `# smates configuration (keep this comment)
version: 1

suites:
  # the checks
  - name: a
    runner: custom
    root: a
    tests: ["*_check.sh"]
    command: "true"

orphans: all   # policy comment

# docs and images
ignore:
  - "design/**"   # images

inputs: []   # pins
`

func configRepo(t *testing.T) (*gitRepo, string) {
	t.Helper()
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{"smates.yml": commentedConfig, "a/x_check.sh": "true\n", ".gitignore": ".smates/\n"})
	g.git("add", "-A")
	g.commit("fixture")
	return g, filepath.Join(g.dir, "smates.yml")
}

func TestConfigAddIgnore(t *testing.T) {
	g, path := configRepo(t)

	// --dry-run prints the diff and writes nothing; flags may follow the globs.
	code, out, errs := smatesCmd(t, g.dir, "config", "add-ignore", "tools/**", "design/**", "--dry-run")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if readFile(t, path) != commentedConfig {
		t.Errorf("--dry-run wrote the file")
	}
	for _, s := range []string{"already in ignore: design/**", "would add to ignore: tools/**", "+++ b/smates.yml", `+  - "tools/**"`, "dry run: smates.yml not changed"} {
		if !strings.Contains(out, s) {
			t.Errorf("dry run output lacks %q:\n%s", s, out)
		}
	}

	code, out, errs = smatesCmd(t, g.dir, "config", "add-ignore", "--format", "json", "tools/**", "tools/**", "design/**", "vendor/**")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	res := decodeJSON[configEdit](t, out)
	if !reflect.DeepEqual(res.Added, []string{"tools/**", "vendor/**"}) || !reflect.DeepEqual(res.Present, []string{"design/**"}) || !res.Written || !res.Changed || res.File != "smates.yml" || res.Key != "ignore" {
		t.Errorf("result %+v", res)
	}
	want := strings.Replace(commentedConfig, "  - \"design/**\"   # images\n", "  - \"design/**\"   # images\n  - \"tools/**\"\n  - \"vendor/**\"\n", 1)
	if got := readFile(t, path); got != want {
		t.Errorf("smates.yml (comments and layout must stay):\n%s", got)
	}

	// Again: nothing to change.
	code, out, _ = smatesCmd(t, g.dir, "config", "add-ignore", "tools/**")
	if code != 0 || !strings.Contains(out, "nothing to change") {
		t.Errorf("%d %s", code, out)
	}

	// add-input to a flow list keeps the comment after it.
	if code, _, errs := smatesCmd(t, g.dir, "config", "add-input", ".nvmrc", "go.sum"); code != 0 {
		t.Fatal(errs)
	}
	if got := readFile(t, path); !strings.Contains(got, `inputs: [".nvmrc", "go.sum"]   # pins`) || !strings.Contains(got, "(keep this comment)") {
		t.Errorf("smates.yml:\n%s", got)
	}
	c, err := config.Load(g.dir)
	if err != nil || !reflect.DeepEqual(c.Inputs, []string{".nvmrc", "go.sum"}) || len(c.Ignore) != 3 {
		t.Errorf("%v %+v", err, c)
	}

	// Invalid globs are rejected without writing.
	before := readFile(t, path)
	for _, bad := range [][]string{{"/abs/**"}, {"ok/**", "a/[b"}, {"../x"}} {
		code, _, errs := smatesCmd(t, g.dir, "config", append([]string{"add-ignore"}, bad...)...)
		if code == 0 || errs == "" {
			t.Errorf("%v accepted", bad)
		}
	}
	if readFile(t, path) != before {
		t.Errorf("a rejected edit wrote the file")
	}
	if code, _, _ := smatesCmd(t, g.dir, "config", "add-ignore"); code != 2 {
		t.Errorf("no globs: exit %d, want 2", code)
	}
	if code, _, _ := smatesCmd(t, g.dir, "config", "bogus"); code != 2 {
		t.Errorf("unknown subcommand: exit %d, want 2", code)
	}
}

func TestConfigSet(t *testing.T) {
	g, path := configRepo(t)
	code, out, errs := smatesCmd(t, g.dir, "config", "set", "orphans", "ignore", "--dry-run")
	if code != 0 || !strings.Contains(out, "would set orphans: ignore (was all)") || !strings.Contains(out, "+orphans: ignore   # policy comment") {
		t.Errorf("%d %s %s", code, out, errs)
	}
	if readFile(t, path) != commentedConfig {
		t.Errorf("--dry-run wrote the file")
	}
	code, out, errs = smatesCmd(t, g.dir, "config", "set", "--format", "json", "agent.model", "claude-x")
	if code != 0 {
		t.Fatal(errs)
	}
	res := decodeJSON[configEdit](t, out)
	if res.Value == nil || *res.Value != "claude-x" || res.Previous != nil || !res.Written {
		t.Errorf("%+v", res)
	}
	if code, _, errs := smatesCmd(t, g.dir, "config", "set", "orphans", "agent"); code != 0 {
		t.Fatal(errs)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "orphans: agent   # policy comment") || !strings.HasSuffix(got, "inputs: []   # pins\nagent:\n  model: claude-x\n") {
		t.Errorf("smates.yml:\n%s", got)
	}
	before := got
	for _, bad := range [][]string{{"orphans", "sometimes"}, {"ignore", "x"}, {"flaky.retry", "many"}, {"nope", "1"}, {"suites", "x"}} {
		code, _, errs := smatesCmd(t, g.dir, "config", append([]string{"set"}, bad...)...)
		if code == 0 || errs == "" {
			t.Errorf("set %v accepted", bad)
		}
	}
	if readFile(t, path) != before {
		t.Errorf("a rejected edit wrote the file")
	}

	// No smates.yml.
	g2 := newGitRepo(t)
	if code, _, errs := smatesCmd(t, g2.dir, "config", "set", "orphans", "all"); code == 0 || !strings.Contains(errs, "smates init") {
		t.Errorf("%d %s", code, errs)
	}
}

func TestOrphansApply(t *testing.T) {
	g, base := orphanFixture(t)
	path := filepath.Join(g.dir, "smates.yml")
	commented := "# keep me\n" + readFile(t, path)
	if err := os.WriteFile(path, []byte(commented), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeCfg, err := config.Load(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := smatesCmd(t, g.dir, "orphans", "--dry-run"); code != 2 {
		t.Errorf("--dry-run without --apply: exit %d", code)
	}

	rep, _ := runOrphans(t, g, "--since", base, "--apply", "--dry-run")
	var globs []string
	for _, ig := range rep.Ignore {
		globs = append(globs, ig.Glob)
	}
	if len(globs) == 0 || rep.Applied == nil || !reflect.DeepEqual(rep.Applied.Added, globs) || rep.Applied.Written || !rep.Applied.DryRun || rep.Applied.Diff == "" {
		t.Fatalf("dry run: globs %v applied %+v", globs, rep.Applied)
	}
	if readFile(t, path) != commented {
		t.Errorf("--dry-run wrote smates.yml")
	}

	code, out, errs := smatesCmd(t, g.dir, "orphans", "--since", base, "--apply")
	if code != 0 {
		t.Fatal(errs)
	}
	for _, gl := range globs {
		if !strings.Contains(out, "added to ignore: "+gl) {
			t.Errorf("output lacks %s:\n%s", gl, out)
		}
	}
	got := readFile(t, path)
	if !strings.HasPrefix(got, "# keep me\n") {
		t.Errorf("comment lost:\n%s", got)
	}
	after, err := config.Load(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Ignore, append([]string{"design/**"}, globs...)) {
		t.Errorf("ignore %v", after.Ignore)
	}
	// Nothing else changed.
	after.Ignore = beforeCfg.Ignore
	if !reflect.DeepEqual(after, beforeCfg) {
		t.Errorf("--apply changed more than ignore:\n%+v\n%+v", after, beforeCfg)
	}

	// The applied globs clear those orphans: a second --apply has nothing to add.
	rep, _ = runOrphans(t, g, "--since", base, "--apply")
	if len(rep.Applied.Added) != 0 || rep.Applied.Written {
		t.Errorf("second apply: %+v", rep.Applied)
	}
}

func TestOrphansApplyNeedsConfig(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{"go.mod": "module x\n", "x_test.go": "package x\n"})
	g.git("add", "-A")
	g.commit("x")
	if code, _, errs := smatesCmd(t, g.dir, "orphans", "--apply"); code == 0 || !strings.Contains(errs, "smates init") {
		t.Errorf("%d %s", code, errs)
	}
}

func TestDoctorJSON(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		".github/workflows/ci.yml": "on: push\njobs:\n  t:\n    steps:\n      - run: npx vitest run\n",
		"package.json":             `{}`,
		"pnpm-lock.yaml":           "",
		"web/src/a.test.ts":        "const u = process.env.SMATES_TEST_REQUIRED_VAR;",
		".env.example":             "SMATES_TEST_REQUIRED_VAR=x\n",
		"smates.yml":               "version: 1\nsuites:\n  - name: web\n    runner: vitest\n    root: web\n",
	})
	g.git("add", ".")
	g.commit("files")
	code, text, _ := smatesCmd(t, g.dir, "doctor")
	codeJ, out, errs := smatesCmd(t, g.dir, "doctor", "--format", "json")
	if code != 1 || codeJ != 1 {
		t.Errorf("exit codes %d %d, want 1 (a check fails)", code, codeJ)
	}
	rep := decodeJSON[doctorReport](t, out)
	if rep.OK || rep.Version != Version {
		t.Errorf("report %+v %s", rep, errs)
	}
	byID := map[string]*doctorCheck{}
	var lines []string
	for _, c := range rep.Checks {
		if c.ID == "" || slug(c.ID) != c.ID || !slices.Contains([]string{"ok", "warn", "fail"}, c.Status) {
			t.Errorf("bad check %+v", c)
		}
		byID[c.ID] = c
		label := map[string]string{"ok": "ok   ", "warn": "warn ", "fail": "FAIL "}[c.Status]
		lines = append(lines, "  "+label+" "+c.Message)
	}
	// The text output is the same checks.
	if want := "smates " + Version + "\n" + strings.Join(lines, "\n") + "\n"; text != want {
		t.Errorf("text:\n%s\njson as text:\n%s", text, want)
	}
	if c := byID["suite-dependencies-installed"]; c == nil || c.Status != "fail" || c.Subject != "web" || c.Fix == nil || c.Fix.Command != "pnpm install --frozen-lockfile" {
		t.Errorf("node_modules check: %+v", c)
	}
	if c := byID["env-var-set"]; c == nil || c.Status != "warn" || c.Subject != "SMATES_TEST_REQUIRED_VAR" || c.Fix == nil || c.Fix.Config == nil ||
		c.Fix.Config.Key != "suites.web.env.SMATES_TEST_REQUIRED_VAR" || c.Fix.Config.Value != "x" {
		t.Errorf("env check: %+v", c)
	}
	if c := byID["agent"]; c == nil || c.Status != "warn" || c.Fix == nil || c.Fix.Command != "smates config set orphans all" || c.Fix.Config.Key != "orphans" {
		t.Errorf("agent check: %+v", c)
	}
	if c := byID["state-recorded"]; c == nil || c.Status != "warn" || c.Fix.Command != "smates run --all" {
		t.Errorf("state check: %+v", c)
	}
	if c := byID["workflow-test-step"]; c == nil || c.Status != "warn" || c.Subject != ".github/workflows/ci.yml:t:#1" {
		t.Errorf("workflow check: %+v", c)
	}
	if code, _, _ := smatesCmd(t, g.dir, "doctor", "--format", "yaml"); code != 2 {
		t.Errorf("unknown format: exit %d", code)
	}
}

// doctor counts the units each rule with reaches applies to, and warns about one that
// applies to none.
func TestDoctorRuleReaches(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		"go.mod":                        "module example.com/m\n\ngo 1.22\n",
		"pkg/shared/testkit/testkit.go": "package testkit\n",
		"pkg/app/repo/repo_test.go":     "package repo\n\nimport (\n\t\"testing\"\n\n\t_ \"example.com/m/pkg/shared/testkit\"\n)\n\nfunc TestRepo(t *testing.T) {}\n",
		"pkg/app/pure/pure_test.go":     "package pure\n\nimport \"testing\"\n\nfunc TestPure(t *testing.T) {}\n",
		"database/migrations/001.sql":   "create table t()",
		"smates.yml": "version: 1\nsuites:\n  - name: go\n    runner: go\n    rules:\n" +
			"      - reaches: [\"pkg/shared/testkit/**\"]\n        deps: [\"database/migrations/**\"]\n" +
			"      - reaches: [\"pkg/shared/testkits/**\"]\n        deps: [\"database/migrations/**\"]\n" +
			// One glob matches, the other (an import path) names nothing.
			"      - reaches: [\"pkg/shared/testkit\", \"example.com/m/pkg/shared/other\"]\n        deps: [\"database/fixtures/**\"]\n",
	})
	g.git("add", ".")
	g.commit("files")
	_, out, errs := smatesCmd(t, g.dir, "doctor", "--format", "json")
	rep := decodeJSON[doctorReport](t, out)
	got := map[string]*doctorCheck{}
	for _, c := range rep.Checks {
		if c.ID == "suite-rule-reaches" {
			got[c.Subject] = c
		}
	}
	if c := got["go: rules[0]"]; c == nil || c.Status != "ok" || !strings.Contains(c.Message, "applies to 1 of 2 test units") {
		t.Errorf("rules[0]: %+v %s", c, errs)
	}
	if c := got["go: rules[1]"]; c == nil || c.Status != "warn" || !strings.Contains(c.Message, "applies to no test unit") || c.Fix == nil {
		t.Errorf("rules[1]: %+v %s", c, errs)
	}
	if c := got["go: rules[2]"]; c == nil || c.Status != "warn" || !strings.Contains(c.Message, "applies to 1 of 2 test units, but its reaches example.com/m/pkg/shared/other match no file") || c.Fix == nil {
		t.Errorf("rules[2]: %+v %s", c, errs)
	}
}

func TestReadCommandsJSON(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		"smates.yml": `version: 1
orphans: all
suites:
  - name: api
    runner: custom
    root: backend
    tests: ["*_check.sh"]
    command: "true"
  - name: e2e
    runner: custom
    root: e2e
    tests: ["*_check.sh"]
    command: "true"
    depends_on: [api]
`,
		"backend/user.go":    "package api\n",
		"backend/x_check.sh": "",
		"e2e/login_check.sh": "",
	})
	g.git("add", "-A")
	g.commit("fixture")

	_, out, errs := smatesCmd(t, g.dir, "why", "--format", "json", "backend/user.go")
	why := decodeJSON[whyReport](t, out)
	want := whyTest{Suite: "e2e", Test: "e2e/login_check.sh", Kind: "content", How: []string{"depends_on suite api"}}
	if why.Target != "backend/user.go" || !slices.ContainsFunc(why.Tests, func(w whyTest) bool { return reflect.DeepEqual(w, want) }) {
		t.Errorf("why: %+v %s", why, errs)
	}
	_, text, _ := smatesCmd(t, g.dir, "why", "backend/user.go")
	if !strings.Contains(text, "e2e\te2e/login_check.sh\t(depends_on suite api)") {
		t.Errorf("why text:\n%s", text)
	}
	_, out, _ = smatesCmd(t, g.dir, "why", "--format", "json", "nothing.txt")
	if why := decodeJSON[whyReport](t, out); why.Tests == nil || len(why.Tests) != 0 {
		t.Errorf("why with no tests: %s", out)
	}

	code, out, errs := smatesCmd(t, g.dir, "deps", "--format", "json", "e2e/login_check.sh")
	deps := decodeJSON[depsReport](t, out)
	if code != 0 || deps.Suite != "e2e" || deps.Test != "e2e/login_check.sh" || !slices.ContainsFunc(deps.Deps, func(d depEntry) bool { return d.Dep == "e2e/login_check.sh" }) {
		t.Errorf("deps: %d %+v %s", code, deps, errs)
	}
	for _, d := range deps.Deps {
		if d.Sources == nil {
			t.Errorf("deps: nil sources for %s", d.Dep)
		}
	}
	// Flags after the positional argument, as the usage shows them; after "--" every
	// argument is positional.
	code, out, errs = smatesCmd(t, g.dir, "deps", "e2e/login_check.sh", "--format", "json")
	if deps := decodeJSON[depsReport](t, out); code != 0 || deps.Test != "e2e/login_check.sh" {
		t.Errorf("deps <test> --format json: %d %+v %s", code, deps, errs)
	}
	code, out, errs = smatesCmd(t, g.dir, "why", "backend/user.go", "--format", "json")
	if why := decodeJSON[whyReport](t, out); code != 0 || why.Target != "backend/user.go" || len(why.Tests) == 0 {
		t.Errorf("why <file> --format json: %d %+v %s", code, why, errs)
	}
	if code, _, errs := smatesCmd(t, g.dir, "why", "--", "backend/user.go", "--format", "json"); code != 2 || !strings.Contains(errs, "usage: smates why") {
		t.Errorf("why -- <file> --format json: exit %d %s", code, errs)
	}

	_, out, _ = smatesCmd(t, g.dir, "list", "--format", "json")
	list := decodeJSON[listReport](t, out)
	if list.Config != "smates.yml" || len(list.Suites) != 2 || list.Suites[1].Name != "e2e" || list.Suites[1].UnitCount != 1 || !reflect.DeepEqual(list.Suites[1].Units, []string{"e2e/login_check.sh"}) {
		t.Errorf("list: %+v", list)
	}

	// state show: --format json, and the deprecated --json.
	type stateInfo struct {
		Path   string         `json:"path"`
		Files  int            `json:"snapshot_files"`
		Suites map[string]any `json:"suites"`
	}
	for _, flag := range [][]string{{"--format", "json"}, {"--json"}} {
		code, out, errs := smatesCmd(t, g.dir, "state", append([]string{"show"}, flag...)...)
		var st stateInfo
		if err := json.Unmarshal([]byte(out), &st); code != 0 || err != nil || !strings.HasSuffix(st.Path, "state.json.gz") {
			t.Errorf("state show %v: %d %v %s %s", flag, code, err, out, errs)
		}
	}
	if code, _, _ := smatesCmd(t, g.dir, "state", "show", "--format", "xml"); code != 2 {
		t.Errorf("state show --format xml: exit %d", code)
	}
	for _, cmd := range []string{"why", "deps", "list"} {
		if code, _, _ := smatesCmd(t, g.dir, cmd, "--format", "xml", "x"); code != 2 {
			t.Errorf("%s --format xml: exit %d", cmd, code)
		}
	}
}

func TestInitJSON(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	files := map[string]string{
		"go.mod":             "module x\n\ngo 1.22\n",
		"db_test.go":         "package x\nimport \"os\"\nvar _ = os.Getenv(\"APP_SECRET_KEY\") + os.Getenv(\"APP_MODE\")\n",
		".env.example":       "APP_SECRET_KEY=\nAPP_MODE=dev\n",
		"docker-compose.yml": "services:\n  db:\n    image: postgres:16\n    environment:\n      POSTGRES_PASSWORD: pw\n",
		"Makefile":           "test:\n\tgo test -race -count=1 ./...\n",
	}
	g := newGitRepo(t)
	writeTree(t, g.dir, files)
	g.git("add", "-A")
	g.commit("fixture")
	code, out, errs := smatesCmd(t, g.dir, "init", "--format", "json")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	rep := decodeJSON[initReport](t, out)
	if !rep.Written || rep.Config != "smates.yml" || !reflect.DeepEqual(rep.FilesWritten, []string{"smates.yml", ".gitignore"}) {
		t.Errorf("report %+v", rep)
	}
	if len(rep.Suites) != 1 || rep.Suites[0].Runner != "go" || rep.Suites[0].Exclude == nil {
		t.Errorf("suites %+v", rep.Suites)
	}
	items := map[string]reviewItem{}
	for _, it := range rep.NeedsReview {
		if it.ID == "" || slug(it.ID) != it.ID || it.Reason == "" || it.Action.Description == "" || it.Evidence == nil {
			t.Errorf("bad item %+v", it)
		}
		items[it.ID] = it
	}
	if it, ok := items["service-db"]; !ok || it.Kind != "service" || !strings.Contains(it.Reason, "postgres service db found in docker-compose.yml") ||
		it.Action.Command != "docker compose up -d db" || !strings.Contains(it.Action.Snippet, "image: postgres:16") || !strings.Contains(it.Action.Snippet, "DATABASE_URL") {
		t.Errorf("service item: %+v", it)
	}
	if it := items["env-app-mode"]; it.Action.Config == nil || it.Action.Config.Key != "suites.go.env.APP_MODE" || it.Action.Config.Value != "dev" || it.Evidence[0] != "db_test.go" {
		t.Errorf("env item: %+v", it)
	}
	if it := items["env-app-secret-key"]; it.Action.Config != nil || !strings.Contains(it.Action.Snippet, "secrets.APP_SECRET_KEY") {
		t.Errorf("secret env item: %+v", it)
	}
	if it, ok := items["makefile-command-go"]; !ok || it.Action.Config == nil || it.Action.Config.Key != "suites.go.command" {
		t.Errorf("makefile item: %+v", it)
	}
	if it, ok := items["orphans-policy"]; !ok || it.Action.Command != "smates config set orphans all" {
		t.Errorf("orphans item: %+v", it)
	}

	// The written config is the one the text mode writes.
	g2 := newGitRepo(t)
	writeTree(t, g2.dir, files)
	g2.git("add", "-A")
	g2.commit("fixture")
	if code, text, errs := smatesCmd(t, g2.dir, "init"); code != 0 || !strings.Contains(text, "wrote smates.yml with 1 suite(s)") || !strings.Contains(text, "Next steps:") {
		t.Fatalf("%d %s %s", code, text, errs)
	}
	if readFile(t, filepath.Join(g.dir, "smates.yml")) != readFile(t, filepath.Join(g2.dir, "smates.yml")) {
		t.Errorf("init --format json wrote a different smates.yml")
	}

	// --stdout: the document in config_yaml, nothing written.
	g3 := newGitRepo(t)
	writeTree(t, g3.dir, files)
	_, out, _ = smatesCmd(t, g3.dir, "init", "--stdout", "--format", "json")
	rep = decodeJSON[initReport](t, out)
	if rep.Written || len(rep.FilesWritten) != 0 || !strings.Contains(rep.ConfigYAML, "runner: go") || fileExists(g3.dir, "smates.yml") {
		t.Errorf("--stdout: %+v", rep)
	}

	// --github: the workflow is reported.
	g4 := newGitRepo(t)
	writeTree(t, g4.dir, files)
	_, out, errs = smatesCmd(t, g4.dir, "init", "--github", "--format", "json")
	rep = decodeJSON[initReport](t, out)
	if rep.Workflow == nil || !rep.Workflow.Written || !slices.Contains(rep.FilesWritten, ".github/workflows/smates.yml") {
		t.Errorf("--github: %+v %s", rep, errs)
	}
}
