package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/hints"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/summary"
)

// Bounds of failure hints.
const (
	maxHints          = 100              // failing units with a hint (report)
	maxHintsShown     = 10               // in the log and the step summary
	maxCandidateShown = 5                // candidates per hint in the log
	maxExplainTimeout = 30 * time.Second // per explanation request
)

// failureHints computes failure-cause hints while `smates run` runs suites, and asks the
// agent to explain a few of them in the background.
type failureHints struct {
	ctx         context.Context
	p           *project
	res         *selector.Result
	hasher      *selector.Hasher
	flagBase    string
	hadSnapshot bool

	baseOnce sync.Once
	base     string

	list    []*hints.Hint
	omitted int

	explain  bool
	budget   int
	deferred []*explainJob
	topSeen  map[string]bool // top candidates of the explanations started first
	wg       sync.WaitGroup
	mu       sync.Mutex
	jobs     []*explainJob
}

type explainJob struct {
	hint *hints.Hint
	req  *agent.ExplainRequest
	key  string
	log  bytes.Buffer // the agent's progress (printed by wait, with -v)
	ex   *hints.Explanation
	err  error
}

func newFailureHints(ctx context.Context, p *project, res *selector.Result, hasher *selector.Hasher, base string, noAgent bool) *failureHints {
	fh := &failureHints{ctx: ctx, p: p, res: res, hasher: hasher, flagBase: base, hadSnapshot: len(p.state.Files) > 0}
	a := p.cfg.Agent
	if !noAgent && a.Enabled != config.AgentNever && config.Bool(a.ExplainFailures, true) && a.ExplainMax > 0 {
		if ok, why := agent.ExplainSupported(a); ok {
			fh.explain, fh.budget = true, a.ExplainMax
		} else {
			p.debugf("failure explanations off: %s", why)
		}
	}
	return fh
}

// baseRev is the revision changes are compared with when nothing is recorded.
func (fh *failureHints) baseRev() string {
	fh.baseOnce.Do(func() {
		fh.base = fh.flagBase
		if fh.base == "" && fh.res.Mode != selector.ModeAll {
			fh.base = autoBase(fh.p.ci, fh.p.repo)
		}
	})
	return fh.base
}

// add computes hints for the failing units of a suite that just ran. It must run
// before the suite's results are recorded (it compares with the previous entries).
func (fh *failureHints) add(sr *selector.Suite, obs map[string]*runner.Observation) {
	var failing []string
	for u, o := range obs {
		if o.Status == state.Fail {
			failing = append(failing, u)
		}
	}
	if len(failing) == 0 {
		return
	}
	sort.Strings(failing)
	var changed map[string]bool
	if fh.res.Mode != selector.ModeAll && (fh.hadSnapshot || fh.baseRev() != "") {
		changed = map[string]bool{}
		for _, f := range fh.res.Changed {
			changed[f] = true
		}
	}
	var orphans map[string]string
	if len(fh.res.Orphans) > 0 {
		orphans = map[string]string{}
		for _, o := range fh.res.Orphans {
			orphans[o], _ = fh.p.state.FileHash(o)
		}
	}
	st := fh.p.state.Suites[sr.Name]
	for _, u := range failing {
		if len(fh.list) == maxHints {
			fh.omitted++
			continue
		}
		o := obs[u]
		var entry *state.Entry
		if st != nil {
			entry = st.Tests[u]
		}
		files := unitFiles(sr, u)
		static := map[string]bool{}
		if fh.p.graph != nil && config.Bool(sr.Suite.Static, true) {
			for _, f := range fh.p.graph.Closure(files...) {
				static[f] = true
			}
		}
		sh := fh.hasher.ForSuite(sr.Suite)
		in := hints.Input{
			Suite: sr.Name, Unit: u, Inferred: o.Inferred, TestFiles: files,
			Deps: sr.ExpandSets(sr.Deps[u]), Observed: o.Files, Static: static, Inputs: sr.Inputs,
			Entry: entry, Changed: changed, Orphans: orphans, Hash: sh.Hash, Unchanged: sh.Unchanged, Root: fh.p.repo.Root,
		}
		if len(sr.Services) > 0 {
			in.Service = func(f string) bool { return sr.ServiceVia(u, f) != "" }
		}
		if fh.p.graph != nil {
			in.Direct = fh.p.graph.Direct
		}
		h := hints.Compute(in)
		h.Candidates = displayCandidates(h.Candidates)
		h.Output = hints.CleanOutput(o.Failure)
		fh.list = append(fh.list, h)
		fh.queue(sr, h, o.Failure)
	}
}

