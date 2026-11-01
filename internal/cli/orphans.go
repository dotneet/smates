package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/selector"
)

// handleOrphans applies the orphan policy to the changed files no test depends on
// (res.Orphans): tests that name them are selected; what is left is ignored, handed to
// the agent or makes the affected suites run in full, as smates.yml says.
func (p *project) handleOrphans(ctx context.Context, sel *selection, opt selectOptions, base string) {
	res := sel.res
	if len(res.Orphans) == 0 && len(res.LeftOut) == 0 {
		return
	}

	// 1. Deterministic: tests that mention an orphan by name. Changes owned by the
	// suites --suite left out are searched too: a test of this run that names one is
	// selected, as in a run over every suite.
	refs, explained := res.References(p.graph, append(slices.Clip(res.Orphans), res.LeftOut...))
	res.Apply(refs)
	explained = intersect(explained, res.Orphans)
	res.HandleOrphans(explained, selector.OrphanReference)
	remaining := subtract(res.Orphans, explained)
	if opt.orphanScope != nil {
		var in []string
		for _, o := range remaining {
			if opt.orphanScope[o] {
				in = append(in, o)
			}
		}
		if n := len(remaining) - len(in); n > 0 {
			p.debugf("%d unattributed change(s) predate this cycle: orphan policy not applied to them", n)
		}
		remaining = in
	}
	if len(remaining) == 0 {
		return
	}

	// 2. Policy for what is left.
	policy := p.cfg.Orphans
	if policy == config.OrphansIgnore {
		res.Notes = append(res.Notes, fmt.Sprintf("ignored %d unattributed change(s) (orphans: ignore)", len(remaining)))
		res.HandleOrphans(remaining, selector.OrphanIgnored)
		return
	}
	if policy == config.OrphansAgent && !opt.noAgent && p.cfg.Agent.Enabled != config.AgentNever {
		ok, why := agent.Available(p.cfg.Agent)
		if ok {
			dec, cached, err := p.runAgent(ctx, res, remaining, base)
			if err == nil {
				sel.agentDec, sel.agentCache = dec, cached
				applyDecision(res, dec)
				if !dec.RunAll {
					res.HandleOrphans(intersect(remaining, dec.NoImpact), selector.OrphanNoImpact)
				}
				res.HandleOrphans(remaining, selector.OrphanAgent)
				return
			}
			sel.agentNote = "agent failed (" + err.Error() + "); running affected suites in full"
		} else {
			sel.agentNote = "agent unavailable (" + why + "); running affected suites in full"
		}
	}
	res.Apply(res.OrphanAll(remaining))
	res.HandleOrphans(remaining, selector.OrphanPolicy)
	return
}

// subtract returns the elements of a that are not in b.
func subtract(a, b []string) []string {
	drop := map[string]bool{}
	for _, x := range b {
		drop[x] = true
	}
	var out []string
	for _, x := range a {
		if !drop[x] {
			out = append(out, x)
		}
	}
	return out
}

// intersect returns the elements of a that are in b.
func intersect(a, b []string) []string {
	return subtract(a, subtract(a, b))
}

func applyDecision(res *selector.Result, dec *agent.Decision) {
	if dec.RunAll {
		var adds []selector.Addition
		for _, s := range res.Suites {
			adds = append(adds, selector.Addition{Suite: s.Name, Reason: selector.ReasonAgent, Detail: "agent: " + dec.Summary})
		}
		res.Apply(adds)
		return
	}
	res.Apply(dec.Add)
}

func (p *project) runAgent(ctx context.Context, res *selector.Result, orphans []string, base string) (*agent.Decision, bool, error) {
	req := &agent.Request{Repo: p.repo, Config: p.cfg.Agent, Selection: res, Log: io.Discard}
	if p.verbose {
		req.Log = p.log
	}
	for _, o := range orphans {
		old, hadOld := selector.SnapshotHash(p.state.Files, o)
		cur, has := p.snap[o]
		st := "modified"
		switch {
		case !has:
			st = "deleted"
		case !hadOld && len(p.state.Files) > 0:
			st = "added"
		}
		req.Orphans = append(req.Orphans, agent.Orphan{Path: o, Status: st, OldBlob: old, NewBlob: cur, Base: base})
	}
	key := agent.CacheKey(req)
	if raw, ok := p.state.AgentCache[key]; ok {
		var dec agent.Decision
		if err := json.Unmarshal(raw, &dec); err == nil {
			p.logf("agent: reusing cached decision for %d orphan file(s)", len(orphans))
			return &dec, true, nil
		}
	}
	p.logf("agent: asking %s about %d unattributed change(s): %s", agent.Describe(p.cfg.Agent), len(orphans), strings.Join(limit(orphans, 5), ", "))
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Agent.Timeout)
	defer cancel()
	start := time.Now()
	dec, err := agent.New(p.cfg.Agent).Decide(ctx, req)
	if err != nil {
		return nil, false, err
	}
	p.logf("agent: %s (%d turns, %d in / %d out tokens, %s)", oneLine(dec.Summary), dec.Usage.Turns, dec.Usage.InputTokens+dec.Usage.CacheReadTokens+dec.Usage.CacheWriteTokens, dec.Usage.OutputTokens, time.Since(start).Round(100*time.Millisecond))
	if raw, err := json.Marshal(dec); err == nil {
		p.state.AgentCache[key] = raw
	}
	return dec, false, nil
}
