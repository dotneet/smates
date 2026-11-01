package runner

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

// A selected function that -test.list does not list ran nothing. One in a file this
// configuration does not compile (build constraints) is not recorded: a pass would hide
// it from a configuration that compiles it and shares the state. An example without
// output is compiled and never runs anywhere: it passes.
func TestRecordFuncsNotListed(t *testing.T) {
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
	if o := res.Observations["p#TestA"]; o == nil || o.Status != state.Pass || !o.Files["p/p.go"] {
		t.Errorf("TestA: %+v\n%s", o, &out)
	}
	if o := res.Observations["p#ExampleF"]; o == nil || o.Status != state.Pass {
		t.Errorf("ExampleF: %+v", o)
	}
	if o, ok := res.Observations["p#TestTagged"]; ok {
		t.Errorf("TestTagged recorded although it never ran: %+v", o)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "TestTagged") {
		t.Errorf("warnings %q", res.Warnings)
	}
}