// queue starts (or defers) an agent explanation for a hint, reusing a cached answer.
func (fh *failureHints) queue(sr *selector.Suite, h *hints.Hint, output string) {
	if !fh.explain || (output == "" && len(h.Candidates) == 0) {
		return
	}
	req := &agent.ExplainRequest{Repo: fh.p.repo, Config: fh.p.cfg.Agent, Hint: h, Runner: sr.Suite.Runner, Output: output}
	job := &explainJob{hint: h, req: req, key: agent.ExplainCacheKey(req)}
	req.Log = &job.log
	if raw, ok := fh.p.state.AgentCache[job.key]; ok {
		var ex hints.Explanation
		if json.Unmarshal(raw, &ex) == nil && ex.Reason != "" {
			ex.Cached = true
			h.Explanation = &ex
			return
		}
	}
	// A failure inferred from a shared exit code may belong to another test, and one
	// whose most likely candidate is already being explained probably has the same
	// cause: those are explained only when budget is left at the end.
	top := ""
	if len(h.Candidates) > 0 {
		top = h.Candidates[0].Path
	}
	if h.Inferred || (top != "" && fh.topSeen[top]) {
		fh.deferred = append(fh.deferred, job)
		return
	}
	if top != "" {
		if fh.topSeen == nil {
			fh.topSeen = map[string]bool{}
		}
		fh.topSeen[top] = true
	}
	fh.start(job)
}

func (fh *failureHints) start(job *explainJob) {
	if fh.budget == 0 {
		return
	}
	fh.budget--
	job.req.Base = fh.baseRev()
	fh.jobs = append(fh.jobs, job)
	if len(fh.jobs) == 1 {
		fh.p.logf("agent: asking %s to explain failing tests in the background", agent.Describe(fh.p.cfg.Agent))
	}
	fh.wg.Add(1)
	go func() {
		defer fh.wg.Done()
		ctx, cancel := context.WithTimeout(fh.ctx, min(fh.p.cfg.Agent.Timeout, maxExplainTimeout))
		defer cancel()
		ex, _, err := agent.Explain(ctx, job.req)
		fh.mu.Lock()
		job.ex, job.err = ex, err
		fh.mu.Unlock()
	}()
}

// wait collects the explanations (bounded by the request timeout) and caches them in
// the state. Agent errors are reported, never fatal.
func (fh *failureHints) wait() {
	for _, job := range fh.deferred {
		fh.start(job)
	}
	fh.deferred = nil
	if len(fh.jobs) == 0 {
		return
	}
	fh.wg.Wait()
	var errs []string
	for _, job := range fh.jobs {
		if fh.p.verbose {
			fh.p.log.Write(job.log.Bytes())
		}
		if job.err != nil {
			errs = append(errs, job.err.Error())
			continue
		}
		job.hint.Explanation = job.ex
		if raw, err := json.Marshal(job.ex); err == nil {
			fh.p.state.AgentCache[job.key] = raw
		}
	}
	if len(errs) > 0 {
		fh.p.logf("agent: could not explain %d failure(s): %s", len(errs), oneLine(errs[0]))
	}
	fh.jobs = nil
}

// all returns the computed hints (never nil).
func (fh *failureHints) all() []*hints.Hint {
	if fh.list == nil {
		return []*hints.Hint{}
	}
	return fh.list
}

// print writes the hints to the run log.
func (fh *failureHints) print(w io.Writer) {
	if len(fh.list) == 0 {
		return
	}
	fh.p.logf("failure hints for %d failing test unit(s):", len(fh.list)+fh.omitted)
	for i, h := range fh.list {
		if i == maxHintsShown {
			fmt.Fprintf(w, "  … %d more in the --report output\n", len(fh.list)+fh.omitted-i)
			break
		}
		fmt.Fprintf(w, "  %s: %s — %s\n", h.Suite, h.Test, hintSummary(h))
		for j, c := range h.Candidates {
			if j == maxCandidateShown {
				break
			}
			fmt.Fprintf(w, "      %s  (%s)\n", c.Path, candidateNote(c))
		}
		if more := len(h.Candidates) - min(len(h.Candidates), maxCandidateShown) + h.More; more > 0 {
			fmt.Fprintf(w, "      … and %d more\n", more)
		}
		if ex := h.Explanation; ex != nil {
			fmt.Fprintf(w, "    → %s\n", explanationLine(ex))
		}
	}
}

