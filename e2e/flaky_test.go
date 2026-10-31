//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFlaky flips the outcome of fixtures/flaky/test/coin.test.js through a file
// outside the repository, so no input smates tracks changes, and checks that smates
// reports it as flaky, keeps selecting the failure, lists it in `smates flaky`, and
// with flaky.retry retries it instead of failing the run.
func TestFlaky(t *testing.T) {
	if only := os.Getenv("SMATES_E2E_FIXTURE"); only != "" && only != "flaky" {
		t.Skip("SMATES_E2E_FIXTURE excludes flaky")
	}
	coin := filepath.Join(t.TempDir(), "coin")
	t.Setenv("SMATES_E2E_COIN", coin)
	r := prepare(t, "flaky")
	const unit = "test/coin.test.js"
	type flake struct {
		Suite   string `json:"suite"`
		Unit    string `json:"unit"`
		Flip    string `json:"flip"`
		Count   int    `json:"count"`
		Retried bool   `json:"retried"`
	}
	run := func(args ...string) (*report, []flake, string, int) {
		t.Helper()
		out, code := r.smates(append([]string{"run", "--no-agent", "--report", r.reportPath()}, args...)...)
		var extra struct {
			Flaky []flake `json:"flaky"`
		}
		data, err := os.ReadFile(r.reportPath())
		if err != nil {
			t.Fatalf("no report: %v\n%s", err, out)
		}
		if err := json.Unmarshal(data, &extra); err != nil {
			t.Fatal(err)
		}
		return r.readReport(), extra.Flaky, out, code
	}
	setCoin := func(v string) {
		if err := os.WriteFile(coin, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 1. A full run with identical inputs: the coin test now fails. Flaky by proof, and
	// the failure still fails the run.
	setCoin("fail-once")
	rep, flakes, out, code := run("--all")
	if code == 0 || rep.Exit == 0 {
		t.Fatalf("flaky failure must fail the run:\n%s", tail(out, 40))
	}
	if len(flakes) != 1 || flakes[0].Unit != unit || flakes[0].Flip != "pass->fail" || flakes[0].Retried {
		t.Fatalf("flakes %+v\n%s", flakes, tail(out, 40))
	}
	if !strings.Contains(out, "FLAKY: 1 test unit(s)") {
		t.Errorf("run log does not report the flake:\n%s", tail(out, 40))
	}

	// 2. Nothing changed: only the failure is selected again; it passes (second flip).
	rep, flakes, out, code = run()
	if code != 0 {
		t.Fatalf("rerun failed:\n%s", tail(out, 40))
	}
	if sel := rep.selected(); !sel[unit] || sel["test/math.test.js"] || len(sel) != 1 {
		t.Errorf("selected %v, want only %s", sel, unit)
	}
	if len(flakes) != 1 || flakes[0].Flip != "fail->pass" || flakes[0].Count != 2 {
		t.Errorf("flakes after rerun %+v\n%s", flakes, tail(out, 40))
	}

	// 3. `smates flaky` lists it with its evidence; the stable test is not listed.
	out, code = r.smates("flaky", "--format", "json")
	var listed struct {
		Flaky []struct {
			Suite  string   `json:"suite"`
			Unit   string   `json:"unit"`
			Count  int      `json:"count"`
			Recent []string `json:"recent"`
		} `json:"flaky"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &listed) != nil {
		t.Fatalf("smates flaky (exit %d):\n%s", code, out)
	}
	if len(listed.Flaky) != 1 || listed.Flaky[0].Unit != unit || listed.Flaky[0].Count != 2 || strings.Join(listed.Flaky[0].Recent, " ") != "pass fail pass" {
		t.Errorf("smates flaky: %+v", listed.Flaky)
	}

	// 4. A change to an input it depends on explains a new outcome: not a flake.
	r.apply(edit{File: "src/math.js", Op: "append", Content: "// touched\n"})
	setCoin("fail")
	_, flakes, out, code = run()
	if code == 0 || len(flakes) != 0 {
		t.Errorf("failure after an input change: exit %d, flakes %+v\n%s", code, flakes, tail(out, 40))
	}
	r.must("git", "checkout", "-q", "--", "src/math.js")

	// 5. Opt-in retry: a known flaky unit that fails, then passes, does not fail the run.
	r.apply(edit{File: "smates.yml", Op: "append", Content: "flaky:\n  retry: 1\n"})
	setCoin("fail-once")
	rep, flakes, out, code = run("--all")
	if code != 0 || rep.Exit != 0 {
		t.Fatalf("retried flake must not fail the run (exit %d):\n%s", code, tail(out, 40))
	}
	if len(flakes) != 1 || flakes[0].Flip != "fail->pass" || !flakes[0].Retried {
		t.Errorf("flakes after retry %+v\n%s", flakes, tail(out, 40))
	}
	if rep.Results["node"][unit] != "pass" {
		t.Errorf("results %v", rep.Results)
	}
	// ...but a failure that persists through the retry still fails it.
	setCoin("fail")
	if _, _, out, code = run("--all"); code == 0 {
		t.Errorf("persistent failure passed with retry:\n%s", tail(out, 40))
	}
}
