package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
)

func TestParseShard(t *testing.T) {
	ok := map[string][2]int{"": {0, 0}, "1/1": {1, 1}, "1/4": {1, 4}, "4/4": {4, 4}, "2/3": {2, 3}}
	for in, want := range ok {
		i, n, err := parseShard(in)
		if err != nil || i != want[0] || n != want[1] {
			t.Errorf("parseShard(%q) = %d/%d %v", in, i, n, err)
		}
	}
	for _, in := range []string{"0/4", "5/4", "1/0", "-1/2", "1", "a/b", "1/2/3", "/", "1/", "/2", " 1/2"} {
		if _, _, err := parseShard(in); err == nil {
			t.Errorf("parseShard(%q) accepted", in)
		} else {
			var ue usageError
			if !errors.As(err, &ue) {
				t.Errorf("%q: not a usage error: %T", in, err)
			}
		}
	}
}

func shardResult() *selector.Result {
	mk := func(name string, n int, durations func(i int) int64) *selector.Suite {
		s := &selector.Suite{Name: name, All: true}
		for i := 0; i < n; i++ {
			s.Tests = append(s.Tests, &selector.Test{ID: fmt.Sprintf("%s/t%02d", name, i), Selected: i%5 != 4, Duration: durations(i)})
		}
		return s
	}
	return &selector.Result{Suites: []*selector.Suite{
		mk("a", 20, func(i int) int64 { return int64((i*37)%11) * 100 }),
		mk("b", 13, func(i int) int64 { return 0 }), // unknown durations count as 1s
	}}
}

func TestApplyShard(t *testing.T) {
	base := shardResult()
	var wantSelected []string
	for _, s := range base.Suites {
		wantSelected = append(wantSelected, s.Selected()...)
	}
	for _, n := range []int{2, 3, 4, 7} {
		seen := map[string]int{}
		loads := make([]int64, n)
		for i := 1; i <= n; i++ {
			r := shardResult()
			applyShard(r, i, n)
			for _, s := range r.Suites {
				if s.All {
					t.Errorf("n=%d: All must be cleared when sharding", n)
				}
				for _, tt := range s.Tests {
					if tt.Shard < 1 || tt.Shard > n || tt.Selected && tt.Shard != i {
						t.Errorf("n=%d i=%d: %s owned by shard %d", n, i, tt.ID, tt.Shard)
					}
					if tt.Selected {
						seen[tt.ID]++
						ms := tt.Duration
						if ms <= 0 {
							ms = 1000
						}
						loads[i-1] += ms
					} else if strings.HasPrefix(tt.Detail, "in shard") && tt.Detail == fmt.Sprintf("in shard %d/%d", i, n) {
						t.Errorf("n=%d: %s deselected but labelled as this shard", n, tt.ID)
					}
				}
			}
			// Determinism: the same input gives the same shard.
			r2 := shardResult()
			applyShard(r2, i, n)
			if !reflect.DeepEqual(selectedIDs(r), selectedIDs(r2)) {
				t.Errorf("n=%d shard %d not deterministic", n, i)
			}
		}
		// Every selected test runs in exactly one shard; unselected tests in none.
		if len(seen) != len(wantSelected) {
			t.Errorf("n=%d: %d tests across shards, want %d", n, len(seen), len(wantSelected))
		}
		for _, id := range wantSelected {
			if seen[id] != 1 {
				t.Errorf("n=%d: %s ran in %d shards", n, id, seen[id])
			}
		}
		// Shard ownership must not depend on the state a shard restored: with other
		// durations and selections every test keeps its shard.
		for i := 1; i <= n; i++ {
			a, b := shardResult(), shardResult()
			for _, s := range b.Suites {
				for k, tt := range s.Tests {
					tt.Duration = int64(k*7919%5000 + 1)
					tt.Selected = true
				}
			}
			applyShard(a, i, n)
			applyShard(b, i, n)
			sa, sb := selectedIDs(a), selectedIDs(b)
			for _, id := range sa {
				if !contains(sb, id) {
					t.Errorf("n=%d shard %d: %s owned with one state but not with another", n, i, id)
				}
			}
		}
	}
}

func selectedIDs(r *selector.Result) []string {
	var out []string
	for _, s := range r.Suites {
		out = append(out, s.Selected()...)
	}
	return out
}

// ---- CI detection / branch policy ----

