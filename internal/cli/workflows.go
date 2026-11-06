package cli

// Existing GitHub workflows that already run the tests, and the patch that switches
// them to the smates action.

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"gopkg.in/yaml.v3"
)

// ---- existing workflows ----

// testStep is a workflow step that runs tests directly.
type testStep struct {
	File    string // repo-relative workflow path
	Job     string
	Step    string // the step's name, or "#N" (1-based)
	Command string // the matching command line
	Runners []string
	Line    int // 1-based first line of the step ("- ...")
	End     int // 1-based last line of the step
	Dash    int // 0-based column of the step's "-"
	// Blocks are the 1-based first and last lines of each key of the step.
	Blocks map[string][2]int
	// TestLine is the line of a multi-line run: block holding only the test command,
	// when the block runs other commands too (0: the step runs only the tests).
	TestLine int
	// WorkDir is the directory the step runs in, relative to the repository root: its
	// working-directory, else the job's or the workflow's defaults.run.working-directory
	// ("" when none is set: the root).
	WorkDir string
	// Env is what the workflow, the job and the step set in env: (in that order, later
	// ones win), for the hints in smates.yml.
	Env [][2]string
	// Carry are the go test flags a suite's command must keep for the smates action to
	// stand in for the step.
	Carry goCarry
	// Skip says why the step is reported but not patched ("" = patchable).
	Skip string
	// Other is set for test targets other than the unit tests (test:e2e, test-integration,
	// go test -run).
	Other bool
}

// testCmdRe finds test commands; suffixed script names ("test:e2e", "test-e2e") are
// told apart by plainTestCmd.
// turbo and node --test steps are found (they run the tests, so the smates workflow
// would run them a second time) but never patched.
var testCmdRe = regexp.MustCompile(`(?:^|[\s;&|(])((?:go test|vitest|jest|pytest|bun test|node --test|turbo (?:run )?test(?::[\w:-]+)?|(?:npm|pnpm|yarn) (?:run )?test(?::[\w:-]+)?|make test[\w-]*))(?:\s|$|;|&|\|)`)

// plainTestCmd reports whether a matched test command runs the unit tests (plain
// "test", "test:unit", "make test", "make test-unit") rather than another test target
// such as test:e2e or test-integration, which smates may not cover.
func plainTestCmd(cmd string) bool {
	switch {
	case strings.HasPrefix(cmd, "make "):
		t := strings.TrimPrefix(cmd, "make ")
		return t == "test" || t == "test-unit" || t == "test_unit"
	case strings.HasSuffix(cmd, " test"), strings.HasSuffix(cmd, " test:unit"):
		return true
	case strings.Contains(cmd, " test"): // npm/pnpm/yarn test:<other>
		return strings.HasPrefix(cmd, "go ") || strings.HasPrefix(cmd, "bun ")
	}
	return true // vitest, jest, pytest
}

// runnersOf maps a test command to the runners it covers (nil: any suite).
func runnersOf(cmd string) []string {
	switch {
	case strings.HasPrefix(cmd, "go test"):
		return []string{"go"}
	case cmd == "vitest":
		return []string{"vitest"}
	case cmd == "jest":
		return []string{"jest"}
	case cmd == "pytest":
		return []string{"pytest"}
	case cmd == "bun test":
		return []string{"bun"}
	case cmd == "node --test":
		return []string{"node"}
	case strings.HasPrefix(cmd, "make"):
		return nil
	}
	return []string{"vitest", "jest", "node", "bun"} // npm/pnpm/yarn test
}

// workflowTestSteps finds steps of .github/workflows/*.y*ml that run tests without
// smates.
func workflowTestSteps(root string) []testStep {
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	var out []testStep
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel := ".github/workflows/" + filepath.Base(f)
		out = append(out, parseWorkflowSteps(rel, data)...)
	}
	return out
}

