package cli

// Makefile test targets whose flags or variables smates' default command would drop.

import (
	"fmt"
	"io"
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
)

// ---- Makefile ----

// makeHint is a Makefile test target whose flags or environment smates' own command
// for the suite would not pass.
type makeHint struct {
	Suite   string
	File    string // repo-relative Makefile
	Target  string
	Line    string // the recipe line
	Command string // the runner invocation without positional package/path arguments
	Env     [][2]string
	// Exact is set when the recipe runs in the suite's root (not a fallback to the
	// first suite of the runner).
	Exact bool
	// Carry are the go test flags of the command that the suite's command keeps.
	Carry goCarry
	// Picked is a command substitution choosing the packages ($(go list ./... | grep -v x)).
	Picked string
	// Partial is set when the command runs only some of the suite's tests (named
	// packages or paths, Picked, a filter): its -tags say nothing about the others.
	Partial bool
}

var makeTargetRe = regexp.MustCompile(`^([A-Za-z0-9_.%/-]+(?:\s+[A-Za-z0-9_.%/-]+)*)\s*::?(?:[^=]|$)`)

// makefileHints reads the test and test-* targets of the Makefile at the repository
// root and at each suite root.
func makefileHints(root string, suites []config.Suite) []makeHint {
	dirs := []string{"."}
	for _, s := range suites {
		if !slices.Contains(dirs, s.Root) {
			dirs = append(dirs, s.Root)
		}
	}
	var out []makeHint
	seen := map[string]bool{}
	for _, d := range dirs {
		for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
			rel := joinRel(d, name)
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				continue
			}
			recipes := makeTestRecipes(string(data))
			targets := slices.Sorted(maps.Keys(recipes)) // "test" before "test-*"
			for _, target := range targets {
				for _, line := range recipes[target] {
					h, ok := parseRecipeLine(line, d, suites, false)
					if !ok || seen[h.Suite] {
						continue
					}
					seen[h.Suite] = true
					h.File, h.Target, h.Line = rel, target, line
					out = append(out, h)
				}
			}
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Suite < out[j].Suite })
	return out
}

// makeGoVotes returns the go test runs of the plain unit-test targets (test, test-unit,
// test_unit) of the Makefiles at the repository root and the suite roots that run in a
// Go suite's own directory and cover all of it, with or without flags: each says which
// -tags (none included) the suite's unit tests run with (see carryGoFlags). Other
// targets (test-integration) run other tests.
func makeGoVotes(root string, suites []config.Suite) []makeHint {
	dirs := []string{"."}
	for _, s := range suites {
		if !slices.Contains(dirs, s.Root) {
			dirs = append(dirs, s.Root)
		}
	}
	var out []makeHint
	for _, d := range dirs {
		for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
			rel := joinRel(d, name)
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				continue
			}
			recipes := makeTestRecipes(string(data))
			for _, target := range []string{"test", "test-unit", "test_unit"} {
				for _, line := range recipes[target] {
					h, ok := parseRecipeLine(line, d, suites, true)
					if !ok || !h.Exact || h.Partial || !slices.ContainsFunc(suites, func(s config.Suite) bool { return s.Name == h.Suite && s.Runner == "go" }) {
						continue
					}
					h.File, h.Target, h.Line = rel, target, line
					out = append(out, h)
				}
			}
			break
		}
	}
	return out
}

// makeTestRecipes returns the recipe lines (continuations joined) of the test and
// test-* targets, in order of the targets' names.
func makeTestRecipes(mk string) map[string][]string {
	out := map[string][]string{}
	var cur []string
	var raw []string
	for _, l := range strings.Split(mk, "\n") {
		raw = append(raw, strings.TrimRight(l, "\r"))
	}
	for i := 0; i < len(raw); i++ {
		l := raw[i]
		if strings.HasPrefix(l, "\t") {
			if cur == nil {
				continue
			}
			for strings.HasSuffix(l, "\\") && i+1 < len(raw) {
				i++
				l = strings.TrimSuffix(l, "\\") + " " + strings.TrimSpace(raw[i])
			}
			t := strings.TrimLeft(strings.TrimSpace(l), "@-+")
			for _, c := range cur {
				out[c] = append(out[c], strings.TrimSpace(t))
			}
			continue
		}
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		cur = nil
		if m := makeTargetRe.FindStringSubmatch(l); m != nil {
			for _, t := range strings.Fields(m[1]) {
				if t == "test" || strings.HasPrefix(t, "test-") || strings.HasPrefix(t, "test_") {
					cur = append(cur, t)
				}
			}
		}
	}
	return out
}

