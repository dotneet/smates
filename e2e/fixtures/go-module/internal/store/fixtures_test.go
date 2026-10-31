package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every fixture dropped into testdata/ must be a valid JSON array of objects.
func TestFixturesAreValid(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
		if err := Validate(filepath.Join("testdata", e.Name())); err != nil {
			t.Errorf("fixture %s: %v", e.Name(), err)
		}
	}
	if n == 0 {
		t.Fatal("no fixtures found")
	}
}
