package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/hints"
)

func (f *fixture) explainRequest() *ExplainRequest {
	h := &hints.Hint{
		Suite: "web", Test: "src/db.test.ts", Verdict: hints.VerdictChanged, Basis: hints.BasisLastRun,
		LastStatus: "pass", Inputs: 12, Changed: 3,
		Candidates: []hints.Candidate{
			{Path: "scripts/seed.sh", Kind: hints.KindSource, Link: "traced", Change: "modified", Old: f.oldSeed, New: f.newSeed},
			{Path: ".env", Kind: hints.KindConfig, Link: "suite input", Change: "modified", Old: "0000", New: "1111"},
			{Path: "src/", Kind: hints.KindListing, Link: "traced", Change: "modified"},
		},
	}
	return &ExplainRequest{
		Repo: f.repo, Config: testConfig(), Hint: h, Runner: "vitest",
		Output: "\x1b[31mFAIL\x1b[0m src/db.test.ts > db (12ms)\nError: psql: fixtures/extra.sql: No such file",
	}
}

func textReply(text string) fakeReply {
	b, _ := json.Marshal(text)
	return reply("end_turn", `{"type":"text","text":`+string(b)+`}`)
}

func TestExplain(t *testing.T) {
	f := newFixture(t)
	answer := `{"file":"scripts/seed.sh","line":4,"reason":"seed.sh now loads fixtures/extra.sql, which does not exist.","confidence":"high"}`
	rep := textReply(answer)
	rep.inspect = func(t *testing.T, r recordedRequest) {
		var oc struct {
			Effort string `json:"effort"`
			Format struct {
				Type   string          `json:"type"`
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		}
		if err := json.Unmarshal(r.body["output_config"], &oc); err != nil || oc.Format.Type != "json_schema" || oc.Effort != "low" || len(oc.Format.Schema) == 0 {
			t.Errorf("output_config %s", r.body["output_config"])
		}
		if _, ok := r.body["tools"]; ok {
			t.Error("explanations must not offer tools")
		}
		if string(r.body["model"]) != `"claude-haiku-5-5"` {
			t.Errorf("model %s", r.body["model"])
		}
		prompt := string(r.raw)
		for _, want := range []string{"psql < fixtures/extra.sql", "No such file", "scripts/seed.sh (source, traced, modified)", "runner vitest"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt lacks %q", want)
			}
		}
		for _, bad := range []string{"do-not-leak", `\u001b[31m`} {
			if strings.Contains(prompt, bad) {
				t.Errorf("prompt contains %q", bad)
			}
		}
	}
	backend, fake := newTestBackend(t, rep)
	ex, usage, err := backend.explain(context.Background(), f.explainRequest())
	if err != nil {
		t.Fatal(err)
	}
	if ex.File != "scripts/seed.sh" || ex.Line != 4 || ex.Confidence != "high" || !strings.Contains(ex.Reason, "extra.sql") || ex.Model != "claude-haiku-5-5" {
		t.Errorf("explanation %+v", ex)
	}
	if usage.Turns != 1 || usage.InputTokens != 100 {
		t.Errorf("usage %+v", usage)
	}
	if n := len(fake.requests()); n != 1 {
		t.Errorf("%d requests", n)
	}
}

func TestExplainValidatesAnswer(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("word ", 200)
	backend, _ := newTestBackend(t, textReply(`{"file":"../outside.go","line":7,"reason":"  `+long+`\n","confidence":"certain"}`))
	ex, _, err := backend.explain(context.Background(), f.explainRequest())
	if err != nil {
		t.Fatal(err)
	}
	if ex.File != "" || ex.Line != 0 || ex.Confidence != "low" || len(ex.Reason) > maxExplainReason+len("…") || strings.Contains(ex.Reason, "\n") {
		t.Errorf("explanation %+v", ex)
	}
}

func TestExplainErrors(t *testing.T) {
	f := newFixture(t)
	for name, rep := range map[string]fakeReply{
		"refusal":   reply("refusal"),
		"not json":  textReply("I think it is the seed script."),
		"no reason": textReply(`{"file":"","line":0,"reason":" ","confidence":"low"}`),
		"bad req":   {status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`},
	} {
		backend, _ := newTestBackend(t, rep)
		if _, _, err := backend.explain(context.Background(), f.explainRequest()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestExplainCacheKey(t *testing.T) {
	f := newFixture(t)
	a := f.explainRequest()
	b := f.explainRequest()
	b.Output = strings.Replace(b.Output, "(12ms)", "(48ms)", 1)
	if ExplainCacheKey(a) != ExplainCacheKey(b) {
		t.Error("durations must not change the key")
	}
	b.Hint.Candidates[0].New = "other"
	if ExplainCacheKey(a) == ExplainCacheKey(b) {
		t.Error("candidate content must change the key")
	}
	c := f.explainRequest()
	c.Output += "\nanother assertion"
	if ExplainCacheKey(a) == ExplainCacheKey(c) {
		t.Error("output must change the key")
	}
	if !strings.HasPrefix(ExplainCacheKey(a), "explain:") {
		t.Error("explanation keys are namespaced")
	}
}

func TestExplainBriefBounds(t *testing.T) {
	f := newFixture(t)
	req := f.explainRequest()
	var out strings.Builder
	for i := 0; out.Len() < 3*maxExplainOutput; i++ {
		out.WriteString("log line " + strconv.Itoa(i) + " ü\n")
	}
	req.Output = out.String() + "THE ASSERTION"
	for i := range 20 {
		req.Hint.Candidates = append(req.Hint.Candidates, hints.Candidate{Path: "src/helpers/long.txt", Kind: hints.KindData, Change: "modified", Old: "x" + strconv.Itoa(i)})
	}
	brief := explainBrief(context.Background(), req)
	if !strings.Contains(brief, "THE ASSERTION") || strings.Contains(brief, "log line 0 ") {
		t.Error("output must keep its tail")
	}
	if len(brief) > maxExplainOutput+maxExplainDiffTotal+4000 {
		t.Errorf("brief is %d bytes", len(brief))
	}
	if !strings.Contains(brief, "\n### scripts/seed.sh\n") || strings.Contains(brief, "### .env") || strings.Contains(brief, "### src/\n") {
		t.Errorf("diff selection:\n%s", brief)
	}
}

func TestExplainSupported(t *testing.T) {
	clearCredentials(t)
	t.Setenv(envAPIKey, "k")
	cfg := testConfig()
	if ok, _ := ExplainSupported(cfg); !ok {
		t.Error("API key present")
	}
	cfg.Command = "my-agent"
	if ok, why := ExplainSupported(cfg); ok || !strings.Contains(why, "agent.command") {
		t.Errorf("command backend: %v %s", ok, why)
	}
	t.Setenv(envAPIKey, "")
	t.Setenv(envAuthToken, "")
	if ok, _ := ExplainSupported(testConfig()); ok {
		t.Error("no credentials")
	}
}
