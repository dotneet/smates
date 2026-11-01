package main

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/pathutil"
)

// TestE2ERulesCoverSources checks that every source file of the smates binary is a rule
// dependency of some e2e suite in the repository's smates.yml. The go suite covers a
// file the e2e rules miss, so it would not be an orphan either: a change to it would run
// no e2e test on a pull request. This test lives in package main, whose static
// dependencies include every package the binary links, so it is selected for them.
func TestE2ERulesCoverSources(t *testing.T) {
	root := filepath.Join("..", "..")
	cfg, err := config.Load(root)
	if err != nil || cfg == nil {
		t.Fatalf("loading smates.yml: %v", err)
	}
	var deps []string
	for _, s := range cfg.Suites {
		if strings.HasPrefix(s.Name, "e2e") {
			for _, r := range s.Rules {
				deps = append(deps, r.Deps...)
			}
		}
	}
	if len(deps) == 0 {
		t.Fatal("no e2e suite rules in smates.yml")
	}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// internal/testutil is imported by tests only (its own test checks
				// this), so it is not part of the binary the e2e tests run.
				if d.Name() == "testdata" || filepath.ToSlash(p) == filepath.ToSlash(filepath.Join(root, "internal", "testutil")) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if !strings.HasSuffix(rel, "_test.go") && !pathutil.Match(deps, rel) {
				t.Errorf("%s is in no e2e suite's rules: add it to one in smates.yml", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