// parseRecipeLine finds a direct runner invocation with flags or environment in a
// recipe line run from directory dir (with all, one without them too).
func parseRecipeLine(line, dir string, suites []config.Suite, all bool) (makeHint, bool) {
	var env [][2]string
	cd := dir
	// make passes $$ to the shell as $; $(VAR) is make's own and stays one word.
	words := shellWords(strings.ReplaceAll(line, "$$", "$"))
	for len(words) > 0 {
		end := slices.IndexFunc(words, shellControlOp)
		if end < 0 {
			end = len(words)
		}
		seg := slices.Clone(words[:end])
		words = words[min(end+1, len(words)):]
		if len(seg) >= 2 && seg[0] == "cd" {
			cd = path.Clean(joinRel(dir, seg[1]))
			continue
		}
		if len(seg) > 0 && seg[0] == "export" {
			seg = seg[1:]
		}
		var local [][2]string
		for len(seg) > 0 && strings.Contains(seg[0], "=") && !strings.HasPrefix(seg[0], "-") {
			k, v, _ := strings.Cut(seg[0], "=")
			local = append(local, [2]string{k, strings.Trim(v, `"'`)})
			seg = seg[1:]
		}
		runner, at := "", -1
		for i, w := range seg {
			base := path.Base(w)
			switch {
			case (base == "go" || w == "$(GO)" || w == "${GO}") && i+1 < len(seg) && seg[i+1] == "test":
				runner, at = "go", i+2
			case base == "bun" && i+1 < len(seg) && seg[i+1] == "test":
				runner, at = "bun", i+2
			case base == "vitest" || base == "jest" || base == "pytest":
				runner, at = base, i+1
			}
			if runner != "" {
				break
			}
		}
		if runner == "" {
			if len(seg) == 0 { // only assignments: "export TZ=UTC" for the next commands
				env = append(env, local...)
			}
			continue
		}
		env = append(env, local...)
		cmd := slices.Clone(seg[:at])
		var args []string
		flags, picked, partial := 0, "", false
		for _, w := range seg[at:] {
			if shellRedirect(w) {
				break
			}
			if strings.HasPrefix(w, "$(") && strings.ContainsAny(w, " \t") || strings.HasPrefix(w, "`") {
				picked = w // packages picked by a command: smates passes its own
				continue
			}
			if strings.HasPrefix(w, "./") || strings.HasPrefix(w, "../") || strings.Contains(w, "...") || w == "." {
				partial = partial || w != "./..." && w != "."
				continue // packages / test paths: smates passes its own
			}
			if strings.HasPrefix(w, "-") {
				flags++
			}
			cmd = append(cmd, w)
			args = append(args, w)
		}
		if runner == "vitest" && len(cmd) == at && at == 1 {
			cmd = append(cmd, "run")
		}
		if flags == 0 && len(env) == 0 && !all {
			return makeHint{}, false
		}
		suite, exact := "", false
		for _, s := range suites {
			if s.Runner == runner && s.Root == cd {
				suite, exact = s.Name, true
				break
			}
			if s.Runner == runner && suite == "" {
				suite = s.Name
			}
		}
		if suite == "" {
			return makeHint{}, false
		}
		h := makeHint{Suite: suite, Command: strings.Join(cmd, " "), Env: env, Exact: exact, Picked: picked, Partial: partial || picked != ""}
		if runner == "go" {
			c := checkGoTestArgs(args)
			if !c.other {
				h.Carry = c.carry
			}
			h.Partial = h.Partial || !c.full
		}
		return h, true
	}
	return makeHint{}, false
}

