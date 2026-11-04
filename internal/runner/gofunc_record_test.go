package runner

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

// A test function declared only in a file the suite's configuration does not compile
// (build constraints: a //go:build tag the suite does not pass, a GOOS file) is not a
// unit: go test would run nothing for it, and a pass recorded here would hide it from a
// configuration that compiles it. With the tag (GOFLAGS, or `smates run -- -tags=...`)
// it is one.
func TestFuncUnitsBuildConstraints(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	t.Setenv("GOWORK", "")
	repo := tempRepo(t)
	write := func(rel, content string) { testutil.Write(t, repo.Root, rel, content) }
	write("go.mod", "module example.com/m\n\ngo 1.22\n")
	write("p/p.go", "package p\n\nfunc F() int { return 1 }\n")
	write("p/p_test.go", "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { F() }\n\nfunc ExampleF() { F() }\n")
	write("p/tagged_test.go", "//go:build smates_never\n\npackage p\n\nimport \"testing\"\n\nfunc TestTagged(t *testing.T) {}\n")
	write("p/x_plan9_test.go", "package p\n\nimport \"testing\"\n\nfunc TestPlan9(t *testing.T) {}\n")
	files := []string{"p/p_test.go", "p/tagged_test.go", "p/x_plan9_test.go"}
	units := func(goflags string) []string {
		suites, err := Resolve(repo, []config.Suite{{Name: "go", Root: ".", Runner: "go", Granularity: config.GranularityFunc, Env: map[string]string{"GOFLAGS": goflags}}})
		if err != nil {
			t.Fatal(err)
		}
		return goTest{}.Units(suites[0], files)
	}
	if got, want := units(""), []string{"p#TestA", "p#ExampleF", "p#(build)"}; !slices.Equal(got, want) {
		t.Errorf("units %v, want %v", got, want)
	}
	if got, want := units("-tags=smates_never"), []string{"p#TestA", "p#ExampleF", "p#TestTagged", "p#(build)"}; !slices.Equal(got, want) {
		t.Errorf("with the tag: units %v, want %v", got, want)
	}
	// Cached by file stat: the same answer again.
	if got, want := units(""), []string{"p#TestA", "p#ExampleF", "p#(build)"}; !slices.Equal(got, want) {
		t.Errorf("again: units %v, want %v", got, want)
	}
	extra, err := Resolve(repo, []config.Suite{{Name: "go", Root: ".", Runner: "go", Granularity: config.GranularityFunc, Env: map[string]string{"GOFLAGS": ""}}})
	if err != nil {
		t.Fatal(err)
	}
	extra[0].ExtraArgs = []string{"-tags=smates_never"}
	if got, want := (goTest{}).Units(extra[0], files), []string{"p#TestA", "p#ExampleF", "p#TestTagged", "p#(build)"}; !slices.Equal(got, want) {
		t.Errorf("with the tag passed through: units %v, want %v", got, want)
	}

	// Recording: an example without output (compiled, never run anywhere) passes; a
	// function that still reaches recording without being compiled ran nothing and is
	// not recorded.
	suites, err := Resolve(repo, []config.Suite{{Name: "go", Root: ".", Runner: "go", Granularity: config.GranularityFunc, Env: map[string]string{"GOFLAGS": ""}}})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	rc := &RunContext{Repo: repo, Suite: suites[0], Record: true, TraceDir: t.TempDir(), ShimDir: t.TempDir(), Stdout: &out, Stderr: &errb}
	res, err := recordFuncs(context.Background(), rc, []string{"p"}, map[string][]string{"p": {"TestA", "ExampleF", "TestTagged"}})
	if err != nil {
		t.Fatalf("record: %v\n%s%s", err, &out, &errb)
	}
	if o := res.Observations["p#TestA"]; o == nil || o.Status != state.Pass || !o.Files["p/p.go#decl:F"] || !o.Files["p/p.go#decls"] {
		t.Errorf("TestA: %+v\n%s", o, &out)
	}
	if o := res.Observations["p#ExampleF"]; o == nil || o.Status != state.Pass {
		t.Errorf("ExampleF: %+v", o)
	}
	if o, ok := res.Observations["p#TestTagged"]; ok {
		t.Errorf("TestTagged recorded although it never ran: %+v", o)
	}
}

func TestMayBeConstrained(t *testing.T) {
	root := t.TempDir()
	for name, want := range map[string]bool{
		"a_test.go": false, "linux_test.go": false, "a_linux_test.go": true, "a_windows_amd64_test.go": true,
		"a_arm64_test.go": true, "a_linuxish_test.go": false, "tagged_test.go": true, "plusbuild_test.go": true,
		"late_test.go": false,
	} {
		content := "package p\n"
		switch name {
		case "tagged_test.go":
			content = "// Copyright\n\n//go:build integration\n\npackage p\n"
		case "plusbuild_test.go":
			content = "// +build linux\n\npackage p\n"
		case "late_test.go":
			content = "package p\n\n//go:build x\n"
		}
		testutil.Write(t, root, name, content)
		if got := mayBeConstrained(root + "/" + name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}
