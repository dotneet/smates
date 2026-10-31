package configlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every YAML file shipped in config/ must lint cleanly.
func TestConfigDirIsClean(t *testing.T) {
	const dir = "../../config"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range Lint(e.Name(), data) {
			t.Error(p)
		}
	}
}

func TestLintFindsDuplicates(t *testing.T) {
	if got := Lint("x.yaml", []byte("a: 1\na: 2\nbroken\n")); len(got) != 2 {
		t.Fatalf("Lint = %v", got)
	}
}
