package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/selector"
)

func TestInventoryResolve(t *testing.T) {
	inv := newInventory(testSelection())
	tests := []struct {
		suite, test       string
		wantSuite, wantID string
		wantErr           string
	}{
		{suite: "web", test: "src/db.test.ts", wantSuite: "web", wantID: "src/db.test.ts"},
		{suite: "web", test: "./src/db.test.ts", wantSuite: "web", wantID: "src/db.test.ts"},
		{suite: "web", test: "db.test.ts", wantSuite: "web", wantID: "src/db.test.ts"},
		{suite: "py", test: "tests/test_api.py::test_api", wantSuite: "py", wantID: "tests/test_api.py"},
		{suite: "go", test: "pkg/a", wantSuite: "go", wantID: "svc/pkg/a"},
		{suite: "web", test: "*", wantSuite: "web", wantID: ""},
		{suite: "web", test: "", wantSuite: "web", wantID: ""},
		{suite: "", test: "src/db.test.ts", wantSuite: "web", wantID: "src/db.test.ts"},
		{suite: "frontend", test: "src/db.test.ts", wantSuite: "web", wantID: "src/db.test.ts"},
		{suite: "go", test: "dir", wantErr: "ambiguous"},
		{suite: "go", test: "b/b", wantErr: `no test "b/b" in suite "go"`},
		{suite: "web", test: "test.ts", wantErr: `no test`},
		{suite: "nope", test: "*", wantErr: `unknown suite "nope"`},
		{suite: "nope", test: "x.test.ts", wantErr: `unknown suite "nope" and no test`},
	}
	for _, tt := range tests {
		t.Run(tt.suite+"/"+tt.test, func(t *testing.T) {
			s, id, err := inv.resolve(tt.suite, tt.test)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || s != tt.wantSuite || id != tt.wantID {
				t.Fatalf("resolve = %q, %q, %v; want %q, %q", s, id, err, tt.wantSuite, tt.wantID)
			}
		})
	}
}

func TestToDecision(t *testing.T) {
	f := newFixture(t)
	req := f.request(testConfig())
	sub, err := parseSubmission([]byte(`{
		"decision": "tests",
		"tests": [
			{"suite": "web", "test": "src/db.test.ts", "why": "runs scripts/seed.sh"},
			{"suite": "web", "test": "db.test.ts", "why": "duplicate via suffix"},
			{"suite": "go", "test": "*", "why": "whole suite"},
			{"suite": "web", "test": "missing.test.ts", "why": "hallucinated"},
			{"suite": "py", "test": "tests/test_api.py", "why": ""}
		],
		"no_impact": ["scripts/gone.sh", "./scripts/gone.sh", "not/an/orphan"],
		"summary": "  seed script feeds the db tests  "
	}`))
	if err != nil {
		t.Fatal(err)
	}
	d, err := toDecision(sub, req)
	if err != nil {
		t.Fatal(err)
	}
	wantAdd := []selector.Addition{
		{Suite: "web", Test: "src/db.test.ts", Reason: selector.ReasonAgent, Detail: "runs scripts/seed.sh"},
		{Suite: "go", Test: "", Reason: selector.ReasonAgent, Detail: "whole suite"},
		{Suite: "py", Test: "tests/test_api.py", Reason: selector.ReasonAgent, Detail: "possibly affected by an orphan change"},
	}
	if !reflect.DeepEqual(d.Add, wantAdd) {
		t.Errorf("Add = %+v\nwant %+v", d.Add, wantAdd)
	}
	if len(d.Dropped) != 1 || !strings.Contains(d.Dropped[0], "missing.test.ts") {
		t.Errorf("Dropped = %v", d.Dropped)
	}
	if !reflect.DeepEqual(d.NoImpact, []string{"scripts/gone.sh"}) {
		t.Errorf("NoImpact = %v", d.NoImpact)
	}
	if d.RunAll || d.Summary != "seed script feeds the db tests" {
		t.Errorf("RunAll=%v Summary=%q", d.RunAll, d.Summary)
	}
	// The additions apply cleanly to the selection.
	if unknown := req.Selection.Apply(d.Add); len(unknown) != 0 {
		t.Errorf("unknown additions: %v", unknown)
	}
}

