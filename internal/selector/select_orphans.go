package selector

import (
	"fmt"
	"slices"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
)

// classifyOrphans sorts out the changes no test's D(T) explains (Explains), other
// than vendored and ignored files: they are orphans (Result.Orphans), or, in a
// --suite run, LeftOut when only suites the run left out own them (Ownership.Owned).
// Nothing is classified in ModeAll.
func (s *selection) classifyOrphans() {
	in, res := s.in, s.res
	if res.Mode == ModeAll {
		return
	}
	own := NewOwnership(in.Suites, in.Others)
	for _, f := range res.Changed {
		if pathutil.IsVendored(f) || pathutil.Match(IgnoreGlobs(in.Config), f) {
			continue
		}
		existenceChanged := in.Snap[f] == "" || !existedBefore(in, f)
		if !res.Explains(f, existenceChanged) {
			if len(in.Others) > 0 && !own.Owned(f) {
				res.LeftOut = append(res.LeftOut, f)
				continue
			}
			res.Orphans = append(res.Orphans, f)
		}
	}
	if len(res.LeftOut) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d unattributed change(s) belong only to suites left out by --suite: left to the jobs that run them", len(res.LeftOut)))
	}
}

// Explains reports whether some test's D(T) explains a change to f (see
// depkey.Covers): f is covered, or belongs to a depends_on/serves declaration some
// test depends on (also when f no longer exists, which Covered cannot tell).
func (r *Result) Explains(f string, existenceChanged bool) bool {
	return depkey.Covers(r.Covered, f, existenceChanged) != depkey.CoverNone || slices.ContainsFunc(r.declared, func(d *ServiceDeps) bool { return d.match(f) })
}

// existedBefore reports whether f existed when the state was recorded (or at the base
// revision in diff mode; unknown counts as existing, which keeps f an orphan).
func existedBefore(in *Input, f string) bool {
	if len(in.State.Files) > 0 {
		_, ok := in.State.FileHash(f)
		return ok
	}
	return true
}
