package runner

import (
	"os/exec"
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/testutil"
)

// With tests widened to non-test files (tests: ["**/*_test.go", "**/cmd/**/*.go"]), a
// package without tests is a unit that compiles it. A directory none of whose Go files
// the suite's configuration compiles is not: go test would fail it with "build
// constraints exclude all Go files". Whenever any Go file of the directory may
// compile, or go list cannot tell, the unit stays.
func TestPackageUnitsBuildConstraints(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	t.Setenv("GOWORK", "")
	repo := tempRepo(t)
	never := "//go:build smates_never\n\n"
	for rel, content := range map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.22\n",
		"lib/lib.go":    "package lib\n\nfunc F() int { return 1 }\n",
		"lib/l_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { F() }\n",
		// A main package without tests.
		"cmd/app/main.go": "package main\n\nimport \"example.com/m/lib\"\n\nfunc main() { lib.F() }\n",
		// Only files a tag or the platform excludes.
		"cmd/tool/main.go":       never + "package main\n\nfunc main() {}\n",
		"cmd/plan/main_plan9.go": "package main\n\nfunc main() {}\n",
		// One file excluded, one not.
		"cmd/mixed/main.go":  "package main\n\nfunc main() {}\n",
		"cmd/mixed/extra.go": never + "package main\n",
		// Excluded tests of a package that compiles.
		"tagged/t.go":      "package tagged\n",
		"tagged/t_test.go": never + "package tagged\n\nimport \"testing\"\n\nfunc TestT(t *testing.T) {}\n",
		// Everything excluded, tests included; a file go ignores by name does not count.
		"alltagged/a.go":      never + "package alltagged\n",
		"alltagged/a_test.go": never + "package alltagged\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"alltagged/_old.go":   "package alltagged\n",
		// Only an external test package.
		"xonly/x_test.go": "package xonly_test\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n",
	} {
		testutil.Write(t, repo.Root, rel, content)
	}
	files := []string{
		"lib/l_test.go", "cmd/app/main.go", "cmd/tool/main.go", "cmd/plan/main_plan9.go",
		"cmd/mixed/main.go", "cmd/mixed/extra.go", "tagged/t_test.go", "alltagged/a_test.go", "xonly/x_test.go",
	}
	everything := []string{"lib", "cmd/app", "cmd/tool", "cmd/plan", "cmd/mixed", "tagged", "alltagged", "xonly"}
	tests := []struct {
		name        string
		env         map[string]string
		extra       []string
		granularity string
		noRoot      bool
		files       []string
		want        []string
	}{
		{name: "excluded directories are no units", files: files,
			want: []string{"lib", "cmd/app", "cmd/mixed", "tagged", "xonly"}},
		{name: "tag in GOFLAGS", env: map[string]string{"GOFLAGS": "-tags=smates_never"}, files: files,
			want: []string{"lib", "cmd/app", "cmd/tool", "cmd/mixed", "tagged", "alltagged", "xonly"}},
		{name: "tag passed through", extra: []string{"-tags", "smates_never"}, files: files,
			want: []string{"lib", "cmd/app", "cmd/tool", "cmd/mixed", "tagged", "alltagged", "xonly"}},
		// Must keep: go list fails (a -modfile that does not exist), so nothing is known
		// to be excluded.
		{name: "go list fails", env: map[string]string{"GOFLAGS": "-modfile=missing.mod"}, files: files, want: everything},
		// Must keep: without the repository root nothing can be checked.
		{name: "no repository root", noRoot: true, files: files, want: everything},
		// Must keep: the excluded test file alone (as the selector maps each file to its
		// units) still names its package, whose other files compile.
		{name: "excluded test of a compiling package", files: []string{"tagged/t_test.go"}, want: []string{"tagged"}},
		{name: "excluded file of a compiling main", files: []string{"cmd/mixed/extra.go"}, want: []string{"cmd/mixed"}},
		{name: "excluded file alone", files: []string{"cmd/tool/main.go"}, want: nil},
		{name: "func granularity", granularity: config.GranularityFunc, files: files,
			want: []string{"lib#TestF", "xonly#TestX", "lib#(build)", "cmd/app#(build)", "cmd/mixed#(build)", "xonly#(build)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"GOFLAGS": ""}
			for k, v := range tt.env {
				env[k] = v
			}
			suites, err := Resolve(repo, []config.Suite{{
				Name: "go", Root: ".", Runner: "go", Granularity: tt.granularity, Env: env,
				Tests: []string{"**/*_test.go", "**/cmd/**/*.go"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			s := suites[0]
			s.ExtraArgs = tt.extra
			if tt.noRoot {
				s.RepoRoot = ""
			}
			if got := (goTest{}).Units(s, tt.files); !slices.Equal(got, tt.want) {
				t.Errorf("units %v, want %v", got, tt.want)
			}
		})
	}
}
