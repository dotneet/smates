package selector

import (
	"fmt"
	"slices"
	"sort"
	"strings"

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
	s.neverRecordedOrphans()
	if len(res.LeftOut) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d unattributed change(s) belong only to suites left out by --suite: left to the jobs that run them", len(res.LeftOut)))
	}
}

// neverRecordedOrphans notes the suites of the run that have never been recorded
// while others have (markerFilter), and adds to the orphans the changes it dropped for
// them (unseen) that changed since Base, that no test explains and that one of them
// owns: the orphans diff mode would find for them. Without Base (in CI, or none could
// be resolved) their tests all run as new, so no change needs to reach them.
func (s *selection) neverRecordedOrphans() {
	in, res := s.in, s.res
	never := s.neverRecorded
	if len(never) == 0 {
		return
	}
	how := "their tests run as new: no base revision to compare with"
	if s.baseChanged != nil {
		how = "their tests are decided against the base revision by static analysis"
		own := NewOwnership(in.Suites, in.Others)
		ignore := IgnoreGlobs(in.Config)
		n := len(res.Orphans)
		for f := range s.unseen {
			if !s.baseChanged[f] || pathutil.IsVendored(f) || pathutil.Match(ignore, f) || res.Explains(f, in.Snap[f] == "") {
				continue
			}
			v, _ := in.State.Snap(f)
			if slices.ContainsFunc(own.Owners(f), func(name string) bool { return slices.Contains(never, name) && !v.Seen(name) }) {
				res.Orphans = append(res.Orphans, f)
			}
		}
		if len(res.Orphans) > n {
			sort.Strings(res.Orphans)
		}
	}
	names := never
	if len(names) > 5 {
		names = append(slices.Clip(names[:5]), fmt.Sprintf("… (+%d)", len(never)-5))
	}
	res.Notes = append(res.Notes, fmt.Sprintf("%d suite(s) have never been recorded in a full run (%s), and the changes only they have not seen are no change for the others: %s", len(never), how, strings.Join(names, ", ")))
}

// Explains reports whether some test's D(T) explains a change to f (see
// depkey.Covers): f is covered, or belongs to a depends_on/serves declaration some
// test depends on (also when f no longer exists, which Covered cannot tell).
func (r *Result) Explains(f string, existenceChanged bool) bool {
	// The index is built once Covered is complete, and again should it grow.
	if r.coverIndex == nil || r.coverIndex.Len() != len(r.Covered) {
		r.coverIndex = depkey.NewCoverIndex(r.Covered)
	}
	return r.coverIndex.Covers(f, existenceChanged) != depkey.CoverNone || slices.ContainsFunc(r.declared, func(d *ServiceDeps) bool { return d.match(f) })
}

// existedBefore reports whether f existed when the state was recorded (or at the base
// revision in diff mode; unknown counts as existing, which keeps f an orphan). A marker
// of --suite runs tells by the hash its suites saw (state.SnapValue.Existed).
func existedBefore(in *Input, f string) bool {
	if len(in.State.Files) > 0 {
		v, ok := in.State.Snap(f)
		return ok && v.Existed()
	}
	return true
}
