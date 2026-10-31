package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
)

// TestLiveAgent calls the real Messages API. It runs only with SMATES_LIVE_AGENT=1
// and credentials in the environment:
//
//	SMATES_LIVE_AGENT=1 ANTHROPIC_API_KEY=... go test -run Live -v ./internal/agent/
func TestLiveAgent(t *testing.T) {
	if os.Getenv("SMATES_LIVE_AGENT") != "1" {
		t.Skip("set SMATES_LIVE_AGENT=1 to call the real API")
	}
	cfg := config.AgentConfig{Model: os.Getenv("SMATES_LIVE_MODEL"), MaxTurns: 8, Timeout: 120 * time.Second, Effort: "low"}
	if ok, why := Available(cfg); !ok {
		t.Skip(why)
	}

	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	for p, c := range map[string]string{
		"package.json":             `{"name":"live","scripts":{"pretest":"sh scripts/gen-users.sh","test":"node --test"}}` + "\n",
		"scripts/gen-users.sh":     "#!/bin/sh\n# Regenerates the users fixture consumed by the tests.\ncat > fixtures/users.json <<'EOF'\n[{\"name\":\"alice\",\"role\":\"admin\"}]\nEOF\n",
		"fixtures/users.json":      `[{"name":"alice","role":"admin"}]` + "\n",
		"tests/users.test.js":      "const test = require('node:test');\nconst assert = require('node:assert');\nconst users = require('../fixtures/users.json');\ntest('has an admin', () => assert.ok(users.some(u => u.role === 'admin')));\n",
		"tests/math.test.js":       "const test = require('node:test');\nconst assert = require('node:assert');\nconst { add } = require('../src/math');\ntest('add', () => assert.strictEqual(add(1, 2), 3));\n",
		"tests/strings.test.js":    "const test = require('node:test');\nconst assert = require('node:assert');\nconst { upper } = require('../src/strings');\ntest('upper', () => assert.strictEqual(upper('a'), 'A'));\n",
		"src/math.js":              "exports.add = (a, b) => a + b;\n",
		"src/strings.js":           "exports.upper = s => s.toUpperCase();\n",
		"scripts/deploy-docs.sh":   "#!/bin/sh\nrsync -a site/ docs@example.com:/srv/docs\n",
		"README.md":                "# live\n",
		".github/workflows/ci.yml": "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: npm test\n",
	} {
		writeFile(t, dir, p, c)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "base")
	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	oldBlob, _ := gitx.HashFile(filepath.Join(repo.Root, "scripts", "gen-users.sh"))
	writeFile(t, repo.Root, "scripts/gen-users.sh", "#!/bin/sh\n# Regenerates the users fixture consumed by the tests.\ncat > fixtures/users.json <<'EOF'\n[{\"name\":\"alice\",\"role\":\"member\"}]\nEOF\n")
	newBlob, _ := gitx.HashFile(filepath.Join(repo.Root, "scripts", "gen-users.sh"))

	ids := []string{"tests/math.test.js", "tests/strings.test.js", "tests/users.test.js"}
	sel := &selector.Result{Mode: "state", Suites: []*selector.Suite{{
		Suite: &runner.Suite{Suite: config.Suite{Name: "node", Root: ".", Runner: "node"}},
		Name:  "node",
		Deps: map[string][]string{
			"tests/math.test.js":    {"package.json", "src/math.js", "tests/math.test.js"},
			"tests/strings.test.js": {"package.json", "src/strings.js", "tests/strings.test.js"},
			"tests/users.test.js":   {"fixtures/users.json", "package.json", "tests/users.test.js"},
		},
	}}}
	for _, id := range ids {
		sel.Suites[0].Tests = append(sel.Suites[0].Tests, &selector.Test{ID: id, Reason: selector.ReasonUnaffected})
	}

	var log bytes.Buffer
	req := &Request{
		Repo:      repo,
		Config:    cfg,
		Orphans:   []Orphan{{Path: "scripts/gen-users.sh", Status: "modified", OldBlob: oldBlob, NewBlob: newBlob}},
		Selection: sel,
		Log:       &log,
	}
	started := time.Now()
	d, err := New(cfg).Decide(context.Background(), req)
	elapsed := time.Since(started)
	t.Logf("log:\n%s", log.String())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	u := d.Usage
	cost := (float64(u.InputTokens)*0.10 + float64(u.OutputTokens)*0.50 + float64(u.CacheReadTokens)*0.01 + float64(u.CacheWriteTokens)*0.125) / 1e6
	t.Logf("model %s: %s in %s, %d turns, tokens in=%d out=%d cache_read=%d cache_write=%d, est. cost $%.5f",
		d.Model, describe(d), elapsed.Round(time.Millisecond), u.Turns, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, cost)
	t.Logf("summary: %s", d.Summary)
	for _, a := range d.Add {
		t.Logf("add %s %s: %s", a.Suite, a.Test, a.Detail)
	}
	if len(d.Dropped) > 0 {
		t.Logf("dropped: %v", d.Dropped)
	}

	// The script regenerates the fixture the users test reads: the agent must either
	// add that test (or its suite) or ask for everything.
	found := d.RunAll
	for _, a := range d.Add {
		if a.Suite == "node" && (a.Test == "" || a.Test == "tests/users.test.js") {
			found = true
		}
	}
	if !found {
		t.Errorf("decision does not cover tests/users.test.js: %+v", d)
	}
}