func parseWorkflowSteps(rel string, data []byte) []testStep {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	top := doc.Content[0]
	jobs := mapGet(top, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	var out []testStep
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		job := jobs.Content[i].Value
		jobNode := jobs.Content[i+1]
		steps := mapGet(jobNode, "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		if slices.ContainsFunc(steps.Content, func(st *yaml.Node) bool {
			u := mapGet(st, "uses")
			return u != nil && strings.HasPrefix(u.Value, "dotneet/smates@")
		}) {
			continue // the job already runs smates
		}
		for si, st := range steps.Content {
			if st.Kind != yaml.MappingNode {
				continue
			}
			run := mapGet(st, "run")
			if run == nil || mapGet(st, "uses") != nil || strings.Contains(run.Value, "smates") {
				continue
			}
			var cmd, match string
			others, after := 0, 0
			for _, l := range strings.Split(run.Value, "\n") {
				t := strings.TrimSpace(l)
				if t == "" || strings.HasPrefix(t, "#") {
					continue
				}
				if m := testCmdRe.FindStringSubmatch(l); m != nil && cmd == "" {
					cmd, match = t, m[1]
					continue
				}
				others++
				if cmd != "" {
					after++
				}
			}
			if cmd == "" {
				continue
			}
			ts := testStep{File: rel, Job: job, Step: fmt.Sprintf("#%d", si+1), Command: cmd, Runners: runnersOf(match), Line: st.Line, Dash: st.Column - 3, Blocks: map[string][2]int{}}
			if n := mapGet(st, "name"); n != nil {
				ts.Step = n.Value
			}
			wdSkip := ""
			for _, n := range []*yaml.Node{mapGet(st, "working-directory"), defaultsWorkDir(jobNode), defaultsWorkDir(top)} {
				if n == nil {
					continue
				}
				wd, ok := workDir(n.Value)
				switch {
				case ok:
					ts.WorkDir = wd
				case strings.Contains(n.Value, "matrix."):
					wdSkip = fmt.Sprintf("its working-directory %q changes with the matrix leg: smates cannot tell which suite each leg runs; give each leg `args: --suite <name>` and `cache-key: <name>` by hand", n.Value)
				default:
					wdSkip = fmt.Sprintf("its working-directory %q is not a directory of the repository smates can resolve", n.Value)
				}
				break
			}
			for _, n := range []*yaml.Node{top, jobNode, st} {
				ts.Env = mergeEnv(ts.Env, mapGet(n, "env"))
			}
			check := checkTestCmd(cmd, match)
			if !check.other {
				ts.Carry = check.carry
			}
			if ts.Dash < 0 {
				ts.Dash = 0
			}
			// The step ends before the next line indented at or left of its "-".
			end := ts.Line
			for j := ts.Line; j < len(lines); j++ { // lines[j] is line j+1
				l := lines[j]
				if strings.TrimSpace(l) == "" {
					continue
				}
				if len(l)-len(strings.TrimLeft(l, " ")) <= ts.Dash {
					break
				}
				end = j + 1
			}
			ts.End = end
			for k := 0; k+1 < len(st.Content); k += 2 {
				from, to := st.Content[k].Line, ts.End
				if k+2 < len(st.Content) {
					to = st.Content[k+2].Line - 1
				}
				for to > from && strings.TrimSpace(lines[to-1]) == "" {
					to--
				}
				ts.Blocks[st.Content[k].Value] = [2]int{from, to}
			}
			switch {
			case !plainTestCmd(match):
				ts.Other = true
				ts.Skip = fmt.Sprintf("%q is not the plain unit-test command; switch it to smates by hand if smates should select those tests", match)
			case strings.HasSuffix(cmd, "\\"):
				ts.Skip = "the test command is chained with other commands on one line; split it out, then use the smates action for the test part"
			case check.skip != "":
				ts.Skip, ts.Other = check.skip, check.other
			case wdSkip != "":
				ts.Skip = wdSkip
			case after > 0:
				ts.Skip = "commands after the test line use its output (or must run after the tests); move them to their own step, then use the smates action for the tests"
			case others > 0:
				// Keep the commands before it: drop only the test line of the run: block.
				b := ts.Blocks["run"]
				for j := b[0] + 1; j <= b[1]; j++ {
					if strings.TrimSpace(lines[j-1]) == cmd {
						ts.TestLine = j
						break
					}
				}
				if ts.TestLine == 0 {
					ts.Skip = "the run: block also runs other commands and its test line could not be isolated"
				}
			}
			out = append(out, ts)
		}
	}
	return out
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// defaultsWorkDir returns the defaults.run.working-directory of a workflow or a job.
func defaultsWorkDir(n *yaml.Node) *yaml.Node {
	return mapGet(mapGet(mapGet(n, "defaults"), "run"), "working-directory")
}