var ciVars = []string{
	"GITHUB_ACTIONS", "GITHUB_REF_NAME", "GITHUB_BASE_REF", "GITHUB_HEAD_REF", "GITHUB_EVENT_NAME", "GITHUB_EVENT_PATH",
	"GITLAB_CI", "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME", "CI_COMMIT_BRANCH", "CI_DEFAULT_BRANCH", "CI_MERGE_REQUEST_IID", "CI_MERGE_REQUEST_TARGET_BRANCH_NAME",
	"CIRCLECI", "CIRCLE_BRANCH", "CIRCLE_PULL_REQUEST",
	"BUILDKITE", "BUILDKITE_BRANCH", "BUILDKITE_PIPELINE_DEFAULT_BRANCH", "BUILDKITE_PULL_REQUEST", "BUILDKITE_PULL_REQUEST_BASE_BRANCH",
	"JENKINS_URL", "CHANGE_BRANCH", "BRANCH_NAME", "CHANGE_ID", "CHANGE_TARGET", "CI",
}

func clearCI(t *testing.T) {
	for _, v := range ciVars {
		t.Setenv(v, "")
	}
}

// noAgentCredentials unsets the API keys of every agent provider.
func noAgentCredentials(t *testing.T) {
	for _, v := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(v, "")
	}
}

