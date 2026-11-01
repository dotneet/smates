package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/hints"
	"github.com/dotneet/smates/internal/state"
)

// TestFailureHints breaks a module one test imports and checks the hints in the run
// log, the JSON report and the step summary, then the agent explanation (against a
// fake Messages API) and its cache.
func TestFailureHints(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	clearCI(t)
	t.Setenv("GITHUB_OUTPUT", "")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	noAgentCredentials(t)
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Each test file runs in its own process (isolate), so statuses are per unit.
	write("smates.yml", `version: 1
orphans: ignore
suites:
  - name: py
    runner: custom
    tests: ["tests/*_check.py"]
    command: sh -c '[ $# -gt 0 ] || set -- tests/*_check.py; for f; do python3 "$f" || exit 1; done' sh
`)
	write(".gitignore", ".smates/\n")
	write("src/mod_a.py", "X = 1\n")
	write("src/mod_b.py", "Y = 2\n")
	write("data/a.txt", "hello\n")
	write("tests/a_check.py", "import sys\nsys.path.insert(0, 'src')\nimport mod_a\nassert open('data/a.txt').read().startswith('hello')\nassert mod_a.X == 1\n")
	write("tests/b_check.py", "import sys\nsys.path.insert(0, 'src')\nimport mod_b\nassert mod_b.Y == 2\n")
	g.git("add", "-A")
	g.commit("fixture")

	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append(args[:1:1], append([]string{"-C", g.dir}, args[1:]...)...), &out, &errb)
		return code, errb.String()
	}
	readReport := func(path string) []*hints.Hint {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var rep struct {
			Hints []*hints.Hint `json:"failure_hints"`
		}
		if err := json.Unmarshal(data, &rep); err != nil {
			t.Fatal(err)
		}
		return rep.Hints
	}
	if code, errs := run("run", "--all", "--no-agent"); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, errs)
	}
	baseline, err := os.ReadFile(filepath.Join(g.dir, ".smates", "state.json.gz"))
	if err != nil {
		t.Fatal(err)
	}

	write("src/mod_a.py", "X = 2\n")
	write("data/a.txt", "hello world\n")
	dir := t.TempDir()
	report, summary := filepath.Join(dir, "report.json"), filepath.Join(dir, "summary.md")
	code, errs := run("run", "--all", "--no-agent", "--report", report, "--summary", summary)
	if code == 0 {
		t.Fatalf("broken module must fail\n%s", errs)
	}
	hs := readReport(report)
	byTest := map[string]*hints.Hint{}
	for _, h := range hs {
		byTest[h.Test] = h
	}
	a := byTest["tests/a_check.py"]
	if a == nil || a.Verdict != hints.VerdictChanged || a.Basis != hints.BasisLastRun || a.LastStatus != "pass" {
		t.Fatalf("hint for a: %+v (all: %v)\n%s", a, hs, errs)
	}
	var got []string
	for _, c := range a.Candidates {
		got = append(got, c.Path+":"+c.Kind)
	}
	if strings.Join(got, ",") != "src/mod_a.py:source,data/a.txt:data" || a.Candidates[0].Link != "direct import" {
		t.Errorf("candidates %v", got)
	}
	// The custom runner stops at the first failure: b did not run, or ran and passed.
	if b := byTest["tests/b_check.py"]; b != nil && len(b.Candidates) != 0 {
		t.Errorf("hint for b: %+v", b)
	}
	if !strings.Contains(errs, "failure hints for") || !strings.Contains(errs, "src/mod_a.py  (source, direct import, modified)") {
		t.Errorf("run log:\n%s", errs)
	}
	md, _ := os.ReadFile(summary)
	if !strings.Contains(string(md), "**Failure hints (") || !strings.Contains(string(md), "`src/mod_a.py` (source") {
		t.Errorf("summary:\n%s", md)
	}

	// Run again: compared with the failing run just recorded, nothing changed.
	run("run", "--no-agent", "--report", report)
	if hs := readReport(report); len(hs) == 0 || hs[0].Verdict != hints.VerdictUnchanged || hs[0].LastStatus != "fail" {
		t.Errorf("repeated failure: %+v", hs)
	}

	// The agent explains the failure; a second identical failure reuses the answer.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `-X = 1`) || !strings.Contains(string(body), `"json_schema"`) {
			t.Errorf("request lacks the diff or the schema: %s", body)
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"{\"file\":\"src/mod_a.py\",\"line\":1,\"reason\":\"X changed from 1 to 2, so the assertion fails.\",\"confidence\":\"high\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	// Each run starts from the state recorded before the breaking change (as a CI job
	// restoring the default branch's state does), keeping the agent cache.
	statePath := filepath.Join(g.dir, ".smates", "state.json.gz")
	for i, wantCached := range []bool{false, true} {
		st, err := state.Load(statePath)
		if err != nil {
			t.Fatal(err)
		}
		base, err := state.Read(bytes.NewReader(baseline), true)
		if err != nil {
			t.Fatal(err)
		}
		base.AgentCache = st.AgentCache
		if err := base.Save(statePath); err != nil {
			t.Fatal(err)
		}
		code, errs = run("run", "--report", report)
		if code == 0 {
			t.Fatalf("run %d must fail\n%s", i, errs)
		}
		var ex *hints.Explanation
		for _, h := range readReport(report) {
			if h.Test == "tests/a_check.py" {
				ex = h.Explanation
			}
		}
		if ex == nil || ex.File != "src/mod_a.py" || ex.Line != 1 || ex.Confidence != "high" || ex.Cached != wantCached {
			t.Fatalf("run %d: explanation %+v\n%s", i, ex, errs)
		}
		if !strings.Contains(errs, "→ agent (high confidence") {
			t.Errorf("run %d log:\n%s", i, errs)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d API calls, want 1 (second run cached)", n)
	}

	// --no-agent and agent.explain_failures: false turn it off.
	write("src/mod_a.py", "X = 3\n")
	if _, errs = run("run", "--no-agent", "--report", report); strings.Contains(errs, "→ agent") {
		t.Errorf("--no-agent:\n%s", errs)
	}
	cfg, _ := os.ReadFile(filepath.Join(g.dir, "smates.yml"))
	write("smates.yml", string(cfg)+"agent:\n  explain_failures: false\n")
	if _, errs = run("run", "--report", report); strings.Contains(errs, "→ agent") {
		t.Errorf("explain_failures false:\n%s", errs)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d API calls after disabling", n)
	}
}

func TestDisplayCandidates(t *testing.T) {
	key := depkey.Config("smates.yml")
	got := displayCandidates([]hints.Candidate{{Path: key, Old: "config:1", New: "config:2"}, {Path: "a.go", Old: "x", New: "y"}})
	if len(got) != 2 || got[0].Path != "smates.yml" || got[0].Old != "" || got[0].New != "" || got[1].Old != "x" {
		t.Errorf("mapped: %+v", got)
	}
	got = displayCandidates([]hints.Candidate{{Path: key, Old: "config:1"}, {Path: "smates.yml", Old: "b1", New: "b2"}})
	if len(got) != 1 || got[0].Old != "b1" {
		t.Errorf("the content candidate wins: %+v", got)
	}
}