// workDir normalizes a working-directory value to a directory relative to the
// repository root ("." for the root itself); ok is false for one smates cannot resolve:
// an expression (other than the workspace), an absolute path, a path out of the
// repository.
func workDir(v string) (string, bool) {
	v = strings.TrimSpace(v)
	for _, ws := range []string{"${{ github.workspace }}", "${{github.workspace}}", "${GITHUB_WORKSPACE}", "$GITHUB_WORKSPACE"} {
		if rest, ok := strings.CutPrefix(v, ws); ok && (rest == "" || rest[0] == '/') {
			v = "." + rest
		}
	}
	if v == "" || strings.ContainsAny(v, "$`~\\") || path.IsAbs(v) {
		return "", false
	}
	v = path.Clean(v)
	if v == ".." || strings.HasPrefix(v, "../") {
		return "", false
	}
	return v, true
}

// mergeEnv adds the scalar entries of an env: mapping to env (replacing a key's earlier
// value).
func mergeEnv(env [][2]string, n *yaml.Node) [][2]string {
	if n == nil || n.Kind != yaml.MappingNode {
		return env
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		if v.Kind != yaml.ScalarNode {
			continue
		}
		env = slices.DeleteFunc(env, func(kv [2]string) bool { return kv[0] == k })
		env = append(env, [2]string{k, v.Value})
	}
	return env
}

// stepSuites returns the suites whose tests a workflow step runs, by its runner and by
// the directory it runs in: every suite rooted at or below that directory (a nested
// module or package may be reached too, so all of them are kept). When the directory
// lies inside a suite's root instead, the step runs only part of that suite: the suite
// is listed too, and partial names it.
func stepSuites(st testStep, suites []config.Suite) (names []string, partial string) {
	wd := st.WorkDir
	if wd == "" {
		wd = "."
	}
	rootAt := map[string]bool{} // runners with a suite rooted at wd
	deepest := map[string]config.Suite{}
	for _, s := range suites {
		if st.Runners != nil && !slices.Contains(st.Runners, s.Runner) {
			continue
		}
		root := cleanRoot(s.Root)
		switch {
		case wd == "." || root == wd || strings.HasPrefix(root, wd+"/"):
			names = append(names, s.Name)
			rootAt[s.Runner] = rootAt[s.Runner] || root == wd
		case root == "." || strings.HasPrefix(wd, root+"/"):
			if d, ok := deepest[s.Runner]; !ok || rootDepth(root) > rootDepth(cleanRoot(d.Root)) {
				deepest[s.Runner] = s
			}
		}
	}
	for _, r := range slices.Sorted(maps.Keys(deepest)) {
		s := deepest[r]
		// A suite rooted at wd ends the outer one there (a Go module, a package), and an
		// outer suite may exclude the directory.
		if rootAt[r] || excludesDir(s, wd) {
			continue
		}
		names = append(names, s.Name)
		if partial == "" {
			partial = s.Name
		}
	}
	return names, partial
}

func cleanRoot(root string) string {
	if root == "" {
		return "."
	}
	return path.Clean(root)
}

func rootDepth(root string) int {
	if root == "." {
		return 0
	}
	return strings.Count(root, "/") + 1
}

