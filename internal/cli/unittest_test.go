package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

// TestUnittestEndToEnd drives `smates run` / `smates select` through Main on a small
// project tested with the standard library's unittest (detected without a smates.yml):
// recording, selection of only the test modules a change affects, and per-module status.
func TestUnittestEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix paths")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	clearCI(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	noAgentCredentials(t)
	// Edits below swap same-size contents within one second, which a cached .pyc
	// (validated by mtime and size) would not notice.
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	g := newGitRepo(t)
	write := func(rel, content string) { testutil.Write(t, g.dir, rel, content) }
	write(".gitignore", ".smates/\n")
	write("pyproject.toml", "[project]\nname = \"shop\"\nversion = \"0.1\"\n")
	write("shop/__init__.py", "")
	write("shop/prices.py", "def price(n):\n    return n * 10\n")
	write("shop/tax.py", "RATE = 8\n")
	write("shop/rates.txt", "8\n")
	write("tests/__init__.py", "")
	write("tests/test_prices.py", `import unittest
from shop.prices import price
class Prices(unittest.TestCase):
    def test_price(self):
        self.assertEqual(price(2), 20)
`)
	write("tests/test_tax.py", `import os, unittest
from shop import tax
class Tax(unittest.TestCase):
    def test_rate(self):
        with open(os.path.join(os.path.dirname(__file__), "..", "shop", "rates.txt")) as f:
            self.assertEqual(int(f.read()), tax.RATE)
`)
	g.git("add", "-A")
	g.commit("fixture")

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir, "--no-agent"}, args[1:]...)...), &out, &errb)
		return code, out.String(), errb.String()
	}
	selected := func() map[string]string {
		code, out, errs := run("select", "--format", "json")
		if code != 0 {
			t.Fatalf("select: %d %s", code, errs)
		}
		var js struct {
			Suites []struct {
				Name  string `json:"name"`
				Tests []struct {
					ID       string `json:"id"`
					Selected bool   `json:"selected"`
					Reason   string `json:"reason"`
				} `json:"tests"`
			} `json:"suites"`
		}
		if err := json.Unmarshal([]byte(out), &js); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		m := map[string]string{}
		for _, s := range js.Suites {
			if s.Name != "unittest" {
				t.Errorf("suite %q, want the detected unittest suite", s.Name)
			}
			for _, tt := range s.Tests {
				if tt.Selected {
					m[tt.ID] = tt.Reason
				}
			}
		}
		return m
	}

	if code, out, errs := run("run", "--all"); code != 0 {
		t.Fatalf("baseline: %d\n%s\n%s", code, out, errs)
	}
	if got := selected(); len(got) != 0 {
		t.Fatalf("nothing changed, selected %v", got)
	}
	write("shop/rates.txt", "9\n")
	if got := selected(); len(got) != 1 || got["tests/test_tax.py"] != "changed" {
		t.Errorf("data file read by one module: %v", got)
	}
	write("shop/rates.txt", "8\n")

	// A broken dependency selects (and fails) only the module using it.
	write("shop/prices.py", "def price(n):\n    return n * 11\n")
	if got := selected(); len(got) != 1 || got["tests/test_prices.py"] != "changed" {
		t.Errorf("module used by one test module: %v", got)
	}
	report := filepath.Join(t.TempDir(), "report.json")
	if code, _, _ := run("run", "--report", report); code == 0 {
		t.Fatal("run with a broken module must fail")
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Results map[string]map[string]string `json:"results"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Results["unittest"]["tests/test_prices.py"] != "fail" || len(rep.Results["unittest"]) != 1 {
		t.Errorf("results %v", rep.Results)
	}
	// A full run reports the status of every module on its own.
	report = filepath.Join(t.TempDir(), "report.json")
	run("run", "--all", "--report", report)
	data, _ = os.ReadFile(report)
	rep.Results = nil
	json.Unmarshal(data, &rep)
	if r := rep.Results["unittest"]; r["tests/test_prices.py"] != "fail" || r["tests/test_tax.py"] != "pass" {
		t.Errorf("full run results %v", rep.Results)
	}
	if got := selected(); len(got) != 1 || got["tests/test_prices.py"] == "" {
		t.Errorf("after failure: %v", got)
	}
	write("shop/prices.py", "def price(n):\n    return n * 10\n")
	if code, _, errs := run("run"); code != 0 {
		t.Fatalf("fixed run: %d\n%s", code, errs)
	}
	if got := selected(); len(got) != 0 {
		t.Errorf("after fix: %v", got)
	}
}
