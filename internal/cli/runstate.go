package cli

import (
	"fmt"
	"slices"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
)

// recordSuites writes the collected outcomes into the state. Units that did not run
// (--fail-fast) keep their entries, so they stay selected; a suite whose run covered
// all its tests replaces its entries only when every batch of it ran.
func (c *cycle) recordSuites() {
	p := c.p
	for _, sr := range c.sel.res.Suites {
		res := c.results[sr.Name]
		if res == nil {
			continue
		}
		full := res.full && len(c.notRun[sr.Name]) == 0
		if w := untrustedWarning(sr, res.obs); w != "" {
			p.logf("%s: %s", sr.Name, w)
		}
		events := selector.Record(p.state, sr, res.obs, c.hasher.ForSuite(sr.Suite), selector.RecordOptions{Commit: p.repo.Head(), Now: c.now, Full: full, ReadsBy: res.readsBy, Retried: res.retried, Unreliable: len(c.f.extra) > 0})
		c.flakes = append(c.flakes, flakeReports(sr.Name, events)...)
	}
}

// saveState updates the state's repository snapshot and saves the state.
func (c *cycle) saveState() error {
	p, res := c.p, c.sel.res
	picked, left := c.ranSuites()
	switch {
	case c.stopped || len(picked) == 0:
		// --fail-fast stopped the run (or no suite's runner could start): the
		// repository-wide snapshot stays as it was, so changes and orphans the tests
		// that did not run would have covered are seen again next time.
	case len(left) > 0 && (res.Mode == selector.ModeAll || c.f.shadow):
		// Every test of the suites --suite picked ran (a suite whose runner could not
		// start counts as left out). Files only they own get their hash; files suites
		// left out also own (or own alone) are marked as seen by the picked suites
		// (state.MarkerSnap), so the suites left out still find them changed on a
		// shared state (locally, after `state merge`, or in CI without a cache-key per
		// job). Commit and Created stay as they were on purpose: a partial refresh
		// must not win State.Merge over a full snapshot, and hooks judge staleness by
		// the last full run.
		if p.state.Files == nil {
			p.state.Files = map[string]string{}
		}
		own := selector.NewOwnership(picked, left)
		for f, h := range p.snap {
			p.recordSeen(own, f, h)
		}
		for f := range p.state.Files {
			if _, ok := p.snap[f]; !ok {
				p.recordSeen(own, f, "")
			}
		}
	case res.Mode == selector.ModeAll || c.f.shadow:
		p.state.Files = map[string]string(p.snap)
		p.state.Commit = p.repo.Head()
		p.state.Created = c.now
	default:
		c.markHandledOrphans(picked, left)
	}
	return c.save()
}

// ranSuites splits the suites of the run into those that ran (picked) and those
// --suite left out or whose runner could not start (left).
func (c *cycle) ranSuites() (picked, left []*runner.Suite) {
	p := c.p
	if len(c.unstarted) == 0 {
		return p.suites, p.others
	}
	left = slices.Clip(p.others)
	for _, s := range p.suites {
		if _, ok := c.unstarted[s.Name]; ok {
			left = append(left, s)
		} else {
			picked = append(picked, s)
		}
	}
	return picked, left
}

// markHandledOrphans records the orphans the run handled (by name, by the agent or by
// running the affected suites) with their current content, so the next run on this
// branch does not ask again. Other files stay as recorded; per-test entries track
// them. Orphans the policy was not applied to (a watch cycle's older changes, --only)
// stay unseen. With suites left out (ranSuites) an orphan is recorded as seen by the
// picked suites only, and only by those the snapshot already holds a view of
// (selector.Snapshotted): a marker naming a suite no full recording covered would make
// it count as snapshotted, and every marker it has not seen a change for it again. Such
// a suite is treated like one left out: an orphan only it owns may be asked about again.
func (c *cycle) markHandledOrphans(picked, left []*runner.Suite) {
	p, res := c.p, c.sel.res
	if len(p.state.Files) == 0 {
		return
	}
	if len(left) > 0 {
		snapshotted := selector.Snapshotted(p.state)
		var kept []*runner.Suite
		left = slices.Clone(left)
		for _, s := range picked {
			if snapshotted(s.Name) {
				kept = append(kept, s)
			} else {
				left = append(left, s)
			}
		}
		if picked = kept; len(picked) == 0 {
			return
		}
	}
	own := selector.NewOwnership(picked, left)
	for _, o := range res.Orphans {
		if _, handled := res.OrphanHandling[o]; !handled {
			continue
		}
		if len(left) > 0 {
			p.recordSeen(own, o, p.snap[o])
		} else if h, ok := p.snap[o]; ok {
			p.state.Files[o] = h
		} else {
			delete(p.state.Files, o)
		}
	}
}

// saveWithoutRun saves what the selection learned when no test runs (--dry-run, or a
// selection that runs nothing under skipEmpty): the agent's new answer, so the next
// run on the same change does not ask again (as `smates select` does), and, with
// markOrphans, the orphans handled (the orphan policy left no test to run for them).
// No outcome is recorded.
func (c *cycle) saveWithoutRun(markOrphans bool) error {
	if !c.record {
		return nil
	}
	fresh := c.sel.agentDec != nil && !c.sel.agentCache
	handled := markOrphans && len(c.sel.res.OrphanHandling) > 0 && len(c.p.state.Files) > 0
	if !fresh && !handled {
		return nil
	}
	if handled {
		c.markHandledOrphans(c.p.suites, c.p.others)
	}
	return c.save()
}

// save writes the state.
func (c *cycle) save() error {
	p := c.p
	p.state.Tool = "smates/" + Version
	if err := p.store.Save(p.state); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}

// recordSeen records in the snapshot that the suites of a --suite run (own's picked
// suites) saw f with hash h ("" = absent): the hash itself when no suite left out owns
// f, a marker otherwise (state.MarkerSnap). An entry still matching h (gitx.SameHash:
// a hash an older smates recorded without the executable marker) stays as it is, and
// so does the content hash of a file only suites left out own (they have not compared
// it yet).
func (p *project) recordSeen(own selector.Ownership, f, h string) {
	v, ok := p.state.Snap(f)
	switch {
	case own.OwnedOnly(f):
		p.state.SetSnap(f, state.HashSnap(h))
	case ok && !v.Unrecorded() && gitx.SameHash(v.String(), h):
	case ok && !v.Unrecorded() && !own.Owned(f):
	case !ok && h == "":
	default:
		// Once every suite that owns f has seen this content, a plain entry.
		p.state.SetSnap(f, v.MarkSeen(h, own.Picked()).CollapseIfComplete(own.Owners(f)))
	}
}