// excludesDir reports whether an exclude: pattern of suite s leaves out all of
// directory dir (repo-relative, inside the suite's root).
func excludesDir(s config.Suite, dir string) bool {
	rel := dir
	if root := cleanRoot(s.Root); root != "." {
		rel = strings.TrimPrefix(dir, root+"/")
	}
	for _, e := range s.Exclude {
		e = strings.TrimSuffix(strings.TrimSuffix(e, "/**"), "/")
		if e != "" && !strings.ContainsAny(e, "*?[{") && (rel == e || strings.HasPrefix(rel, e+"/")) {
			return true
		}
	}
	return false
}

// stepPatch returns the suites the smates action would run in place of a workflow
// step, and why it cannot stand in for the step ("" when it can).
func stepPatch(st testStep, suites []config.Suite) ([]string, string) {
	names, partial := stepSuites(st, suites)
	switch {
	case st.Skip != "":
		return names, st.Skip
	case len(names) == 0:
		return nil, "it runs tests of no suite smates detected"
	case partial != "":
		s := suites[slices.IndexFunc(suites, func(s config.Suite) bool { return s.Name == partial })]
		return names, fmt.Sprintf("it runs in %s, inside suite %q (root %s), so it runs only part of that suite and the smates action would select among all of it: add a suite rooted at %s to smates.yml, or switch the step by hand", st.WorkDir, s.Name, cleanRoot(s.Root), st.WorkDir)
	}
	if st.Carry.empty() {
		return names, ""
	}
	for _, s := range suites {
		if s.Runner != "go" || !slices.Contains(names, s.Name) {
			continue
		}
		_, args, ok := goSuiteCommand(s.Command)
		if !ok {
			return names, fmt.Sprintf("it runs go test with %s, and suite %q's command is not go test: check that the command keeps them, then switch the step by hand", st.Carry, s.Name)
		}
		parsed, _ := runner.ParseGoTestArgs(args, false)
		if unmet := st.Carry.unmet(goCarryOf(parsed)); len(unmet) > 0 {
			want, _, _ := withGoCarry(s.Command, st.Carry)
			return names, fmt.Sprintf("it runs go test with %s, which the command of suite %q in smates.yml (%s) lacks: set `command: %s` for the suite, then rerun", strings.Join(unmet, ", "), s.Name, orGoTest(s.Command), yamlStr(want))
		}
	}
	return names, ""
}

func orGoTest(command string) string {
	if command == "" {
		return "go test"
	}
	return command
}

var (
	cacheKeyRe  = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	plainWordRe = regexp.MustCompile(`^[A-Za-z0-9_./:@,=-]+$`)
)

// keptStepKeys are copied from a replaced test step to the smates step.
var keptStepKeys = []string{"id", "if", "timeout-minutes", "continue-on-error", "env"}

