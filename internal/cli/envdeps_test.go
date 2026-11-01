package cli

import (
	"reflect"
	"testing"

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
		{selector.Test{Reason: selector.ReasonFailed, Detail: "last run: fail"}, "failed: last run: fail"},
		{selector.Test{Reason: selector.ReasonNew}, "new"},
	} {
		if got := selectionReason(&c.t); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.t, got, c.want)
		}
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