// section is the hints' part of the Markdown summary (ok is false without hints).
func (fh *failureHints) section() (sec summary.Section, ok bool) {
	if len(fh.list) == 0 {
		return sec, false
	}
	var b strings.Builder
	for i, h := range fh.list {
		if i == maxHintsShown {
			fmt.Fprintf(&b, "- … %d more (see the JSON report)\n", len(fh.list)+fh.omitted-i)
			break
		}
		fmt.Fprintf(&b, "- **`%s`** (%s) — %s\n", h.Test, h.Suite, mdEscape(hintSummary(h)))
		for j, c := range h.Candidates {
			if j == maxCandidateShown {
				fmt.Fprintf(&b, "  - … and %d more\n", len(h.Candidates)-j+h.More)
				break
			}
			fmt.Fprintf(&b, "  - `%s` (%s)\n", c.Path, candidateNote(c))
		}
		if ex := h.Explanation; ex != nil {
			fmt.Fprintf(&b, "  - :robot: %s\n", mdEscape(explanationLine(ex)))
		}
	}
	return summary.Section{Title: fmt.Sprintf("Failure hints (%d failing test unit(s))", len(fh.list)+fh.omitted), Body: b.String()}, true
}

// hintSummary is the one-line verdict of a hint.
func hintSummary(h *hints.Hint) string {
	var s string
	since := ""
	if h.Basis == hints.BasisLastRun {
		since = " since its last recorded run"
		if h.Since != "" {
			since += " (" + shortSHA(h.Since) + ", " + statusWord(h.LastStatus) + ")"
		}
	} else if h.Basis == hints.BasisDiff {
		since = " in this change"
	}
	switch h.Verdict {
	case hints.VerdictNoRecord:
		s = "no earlier run of this test is recorded to compare with (it may have been failing already)"
	case hints.VerdictUnattributed:
		s = fmt.Sprintf("none of its %d known inputs changed%s, but changed files not attributed to any test may affect it", h.Inputs, since)
	case hints.VerdictUnchanged:
		s = fmt.Sprintf("none of its %d known inputs changed%s: likely flaky, environmental, or caused by something tracing cannot see", h.Inputs, since)
		if h.LastStatus != "" && h.LastStatus != state.Pass {
			s = fmt.Sprintf("none of its %d known inputs changed%s, and it did not pass then either: the cause predates that run, or the test is flaky", h.Inputs, since)
		}
	default:
		s = fmt.Sprintf("%d of its %d known inputs changed%s", h.Changed, h.Inputs, since)
	}
	if h.Inferred {
		s += " [status inferred from the run's exit code: the failure may belong to another test]"
	}
	return s
}

func candidateNote(c hints.Candidate) string {
	if c.Kind == hints.KindEnv {
		change := map[string]string{"added": "set", "deleted": "unset"}[c.Change]
		if change == "" {
			change = "changed"
		}
		return "environment variable, read in its last run, " + change
	}
	if c.Link == hints.LinkUnattributed {
		return c.Kind + ", not attributed to any test, " + c.Change
	}
	return c.Kind + ", " + c.Link + ", " + c.Change
}

func explanationLine(ex *hints.Explanation) string {
	loc := ex.File
	if loc != "" && ex.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, ex.Line)
	}
	s := "agent"
	if ex.Confidence != "" {
		s += " (" + ex.Confidence + " confidence"
		if ex.Cached {
			s += ", cached"
		}
		s += ")"
	}
	s += ": "
	if loc != "" {
		s += loc + " — "
	}
	return s + ex.Reason
}

func statusWord(s string) string {
	switch s {
	case state.Pass:
		return "passed"
	case state.Fail:
		return "failed"
	case "":
		return "status unknown"
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// mdEscape keeps untrusted text (agent output) from opening markdown or HTML
// constructs, mentions or links in the step summary and PR comment.
func mdEscape(s string) string {
	return summary.Inline(strings.NewReplacer("\n", " ", "`", "'").Replace(s))
}

// displayCandidates shows the suite's settings key (depkey.Config) as the config
// file. Its hashes are settings fingerprints, not blobs: they are cleared so a diff
// uses the base revision, and a candidate for the file's own content wins over it.
func displayCandidates(in []hints.Candidate) []hints.Candidate {
	content := map[string]bool{}
	for _, c := range in {
		if depkey.Display(c.Path) == c.Path {
			content[c.Path] = true
		}
	}
	out := in[:0]
	for _, c := range in {
		if p := depkey.Display(c.Path); p != c.Path {
			if content[p] {
				continue
			}
			c.Path, c.Old, c.New = p, "", ""
			content[p] = true
		}
		out = append(out, c)
	}
	return out
}