// workflowPatch returns a unified diff (git apply-able) that switches the test steps
// of existing workflows to the smates action, and notes on what it could not carry
// over. A step that only runs the tests is replaced (keeping its id, if,
// timeout-minutes, continue-on-error and env); when its run: block also runs other
// commands, only the test line is removed and the smates step goes right after it.
// The smates step gets --suite for the suites the step ran (by its runner and the
// directory it runs in), and a cache-key naming them when the tests run in several
// steps. A workflow that then runs smates only on pull requests also gets a push
// trigger for the default branch (def) and a nightly schedule, which record the state
// pull requests start from; so does a workflow that already runs smates that way
// (seedOnly).
func workflowPatch(root string, steps []testStep, suites []config.Suite, def string, seedOnly ...string) (string, []string) {
	type patched struct {
		testStep
		names []string
	}
	var todo []patched
	for _, st := range steps {
		if names, why := stepPatch(st, suites); why == "" {
			todo = append(todo, patched{st, names})
		}
	}
	split := len(todo) > 1
	byFile := map[string][]patched{}
	var order []string
	for _, st := range todo {
		if byFile[st.File] == nil {
			order = append(order, st.File)
		}
		byFile[st.File] = append(byFile[st.File], st)
	}
	for _, f := range seedOnly {
		if !slices.Contains(order, f) {
			order = append(order, f)
		}
	}
	// One cache key per set of suites: steps running the same suites share state.
	keyOf := map[string]string{} // suite set -> key
	used := map[string]bool{}
	cacheKey := func(st patched) string {
		set := strings.Join(st.names, "\x00")
		if k, ok := keyOf[set]; ok {
			return k
		}
		base := st.Job
		if len(st.names) <= 3 {
			base = strings.Join(st.names, "-")
		}
		base = strings.Trim(cacheKeyRe.ReplaceAllString(base, "-"), "-")
		key := base
		for n := 2; used[key]; n++ {
			key = base + "-" + strconv.Itoa(n)
		}
		used[key], keyOf[set] = true, key
		return key
	}
	var notes []string
	var b strings.Builder
	for _, f := range order {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		old := string(data)
		lines := strings.Split(old, "\n")
		// The lines written keep the file's line endings (lines still end in \r).
		crlf := strings.Contains(old, "\r\n")
		// block returns the raw lines of a key of the step, re-indented as a key that
		// is not on the "- " line.
		block := func(st testStep, key string) []string {
			r, ok := st.Blocks[key]
			if !ok {
				return nil
			}
			out := slices.Clone(lines[r[0]-1 : r[1]])
			if r[0] == st.Line && len(out[0]) > st.Dash && out[0][st.Dash] == '-' {
				out[0] = out[0][:st.Dash] + " " + out[0][st.Dash+1:]
			}
			return out
		}
		var edits []lineEdit
		for _, st := range byFile[f] {
			ind := strings.Repeat(" ", st.Dash)
			name := st.Step
			if strings.HasPrefix(name, "#") {
				name = "tests"
			}
			rep := []string{ind + "- name: " + yamlStr(name+" (smates: affected tests only)")}
			for _, k := range keptStepKeys[:4] {
				if k == "id" && st.TestLine > 0 {
					continue // the original step stays and keeps its id
				}
				rep = append(rep, block(st.testStep, k)...)
			}
			rep = append(rep, ind+"  uses: "+actSmates, ind+"  with:")
			if len(st.names) < len(suites) {
				args := ""
				for _, n := range st.names {
					args += " --suite " + shellQuote(n)
				}
				rep = append(rep, ind+"    args: "+yamlStr(strings.TrimSpace(args)))
			}
			if split {
				rep = append(rep, ind+"    cache-key: "+yamlStr(cacheKey(st)))
			}
			rep = append(rep, ind+"    anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional (or openai-api-key / openrouter-api-key)")
			rep = append(rep, block(st.testStep, "env")...)
			if st.TestLine > 0 {
				edits = append(edits, lineEdit{From: st.TestLine, To: st.TestLine}, lineEdit{From: st.End + 1, To: st.End, Lines: rep})
				notes = append(notes, fmt.Sprintf("%s: step %q also runs other commands: only %q moves to the smates step after it", f, st.Step, st.Command))
			} else {
				edits = append(edits, lineEdit{From: st.Line, To: st.End, Lines: rep})
			}
		}
		if e, n, ok := seedPatch(f, data, def); ok {
			edits = append(edits, e)
			notes = append(notes, n...)
		} else {
			notes = append(notes, n...)
		}
		sort.SliceStable(edits, func(i, j int) bool { return edits[i].From < edits[j].From })
		var outLines []string
		next := 1
		for _, e := range edits {
			if e.From < next {
				continue // overlaps an earlier edit
			}
			outLines = append(outLines, lines[next-1:e.From-1]...)
			for _, l := range e.Lines {
				if crlf && !strings.HasSuffix(l, "\r") {
					l += "\r"
				}
				outLines = append(outLines, l)
			}
			next = e.To + 1
		}
		outLines = append(outLines, lines[next-1:]...)
		if nw := strings.Join(outLines, "\n"); nw != old {
			b.WriteString(unifiedDiff(f, old, nw))
		}
	}
	return b.String(), notes
}