// configHints renders the commented smates.yml lines for each suite: the Makefile test
// target smates found for it, and the env: the workflows running its tests set. carried
// holds the go test flags the suite's command took from them (see carryGoFlags).
func configHints(hints []makeHint, wf map[string]envHint, carried map[string]string) map[string][]string {
	out := map[string][]string{}
	for _, h := range hints {
		lines := []string{fmt.Sprintf("# %s target %q runs: %s", h.File, h.Target, h.Line)}
		if c := carried[h.Suite]; c != "" {
			lines = append(lines, "# smates runs the runner itself; command: above keeps "+c+"; to keep the other flags/variables too, replace it with:")
		} else {
			lines = append(lines, "# smates runs the runner itself; to keep those flags/variables, uncomment:")
		}
		lines = append(lines, "# command: "+strconv.Quote(h.Command))
		if h.Picked != "" {
			lines = append(lines, "# It picks the packages with "+h.Picked+": smates runs every package of the suite (add exclude: globs for any it must leave out).")
		}
		if len(h.Env) > 0 {
			lines = append(lines, "# env:")
			for _, kv := range h.Env {
				lines = append(lines, "#   "+kv[0]+": "+yamlStr(kv[1]))
			}
		}
		out[h.Suite] = lines
	}
	for _, name := range slices.Sorted(maps.Keys(wf)) {
		w := wf[name]
		lines := out[name]
		var env [][2]string
		for _, kv := range w.Env {
			if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "#   "+kv[0]+":") }) {
				env = append(env, kv)
			}
		}
		if len(env) == 0 {
			continue
		}
		if slices.Contains(lines, "# env:") {
			lines = append(lines, "# "+w.From+" also sets (CI values: check them before adding them under env:):")
		} else {
			lines = append(lines, "# "+w.From+" sets env: for these tests (CI values: check them before uncommenting):", "# env:")
		}
		for _, kv := range env {
			lines = append(lines, "#   "+kv[0]+": "+yamlStr(kv[1]))
		}
		out[name] = lines
	}
	return out
}

// envHint is the environment workflow steps set for a suite's tests.
type envHint struct {
	From string // the steps, for the comment
	Env  [][2]string
}

// workflowEnvHints collects, per suite, the env: of the workflow steps that run its
// unit tests (the workflow's, the job's and the step's own). smates.yml gets them only
// as comments: CI values (ports, database names) need not hold locally, and the step's
// env reaches the smates action in CI anyway.
//
// A variable that only carries the workflow's own orchestration (its value an expression
// of matrix, needs, steps, ... alone: a shard number, a commit to compare with) is left
// out unless the repository's code reads it (readByCode; nil: none does): the step's
// script uses it, the tests do not.
func workflowEnvHints(steps []testStep, suites []config.Suite, readByCode func(string) bool) map[string]envHint {
	out := map[string]envHint{}
	for _, st := range steps {
		if st.Other || len(st.Env) == 0 {
			continue
		}
		names, _ := stepSuites(st, suites)
		for _, n := range names {
			h := out[n]
			from := fmt.Sprintf("%s (job %q)", st.File, st.Job)
			if h.From == "" {
				h.From = from
			} else if !strings.Contains(h.From, from) {
				h.From += ", " + from
			}
			for _, kv := range st.Env {
				if ciOnlyValue(kv[1]) && (readByCode == nil || !readByCode(kv[0])) {
					continue
				}
				if !slices.ContainsFunc(h.Env, func(e [2]string) bool { return e[0] == kv[0] }) {
					h.Env = append(h.Env, kv)
				}
			}
			out[n] = h
		}
	}
	return out
}

