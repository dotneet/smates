package cli

import (
	"reflect"
	"testing"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/hints"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

func TestSelectionReason(t *testing.T) {
	for _, c := range []struct {
		t    selector.Test
		want string
	}{
		{selector.Test{Reason: selector.ReasonChanged, Changed: []string{"$TZ"}}, "env changed: TZ"},
		{selector.Test{Reason: selector.ReasonChanged, Changed: []string{"src/a.ts", "$TZ", "$LANG"}}, "changed: src/a.ts; env changed: TZ, LANG"},
		{selector.Test{Reason: selector.ReasonChanged, Changed: []string{"a", "b", "c", "d"}}, "changed: a, b, c, … (+1)"},
		{selector.Test{Reason: selector.ReasonChanged, Changed: []string{"backend/api/user.go", "e2e/h.ts"},
			Via:     map[string]string{"backend/api/user.go": selector.ViaService, "e2e/h.ts": selector.ViaImport},
			Service: map[string]string{"backend/api/user.go": "depends_on suite go"}},
			"changed: backend/api/user.go (depends_on suite go), e2e/h.ts"},
		{selector.Test{Reason: selector.ReasonChanged, Changed: []string{"@set:suite:go"},
			Via:     map[string]string{"@set:suite:go": selector.ViaService},
			Service: map[string]string{"@set:suite:go": "depends_on suite go"}},
			"changed: code of depends_on suite go"},
		{selector.Test{Reason: selector.ReasonFailed, Detail: "last run: fail"}, "failed: last run: fail"},
		{selector.Test{Reason: selector.ReasonAgent, Detail: "touches the API client"}, "agent: touches the API client"},
		// A decision cached by an older version, whose details started with "agent: ".
		{selector.Test{Reason: selector.ReasonAgent, Detail: "agent: possibly affected by an orphan change"}, "agent: possibly affected by an orphan change"},
		{selector.Test{Reason: selector.ReasonReference, Detail: "references a.json"}, "reference: references a.json"},
		{selector.Test{Reason: selector.ReasonCaller, Detail: "added by caller"}, "caller: added by caller"},
		{selector.Test{Reason: selector.ReasonNew}, "new"},
	} {
		if got := selectionReason(&c.t); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.t, got, c.want)
		}
	}
}

// An agent decision to run everything is explained once as the agent's: not
// "agent: agent: <summary>".
func TestAgentRunAllReason(t *testing.T) {
	tt := &selector.Test{ID: "a_test.go"}
	res := &selector.Result{Suites: []*selector.Suite{{Name: "go", Tests: []*selector.Test{tt}}}}
	applyDecision(res, &agent.Decision{RunAll: true, Summary: "shared config changed"})
	if got := selectionReason(tt); got != "agent: shared config changed" {
		t.Errorf("got %q", got)
	}
}

func TestEnvDeps(t *testing.T) {
	e := &state.Entry{Deps: map[string]string{"src/a.ts": "h", "$TZ": "env:x", "$APP_MODE": "", "dir/": "d"}}
	if got := envDeps(e); !reflect.DeepEqual(got, []string{"APP_MODE", "TZ"}) {
		t.Errorf("%v", got)
	}
}

func TestCandidateNoteEnv(t *testing.T) {
	for change, want := range map[string]string{"modified": "changed", "added": "set", "deleted": "unset"} {
		got := candidateNote(hints.Candidate{Path: "$TZ", Kind: hints.KindEnv, Link: hints.LinkTraced, Change: change})
		if got != "environment variable, read in its last run, "+want {
			t.Errorf("%s: %q", change, got)
		}
	}
}
