package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

// TestDependsOnCLI: e2e tests exercising a server in another process select on a
// change to the server's code through depends_on, and --explain, why and doctor say so.
func TestDependsOnCLI(t *testing.T) {
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
  - name: smoke
    runner: custom
    root: smoke
    tests: ["*_check.sh"]
    command: "true"
`)
	write("backend/user.go", "package api\n")
	write("backend/x_check.sh", "")
	write("e2e/login_check.sh", "")
	write("smoke/ping_check.sh", "curl http://localhost:8080/health\n")
	g.git("add", "-A")
	base := g.commit("fixture")
	write("backend/user.go", "package api\n// changed\n")

	run := func(args ...string) (string, string) {
		var out, errb bytes.Buffer
		if code := Main(append(args[:1:1], append([]string{"-C", g.dir}, args[1:]...)...), &out, &errb); code != 0 {
			t.Fatalf("%v: %d %s", args, code, errb.String())
		}
		return out.String(), errb.String()
	}
	for _, suites := range [][]string{nil, {"--suite", "e2e"}} {
		args := append([]string{"select", "--no-agent", "--base", base, "--explain", "--format", "markdown"}, suites...)
		md, log := run(args...)
		if !strings.Contains(md, "server code its tests exercise (`depends_on suite api`)") {
			t.Errorf("%v: markdown:\n%s", suites, md)
		}
		if !strings.Contains(log, "+ e2e/login_check.sh  [diff: backend/user.go (depends_on suite api)]") {
			t.Errorf("%v: explain:\n%s", suites, log)
		}
		if strings.Contains(log, "orphan") {
			t.Errorf("%v: backend/user.go must not be an orphan:\n%s", suites, log)
		}
	}
	if out, _ := run("why", "backend/user.go"); !strings.Contains(out, "e2e\te2e/login_check.sh\t(depends_on suite api)") {
		t.Errorf("why:\n%s", out)
	}
	if out, _ := run("why", "--suite", "e2e", "backend/user.go"); !strings.Contains(out, "(depends_on suite api)") {
		t.Errorf("why --suite e2e:\n%s", out)
	}

	var out, errb bytes.Buffer
	Main([]string{"doctor", "-C", g.dir}, &out, &errb)
	if !strings.Contains(out.String(), "suite smoke looks like end-to-end tests (smoke/ping_check.sh calls http://localhost)") {
		t.Errorf("doctor:\n%s", out.String())
	}
	if strings.Contains(out.String(), "suite e2e looks like") {
		t.Errorf("doctor: e2e declares depends_on:\n%s", out.String())
	}
}

func TestServerSuiteHint(t *testing.T) {
	mk := func(c config.Suite) *runner.Suite {
		if c.Root == "" {
			c.Root = "."
		}
		return &runner.Suite{Suite: c}
	}
	cases := []struct {
		s    *runner.Suite
		want string
	}{
		{mk(config.Suite{Name: "e2e", Root: "web"}), "name e2e"},
		{mk(config.Suite{Name: "web", Root: "tests/E2E"}), "root tests/E2E"},
		{mk(config.Suite{Name: "ui", Command: "npx playwright test"}), "runs playwright"},
		{mk(config.Suite{Name: "ui", Command: "cypress run"}), "runs cypress"},
		{mk(config.Suite{Name: "web"}), ""},
		{mk(config.Suite{Name: "e2e", DependsOn: []config.Dependency{{Suite: "api"}}}), ""},
		{mk(config.Suite{Name: "e2e", Serves: []string{"api/**"}}), ""},
		{mk(config.Suite{Name: "e2e", Rules: []config.Rule{{Deps: []string{"api/**"}}}}), ""},
	}
	for _, c := range cases {
		if got := serverSuiteHint(t.TempDir(), c.s, nil); got != c.want {
			t.Errorf("%+v: %q, want %q", c.s.Suite, got, c.want)
		}
	}
}
