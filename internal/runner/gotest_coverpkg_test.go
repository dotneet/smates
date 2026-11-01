package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

func TestCoverPkgs(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	cases := []struct {
		cmd     string
		extra   []string
		goflags string
		want    []string
	}{
		{"go test -cover", nil, "", nil},
		{"go test -coverpkg=./...", nil, "", []string{"./..."}},
		{"go test --coverpkg=./a,./b -coverprofile=c.out", nil, "", []string{"./a", "./b"}},
		{"go test -coverpkg ./x/...", nil, "", []string{"./x/..."}},
		{"go test", []string{"-coverpkg=./y"}, "", []string{"./y"}},
		{"go test", nil, "-count=1 -coverpkg=example.com/m/...", []string{"example.com/m/..."}},
	}
	for _, c := range cases {
		s := &Suite{Suite: config.Suite{Command: c.cmd}}
		if c.goflags != "" {
			s.Env = map[string]string{"GOFLAGS": c.goflags}
		}
		got := coverPkgs(&RunContext{Suite: s, ExtraArgs: c.extra})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q %v GOFLAGS=%q: %v, want %v", c.cmd, c.extra, c.goflags, got, c.want)
		}
	}
}

func TestGoPackageFiles(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"mod/go.mod":          "module example.com/m\n\ngo 1.22\n",
		"mod/a/a.go":          "package a\n",
		"mod/a/a_test.go":     "package a\n",
		"mod/b/b.go":          "package b\n",
		"mod/vendor/v/v.go":   "package v\n",
		"mod/b/testdata/x.go": "package x\n",
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rc := &RunContext{Repo: &gitx.Repo{Root: root}, Suite: &Suite{Suite: config.Suite{Root: "mod"}}}
	got, err := goPackageFiles(context.Background(), rc, []string{"./..."})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if want := []string{"mod/a/a.go", "mod/b/b.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("files %v, want %v", got, want)
	}
}