func TestDetectCI(t *testing.T) {
	event := filepath.Join(t.TempDir(), "event.json")
	os.WriteFile(event, []byte(`{"repository":{"default_branch":"trunk"}}`), 0o644)
	cases := []struct {
		name string
		env  map[string]string
		want ciInfo
	}{
		{"none", nil, ciInfo{}},
		{"github push", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REF_NAME": "main", "GITHUB_EVENT_NAME": "push", "GITHUB_EVENT_PATH": event},
			ciInfo{Name: "github", Branch: "main", DefaultBranch: "trunk"}},
		{"github pr", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REF_NAME": "12/merge", "GITHUB_HEAD_REF": "feat", "GITHUB_BASE_REF": "main", "GITHUB_EVENT_NAME": "pull_request_target"},
			ciInfo{Name: "github", Branch: "feat", PullRequest: true, BaseRef: "main"}},
		{"github merge queue", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REF_NAME": "gh-readonly-queue/main/pr-1", "GITHUB_EVENT_NAME": "merge_group"},
			ciInfo{Name: "github", Branch: "gh-readonly-queue/main/pr-1", PullRequest: true}},
		{"github bad event file", map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_PATH": "/nonexistent"}, ciInfo{Name: "github"}},
		{"gitlab mr", map[string]string{"GITLAB_CI": "true", "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME": "feat", "CI_COMMIT_BRANCH": "", "CI_DEFAULT_BRANCH": "main", "CI_MERGE_REQUEST_IID": "3", "CI_MERGE_REQUEST_TARGET_BRANCH_NAME": "main"},
			ciInfo{Name: "gitlab", Branch: "feat", DefaultBranch: "main", PullRequest: true, BaseRef: "main"}},
		{"gitlab push", map[string]string{"GITLAB_CI": "true", "CI_COMMIT_BRANCH": "main", "CI_DEFAULT_BRANCH": "main"},
			ciInfo{Name: "gitlab", Branch: "main", DefaultBranch: "main"}},
		{"circle", map[string]string{"CIRCLECI": "true", "CIRCLE_BRANCH": "x", "CIRCLE_PULL_REQUEST": "https://github.com/o/r/pull/1"},
			ciInfo{Name: "circleci", Branch: "x", PullRequest: true}},
		{"buildkite non-pr", map[string]string{"BUILDKITE": "true", "BUILDKITE_BRANCH": "main", "BUILDKITE_PIPELINE_DEFAULT_BRANCH": "main", "BUILDKITE_PULL_REQUEST": "false"},
			ciInfo{Name: "buildkite", Branch: "main", DefaultBranch: "main"}},
		{"buildkite pr", map[string]string{"BUILDKITE": "true", "BUILDKITE_BRANCH": "f", "BUILDKITE_PULL_REQUEST": "7", "BUILDKITE_PULL_REQUEST_BASE_BRANCH": "main"},
			ciInfo{Name: "buildkite", Branch: "f", PullRequest: true, BaseRef: "main"}},
		{"jenkins pr", map[string]string{"JENKINS_URL": "http://j", "CHANGE_BRANCH": "f", "BRANCH_NAME": "PR-1", "CHANGE_ID": "1", "CHANGE_TARGET": "main"},
			ciInfo{Name: "jenkins", Branch: "f", PullRequest: true, BaseRef: "main"}},
		{"jenkins branch", map[string]string{"JENKINS_URL": "http://j", "BRANCH_NAME": "main"}, ciInfo{Name: "jenkins", Branch: "main"}},
		{"generic", map[string]string{"CI": "1"}, ciInfo{Name: "ci"}},
		{"generic false", map[string]string{"CI": "false"}, ciInfo{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearCI(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if got := detectCI(); got != c.want {
				t.Errorf("got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

type gitRepo struct {
	t    *testing.T
	dir  string
	repo *gitx.Repo
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	g := &gitRepo{t: t, dir: t.TempDir()}
	g.git("init", "-q", "-b", "main")
	g.commit("one")
	repo, err := gitx.Open(g.dir)
	if err != nil {
		t.Fatal(err)
	}
	g.repo = repo
	return g
}

func (g *gitRepo) git(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g *gitRepo) commit(msg string) string {
	g.git("commit", "-q", "--allow-empty", "-m", msg)
	return g.git("rev-parse", "HEAD")
}

func TestFullRunBranch(t *testing.T) {
	g := newGitRepo(t)
	cases := []struct {
		name       string
		ci         ciInfo
		configured []string
		full       bool
		branch     string
	}{
		{"not on CI", ciInfo{}, nil, false, ""},
		{"pull request", ciInfo{Name: "github", Branch: "main", PullRequest: true}, nil, false, ""},
		{"push to default (from CI)", ciInfo{Name: "github", Branch: "trunk", DefaultBranch: "trunk"}, nil, true, "trunk"},
		{"push to other branch", ciInfo{Name: "github", Branch: "feat", DefaultBranch: "main"}, nil, false, "feat"},
		{"default from repo", ciInfo{Name: "ci"}, nil, true, "main"},
		{"configured branches", ciInfo{Name: "gitlab", Branch: "release", DefaultBranch: "main"}, []string{"main", "release"}, true, "release"},
		{"configured excludes default", ciInfo{Name: "gitlab", Branch: "main", DefaultBranch: "main"}, []string{"release"}, false, "main"},
	}
	for _, c := range cases {
		full, branch := fullRunBranch(c.ci, g.repo, c.configured)
		if full != c.full || branch != c.branch {
			t.Errorf("%s: %v %q, want %v %q", c.name, full, branch, c.full, c.branch)
		}
	}
	g.git("checkout", "-q", "--detach")
	if full, _ := fullRunBranch(ciInfo{Name: "ci"}, g.repo, nil); full {
		t.Error("detached HEAD without a CI branch is not the default branch")
	}
}

func TestAutoBase(t *testing.T) {
	g := newGitRepo(t)
	first := g.git("rev-parse", "HEAD")
	second := g.commit("two")
	g.git("checkout", "-q", "-b", "feat")
	g.commit("feat-1")
	g.commit("feat-2")
	// main moves on after the branch point.
	g.git("checkout", "-q", "main")
	g.commit("main-3")
	g.git("checkout", "-q", "feat")

	if got := autoBase(ciInfo{}, g.repo); got != second {
		t.Errorf("feature branch: merge base with main = %q, want %q", got, second)
	}
	if got := autoBase(ciInfo{Name: "github", BaseRef: "main", PullRequest: true}, g.repo); got != second {
		t.Errorf("PR base ref: %q", got)
	}
	if got := autoBase(ciInfo{Name: "github", BaseRef: "nope", DefaultBranch: "main", PullRequest: true}, g.repo); got != second {
		t.Errorf("unknown base ref falls back to the default branch: %q", got)
	}
	g.git("checkout", "-q", "main")
	if got := autoBase(ciInfo{}, g.repo); got != "HEAD~1" {
		t.Errorf("on the default branch: compare with the previous commit, got %q", got)
	}
	// Origin refs are preferred.
	g.git("update-ref", "refs/remotes/origin/main", first)
	g.git("checkout", "-q", "feat")
	if got := autoBase(ciInfo{}, g.repo); got != first {
		t.Errorf("origin/main preferred: %q, want %q", got, first)
	}

	// Nothing to compare with.
	lone := newGitRepo(t)
	lone.git("branch", "-m", "solo")
	if got := autoBase(ciInfo{}, lone.repo); got != "" {
		t.Errorf("no default branch: %q", got)
	}
	lone.commit("x")
	if got := autoBase(ciInfo{Name: "github", PullRequest: true}, lone.repo); got != "" {
		t.Errorf("PR without refs and no merge commit has no base: %q", got)
	}
	// On the default branch with a single commit there is no previous commit.
	single := newGitRepo(t)
	if got := autoBase(ciInfo{}, single.repo); got != "" {
		t.Errorf("single commit: %q", got)
	}
}

// TestSuiteFilterOrphans checks that --suite selects what a run over every suite selects
// for those suites: a change another suite depends on is not an orphan.
func TestSuiteFilterOrphans(t *testing.T) {
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
  - name: a
    runner: custom
    root: a
    tests: ["*_check.sh"]
    command: "true"
    rules: [{tests: ["*"], deps: ["src/a/**"]}]
  - name: b
    runner: custom
    root: b
    tests: ["*_check.sh"]
    command: "true"
    rules: [{tests: ["*"], deps: ["src/b/**"]}]
`)
	write("a/x_check.sh", "")
	write("b/x_check.sh", "")
	write("src/a/lib", "1")
	write("src/b/lib", "1")
	g.git("add", "-A")
	base := g.commit("fixture")
	selectA := func() (string, []string) {
		var out, errb bytes.Buffer
		if code := Main([]string{"select", "-C", g.dir, "--no-agent", "--base", base, "--suite", "a", "--suite", "a", "--format", "json"}, &out, &errb); code != 0 {
			t.Fatalf("select: %d %s", code, errb.String())
		}
		var js struct {
			Suites []struct {
				Name  string `json:"name"`
				Tests []struct {
					Reason string `json:"reason"`
				} `json:"tests"`
			} `json:"suites"`
			Orphans []string `json:"orphans"`
		}
		if err := json.Unmarshal(out.Bytes(), &js); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if len(js.Suites) != 1 || js.Suites[0].Name != "a" || len(js.Suites[0].Tests) != 1 {
			t.Fatalf("suites: %+v", js.Suites)
		}
		return js.Suites[0].Tests[0].Reason, js.Orphans
	}

	write("src/b/lib", "2")
	if reason, orphans := selectA(); reason != "unaffected" || len(orphans) != 0 {
		t.Errorf("a change only suite b depends on: %s, orphans %v", reason, orphans)
	}
	write("Dockerfile", "FROM x")
	if reason, orphans := selectA(); reason != "orphan" || !reflect.DeepEqual(orphans, []string{"Dockerfile"}) {
		t.Errorf("a change no suite depends on: %s, orphans %v", reason, orphans)
	}

	var out, errb bytes.Buffer
	if code := Main([]string{"select", "-C", g.dir, "--suite", "nope"}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "unknown suite") {
		t.Errorf("unknown suite: %d %s", code, errb.String())
	}
}

func TestRenderConfig(t *testing.T) {
	cases := [][]config.Suite{
		nil,
		{{Name: "go", Runner: "go", Root: "."}},
		{
			{Name: "go:root", Runner: "go", Root: ".", Exclude: []string{"tools/**"}},
			{Name: "go:tools", Runner: "go", Root: "tools"},
			{Name: "vitest:packages/a b", Runner: "vitest", Root: "packages/a b", Exclude: []string{"*: weird/**", "#x"}},
			{Name: "yes", Runner: "pytest", Root: "on"},
		},
	}
	for _, suites := range cases {
		out := renderConfig(suites, map[string][]string{"go:root": {"# command: \"go test -race\"", "# env:", "#   TZ: UTC"}})
		c, err := config.Parse([]byte(out))
		if err != nil {
			t.Fatalf("rendered config does not parse: %v\n%s", err, out)
		}
		if len(c.Suites) != len(suites) {
			t.Fatalf("suites %d, want %d\n%s", len(c.Suites), len(suites), out)
		}
		for i, s := range suites {
			g := c.Suites[i]
			if g.Name != s.Name || g.Runner != s.Runner || g.Root != s.Root || !reflect.DeepEqual(g.Exclude, s.Exclude) {
				t.Errorf("suite %d: %+v, want %+v", i, g, s)
			}
		}
		if c.Orphans != "agent" {
			t.Errorf("orphans %q", c.Orphans)
		}
		// Detected suites resolve with their adapters.
		if _, err := runner.Resolve(&gitx.Repo{Root: t.TempDir()}, c.Suites); err != nil {
			t.Errorf("resolve: %v", err)
		}
	}
}
