package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/summary"
)

// flakeReport is one flip seen in this run (run log, --report, step summary, outputs).
type flakeReport struct {
	Suite string `json:"suite"`
	Unit  string `json:"unit"`
	Flip  string `json:"flip"`
	Count int    `json:"count"`
	// Retried: a known flaky unit failed, passed when run again, and was not counted
	// as a failure (flaky.retry).
	Retried bool `json:"retried,omitempty"`
}

func flakeReports(suite string, events []selector.FlakeEvent) []flakeReport {
	var out []flakeReport
	for _, e := range events {
		out = append(out, flakeReport{Suite: suite, Unit: e.Unit, Flip: e.Flip, Count: e.Count, Retried: e.Retried})
	}
	return out
}

func (f flakeReport) String() string {
	s := fmt.Sprintf("%s: %s (%s", f.Suite, f.Unit, f.Flip)
	if f.Retried {
		s += " on retry; not counted as a failure"
	}
	return s + fmt.Sprintf(", %d flip(s) recorded)", f.Count)
}

func logFlakes(p *project, w io.Writer, flakes []flakeReport) {
	if len(flakes) == 0 {
		return
	}
	p.logf("FLAKY: %d test unit(s) changed outcome although none of their tracked inputs (files and environment variables) changed (flaky, or they depend on something smates cannot see: time, network, environment variables in env_ignore, files outside the repository); see `smates flaky`:", len(flakes))
	for _, f := range flakes {
		fmt.Fprintf(w, "  - %s\n", f)
	}
}

// flakyOutputs are the GitHub outputs for flakes seen in this run.
func flakyOutputs(flakes []flakeReport) []string {
	ids := []string{}
	for _, f := range flakes {
		ids = append(ids, f.Suite+": "+f.Unit)
	}
	j, _ := json.Marshal(ids)
	return []string{fmt.Sprintf("flaky=%d", len(flakes)), "flaky-tests=" + string(j)}
}

// flakySection is the flakes of this run as part of the Markdown summary (ok is false
// without flakes).
func flakySection(flakes []flakeReport) (sec summary.Section, ok bool) {
	if len(flakes) == 0 {
		return sec, false
	}
	var b strings.Builder
	b.WriteString("Outcome changed although no tracked input (file or environment variable) changed:\n")
	for _, f := range flakes {
		fmt.Fprintf(&b, "- `%s` — %s", f.Suite+": "+f.Unit, f.Flip)
		if f.Retried {
			b.WriteString(" on retry (not counted as a failure)")
		}
		fmt.Fprintf(&b, ", %d flip(s) recorded\n", f.Count)
	}
	return summary.Section{Title: fmt.Sprintf(":game_die: %d flaky test unit(s)", len(flakes)), Body: b.String()}, true
}

// retryFlaky re-runs a suite run whose failures are all units already known to be flaky
// (flaky.retry). It does nothing unless every failure is such a unit, reported for the
// unit itself: any other failure fails the run as usual. The retry runs the same units
// as the failed run, not only the failing ones: a run can also fail for reasons no unit
// reports (an unhandled error, teardown, a crash), and only a whole re-run shows they
// are gone. It returns the result with the retries' observations and exit code, and
// the first failed attempts of the units that failed (for Record).
func retryFlaky(ctx context.Context, p *project, rc *runner.RunContext, sr *selector.Suite, r *runner.RunResult) (*runner.RunResult, map[string]*runner.Observation, error) {
	if p.cfg.Flaky.Retry <= 0 || r.ExitCode == 0 || len(r.Observations) == 0 {
		return r, nil, nil
	}
	known := p.state.Suites[sr.Name]
	// flakyFailures lists the failing units of a failed run, ok only when every one of
	// them is known to be flaky.
	flakyFailures := func(res *runner.RunResult) (failing []string, ok bool) {
		for _, u := range rc.Units {
			o := res.Observations[u]
			if o == nil {
				return nil, false // no outcome for a unit of a failed run: not explained by flakes
			}
			if o.Status == state.Pass {
				continue
			}
			if o.Status != state.Fail || o.Inferred || known == nil || known.Tests[u] == nil || known.Tests[u].Flaky == nil {
				return nil, false
			}
			failing = append(failing, u)
		}
		// No failing unit: the run failed for another reason (teardown, coverage, crash).
		return failing, len(failing) > 0
	}
	failing, ok := flakyFailures(r)
	if !ok {
		return r, nil, nil
	}
	first := map[string]*runner.Observation{}
	for n := 1; n <= p.cfg.Flaky.Retry; n++ {
		sort.Strings(failing)
		for _, u := range failing {
			if first[u] == nil {
				first[u] = r.Observations[u]
			}
		}
		p.logf("%s: retrying after %d known flaky test unit(s) failed (attempt %d of %d): %s", sr.Name, len(failing), n, p.cfg.Flaky.Retry, strings.Join(limit(failing, 8), ", "))
		again := *rc
		if rc.TraceDir != "" {
			again.TraceDir = fmt.Sprintf("%s-retry%d", rc.TraceDir, n)
			if err := os.MkdirAll(again.TraceDir, 0o755); err != nil {
				return nil, nil, err
			}
		}
		r2, err := sr.Suite.Adapter.Run(ctx, &again)
		if err != nil {
			return nil, nil, fmt.Errorf("retry: %w", err)
		}
		r.Warnings = append(r.Warnings, r2.Warnings...)
		for by, keys := range r2.ReadsBy {
			if r.ReadsBy == nil {
				r.ReadsBy = map[string]map[string]bool{}
			}
			if r.ReadsBy[by] == nil {
				r.ReadsBy[by] = map[string]bool{}
			}
			for k := range keys {
				r.ReadsBy[by][k] = true
			}
		}
		for u, o := range r2.Observations {
			r.Observations[u] = o
		}
		r.ExitCode = r2.ExitCode
		if r.ExitCode == 0 {
			break
		}
		if failing, ok = flakyFailures(r2); !ok {
			break
		}
	}
	return r, first, nil
}

