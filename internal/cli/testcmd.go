package cli

// The test command of a workflow step or Makefile recipe: whether the smates action
// can stand in for it, and the go test flags a suite's command has to keep.

import (
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/runner"
)

// goCarry are the go test flags that change how the tests of a suite run and that its
// command in smates.yml must keep when the smates action replaces the step: -p
// (packages built and tested at once: 1 for tests sharing a database), -tags (which
// files compile), -race and -timeout. -count, -v and the like do not change what
// passes; -run and other filters are refused instead (testCmdCheck).
type goCarry struct {
	P       int      // -p (0: not given)
	Tags    []string // -tags, sorted
	Race    bool
	Timeout string // -timeout ("": not given)
}

func (c goCarry) empty() bool {
	return c.P == 0 && len(c.Tags) == 0 && !c.Race && c.Timeout == ""
}

// words renders the flags as go test arguments.
func (c goCarry) words() []string {
	var out []string
	if c.P > 0 {
		out = append(out, "-p", strconv.Itoa(c.P))
	}
	if len(c.Tags) > 0 {
		out = append(out, "-tags", strings.Join(c.Tags, ","))
	}
	if c.Race {
		out = append(out, "-race")
	}
	if c.Timeout != "" {
		out = append(out, "-timeout", c.Timeout)
	}
	return out
}

func (c goCarry) String() string { return strings.Join(c.words(), " ") }

// goCarryOf reads the carried flags of go test arguments (the last of a repeated
// flag wins, as with go test).
func goCarryOf(args []runner.GoArg) goCarry {
	var c goCarry
	for _, a := range args {
		switch a.Name {
		case "p":
			if n, err := strconv.Atoi(a.Value); err == nil && n > 0 {
				c.P = n
			}
		case "tags":
			c.Tags = strings.FieldsFunc(a.Value, func(r rune) bool { return r == ',' || r == ' ' })
			slices.Sort(c.Tags)
			c.Tags = slices.Compact(c.Tags)
		case "race":
			c.Race = a.Value == "" || a.Value == "true" || a.Value == "1"
		case "timeout", "test.timeout":
			c.Timeout = a.Value
		}
	}
	return c
}

// timeoutLess reports whether go test timeout a is shorter than b ("0" is no
// timeout, "" go test's default of 10m).
func timeoutLess(a, b string) bool {
	d := func(s string) time.Duration {
		if s == "" {
			return 10 * time.Minute
		}
		v, err := time.ParseDuration(s)
		if err != nil || v <= 0 {
			return 1<<63 - 1
		}
		return v
	}
	return d(a) < d(b)
}

// merge combines the flags of two commands running the same tests toward the safer
// run: the lower -p, -race if either has it, the longer -timeout. Tags are kept only
// when both agree: a tag can also leave files out (//go:build !integration), so a
// union could skip tests one of them ran (conflict is then true).
func (c goCarry) merge(o goCarry) (goCarry, bool) {
	out := c
	if o.P > 0 && (out.P == 0 || o.P < out.P) {
		out.P = o.P
	}
	out.Race = c.Race || o.Race
	if o.Timeout != "" && timeoutLess(c.Timeout, o.Timeout) {
		out.Timeout = o.Timeout
	}
	if !slices.Equal(c.Tags, o.Tags) {
		out.Tags = nil
		return out, true
	}
	return out, false
}

// unmet lists the flags of c that a suite command running with have does not honour:
// a higher or missing -p, no -race, a shorter -timeout, other -tags.
func (c goCarry) unmet(have goCarry) []string {
	var out []string
	if c.P > 0 && (have.P == 0 || have.P > c.P) {
		out = append(out, "-p "+strconv.Itoa(c.P))
	}
	if c.Race && !have.Race {
		out = append(out, "-race")
	}
	if c.Timeout != "" && timeoutLess(have.Timeout, c.Timeout) {
		out = append(out, "-timeout "+c.Timeout)
	}
	if !slices.Equal(c.Tags, have.Tags) {
		if len(c.Tags) == 0 {
			out = append(out, "no -tags")
		} else {
			out = append(out, "-tags "+strings.Join(c.Tags, ","))
		}
	}
	return out
}

// goSuiteCommand splits a go suite's command ("" is smates' default, go test) into the
// words up to "test" and its arguments; ok is false when it is not go test.
func goSuiteCommand(command string) (head, args []string, ok bool) {
	if command == "" {
		command = "go test"
	}
	words, err := runner.SplitWords(command)
	if err != nil || len(words) < 2 || path.Base(words[0]) != "go" || words[1] != "test" {
		return nil, nil, false
	}
	return words[:2], words[2:], true
}

