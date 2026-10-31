// Package summary renders a selection, and optionally the outcome of running it, as
// Markdown for pull request comments and CI job summaries. The output is deterministic
// and bounded for large repositories: long lists are truncated with counts and the
// per-file explanation is collapsible.
package summary

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/selector"
)

// Input is what a summary shows.
type Input struct {
	Result    *selector.Result
	Agent     *agent.Decision
	AgentNote string
	// Shard and Shards identify this job's shard of a sharded selection (0 when not
	// sharded). A sharded summary covers only the tests this shard owns: each shard's
	// recorded state only knows those.
	Shard, Shards int
	// Run is the outcome of running the selection (nil for a selection only).
	Run *Run
	// Sections are appended after the built-in ones (flaky tests, failure hints, ...).
	Sections []Section
}

// Run is the outcome of a run.
type Run struct {
	ExitCode int
	Elapsed  time.Duration
	Shadow   bool
	Failed   []string // "suite: test" of units that failed
	Escapes  []string // shadow mode: failed but would not have been selected
	Possible []string // shadow mode: unselected, failure only inferred
}

// Section is an extra block of the summary.
type Section struct {
	Title string
	Body  string // Markdown
	// Collapsed puts the body in a <details> block with the title as its summary.
	Collapsed bool
}

// Limits keep comments readable (and under GitHub's 65536-character limit) on big
// repositories.
const (
	maxFiles  = 30 // changed files explained
	maxTests  = 10 // tests listed per changed file or reason
	maxLines  = 80 // tests listed over all changed files (fewer per file when many changed)
	maxItems  = 20 // other lists (orphans, failures, escapes)
	openBelow = 20 // the explanation starts expanded when at most this many tests are selected
)

type hit struct {
	suite int
	test  *selector.Test
	via   string
}

type view struct {
	in     *Input
	res    *selector.Result
	suites []*selector.Suite
	scope  map[*selector.Suite][]*selector.Test // tests this summary covers, per suite
	multi  bool                                 // tests of more than one suite are selected
}

// Markdown renders the summary.
func Markdown(in *Input) string {
	v := &view{in: in, res: in.Result, scope: map[*selector.Suite][]*selector.Test{}}
	for _, s := range v.res.Suites {
		for _, t := range s.Tests {
			if in.Shards <= 1 || t.Shard == 0 || t.Shard == in.Shard {
				v.scope[s] = append(v.scope[s], t)
			}
		}
		if len(v.scope[s]) > 0 {
			v.suites = append(v.suites, s)
		}
	}
	affected := 0
	for _, s := range v.suites {
		if slices.ContainsFunc(v.scope[s], func(t *selector.Test) bool { return t.Selected }) {
			affected++
		}
	}
	v.multi = affected > 1

	var b strings.Builder
	v.header(&b)
	v.table(&b)
	v.run(&b)
	if v.res.Mode != selector.ModeAll {
		v.why(&b)
	}
	v.orphans(&b)
	v.agent(&b)
	v.notes(&b)
	for _, s := range in.Sections {
		if s.Collapsed {
			fmt.Fprintf(&b, "\n<details><summary>%s</summary>\n\n%s\n\n</details>\n", html(s.Title), strings.TrimSpace(s.Body))
		} else {
			fmt.Fprintf(&b, "\n**%s**\n\n%s\n", s.Title, strings.TrimSpace(s.Body))
		}
	}
	return b.String()
}

type counts struct {
	selected, total int
	savedMs         int64
	untimed         int // skipped tests without a recorded duration
}

func (v *view) count(s *selector.Suite) counts {
	var c counts
	for _, t := range v.scope[s] {
		c.total++
		switch {
		case t.Selected:
			c.selected++
		case t.Duration > 0:
			c.savedMs += t.Duration
		default:
			c.untimed++
		}
	}
	return c
}

func (v *view) totals() (c counts, suites int) {
	for _, s := range v.suites {
		sc := v.count(s)
		c.selected += sc.selected
		c.total += sc.total
		c.savedMs += sc.savedMs
		c.untimed += sc.untimed
		if sc.selected > 0 {
			suites++
		}
	}
	return
}

