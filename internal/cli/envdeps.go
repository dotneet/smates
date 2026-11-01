package cli

import (
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// selectionReason is what --explain prints for a selected test: its reason with the
// changed files ("changed: src/a.ts") and environment variables ("env changed: TZ").
func selectionReason(t *selector.Test) string {
	var files, env []string
	for _, c := range t.Changed {
		if kind, name := depkey.Parse(c); kind == depkey.KindEnv {
			env = append(env, name)
		} else {
			files = append(files, c)
		}
	}
	var parts []string
	if len(files) > 0 {
		parts = append(parts, t.Reason+": "+strings.Join(limit(files, 3), ", "))
	}
	if len(env) > 0 {
		parts = append(parts, "env changed: "+strings.Join(limit(env, 3), ", "))
	}
	switch {
	case len(parts) > 0:
		return strings.Join(parts, "; ")
	case t.Detail != "":
		return t.Reason + ": " + t.Detail
	}
	return t.Reason
}

// envDeps lists the environment variables an entry depends on.
func envDeps(e *state.Entry) []string {
	out := []string{}
	for k := range e.Deps {
		if kind, name := depkey.Parse(k); kind == depkey.KindEnv {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
