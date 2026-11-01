package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/selector"
)

// Decision values accepted from the model.
const (
	decisionNone   = "none"
	decisionTests  = "tests"
	decisionRunAll = "run_all"
)

// submission is the answer schema shared by submit_selection and the external
// command's JSON output.
type submission struct {
	Decision string `json:"decision"`
	Tests    []struct {
		Suite string `json:"suite"`
		Test  string `json:"test"`
		Why   string `json:"why"`
	} `json:"tests"`
	NoImpact []string `json:"no_impact"`
	Summary  string   `json:"summary"`
}

func parseSubmission(data []byte) (*submission, error) {
	var s submission
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid answer: %w", err)
	}
	s.Decision = strings.ToLower(strings.TrimSpace(s.Decision))
	switch s.Decision {
	case decisionNone, decisionTests, decisionRunAll:
	default:
		return nil, fmt.Errorf("invalid answer: decision must be none, tests or run_all (got %q)", s.Decision)
	}
	return &s, nil
}

// inventory resolves model-supplied suite and test names against the selection.
type inventory struct {
	res *selector.Result
}

func newInventory(res *selector.Result) inventory { return inventory{res} }

// resolve maps (suite, test) to an existing suite name and test ID. test "" or "*"
// means the whole suite (returned ID ""). Matching is exact first, then on a unique
// path suffix ("foo_test.go" for "pkg/foo_test.go"); an unknown suite name falls
// back to searching every suite.
func (inv inventory) resolve(suite, test string) (string, string, error) {
	suite, test = strings.TrimSpace(suite), strings.TrimSpace(test)
	su := inv.res.SuiteByName(suite)
	if test == "" || test == "*" {
		if su == nil {
			return "", "", fmt.Errorf("unknown suite %q", suite)
		}
		return su.Name, "", nil
	}
	scope := inv.res.Suites
	if su != nil {
		scope = []*selector.Suite{su}
	}
	type hit struct{ suite, id string }
	find := func(pred func(id string) bool) []hit {
		var hits []hit
		for _, s := range scope {
			for _, t := range s.Tests {
				if pred(t.ID) {
					hits = append(hits, hit{s.Name, t.ID})
				}
			}
		}
		return hits
	}
	norm := strings.TrimPrefix(test, "./")
	if i := strings.Index(norm, "::"); i > 0 { // pytest node IDs: keep the file
		norm = norm[:i]
	}
	for _, pred := range []func(string) bool{
		func(id string) bool { return id == test },
		func(id string) bool { return id == norm },
		func(id string) bool { return strings.HasSuffix(id, "/"+norm) },
	} {
		switch hits := find(pred); len(hits) {
		case 0:
			continue
		case 1:
			return hits[0].suite, hits[0].id, nil
		default:
			return "", "", fmt.Errorf("test %q is ambiguous (%d matches, e.g. %s %s)", test, len(hits), hits[0].suite, hits[0].id)
		}
	}
	if su == nil && suite != "" && suite != "*" {
		return "", "", fmt.Errorf("unknown suite %q and no test %q", suite, test)
	}
	return "", "", fmt.Errorf("no test %q in suite %q", test, suite)
}

// toDecision validates a submission against the selection: unknown tests are
// dropped (and recorded), duplicates removed, and no_impact restricted to orphans.
// When only some tests resolve, those are kept (the agent can only add). A decision
// that asks for tests but names none that exist is an error (see Decision.Check):
// accepting it would select nothing for the orphans, so the caller falls back to the
// orphan policy instead. The decision is returned with the error for its Dropped list.
func toDecision(sub *submission, req *Request) (*Decision, error) {
	d := &Decision{Summary: strings.TrimSpace(sub.Summary), RunAll: sub.Decision == decisionRunAll}
	inv := newInventory(req.Selection)
	seen := map[[2]string]bool{}
	// Tests listed with decision "none" are still added: the agent can only add, and
	// an explicit test is stronger evidence than the decision label.
	for _, t := range sub.Tests {
		suite, id, err := inv.resolve(t.Suite, t.Test)
		if err != nil {
			d.Dropped = append(d.Dropped, fmt.Sprintf("%s %s: %v", t.Suite, t.Test, err))
			continue
		}
		key := [2]string{suite, id}
		if seen[key] {
			continue
		}
		seen[key] = true
		why := strings.TrimSpace(t.Why)
		if why == "" {
			why = "possibly affected by an orphan change"
		}
		d.Add = append(d.Add, selector.Addition{Suite: suite, Test: id, Reason: selector.ReasonAgent, Detail: why})
	}
	orphans := map[string]bool{}
	for _, o := range req.Orphans {
		orphans[o.Path] = true
	}
	noImpact := map[string]bool{}
	for _, p := range sub.NoImpact {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		if orphans[p] {
			noImpact[p] = true
		}
	}
	for p := range noImpact {
		d.NoImpact = append(d.NoImpact, p)
	}
	sort.Strings(d.NoImpact)
	if !d.RunAll && len(d.Add) == 0 && sub.Decision == decisionTests && len(sub.Tests) == 0 {
		return d, errors.New(`decision "tests" lists no test`)
	}
	return d, d.Check()
}

// parseCommandOutput extracts the answer from an external agent's stdout. A JSON
// envelope with a "final_output" (fx --json) or "result" (claude -p --output-format
// json) field is unwrapped first; then the last balanced JSON object with a top-level
// "decision" key is used, wherever it appears (bare, or inside markdown fences).
func parseCommandOutput(out []byte) (*submission, Usage, error) {
	text := strings.TrimSpace(string(out))
	var usage Usage
	var env map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &env) == nil {
		if _, direct := env["decision"]; !direct {
			usage = envelopeUsage(env)
			for _, k := range []string{"final_output", "result"} {
				raw, ok := env[k]
				if !ok {
					continue
				}
				var s string
				if json.Unmarshal(raw, &s) == nil {
					text = s
				} else {
					text = string(raw)
				}
				break
			}
		}
	}
	obj, ok := lastObjectWithKey(text, "decision")
	if !ok {
		return nil, usage, errors.New("no JSON object with a \"decision\" field in the agent output")
	}
	sub, err := parseSubmission(obj)
	return sub, usage, err
}

// envelopeUsage reads token usage from a claude -p style JSON envelope.
func envelopeUsage(env map[string]json.RawMessage) Usage {
	var u struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	}
	var turns int
	var cost float64
	if raw, ok := env["usage"]; ok {
		_ = json.Unmarshal(raw, &u)
	}
	if raw, ok := env["num_turns"]; ok {
		_ = json.Unmarshal(raw, &turns)
	}
	if raw, ok := env["total_cost_usd"]; ok {
		_ = json.Unmarshal(raw, &cost)
	}
	return Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		Turns:            turns,
		CostUSD:          cost,
	}
}

// lastObjectWithKey returns the last JSON object in s (scanning object starts from
// the end) that decodes and has key at its top level.
func lastObjectWithKey(s, key string) ([]byte, bool) {
	for i := strings.LastIndexByte(s, '{'); i >= 0; i = strings.LastIndexByte(s[:i], '{') {
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var obj map[string]json.RawMessage
		if err := dec.Decode(&obj); err != nil {
			continue
		}
		if _, ok := obj[key]; ok {
			return []byte(s[i : i+int(dec.InputOffset())]), true
		}
	}
	return nil, false
}