// ---- smates flaky ----

type flakyUnit struct {
	Suite  string    `json:"suite"`
	Unit   string    `json:"unit"`
	Count  int       `json:"count"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	Commit string    `json:"commit,omitempty"`
	Flip   string    `json:"flip,omitempty"`
	Status string    `json:"status"`
	Recent []string  `json:"recent"`
	// Env are the environment variables the unit read (tracked, so they did not change
	// across its flips; variables in env_ignore are not compared).
	Env []string `json:"env"`
}

func cmdFlaky(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("flaky", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	format := fs.String("format", "text", "output: text, json")
	clear := fs.Bool("clear", false, "forget the flake evidence of the given units (all when none is given)")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if *format != "text" && *format != "json" {
		return usageError{fmt.Errorf("unknown format %q", *format)}
	}
	if !*clear && fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected arguments %v (units are only taken with --clear)", fs.Args())}
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	suites := map[string]bool{}
	for _, s := range p.suites {
		suites[s.Name] = true
	}
	if *clear {
		only := map[string]bool{}
		for _, a := range fs.Args() {
			only[p.relArg(a)] = true
		}
		n := 0
		for name, su := range p.state.Suites {
			if !suites[name] {
				continue
			}
			for id, e := range su.Tests {
				if e.Flaky != nil && (len(only) == 0 || only[id]) {
					e.Flaky = nil
					n++
				}
			}
		}
		if n > 0 {
			if err := p.state.Save(p.statePath); err != nil {
				return err
			}
		}
		fmt.Fprintf(stderr, "smates: cleared flake evidence of %d test unit(s)\n", n)
		return nil
	}
	list := []flakyUnit{}
	for _, name := range slices.Sorted(maps.Keys(p.state.Suites)) {
		if !suites[name] {
			continue
		}
		su := p.state.Suites[name]
		for _, id := range slices.Sorted(maps.Keys(su.Tests)) {
			e := su.Tests[id]
			if e.Flaky == nil {
				continue
			}
			u := flakyUnit{Suite: name, Unit: id, Count: e.Flaky.Count, First: e.Flaky.First, Last: e.Flaky.Last, Commit: e.Flaky.Commit, Flip: e.Flaky.Flip, Status: e.Status, Recent: []string{}, Env: envDeps(e)}
			for _, o := range e.History {
				s := o.Status
				if o.Inferred {
					s += "?"
				}
				u.Recent = append(u.Recent, s)
			}
			list = append(list, u)
		}
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Flaky []flakyUnit `json:"flaky"`
		}{list})
	}
	if len(list) == 0 {
		fmt.Fprintln(stderr, "smates: no flaky test units recorded")
		return nil
	}
	fmt.Fprintln(stdout, "SUITE\tUNIT\tFLIPS\tLAST FLIP\tLAST SEEN\tCOMMIT\tRECENT (oldest first)\tENV READ")
	for _, u := range list {
		env := strings.Join(u.Env, ",")
		if env == "" {
			env = "-"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", u.Suite, u.Unit, u.Count, u.Flip, u.Last.Format(time.RFC3339), shortCommit(u.Commit), strings.Join(u.Recent, " "), env)
	}
	return nil
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	if c == "" {
		return "-"
	}
	return c
}