func (v *view) header(b *strings.Builder) {
	c, affected := v.totals()
	title := "smates"
	switch {
	case v.in.Shards > 1:
		title += fmt.Sprintf(" (shard %d/%d)", v.in.Shard, v.in.Shards)
	case v.in.Run != nil && v.in.Run.Shadow:
		title += " (shadow mode)"
	}
	fmt.Fprintf(b, "### %s: %d of %d test units selected\n\n", title, c.selected, c.total)
	skipped := c.total - c.selected
	switch {
	case v.res.Mode == selector.ModeAll:
		b.WriteString("**Full run:** every test unit runs (default branch refresh, `--all`, or no recorded state).\n")
	case c.selected == 0:
		fmt.Fprintf(b, "**No test is affected by this change**%s.\n", saved(c))
	default:
		fmt.Fprintf(b, "This change affects **%s in %s**; %d skipped%s.\n",
			plural(c.selected, "test unit"), ofSuites(affected, len(v.suites)), skipped, saved(c))
	}
	if v.res.Mode == selector.ModeDiff {
		b.WriteString("\n_No recorded state yet: selected by static analysis against the base revision._\n")
	}
	if v.in.Shards > 1 {
		fmt.Fprintf(b, "\n_Sharded %d ways: this summary covers the %d test units shard %d owns._\n", v.in.Shards, c.total, v.in.Shard)
	}
}

func ofSuites(n, of int) string {
	if n == of {
		return plural(n, "suite")
	}
	return fmt.Sprintf("%d of %d suites", n, of)
}

func saved(c counts) string {
	switch {
	case c.savedMs > 0 && c.untimed > 0:
		return fmt.Sprintf(", saving about **%s** of recorded test time (%d skipped without a recorded duration)", dur(c.savedMs), c.untimed)
	case c.savedMs > 0:
		return fmt.Sprintf(", saving about **%s** of recorded test time", dur(c.savedMs))
	}
	return ""
}

func (v *view) table(b *strings.Builder) {
	if len(v.suites) == 0 {
		return
	}
	b.WriteString("\n| suite | runner | selected | skipped | time saved |\n|---|---|---:|---:|---:|\n")
	for _, s := range v.suites {
		c := v.count(s)
		ts := "–"
		if c.savedMs > 0 {
			ts = dur(c.savedMs)
		}
		fmt.Fprintf(b, "| %s | %s | %d / %d | %d | %s |\n", cell(s.Name), cell(runnerOf(s)), c.selected, c.total, c.total-c.selected, ts)
	}
}

func runnerOf(s *selector.Suite) string {
	if s.Suite == nil {
		return ""
	}
	return s.Suite.Runner
}

func (v *view) run(b *strings.Builder) {
	r := v.in.Run
	if r == nil {
		return
	}
	b.WriteString("\n")
	if r.ExitCode == 0 {
		fmt.Fprintf(b, "**Result:** ✅ passed in %s.\n", dur(r.Elapsed.Milliseconds()))
	} else {
		fmt.Fprintf(b, "**Result:** ❌ exit code %d after %s", r.ExitCode, dur(r.Elapsed.Milliseconds()))
		if len(r.Failed) > 0 {
			fmt.Fprintf(b, "; %s failed:\n", plural(len(r.Failed), "test unit"))
			list(b, r.Failed, maxItems)
		} else {
			b.WriteString(".\n")
		}
	}
	if !r.Shadow {
		return
	}
	if len(r.Escapes) > 0 {
		fmt.Fprintf(b, "\n> [!WARNING]\n> **Shadow mode:** %s failed but would **not** have been selected:\n", plural(len(r.Escapes), "test unit"))
		for _, e := range limit(r.Escapes, maxItems) {
			fmt.Fprintf(b, "> - %s\n", e)
		}
	} else {
		b.WriteString("\n**Shadow mode:** ✅ every failing test unit was selected.\n")
	}
	if len(r.Possible) > 0 {
		fmt.Fprintf(b, "\n%s not selected failed only by inference (the runner reports no per-file status):\n", plural(len(r.Possible), "test unit"))
		list(b, r.Possible, maxItems)
	}
}

// why explains the selection per changed file, then per other reason.
func (v *view) why(b *strings.Builder) {
	byFile := map[string][]hit{}
	byReason := map[string][]hit{}
	selected := 0
	for si, s := range v.suites {
		for _, t := range v.scope[s] {
			if !t.Selected {
				continue
			}
			selected++
			if len(t.Changed) == 0 {
				byReason[t.Reason] = append(byReason[t.Reason], hit{si, t, text(t.Detail)})
				continue
			}
			// Files themselves first: a file both read and listed was read.
			changed := slices.Clone(t.Changed)
			sort.SliceStable(changed, func(i, j int) bool { return !depkey.IsPseudo(changed[i]) && depkey.IsPseudo(changed[j]) })
			for _, c := range changed {
				for _, f := range v.expand(c) {
					if !slices.ContainsFunc(byFile[f], func(h hit) bool { return h.test == t && h.suite == si }) {
						byFile[f] = append(byFile[f], hit{si, t, viaLabel(t.Via[c], c)})
					}
				}
			}
		}
	}
	if selected == 0 {
		return
	}
	files := slices.Sorted(maps.Keys(byFile))
	reasons := slices.Sorted(maps.Keys(byReason))
	sort.SliceStable(reasons, func(i, j int) bool { return reasonRank(reasons[i]) < reasonRank(reasons[j]) })

	open := ""
	if selected <= openBelow {
		open = " open"
	}
	perFile := maxTests
	if len(files) > 0 {
		perFile = max(3, min(maxTests, maxLines/min(len(files), maxFiles)))
	}
	var parts []string
	if len(files) > 0 {
		parts = append(parts, plural(len(files), "changed file"))
	}
	if len(reasons) > 0 {
		parts = append(parts, plural(len(reasons), "other reason"))
	}
	these := "these " + plural(selected, "test unit")
	if selected == 1 {
		these = "this test unit"
	}
	fmt.Fprintf(b, "\n<details%s><summary>Why %s (%s)</summary>\n", open, these, strings.Join(parts, ", "))
	for i, f := range files {
		if i == maxFiles {
			rest := map[*selector.Test]bool{}
			for _, g := range files[i:] {
				for _, h := range byFile[g] {
					rest[h.test] = true
				}
			}
			fmt.Fprintf(b, "\n… and %d more changed files (%s)\n", len(files)-i, plural(len(rest), "test unit"))
			break
		}
		title := code(f)
		if depkey.KindOf(f) == depkey.KindListing {
			title += " (files added or removed)"
		}
		v.group(b, title, byFile[f], perFile)
	}
	for _, r := range reasons {
		v.group(b, reasonLabel(r), byReason[r], 0)
	}
	b.WriteString("\n</details>\n")
}

