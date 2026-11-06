package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// Build constraints decide which tests go test compiles, with flags smates does not
// always see (a wrapper command) and with configuration that is no dependency of a
// unit (GOFLAGS, GOOS, GOARCH or CGO_ENABLED of the environment). A directory none of
// whose Go files compile, or whose tests are all left out, is no test failure (exit 0)
// and is no recorded pass either: it runs again, so the configuration that compiles its
// tests runs them.
func TestGoBuildConstraintRuns(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	const tagged = "//go:build smates_extra\n\npackage extra\n\nimport \"testing\"\n\nfunc TestExtra(t *testing.T) {}\n"
	const taggedFailing = "//go:build smates_extra\n\npackage extra\n\nimport \"testing\"\n\nfunc TestExtra(t *testing.T) { t.Fatal(\"boom\") }\n"
	const failing = "package bad\n\nimport \"testing\"\n\nfunc TestBad(t *testing.T) { t.Fatal(\"boom\") }\n"
	type step struct {
		goflags string
		args    []string
		want    int
	}
	tests := []struct {
		name        string
		command     string
		granularity string
		files       map[string]string
		steps       []step
		notPass     []string // units whose recorded status must not be a pass
		skip        []string // units whose recorded status must be a skip
		fail        []string // units whose recorded status must be a failure
	}{
		{
			// Must keep: the wrapper may pass the tag, so the directory stays a unit;
			// go test's "build constraints exclude all Go files" is no failure.
			name: "wrapper, all-tag-gated directory", command: "./gt",
			files: map[string]string{"extra/x_test.go": tagged},
			steps: []step{{args: []string{"--all"}}, {}, {}},
			skip:  []string{"extra"},
		},
		{
			name: "wrapper, all-tag-gated directory, func granularity", command: "./gt", granularity: "func",
			files: map[string]string{"extra/x_test.go": tagged},
			steps: []step{{args: []string{"--all"}}, {}, {}},
			skip:  []string{"extra#TestExtra", "extra#(build)"},
		},
		{
			// Must keep: a wrapper running go test again with the tag compiles the
			// directory, and its failing test fails the run although the first pass
			// said build constraints exclude it.
			name: "wrapper running go test twice, the tagged pass fails", command: "./gt2",
			files: map[string]string{"extra/x_test.go": taggedFailing},
			steps: []step{{args: []string{"--all"}, want: 1}, {want: 1}},
			fail:  []string{"extra"},
		},
		{
			name: "wrapper running go test twice, the tagged pass fails, func granularity", command: "./gt2", granularity: "func",
			files: map[string]string{"extra/x_test.go": taggedFailing},
			steps: []step{{args: []string{"--all"}, want: 1}, {want: 1}},
			fail:  []string{"extra#TestExtra"},
		},
		{
			name: "wrapper running go test twice, the tagged pass passes", command: "./gt2",
			files: map[string]string{"extra/x_test.go": tagged},
			steps: []step{{args: []string{"--all"}}, {}},
		},
		{
			name: "go test itself, all-tag-gated directory", command: "go test",
			files: map[string]string{"extra/x_test.go": tagged},
			steps: []step{{args: []string{"--all"}}, {}},
		},
		{
			// Must keep: another package failing still fails the run.
			name: "wrapper, all-tag-gated directory and a failing package", command: "./gt",
			files: map[string]string{"extra/x_test.go": tagged, "bad/b_test.go": failing},
			steps: []step{{args: []string{"--all"}, want: 1}, {want: 1}},
		},
		{
			// Must keep: tests a tag of the environment's GOFLAGS left out run, and
			// fail, once GOFLAGS no longer has it.
			name: "tests excluded by the environment's GOFLAGS", command: "go test",
			files: map[string]string{
				"gated/g.go":      "package gated\n",
				"gated/g_test.go": "//go:build !smates_off\n\npackage gated\n\nimport \"testing\"\n\nfunc TestGated(t *testing.T) { t.Fatal(\"boom\") }\n",
			},
			steps: []step{{goflags: "-tags=smates_off", args: []string{"--all"}}, {want: 1}},
		},
		{
			name: "tests excluded by the environment's GOFLAGS, wrapper", command: "./gt",
			files: map[string]string{
				"gated/g.go":      "package gated\n",
				"gated/g_test.go": "//go:build !smates_off\n\npackage gated\n\nimport \"testing\"\n\nfunc TestGated(t *testing.T) { t.Fatal(\"boom\") }\n",
			},
			steps: []step{{goflags: "-tags=smates_off", args: []string{"--all"}}, {want: 1}},
		},
		{
			// Must keep: a test file of another platform alone; the state may be used
			// on that platform.
			name: "tests of another platform only", command: "go test",
			files: map[string]string{
				"plat/p.go":            "package plat\n",
				"plat/p_plan9_test.go": "package plat\n\nimport \"testing\"\n\nfunc TestPlan9(t *testing.T) {}\n",
			},
			steps:   []step{{args: []string{"--all"}}, {}},
			notPass: []string{"plat"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearCI(t)
			t.Setenv("GITHUB_OUTPUT", "")
			t.Setenv("GITHUB_STEP_SUMMARY", "")
			t.Setenv("GOWORK", "")
			noAgentCredentials(t)
			g := newGitRepo(t)
			write := func(rel, content string, mode os.FileMode) {
				p := filepath.Join(g.dir, filepath.FromSlash(rel))
				os.MkdirAll(filepath.Dir(p), 0o755)
				if err := os.WriteFile(p, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			gran := ""
			if tt.granularity != "" {
				gran = "    granularity: " + tt.granularity + "\n"
			}
			write("smates.yml", "version: 1\norphans: ignore\nsuites:\n  - name: go\n    runner: go\n"+gran+"    command: "+tt.command+"\n", 0o644)
			write(".gitignore", ".smates/\n", 0o644)
			write("gt", "#!/bin/sh\nexec go test \"$@\"\n", 0o755)
			write("gt2", "#!/bin/sh\ngo test \"$@\"; a=$?\ngo test -tags smates_extra \"$@\"; b=$?\nexit $((a|b))\n", 0o755)
			write("go.mod", "module example.com/tags\n\ngo 1.21\n", 0o644)
			write("lib/lib.go", "package lib\n\nfunc F() int { return 1 }\n", 0o644)
			write("lib/lib_test.go", "package lib\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { F() }\n", 0o644)
			for rel, content := range tt.files {
				write(rel, content, 0o644)
			}
			g.git("add", "-A")
			g.commit("fixture")
			for i, s := range tt.steps {
				t.Setenv("GOFLAGS", s.goflags)
				var out, errb bytes.Buffer
				code := Main(append([]string{"run", "-C", g.dir, "--no-agent"}, s.args...), &out, &errb)
				if code != s.want {
					t.Fatalf("step %d (%v, GOFLAGS=%q): exit %d, want %d\n%s%s", i, s.args, s.goflags, code, s.want, out.String(), errb.String())
				}
			}
			st, err := state.Load(filepath.Join(g.dir, ".smates", "state.json.gz"))
			if err != nil {
				t.Fatal(err)
			}
			for _, u := range tt.notPass {
				if e := st.Suites["go"].Tests[u]; e != nil && e.Status == state.Pass {
					t.Errorf("%s recorded as a pass: its tests never ran", u)
				}
			}
			for _, u := range tt.skip {
				if e := st.Suites["go"].Tests[u]; e == nil || e.Status != state.Skip {
					t.Errorf("%s: recorded %+v, want a skip", u, e)
				}
			}
			for _, u := range tt.fail {
				if e := st.Suites["go"].Tests[u]; e == nil || e.Status != state.Fail {
					t.Errorf("%s: recorded %+v, want a failure", u, e)
				}
			}
		})
	}
}