// withGoCarry returns a go suite's command with the flags of c added (merged with its
// own, see merge; a command with -tags keeps its own), and whether it changed. ok is
// false when the command is not go test.
func withGoCarry(command string, c goCarry) (out string, changed, ok bool) {
	head, args, ok := goSuiteCommand(command)
	if !ok {
		return command, false, false
	}
	parsed, tail := runner.ParseGoTestArgs(args, false)
	have := goCarryOf(parsed)
	merged, _ := have.merge(c)
	merged.Tags = have.Tags
	if len(have.Tags) == 0 {
		merged.Tags = c.Tags
	}
	if merged.equal(have) {
		return command, false, true
	}
	words := slices.Clone(head)
	words = append(words, merged.words()...)
	for _, a := range parsed {
		switch a.Name {
		case "p", "tags", "race", "timeout", "test.timeout":
			continue
		}
		words = append(words, a.Words...)
	}
	if tail != nil {
		words = append(append(words, "-args"), tail...)
	}
	for i, w := range words {
		words[i] = shellQuote(w)
	}
	return strings.Join(words, " "), true, true
}

func (c goCarry) equal(o goCarry) bool {
	return c.P == o.P && slices.Equal(c.Tags, o.Tags) && c.Race == o.Race && c.Timeout == o.Timeout
}

// ---- workflow step commands ----

// testCmdCheck says whether the smates action can stand in for a test command line:
// skip is why not ("" when it can), other is set when the command does not run the
// unit tests as a whole (-run, -list, compile-only), so it is no duplicate of what
// smates runs. carry holds the go test flags a suite's command must keep.
type testCmdCheck struct {
	skip  string
	other bool
	carry goCarry
}

// Flags of the JavaScript and Python runners that pick tests or write reports.
var (
	runnerSelectFlags = []string{"-t", "--testNamePattern", "--testPathPattern", "--testPathPatterns", "--testPathIgnorePatterns", "--project", "--selectProjects", "--shard", "--changed", "--changedSince", "--onlyChanged", "-o", "--related", "--findRelatedTests", "--dir", "--exclude",
		"-k", "-m", "--lf", "--last-failed", "--ff", "--deselect", "--ignore", "--ignore-glob", "--pyargs"}
	runnerReportFlags = []string{"--junitxml", "--junit-xml", "--outputFile", "--json", "--outputJson", "--resultlog", "--report-log"}
)

// checkTestCmd reads the test command line cmd of a workflow step; match is the test
// command testCmdRe found in it ("go test", "pnpm test", "turbo run test", ...).
func checkTestCmd(cmd, match string) testCmdCheck {
	var c testCmdCheck
	if strings.Contains(cmd, "${{") {
		if strings.Contains(cmd, "matrix.") {
			c.skip = "the command changes with the matrix leg (${{ matrix.* }}): smates cannot tell which suite each leg runs; give each leg `args: --suite <name>` and `cache-key: <name>` by hand"
		} else {
			c.skip = "the command is built from a ${{ }} expression: smates cannot tell which tests it runs"
		}
		return c
	}
	words := shellWords(cmd)
	mw := strings.Fields(match)
	at := -1
	for i := 0; i+len(mw) <= len(words) && at < 0; i++ {
		ok := true
		for j, m := range mw {
			if w := words[i+j]; w != m && !(j == 0 && path.Base(w) == m) {
				ok = false
				break
			}
		}
		if ok {
			at = i
		}
	}
	if at < 0 {
		c.skip = "smates could not find " + strconv.Quote(match) + " as a command in the line"
		return c
	}
	// The test command must be the only command of the line.
	to := at
	for to < len(words) && !shellControlOp(words[to]) {
		to++
	}
	for i, w := range words {
		if !shellControlOp(w) {
			continue
		}
		if (w == "|" || w == "|&") && i >= to {
			c.skip = "it pipes the test output into another command (" + strings.Join(words[i+1:], " ") + "); run that in its own step, then use the smates action for the tests"
		} else {
			c.skip = "the test command is chained with other commands on one line; split it out, then use the smates action for the test part"
		}
		return c
	}
	var args []string
	for i := at + len(mw); i < to; i++ {
		w := words[i]
		if shellRedirect(w) {
			target := ""
			if !strings.Contains(w, "&") && i+1 < to {
				i++
				target = words[i]
			}
			if strings.Contains(w, ">") && target != "" && target != "/dev/null" {
				c.skip = "it writes the test output to " + target + ", which a later step may read; drop the redirection if nothing does, then rerun"
				return c
			}
			continue
		}
		args = append(args, w)
	}
	switch {
	case strings.HasPrefix(match, "go test"):
		return checkGoTestArgs(args)
	case strings.Contains(match, "turbo"):
		c.skip = "turbo runs the tests through its own task graph and cache (and --filter picks packages): smates does not replace it; switch to `uses: " + actSmates + "` with `args: --suite <name>` per package by hand"
		return c
	case match == "node --test":
		c.skip = "node --test runs the files it is given: switch it to the smates action by hand if a node suite covers them"
		return c
	}
	for i := 0; i < len(args); i++ {
		w := args[i]
		name, _, _ := strings.Cut(w, "=")
		switch {
		case w == "--" || w == "run" && i == 0:
		case slices.Contains(runnerSelectFlags, name):
			c.skip = "it passes " + name + ", which picks the tests to run: smates selects among all of the suite's tests"
			return c
		case slices.Contains(runnerReportFlags, name):
			c.skip = "it passes " + name + ", which writes a report a later step may read: drop it if nothing does, then rerun"
			return c
		case strings.HasPrefix(w, "-"):
		default:
			lit, ok := shellLiteral(w)
			if !ok || strings.ContainsAny(lit, "/*?") {
				c.skip = "it runs only the tests under " + w + ": smates selects among all of the suite's tests; switch it by hand if those are the suite's tests"
				return c
			}
		}
	}
	return c
}