// group writes one changed file (perFile > 0: at most that many tests) or one reason,
// and the tests it selected.
func (v *view) group(b *strings.Builder, title string, hits []hit, perFile int) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].suite != hits[j].suite {
			return hits[i].suite < hits[j].suite
		}
		return hits[i].test.ID < hits[j].test.ID
	})
	// A file every test of a suite depends on (a lockfile, a runner config) is shown as
	// the suite, not as a list of all its tests.
	n := maxTests
	if perFile > 0 {
		n = perFile
		per := map[int][]hit{}
		for _, h := range hits {
			per[h.suite] = append(per[h.suite], h)
		}
		var whole []string
		var rest []hit
		for si, s := range v.suites {
			hs := per[si]
			switch {
			case len(hs) == 0:
			case commonVia(hs) == orphanVia:
				// Tests already selected for other reasons are not counted again.
				whole = append(whole, fmt.Sprintf("all of %s (orphan policy; %s not selected for other reasons)", code(s.Name), plural(len(hs), "test unit")))
			case len(hs) > 3 && len(hs) == len(v.scope[s]):
				whole = append(whole, fmt.Sprintf("all %s of %s%s", plural(len(hs), "test unit"), code(s.Name), suffix(commonVia(hs))))
			default:
				rest = append(rest, hs...)
			}
		}
		if len(whole) == 1 && len(rest) == 0 {
			fmt.Fprintf(b, "\n**%s** → %s\n", title, whole[0])
			return
		}
		fmt.Fprintf(b, "\n**%s** → %s\n", title, plural(len(hits), "test unit"))
		for _, w := range whole {
			fmt.Fprintf(b, "- %s\n", w)
		}
		hits = rest
	} else {
		fmt.Fprintf(b, "\n**%s** → %s\n", title, plural(len(hits), "test unit"))
	}
	for i, h := range hits {
		if i == n {
			fmt.Fprintf(b, "- … and %d more\n", len(hits)-i)
			break
		}
		where := ""
		if v.multi {
			where = " (" + html(v.suites[h.suite].Name) + ")"
		}
		fmt.Fprintf(b, "- %s%s%s\n", code(h.test.ID), where, suffix(h.via))
	}
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return " — " + s
}

func commonVia(hs []hit) string {
	for _, h := range hs[1:] {
		if h.via != hs[0].via {
			return ""
		}
	}
	return hs[0].via
}

// expand maps a dependency key to the changed files it stands for: an existence check
// to the file, a command run below a directory to the changed files under it. A
// directory listing stays as it is (it changed because files were added or removed,
// which the changed files alone do not tell apart from edits).
func (v *view) expand(key string) []string {
	kind, p := depkey.Parse(key)
	switch kind {
	case depkey.KindExistence:
		return []string{p}
	case depkey.KindContent, depkey.KindListing:
		return []string{key}
	}
	prefix := p + "/"
	var out []string
	for _, f := range v.res.Changed {
		if strings.HasPrefix(f, prefix) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return []string{key}
	}
	return out
}

const orphanVia = "orphan policy: suite runs in full"

func viaLabel(via, key string) string {
	switch kind, p := depkey.Parse(key); kind {
	case depkey.KindTree:
		return "an untraced command ran under " + code(p+"/")
	case depkey.KindListing:
		return "lists the directory"
	case depkey.KindExistence:
		return "checks whether it exists"
	}
	switch via {
	case selector.ViaTest:
		return "the test itself"
	case selector.ViaImport:
		return "static dependency"
	case selector.ViaInput:
		return "suite input"
	case selector.ViaTraced:
		return "read it in its last run (traced)"
	case selector.ViaReference:
		return "names it"
	case selector.ViaOrphan:
		return orphanVia
	}
	return via
}

