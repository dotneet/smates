package cli

// Existing GitHub workflows that already run the tests, and the patch that switches
// them to the smates action.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dotneet/smates/internal/config"
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
	EnvFrom int // 1-based first and last line of the step's env: block (0: none)
	EnvTo   int
}

var testCmdRe = regexp.MustCompile(`(?:^|[\s;&|(])((?:go test|vitest|jest|pytest|bun test|(?:npm|pnpm|yarn) (?:run )?test(?::[\w:-]+)?|make test[\w-]*))(?:\s|$|;|&|\|)`)

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
	jobs := mapGet(doc.Content[0], "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	var out []testStep
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		job := jobs.Content[i].Value
		steps := mapGet(jobs.Content[i+1], "steps")
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
			for _, l := range strings.Split(run.Value, "\n") {
				if m := testCmdRe.FindStringSubmatch(l); m != nil && !strings.HasPrefix(strings.TrimSpace(l), "#") {
					cmd, match = strings.TrimSpace(l), m[1]
					break
				}
			}
			if cmd == "" {
				continue
			}
			ts := testStep{File: rel, Job: job, Step: fmt.Sprintf("#%d", si+1), Command: cmd, Runners: runnersOf(match), Line: st.Line, Dash: st.Column - 3}
			if n := mapGet(st, "name"); n != nil {
				ts.Step = n.Value
			}
			if ts.Dash < 0 {
				ts.Dash = 0
			}
			// The step ends before the next line indented at or left of its "-".
			end := ts.Line
			for j := ts.Line; j < len(lines); j++ { // lines[j] is line j+1
				l := lines[j]
				t := strings.TrimSpace(l)
				if t == "" {
					continue
				}
				if len(l)-len(strings.TrimLeft(l, " ")) <= ts.Dash {
					break
				}
				end = j + 1
			}
			ts.End = end
			// The env: block, kept in the replacement.
			for k := 0; k+1 < len(st.Content); k += 2 {
				if st.Content[k].Value != "env" || st.Content[k].Line == ts.Line {
					continue
				}
				ts.EnvFrom, ts.EnvTo = st.Content[k].Line, ts.End
				if k+2 < len(st.Content) {
					ts.EnvTo = st.Content[k+2].Line - 1
				}
				for ts.EnvTo > ts.EnvFrom && strings.TrimSpace(lines[ts.EnvTo-1]) == "" {
					ts.EnvTo--
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

// stepSuites returns the suites a test step covers.
func stepSuites(st testStep, suites []config.Suite) []string {
	var out []string
	for _, s := range suites {
		if st.Runners == nil || slices.Contains(st.Runners, s.Runner) {
			out = append(out, s.Name)
		}
	}
	return out
}

var (
	cacheKeyRe  = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	plainWordRe = regexp.MustCompile(`^[A-Za-z0-9_./:@-]+$`)
	shellSepRe  = regexp.MustCompile(`&&|;|\|\|`)
)

// workflowPatch returns a unified diff (git apply-able) that replaces each test step
// of the existing workflows with the smates action. When the tests run in several
// steps or jobs, each replacement gets --suite for the suites its runner covers and its
// own cache-key.
func workflowPatch(root string, steps []testStep, suites []config.Suite) string {
	split := len(steps) > 1
	byFile := map[string][]testStep{}
	var order []string
	for _, st := range steps {
		if byFile[st.File] == nil {
			order = append(order, st.File)
		}
		byFile[st.File] = append(byFile[st.File], st)
	}
	keys := map[string]int{}
	var b strings.Builder
	for _, f := range order {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		old := string(data)
		lines := strings.Split(old, "\n")
		var outLines []string
		next := 1
		for _, st := range byFile[f] {
			names := stepSuites(st, suites)
			if len(names) == 0 || st.Line < next {
				continue
			}
			outLines = append(outLines, lines[next-1:st.Line-1]...)
			ind := strings.Repeat(" ", st.Dash)
			name := st.Step
			if strings.HasPrefix(name, "#") {
				name = "tests"
			}
			rep := []string{
				ind + "- name: " + yamlStr(name+" (smates: affected tests only)"),
				ind + "  uses: " + actSmates,
				ind + "  with:",
			}
			if split {
				if len(names) < len(suites) {
					args := ""
					for _, n := range names {
						args += " --suite " + shellQuote(n)
					}
					rep = append(rep, ind+"    args: "+yamlStr(strings.TrimSpace(args)))
				}
				key := cacheKeyRe.ReplaceAllString(st.Job, "-")
				if keys[key]++; keys[key] > 1 {
					key += "-" + strconv.Itoa(keys[key])
				}
				rep = append(rep, ind+"    cache-key: "+yamlStr(key))
			}
			rep = append(rep, ind+"    anthropic-api-key: ${{ secrets.ANTHROPIC_API_KEY }}   # optional (or openai-api-key / openrouter-api-key)")
			if st.EnvFrom > 0 {
				rep = append(rep, lines[st.EnvFrom-1:st.EnvTo]...)
			}
			outLines = append(outLines, rep...)
			next = st.End + 1
		}
		outLines = append(outLines, lines[next-1:]...)
		if nw := strings.Join(outLines, "\n"); nw != old {
			b.WriteString(unifiedDiff(f, old, nw))
		}
	}
	return b.String()
}

func shellQuote(s string) string {
	if plainWordRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// unifiedDiff renders a git-style unified diff of two versions of file with three
// lines of context.
func unifiedDiff(file, a, b string) string {
	al, bl := splitLines(a), splitLines(b)
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
			body.WriteString(string(o.kind) + o.text + "\n")
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