func shellQuote(s string) string {
	if plainWordRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// unifiedDiff renders a git-style unified diff of two versions of file with three
// lines of context. A last line without a newline is marked as git does ("\ No
// newline at end of file"), so that git apply accepts the patch.
func unifiedDiff(file, a, b string) string {
	al, bl := splitLines(a), splitLines(b)
	// The last line of a text without a final newline differs from the same line with
	// one: it carries noEOL until it is written.
	const noEOL = "\x00"
	if a != "" && !strings.HasSuffix(a, "\n") {
		al[len(al)-1] += noEOL
	}
	if b != "" && !strings.HasSuffix(b, "\n") {
		bl[len(bl)-1] += noEOL
	}
	// Common prefix and suffix, then an LCS of the middle (workflows are small).
	pre := 0
	for pre < len(al) && pre < len(bl) && al[pre] == bl[pre] {
		pre++
	}
	suf := 0
	for suf < len(al)-pre && suf < len(bl)-pre && al[len(al)-1-suf] == bl[len(bl)-1-suf] {
		suf++
	}
	am, bm := al[pre:len(al)-suf], bl[pre:len(bl)-suf]
	type op struct {
		kind byte // ' ', '-', '+'
		text string
	}
	var ops []op
	for _, l := range al[:pre] {
		ops = append(ops, op{' ', l})
	}
	if len(am)*len(bm) > 4_000_000 {
		for _, l := range am {
			ops = append(ops, op{'-', l})
		}
		for _, l := range bm {
			ops = append(ops, op{'+', l})
		}
	} else {
		lcs := make([][]int, len(am)+1)
		for i := range lcs {
			lcs[i] = make([]int, len(bm)+1)
		}
		for i := len(am) - 1; i >= 0; i-- {
			for j := len(bm) - 1; j >= 0; j-- {
				if am[i] == bm[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < len(am) || j < len(bm) {
			switch {
			case i < len(am) && j < len(bm) && am[i] == bm[j]:
				ops = append(ops, op{' ', am[i]})
				i++
				j++
			case i < len(am) && (j == len(bm) || lcs[i+1][j] >= lcs[i][j+1]):
				ops = append(ops, op{'-', am[i]})
				i++
			default:
				ops = append(ops, op{'+', bm[j]})
				j++
			}
		}
	}
	for _, l := range al[len(al)-suf:] {
		ops = append(ops, op{' ', l})
	}
	const ctx = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", file, file)
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		// A hunk: from ctx lines before this change to ctx lines after the last change
		// that is within 2*ctx unchanged lines of the previous one.
		start := max(0, k-ctx)
		end := k
		for m := k; m < len(ops); m++ {
			if ops[m].kind != ' ' {
				end = m
			} else if m-end > 2*ctx {
				break
			}
		}
		stop := min(len(ops), end+ctx+1)
		aStart, bStart := 1, 1
		for _, o := range ops[:start] {
			if o.kind != '+' {
				aStart++
			}
			if o.kind != '-' {
				bStart++
			}
		}
		aN, bN := 0, 0
		var body strings.Builder
		for _, o := range ops[start:stop] {
			if o.kind != '+' {
				aN++
			}
			if o.kind != '-' {
				bN++
			}
			if text, ok := strings.CutSuffix(o.text, noEOL); ok {
				body.WriteString(string(o.kind) + text + "\n\\ No newline at end of file\n")
			} else {
				body.WriteString(string(o.kind) + o.text + "\n")
			}
		}
		if aN == 0 {
			aStart--
		}
		if bN == 0 {
			bStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n%s", aStart, aN, bStart, bN, body.String())
		k = stop
	}
	return out.String()
}

// splitLines splits text into lines without their newlines (a final newline does not
// add an empty line).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