// checkGoTestArgs reads the arguments after go test.
func checkGoTestArgs(words []string) testCmdCheck {
	var c testCmdCheck
	lits := make([]string, len(words))
	dynamic := map[int]bool{}
	for i, w := range words {
		if l, ok := shellLiteral(w); ok {
			lits[i] = l
		} else {
			lits[i], dynamic[i] = w, true
		}
	}
	args, tail := runner.ParseGoTestArgs(lits, false)
	other, skip := "", ""
	note := func(s *string, why string) {
		if *s == "" {
			*s = why
		}
	}
	if tail != nil {
		note(&skip, "it passes -args to the test binaries; move them to the suite's command in smates.yml, then switch the step by hand")
	}
	i := 0                   // index of the argument's first word in words
	var known []runner.GoArg // the arguments with a literal value
	for _, a := range args {
		first := i
		i += len(a.Words)
		name := strings.TrimPrefix(a.Name, "test.")
		if !dynamic[first] && (len(a.Words) == 1 || !dynamic[first+1]) {
			known = append(known, a)
		}
		switch {
		case a.Name == "" && (a.Words[0] == "-" || a.Words[0] == "--"):
		case a.Name == "":
			p := a.Words[0]
			switch {
			case p == "./..." || p == ".":
			case dynamic[first]:
				note(&skip, "the packages come from "+words[first]+" at run time (a filtered list or a shard): smates selects among all of the suite's packages; switch the step by hand")
			default:
				note(&skip, "it tests only "+p+": smates selects among all of the suite's packages (the job may not provide what the others need); switch it by hand if it should run them")
			}
		case name == "run" || name == "list" || name == "bench" || name == "fuzz":
			note(&other, "it passes -"+name+", so it runs only some tests (or none: -run '^$' compiles them); smates does not replace it")
		case name == "c" || name == "o":
			note(&other, "it passes -"+name+": it builds test binaries instead of running the tests; smates does not replace it")
		case name == "skip":
			note(&skip, "it passes -skip, which leaves tests out: smates selects among all of the suite's tests")
		case name == "json":
			note(&skip, "its -json output feeds another tool (a report, or a sharding pipeline): smates does not replace it")
		case name == "coverprofile" || strings.HasSuffix(name, "profile") || name == "trace" || name == "outputdir":
			note(&skip, "it passes -"+name+", which writes a file a later step may read; drop it if nothing does, then rerun")
		case (name == "p" || name == "tags" || name == "timeout") && (dynamic[first] || len(a.Words) > 1 && dynamic[first+1]):
			note(&skip, "its "+a.Words[0]+" value comes from the environment: set it in the suite's command in smates.yml, then switch the step by hand")
		}
	}
	c.carry = goCarryOf(known)
	switch {
	case other != "":
		c.skip, c.other = other, true
	case skip != "":
		c.skip = skip
	}
	return c
}
