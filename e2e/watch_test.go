//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// watchStep is one edit while `smates watch` runs, and what the cycle it starts must show.
type watchStep struct {
	name  string
	edits []edit
	// reset restores the committed tree instead of editing it.
	reset       bool
	selected    []string // test IDs the cycle must run
	notSelected []string
	fail        bool // the cycle must report failures
}

var watchCases = map[string][]watchStep{
	"go-module": {
		{name: "data file read by a Go test", edits: []edit{{File: "internal/store/testdata/orders.json", Op: "append", Content: "\n"}},
			selected: []string{"internal/store"}, notSelected: []string{"pkg/calc", "pkg/render"}},
		{name: "break calc", edits: []edit{{File: "pkg/calc/calc.go", Op: "replace", Old: "return total\n", New: "return total + 1\n"}},
			selected: []string{"pkg/calc", "pkg/api"}, notSelected: []string{"internal/store"}, fail: true},
		{name: "revert", reset: true, selected: []string{"pkg/calc", "pkg/api", "internal/store"}},
	},
	"python-uv": {
		{name: "break tax data", edits: []edit{{File: "tests/data/tax_cases.yaml", Op: "replace", Old: "expected: 107", New: "expected: 108"}},
			selected: []string{"tests/unit/test_pricing.py"}, notSelected: []string{"tests/unit/test_inventory.py"}, fail: true},
		{name: "revert", reset: true, selected: []string{"tests/unit/test_pricing.py"}},
	},
	"vitest-npm": {
		{name: "break tax rates", edits: []edit{{File: "data/tax-rates.yaml", Op: "replace", Old: "standard: 0.19", New: "standard: 0.21"}},
			selected: []string{"test/tax.test.ts"}, notSelected: []string{"test/slug.test.ts", "test/migrations.test.ts"}, fail: true},
		{name: "revert", reset: true, selected: []string{"test/tax.test.ts"}},
	},
}

// syncBuffer collects the output of a running process.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var (
	watchHeader = regexp.MustCompile(`(?m)^── \d\d:\d\d:\d\d ── .*$`)
	watchFooter = regexp.MustCompile(`(?m)^── [✓✗·] .*$`)
)

// TestWatch runs `smates watch` on a prepared fixture, edits files while it runs, and
// checks that every edit starts exactly one cycle running the affected tests (and not
// the others), that test runs do not retrigger it, and that SIGINT stops it cleanly.
func TestWatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals")
	}
	t.Parallel()
	for _, name := range fixtures(t) {
		steps, ok := watchCases[name]
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fixturePool(name)
			r := p.acquire(t)
			defer p.release(r)
			r.reset()
			var out syncBuffer
			cmd := exec.Command(smatesBin, "watch", "--interval", "200ms", "--debounce", "300ms")
			cmd.Dir = r.dir
			cmd.Env = r.env()
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
				}
			}()
			waitCycles := func(n int, what string) {
				t.Helper()
				deadline := time.Now().Add(3 * time.Minute)
				for len(watchFooter.FindAllStringIndex(out.String(), -1)) < n {
					if time.Now().After(deadline) {
						t.Fatalf("timed out waiting for %s (cycle %d):\n%s", what, n, out.String())
					}
					select {
					case err := <-exited:
						t.Fatalf("smates watch exited (%v) waiting for %s:\n%s", err, what, out.String())
					case <-time.After(100 * time.Millisecond):
					}
				}
			}
			waitCycles(1, "the initial cycle")
			for i, st := range steps {
				if st.reset {
					r.must("git", "checkout", "-q", "--", ".")
				}
				for _, e := range st.edits {
					r.apply(e)
				}
				waitCycles(i+2, st.name)
				s := out.String()
				headers := watchHeader.FindAllStringIndex(s, -1)
				footers := watchFooter.FindAllStringIndex(s, -1)
				if len(headers) != i+2 {
					t.Fatalf("%s: %d cycles started, want %d:\n%s", st.name, len(headers), i+2, s)
				}
				cycle := s[headers[i+1][0]:footers[i+1][1]]
				header := s[headers[i+1][0]:headers[i+1][1]]
				footer := s[footers[i+1][0]:footers[i+1][1]]
				if !strings.Contains(header, "changed:") {
					t.Errorf("%s: cycle not started by a change: %s", st.name, header)
				}
				for _, id := range st.selected {
					if !strings.Contains(cycle, "+ "+id+"  [") {
						t.Errorf("%s: %s not selected:\n%s", st.name, id, cycle)
					}
				}
				for _, id := range st.notSelected {
					if strings.Contains(cycle, "+ "+id+"  [") {
						t.Errorf("%s: %s selected:\n%s", st.name, id, cycle)
					}
				}
				if failed := strings.Contains(footer, "✗"); failed != st.fail {
					t.Errorf("%s: footer %q, want failure=%v", st.name, footer, st.fail)
				}
			}
			// Test runs write caches, reports and the state: none of it may start a cycle.
			time.Sleep(3 * time.Second)
			if n := len(watchHeader.FindAllStringIndex(out.String(), -1)); n != len(steps)+1 {
				t.Fatalf("%d cycles after the last edit, want %d (retriggered by its own run?):\n%s", n, len(steps)+1, out.String())
			}
			cmd.Process.Signal(os.Interrupt)
			select {
			case err := <-exited:
				if err != nil {
					t.Fatalf("smates watch did not exit cleanly: %v\n%s", err, out.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("smates watch did not stop on SIGINT:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "stopped watching") {
				t.Errorf("no clean stop message:\n%s", out.String())
			}
			t.Logf("%s", out.String())
		})
	}
}
