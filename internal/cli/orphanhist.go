package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/selector"
)

// Kinds of orphan files in `smates orphans` (orphanFile.Kind).
const (
	orphanKindIgnore    = "ignore"    // nothing reads it: an ignore glob is suggested
	orphanKindInputs    = "inputs"    // it may change how tests run: inputs/rules, not ignore
	orphanKindReference = "reference" // a test names it, which explains it without ignore
	orphanKindUntested  = "untested"  // source code no test reaches: skipped (untested: skip)
)

// orphanReport is what `smates orphans` prints (the JSON schema of --format json).
type orphanReport struct {
	Commits int    `json:"commits"`
	Since   string `json:"since,omitempty"`
	// RecordedState is false when no run was recorded: dependencies then come from
	// static analysis, inputs and rules only, and the report is pessimistic.
	RecordedState bool     `json:"recorded_state"`
	Warnings      []string `json:"warnings"`
	Policy        string   `json:"orphan_policy"`
	// CommitsWithOrphans had a changed file no test explains (references included in
	// the explanations), so a run on them would have applied the orphan policy.
	CommitsWithOrphans int `json:"commits_with_orphans"`
	// CommitsWithOrphansAfterIgnore would still have one with the suggested ignore globs.
	CommitsWithOrphansAfterIgnore int `json:"commits_with_orphans_after_ignore"`
	// CommitsWithOrphansAfterInputs would still have one when, in addition, the files of
	// kind "inputs" were declared as inputs or rules.
	CommitsWithOrphansAfterInputs int `json:"commits_with_orphans_after_inputs"`
	// Files are the orphans ranked by the number of commits they were one in.
	Files []orphanFile `json:"files"`
	// Ignore are the suggested ignore globs; Inputs the files of kind "inputs".
	Ignore []orphanGlob `json:"ignore"`
	Inputs []orphanFile `json:"inputs"`
	// History lists the commits with orphans (or orphans explained by reference).
	History []orphanCommit `json:"history"`
	// ExistingIgnore is the ignore list of smates.yml today.
	ExistingIgnore []string `json:"existing_ignore"`
	// Applied is what --apply added to smates.yml (absent without --apply).
	Applied *configEdit `json:"applied,omitempty"`
}

// orphanFile is a changed file no test depends on, with the number of commits it was an
// orphan in.
type orphanFile struct {
	Path    string `json:"path"`
	Commits int    `json:"commits"`
	Kind    string `json:"kind"`
	// Why says what kind of execution-relevant file it looks like (kind "inputs").
	Why string `json:"why,omitempty"`
	// Glob is the suggested ignore glob covering it (kind "ignore").
	Glob string `json:"glob,omitempty"`
}

// orphanGlob is a suggested ignore glob.
type orphanGlob struct {
	Glob    string   `json:"glob"`
	Files   []string `json:"files"`
	Commits int      `json:"commits"`
}

// orphanCommit is a commit with orphans.
type orphanCommit struct {
	Commit  string   `json:"commit"`
	Subject string   `json:"subject"`
	Orphans []string `json:"orphans"`
	// Referenced are orphans explained because a test names them.
	Referenced []string `json:"referenced,omitempty"`
}