func TestParseSubmissionRejectsBadDecision(t *testing.T) {
	for _, in := range []string{`{"decision":"maybe"}`, `{"decision":1}`, `nope`} {
		if _, err := parseSubmission([]byte(in)); err == nil {
			t.Errorf("parseSubmission(%s) succeeded", in)
		}
	}
	sub, err := parseSubmission([]byte(`{"decision":" RUN_ALL "}`))
	if err != nil || sub.Decision != decisionRunAll {
		t.Fatalf("got %+v, %v", sub, err)
	}
}

func TestParseCommandOutput(t *testing.T) {
	answer := `{"decision":"tests","tests":[{"suite":"web","test":"src/db.test.ts","why":"x"}],"no_impact":[],"summary":"s"}`
	tests := []struct {
		name      string
		out       string
		wantDec   string
		wantTests int
		wantUsage Usage
		wantErr   string
	}{
		{name: "bare object", out: answer, wantDec: "tests", wantTests: 1},
		{name: "fx final_output string", out: `{"final_output":` + quote("Here you go:\n```json\n"+answer+"\n```\n") + `,"status":"ok"}`, wantDec: "tests", wantTests: 1},
		{name: "fx final_output object", out: `{"final_output":` + answer + `}`, wantDec: "tests", wantTests: 1},
		{name: "claude -p result with usage", out: `{"type":"result","result":` + quote(answer) + `,"num_turns":3,"total_cost_usd":0.0125,"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}`,
			wantDec: "tests", wantTests: 1, wantUsage: Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, Turns: 3, CostUSD: 0.0125}},
		{name: "last object wins", out: "draft: {\"decision\":\"none\",\"tests\":[]}\nfinal:\n" + `{"decision":"run_all","tests":[],"summary":"ci changed"}` + "\ntrailing {not json", wantDec: "run_all"},
		{name: "prose with braces", out: "I looked at {stuff}.\n```\n" + answer + "\n```", wantDec: "tests", wantTests: 1},
		{name: "no decision", out: `{"answer":"none"}`, wantErr: "no JSON object"},
		{name: "empty", out: "", wantErr: "no JSON object"},
		{name: "invalid decision", out: `{"decision":"skip"}`, wantErr: "decision must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, usage, err := parseCommandOutput([]byte(tt.out))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sub.Decision != tt.wantDec || len(sub.Tests) != tt.wantTests {
				t.Errorf("got decision %q with %d tests", sub.Decision, len(sub.Tests))
			}
			if usage != tt.wantUsage {
				t.Errorf("usage = %+v, want %+v", usage, tt.wantUsage)
			}
		})
	}
}

func quote(s string) string {
	return string(mustJSON(s))
}

// A decision that asks for tests but names none that exist is rejected, so the caller
// falls back to the orphan policy instead of handling the orphans with nothing selected.
func TestToDecisionRejectsEmptyTests(t *testing.T) {
	f := newFixture(t)
	req := f.request(testConfig())
	for _, in := range []string{
		`{"decision":"tests","tests":[{"suite":"web","test":"ghost.test.ts","why":"x"}],"no_impact":[],"summary":"s"}`,
		`{"decision":"none","tests":[{"suite":"nope","test":"*","why":"x"}],"no_impact":[],"summary":"s"}`,
		`{"decision":"tests","tests":[],"no_impact":[],"summary":"s"}`,
	} {
		sub, err := parseSubmission([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if d, err := toDecision(sub, req); err == nil {
			t.Errorf("toDecision(%s) = %+v, want an error", in, d)
		}
	}
	for _, in := range []string{
		`{"decision":"none","tests":[],"no_impact":["scripts/seed.sh"],"summary":"s"}`,
		`{"decision":"run_all","tests":[{"suite":"web","test":"ghost.test.ts","why":"x"}],"no_impact":[],"summary":"s"}`,
	} {
		sub, _ := parseSubmission([]byte(in))
		if d, err := toDecision(sub, req); err != nil {
			t.Errorf("toDecision(%s) = %+v, %v", in, d, err)
		}
	}
}