// carryGoFlags moves the go test flags that change how a Go suite's tests run (-p,
// -tags, -race, -timeout: see goCarry) from the workflow steps and the Makefile test
// targets running it into the suite's command, and returns the flags each suite got.
// Sources that disagree combine toward the safer run (the lowest -p, -race, the longest
// -timeout), whatever part of the suite they run. -tags can also leave files out
// (//go:build !integration), so only the runs of all of the suite's unit tests vote on
// them: workflow steps the smates step can stand in for (Full) and the plain test
// targets of votes (makeGoVotes), a run without -tags voting for none; they are carried
// only when all votes agree. A test-integration target or a step testing a few packages
// never puts its tags on the whole suite.
func carryGoFlags(suites []config.Suite, mk, votes []makeHint, steps []testStep, stderr io.Writer) map[string]string {
	type source struct {
		c    goCarry
		from string
		vote bool // a run of all the suite's unit tests: its -tags count
	}
	per := map[string][]source{}
	for _, h := range mk {
		// Only a target run in the suite's own directory: a Makefile elsewhere may
		// test another module.
		if h.Exact && !h.Carry.empty() {
			per[h.Suite] = append(per[h.Suite], source{h.Carry, fmt.Sprintf("%s target %q", h.File, h.Target), false})
		}
	}
	for _, h := range votes {
		per[h.Suite] = append(per[h.Suite], source{h.Carry, fmt.Sprintf("%s target %q", h.File, h.Target), true})
	}
	for _, st := range steps {
		if st.Other || !slices.Equal(st.Runners, []string{"go"}) {
			continue
		}
		names, partial := stepSuites(st, suites)
		for _, n := range names {
			per[n] = append(per[n], source{st.Carry, fmt.Sprintf("%s (job %q)", st.File, st.Job), st.Full && n != partial})
		}
	}
	out := map[string]string{}
	for i := range suites {
		s := &suites[i]
		srcs := per[s.Name]
		if s.Runner != "go" || len(srcs) == 0 {
			continue
		}
		var merged goCarry
		var from, tagFrom []string
		var tags []string
		voted, conflict := false, false
		for _, src := range srcs {
			c := src.c
			c.Tags = nil
			merged, _ = merged.merge(c)
			if !src.c.empty() && !slices.Contains(from, src.from) {
				from = append(from, src.from)
			}
			switch {
			case !src.vote:
				if len(src.c.Tags) > 0 && !slices.Contains(tagFrom, src.from) {
					tagFrom = append(tagFrom, src.from)
				}
			case !voted:
				tags, voted = src.c.Tags, true
			case !slices.Equal(tags, src.c.Tags):
				conflict = true
			}
		}
		switch {
		case conflict:
			tags = nil
			fmt.Fprintf(stderr, "smates: suite %s: the runs of its unit tests use different -tags; smates.yml has one command per suite, so its command sets none (add a suite per set of tags, or set command: yourself)\n", s.Name)
		case len(tagFrom) > 0 && len(tags) == 0:
			fmt.Fprintf(stderr, "smates: suite %s: %s run only some of its tests with -tags; its command sets none (set command: yourself if all of its tests need them)\n", s.Name, strings.Join(tagFrom, ", "))
		}
		merged.Tags = tags
		if len(tags) > 0 {
			for _, src := range srcs {
				if src.vote && !slices.Contains(from, src.from) {
					from = append(from, src.from)
				}
			}
		}
		if merged.empty() {
			continue
		}
		cmd, changed, ok := withGoCarry(s.Command, merged)
		switch {
		case !ok:
			fmt.Fprintf(stderr, "smates: suite %s: %s run go test with %s, but its command is not go test: add them to it yourself\n", s.Name, strings.Join(from, ", "), merged)
		case changed:
			s.Command = cmd
			out[s.Name] = merged.String()
			fmt.Fprintf(stderr, "smates: suite %s: %s run go test with %s; its command in smates.yml keeps them: %s\n", s.Name, strings.Join(from, ", "), merged, cmd)
		}
	}
	return out
}

// ciContextRe finds the contexts a GitHub Actions expression refers to.
var ciContextRe = regexp.MustCompile(`\b(matrix|needs|steps|strategy|github|runner|job|jobs|secrets|vars|env|inputs)\s*[.\[]`)

// ciExprRe finds the expressions of a workflow value.
var ciExprRe = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// orchestration are the contexts that describe the run itself, not what the tests need.
var orchestration = map[string]bool{"matrix": true, "needs": true, "steps": true, "strategy": true, "github": true, "runner": true, "job": true, "jobs": true}

// ciOnlyValue reports whether a workflow env value is made of expressions over the run's
// orchestration contexts alone (matrix, needs, steps, github, ...): no secret, variable,
// input or env, and some context at all.
func ciOnlyValue(v string) bool {
	exprs := ciExprRe.FindAllStringSubmatch(v, -1)
	if len(exprs) == 0 {
		return false
	}
	seen := false
	for _, e := range exprs {
		for _, m := range ciContextRe.FindAllStringSubmatch(e[1], -1) {
			if !orchestration[m[1]] {
				return false
			}
			seen = true
		}
	}
	return seen
}