func cmdOrphans(args []string, stdout, stderr io.Writer) error {
	c := newCommand("orphans", stderr)
	var cf commonFlags
	c.common(&cf)
	history := c.Int("history", 100, "replay the last `n` first-parent commits (0: no limit)")
	since := c.String("since", "", "replay the first-parent commits after `rev` (no count limit unless --history is set)")
	c.withFormat("text", "json")
	apply := c.Bool("apply", false, "add the suggested ignore globs to smates.yml (and nothing else)")
	dryRun := c.Bool("dry-run", false, "with --apply: print the diff instead of writing")
	if err := c.parse(args); err != nil {
		return err
	}
	if err := c.noArgs(); err != nil {
		return err
	}
	if *dryRun && !*apply {
		return usageError{errors.New("--dry-run needs --apply")}
	}
	if *history < 0 {
		return usageError{errors.New("--history must be >= 0")}
	}
	n := *history
	if *since != "" {
		explicit := false
		c.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "history" })
		if !explicit {
			n = 0
		}
	}
	p, err := load(&cf, stderr)
	if err != nil {
		return err
	}
	if len(p.suites) == 0 {
		return configError{fmt.Errorf("no test suites configured or detected; run `smates init` or write smates.yml")}
	}
	var cfgAbs, cfgName string
	if *apply {
		if cfgAbs, cfgName, err = configFile(cf.dir, cf.config); err != nil {
			return err
		}
	}
	if *since != "" && !p.repo.RefExists(*since) {
		return usageError{fmt.Errorf("unknown revision %q", *since)}
	}
	if p.repo.Head() == "" {
		return errors.New("no commits to replay")
	}
	commits, err := p.repo.FirstParentCommits(*since, n)
	if err != nil {
		return err
	}
	// A shallow clone's oldest commit has no parent to diff against (it is not a root):
	// replay stops there.
	shallow := 0
	for i, c := range commits {
		if c.Shallow && c.Parent == "" {
			shallow = len(commits) - i
			commits = commits[:i]
			break
		}
	}
	changes := make([][]gitx.FileChange, len(commits))
	for i, c := range commits {
		if changes[i], err = p.repo.ChangedInCommit(c); err != nil {
			return err
		}
	}
	res, err := selector.Select(&selector.Input{Repo: p.repo, Config: p.cfg, Suites: p.suites, Others: p.others, State: p.state, Snap: p.snap, Graph: p.graph, All: true})
	if err != nil {
		return err
	}
	rep := p.orphanHistory(res, commits, changes)
	rep.Since = *since
	if shallow > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("shallow clone: stopped at the oldest fetched commit, whose parent is missing (%d commit(s) not replayed); fetch more history (`git fetch --deepen=N` or `--unshallow`) to replay further", shallow))
	}
	if *apply {
		globs := make([]string, 0, len(rep.Ignore))
		for _, g := range rep.Ignore {
			globs = append(globs, g.Glob)
		}
		if len(globs) == 0 {
			rep.Applied = &configEdit{File: cfgName, Key: "ignore", Added: []string{}, Present: []string{}, DryRun: *dryRun}
		} else if rep.Applied, err = addGlobs(cfgAbs, cfgName, "ignore", globs, *dryRun); err != nil {
			return err
		}
	}
	return c.emit(stdout, rep, func() {
		for _, w := range rep.Warnings {
			fmt.Fprintf(stderr, "smates: warning: %s\n", w)
		}
		printOrphanReport(stdout, rep, p.cfg.Path)
		if rep.Applied != nil {
			fmt.Fprintln(stdout)
			if len(rep.Applied.Added)+len(rep.Applied.Present) == 0 {
				fmt.Fprintln(stdout, "--apply: no ignore globs to add.")
			} else {
				printConfigEdit(stdout, rep.Applied)
			}
		}
	})
}