func reasonRank(r string) int {
	switch r {
	case selector.ReasonFailed:
		return 0
	case selector.ReasonNew:
		return 1
	case selector.ReasonAgent:
		return 2
	}
	return 3
}

func reasonLabel(r string) string {
	switch r {
	case selector.ReasonFailed:
		return "Failed or unfinished last time"
	case selector.ReasonNew:
		return "Never recorded"
	case selector.ReasonAgent:
		return "Selected by the agent"
	case selector.ReasonReference:
		return "Names a changed file"
	case selector.ReasonOrphan:
		return "Orphan policy"
	case selector.ReasonDiff:
		return "Changed since the base revision"
	}
	return html(r)
}

func (v *view) orphans(b *strings.Builder) {
	if len(v.res.Orphans) == 0 {
		return
	}
	title := fmt.Sprintf("Changes no test depends on (%d)", len(v.res.Orphans))
	if len(v.res.Orphans) > 5 {
		fmt.Fprintf(b, "\n<details><summary>%s</summary>\n\n", title)
		defer b.WriteString("\n</details>\n")
	} else {
		fmt.Fprintf(b, "\n**%s:**\n", title)
	}
	for i, o := range v.res.Orphans {
		if i == maxItems {
			fmt.Fprintf(b, "- … and %d more\n", len(v.res.Orphans)-i)
			break
		}
		fmt.Fprintf(b, "- %s — %s\n", code(o), v.handling(o))
	}
}

func (v *view) handling(o string) string {
	var tests int
	var suites []string
	for _, s := range v.suites {
		in := false
		for _, t := range v.scope[s] {
			if t.Selected && slices.Contains(t.Changed, o) {
				tests++
				in = true
			}
		}
		if in {
			suites = append(suites, code(s.Name))
		}
	}
	switch v.res.OrphanHandling[o] {
	case selector.OrphanReference:
		return fmt.Sprintf("named by tests: selected %s", plural(tests, "test unit"))
	case selector.OrphanNoImpact:
		return "the agent found no affected test"
	case selector.OrphanAgent:
		return "left to the agent (below)"
	case selector.OrphanPolicy:
		if len(suites) == 0 {
			return "orphan policy: the suites it may affect run in full"
		}
		if len(suites) > 5 {
			suites = append(suites[:5], fmt.Sprintf("%d more suites", len(suites)-5))
		}
		return "orphan policy: runs " + strings.Join(suites, ", ") + " in full"
	case selector.OrphanIgnored:
		return "ignored (`orphans: ignore`)"
	}
	return "not handled"
}

func (v *view) agent(b *strings.Builder) {
	if d := v.in.Agent; d != nil {
		fmt.Fprintf(b, "\n**Agent** (%s): %s\n", code(d.Model), text(d.Summary))
	}
	if v.in.AgentNote != "" {
		fmt.Fprintf(b, "\n_%s_\n", text(v.in.AgentNote))
	}
}

func (v *view) notes(b *strings.Builder) {
	if len(v.res.Notes) == 0 {
		return
	}
	b.WriteString("\n")
	for _, n := range v.res.Notes {
		fmt.Fprintf(b, "> %s\n", text(n))
	}
}

func list(b *strings.Builder, items []string, n int) {
	for _, it := range limit(items, n) {
		fmt.Fprintf(b, "- %s\n", it)
	}
}

// limit formats the first n items as code and counts the rest.
func limit(items []string, n int) []string {
	var out []string
	for i, it := range items {
		if i == n {
			out = append(out, fmt.Sprintf("… and %d more", len(items)-i))
			break
		}
		out = append(out, code(it))
	}
	return out
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// dur formats a duration in milliseconds compactly.
func dur(ms int64) string {
	switch {
	case ms < 50:
		return "<0.1s"
	case ms < 1000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 60_000:
		return fmt.Sprintf("%ds", ms/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm %ds", ms/60_000, ms%60_000/1000)
	}
	return fmt.Sprintf("%dh %dm", ms/3_600_000, ms%3_600_000/60_000)
}

// code formats a path or name as inline code.
func code(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if strings.Contains(s, "`") {
		return "`` " + s + " ``"
	}
	return "`" + s + "`"
}

// text makes free text (agent answers, notes) safe to embed: one line, no HTML, no
// @-mentions.
func text(s string) string {
	return strings.ReplaceAll(html(oneLine(s)), "@", "@​")
}

func cell(s string) string { return strings.ReplaceAll(html(s), "|", `\|`) }

func html(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:297]) + "..."
	}
	return s
}