// orphanHistory replays commits against the current dependency map (res, an All
// selection) with the selector's orphan rule, and suggests how to cut the orphans.
func (p *project) orphanHistory(res *selector.Result, commits []gitx.Commit, changes [][]gitx.FileChange) *orphanReport {
	rep := &orphanReport{
		Commits: len(commits), RecordedState: !p.state.Empty(), Policy: p.cfg.Orphans,
		Warnings: []string{}, Files: []orphanFile{}, Ignore: []orphanGlob{}, Inputs: []orphanFile{}, History: []orphanCommit{},
		ExistingIgnore: append([]string{}, p.cfg.Ignore...),
	}
	if rep.Policy == "" {
		rep.Policy = config.OrphansAgent
	}
	if !rep.RecordedState {
		rep.Warnings = append(rep.Warnings, "no recorded state: dependencies come from static analysis, inputs and rules only, so files tests read at run time show up as orphans; run `smates run --all` first for an accurate report")
	}
	ignore := selector.IgnoreGlobs(p.cfg)
	perCommit := make([][]string, len(commits))
	count := map[string]int{}
	for i, ch := range changes {
		for _, c := range ch {
			f := c.Path
			if pathutil.IsVendored(f) || pathutil.Match(ignore, f) {
				continue
			}
			if !res.Explains(f, c.Status == 'A' || c.Status == 'D') {
				perCommit[i] = append(perCommit[i], f)
				count[f]++
			}
		}
	}
	all := make([]string, 0, len(count))
	for f := range count {
		all = append(all, f)
	}
	sort.Strings(all)
	_, explained := res.References(p.graph, all)
	referenced := map[string]bool{}
	for _, f := range explained {
		referenced[f] = true
	}
	// Source code no test reaches selects nothing with `untested: skip` (judged
	// against the recorded state, as runs do).
	untested := map[string]bool{}
	if p.cfg.Untested == config.UntestedSkip && !p.state.Empty() {
		rs := *res
		rs.Mode = selector.ModeState
		for _, f := range rs.Untested(subtract(all, explained)) {
			untested[f] = true
		}
	}

	// Classify and rank.
	for _, f := range all {
		of := orphanFile{Path: f, Commits: count[f], Kind: orphanKindIgnore}
		switch {
		case referenced[f]:
			of.Kind = orphanKindReference
		case untested[f]:
			of.Kind = orphanKindUntested
		case affectsExecution(f) != "":
			of.Kind, of.Why = orphanKindInputs, affectsExecution(f)
		}
		rep.Files = append(rep.Files, of)
	}
	sort.SliceStable(rep.Files, func(i, j int) bool { return rep.Files[i].Commits > rep.Files[j].Commits })
	globOf := suggestGlobs(rep.Files, p.files, res.Covered, func(files []string) []string {
		_, ex := res.References(p.graph, files)
		return ex
	})
	commitsOf := map[string]map[int]bool{}
	files := map[string][]string{}
	for i := range rep.Files {
		of := &rep.Files[i]
		switch of.Kind {
		case orphanKindIgnore:
			of.Glob = globOf[of.Path]
			files[of.Glob] = append(files[of.Glob], of.Path)
		case orphanKindInputs:
			rep.Inputs = append(rep.Inputs, *of)
		}
	}

	// Replay with the suggestions.
	for i, c := range commits {
		var orphans, refs []string
		cleared, clearedInputs := true, true
		for _, f := range perCommit[i] {
			if referenced[f] {
				refs = append(refs, f)
				continue
			}
			if untested[f] {
				continue
			}
			orphans = append(orphans, f)
			if g := globOf[f]; g != "" {
				if commitsOf[g] == nil {
					commitsOf[g] = map[int]bool{}
				}
				commitsOf[g][i] = true
				continue
			}
			cleared = false
			if affectsExecution(f) == "" {
				clearedInputs = false
			}
		}
		if len(orphans) == 0 && len(refs) == 0 {
			continue
		}
		rep.History = append(rep.History, orphanCommit{Commit: c.SHA, Subject: c.Subject, Orphans: nonNil(orphans), Referenced: refs})
		if len(orphans) > 0 {
			rep.CommitsWithOrphans++
		}
		if !cleared {
			rep.CommitsWithOrphansAfterIgnore++
		}
		if !clearedInputs {
			rep.CommitsWithOrphansAfterInputs++
		}
	}
	for g, fs := range files {
		rep.Ignore = append(rep.Ignore, orphanGlob{Glob: g, Files: fs, Commits: len(commitsOf[g])})
	}
	sort.Slice(rep.Ignore, func(i, j int) bool {
		a, b := rep.Ignore[i], rep.Ignore[j]
		if a.Commits != b.Commits {
			return a.Commits > b.Commits
		}
		return a.Glob < b.Glob
	})
	return rep
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// suggestGlobs maps each orphan of kind "ignore" to the glob suggested for it: the
// shallowest directory "dir/**" holding at least two of them where no file (in the
// ranking or in the working tree) is a dependency of a test, explained by a reference
// (refs returns the files of a list that tests name) or execution-relevant; otherwise
// the file itself. Glob metacharacters in paths are escaped.
func suggestGlobs(ranked []orphanFile, tree []string, covered map[string]bool, refs func([]string) []string) map[string]string {
	blocked := map[string]bool{} // directories that must not be ignored as a whole
	block := func(f string) {
		for d := path.Dir(f); d != "." && d != "/"; d = path.Dir(d) {
			blocked[d] = true
		}
	}
	candidates := map[string]int{} // directory -> orphans of kind "ignore" below it
	for _, of := range ranked {
		if of.Kind != orphanKindIgnore {
			block(of.Path)
			continue
		}
		for d := path.Dir(of.Path); d != "." && d != "/"; d = path.Dir(d) {
			candidates[d]++
		}
	}
	// Every dependency blocks the directories holding it, including listings and
	// existence checks (a test that lists a directory notices files appearing there)
	// and files no longer in the working tree.
	for key := range covered {
		switch kind, p := depkey.Parse(key); kind {
		case depkey.KindListing, depkey.KindTree:
			if p != "" && p != "." && p != "/" {
				blocked[p] = true
				block(p)
			}
		case depkey.KindContent, depkey.KindConfig, depkey.KindExistence:
			block(p)
		}
	}
	ranks := map[string]bool{}
	for _, of := range ranked {
		ranks[of.Path] = true
	}
	var unexplained []string // files below a candidate directory that a test may name
	for _, f := range tree {
		if depkey.Covers(covered, f, false) != depkey.CoverNone || affectsExecution(f) != "" {
			block(f)
			continue
		}
		if ranks[f] {
			continue // already classified
		}
		for d := path.Dir(f); d != "." && d != "/"; d = path.Dir(d) {
			if candidates[d] >= 2 {
				unexplained = append(unexplained, f)
				break
			}
		}
	}
	if refs != nil && len(unexplained) > 0 {
		for _, f := range refs(unexplained) {
			block(f)
		}
	}
	out := map[string]string{}
	for _, of := range ranked {
		if of.Kind != orphanKindIgnore {
			continue
		}
		out[of.Path] = escapeGlob(of.Path)
		var dirs []string
		for d := path.Dir(of.Path); d != "." && d != "/"; d = path.Dir(d) {
			dirs = append(dirs, d)
		}
		for i := len(dirs) - 1; i >= 0; i-- { // shallowest first
			if d := dirs[i]; !blocked[d] && candidates[d] >= 2 {
				out[of.Path] = escapeGlob(d) + "/**"
				break
			}
		}
	}
	return out
}

// escapeGlob escapes the doublestar metacharacters in a path, so that a glob made from
// it (app/[id]/page.tsx, a file named "a*b") matches the path itself.
func escapeGlob(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`*?[]{}\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// affectsExecution says what kind of file f is when it may change how tests run (a CI
// workflow, a build script, a container definition, a toolchain pin): such a file is
// better declared in inputs or rules than ignored. "" for other files.
func affectsExecution(f string) string {
	base := strings.ToLower(path.Base(f))
	switch {
	case strings.HasPrefix(f, ".github/workflows/"), strings.HasPrefix(f, ".circleci/"), strings.HasPrefix(f, ".buildkite/"),
		base == ".gitlab-ci.yml", base == "jenkinsfile", base == "azure-pipelines.yml", base == ".travis.yml", base == "bitbucket-pipelines.yml":
		return "CI configuration"
	case base == "makefile", base == "gnumakefile", path.Ext(base) == ".mk", base == "justfile", base == "taskfile.yml", base == "taskfile.yaml":
		return "build script"
	case strings.HasPrefix(base, "dockerfile"), strings.HasSuffix(base, ".dockerfile"), base == "containerfile",
		strings.HasPrefix(base, "docker-compose") && isYAML(base), strings.HasPrefix(base, "compose") && isYAML(base):
		return "container definition"
	case base == ".nvmrc", base == ".node-version", base == ".tool-versions", base == "mise.toml", base == ".mise.toml",
		base == "go.mod", base == "go.work", base == ".python-version", base == "rust-toolchain", base == "rust-toolchain.toml",
		base == ".ruby-version", base == ".java-version", base == ".sdkmanrc", base == ".bun-version":
		return "toolchain pin"
	}
	return ""
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

func printOrphanReport(w io.Writer, rep *orphanReport, configPath string) {
	what := fmt.Sprintf("the last %d first-parent commit(s)", rep.Commits)
	if rep.Since != "" {
		what = fmt.Sprintf("%d first-parent commit(s) since %s", rep.Commits, rep.Since)
	}
	fmt.Fprintf(w, "Replayed %s against the current dependency map.\n", what)
	if rep.Commits == 0 {
		return
	}
	effect := "the agent would have been asked (orphans: agent)"
	switch rep.Policy {
	case config.OrphansAll:
		effect = "the suites they may affect would have run in full (orphans: all)"
	case config.OrphansIgnore:
		effect = "they were silently ignored (orphans: ignore)"
	}
	fmt.Fprintf(w, "%d of %d commit(s) changed a file no test depends on; %s.\n", rep.CommitsWithOrphans, rep.Commits, effect)
	if len(rep.Files) == 0 {
		fmt.Fprintln(w, "No orphans: nothing to add.")
		return
	}

	fmt.Fprintln(w, "\nOrphans by number of commits:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, of := range rep.Files {
		note := ""
		switch of.Kind {
		case orphanKindIgnore:
			note = "ignore: " + of.Glob
		case orphanKindInputs:
			note = of.Why + ": consider inputs:/rules: instead of ignore"
		case orphanKindReference:
			note = "explained by reference (a test names it)"
		case orphanKindUntested:
			note = "untested code: no test reaches it, nothing runs (untested: skip)"
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\n", of.Commits, of.Path, note)
	}
	tw.Flush()

	if len(rep.Ignore) > 0 {
		fmt.Fprintln(w, "\nSuggested ignore globs (no test depends on these files):")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, g := range rep.Ignore {
			fmt.Fprintf(tw, "  %s\t%d commit(s), %d file(s)\n", g.Glob, g.Commits, len(g.Files))
		}
		tw.Flush()
	}
	if len(rep.Inputs) > 0 {
		fmt.Fprintln(w, "\nMay change how tests run; declare them in inputs: (every test) or rules: (some tests) instead of ignoring them:")
		for _, of := range rep.Inputs {
			fmt.Fprintf(w, "  %s (%s, %d commit(s))\n", of.Path, of.Why, of.Commits)
		}
	}
	fmt.Fprintf(w, "\nWith these globs, %d of %d commit(s) would still have orphans", rep.CommitsWithOrphansAfterIgnore, rep.Commits)
	if len(rep.Inputs) > 0 {
		fmt.Fprintf(w, " (%d once the files above are in inputs:/rules:)", rep.CommitsWithOrphansAfterInputs)
	}
	fmt.Fprintln(w, ".")
	if len(rep.Ignore) == 0 {
		return
	}
	where := "smates.yml"
	if configPath != "" {
		where = configPath
	}
	fmt.Fprintf(w, "\nPaste into %s:\n\n", where)
	fmt.Fprintln(w, "ignore:")
	for _, g := range rep.ExistingIgnore {
		fmt.Fprintf(w, "  - %s\n", strconv.Quote(g))
	}
	for _, g := range rep.Ignore {
		fmt.Fprintf(w, "  - %s\n", strconv.Quote(g.Glob))
	}
}
