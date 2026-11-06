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
	Tags    []string // -tags, sorted (nil: not given; empty: -tags "" clears them)
	Race    bool
	Timeout string // -timeout ("": not given)
	// raceSet is set when -race is given at all (-race=false too): it then wins over
	// the -race of GOFLAGS (see over).
	raceSet bool
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
			if c.Tags == nil {
				c.Tags = []string{} // given, empty: it clears the tags of GOFLAGS
			}
		case "race":
			c.Race = a.Value == "" || a.Value == "true" || a.Value == "1"
			c.raceSet = true
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

// over returns c with the flags it does not set taken from base: flags on the go test
// command line win over those of GOFLAGS (base).
func (c goCarry) over(base goCarry) goCarry {
	if c.P == 0 {
		c.P = base.P
	}
	if c.Tags == nil {
		c.Tags = base.Tags
	}
	if !c.raceSet {
		c.Race, c.raceSet = base.Race, base.raceSet
	}
	if c.Timeout == "" {
		c.Timeout = base.Timeout
	}
	return c
}

// envGoCarry reads the carried flags (see goCarry) of the GOFLAGS that env (the env:
// of a workflow, job and step, in order) sets; ok is false when smates cannot read it
// (a ${{ }} expression, a $VAR).
func envGoCarry(env [][2]string) (c goCarry, ok bool) {
	v, _ := envGoflags(env)
	return goflagsCarry(v)
}

// envGoflags is the GOFLAGS env sets (the last one), and whether it sets one.
func envGoflags(env [][2]string) (v string, set bool) {
	for _, kv := range env {
		if kv[0] == "GOFLAGS" {
			v, set = kv[1], true
		}
	}
	return v, set
}

// goflagsCarry reads the carried flags (see goCarry) of a GOFLAGS value; ok is false
// when smates cannot read it (a ${{ }} expression, a $VAR, a command substitution).
func goflagsCarry(v string) (c goCarry, ok bool) {
	if strings.ContainsAny(v, "$`") {
		return goCarry{}, false
	}
	args, _ := runner.ParseGoTestArgs(strings.Fields(v), true)
	return goCarryOf(args), true
}

// goflagsFilter returns the first flag of GOFLAGS value v that leaves tests out
// (-short, -run, -skip), "" when there is none.
func goflagsFilter(v string) string {
	args, _ := runner.ParseGoTestArgs(strings.Fields(v), true)
	return goArgsFilter(args)
}

// goArgsFilter returns the first go test flag of args that leaves tests out (-short,
// -run, -skip), "" when there is none.
func goArgsFilter(args []runner.GoArg) string {
	for _, a := range args {
		switch n := strings.TrimPrefix(a.Name, "test."); n {
		case "short", "run", "skip":
			return "-" + n
		}
	}
	return ""
}

// envOverride says why the smates step cannot stand in for a go test step whose
// command line (cmd) turns off a flag of the GOFLAGS of its env: (base) that its
// command in smates.yml cannot turn off ("" when it does not): -race=false, -tags "".
// The smates step keeps the env:, so it would run with them, and both change which
// files compile (the race build tag, //go:build !integration).
func envOverride(cmd, base goCarry) string {
	switch {
	case base.Race && cmd.raceSet && !cmd.Race:
		return "it turns -race off on its command line, over the -race of GOFLAGS in its env:, which the smates step keeps (the race build tag can leave out test files the step compiles): drop -race from GOFLAGS for this step, or switch the step by hand"
	case len(base.Tags) > 0 && cmd.Tags != nil && len(cmd.Tags) == 0:
		return "it clears the -tags of GOFLAGS in its env: on its command line, which the smates step keeps (the tags can leave out test files the step compiles): drop -tags from GOFLAGS for this step, or switch the step by hand"
	}
	return ""
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
	// full is set for a go test command read in full that runs every test of the
	// packages of its directory (./... or ., no filter): its -tags are those of the
	// suite's unit tests (see carryGoFlags).
	full bool
	// paths are the directories a pytest command names (literal, relative to the
	// step's directory): it runs only the tests under them, which stepPatch accepts
	// only when each is the root of a suite it covers.
	paths []string
	// inlineGoflags is set when the line sets GOFLAGS for the command (GOFLAGS=...
	// go test): it replaces the GOFLAGS of the step's env:. inlineValue is its value.
	inlineGoflags bool
	inlineValue   string
}

// Flags of the JavaScript and Python runners, and of the package managers running
// them, that pick tests or packages or write reports.
var (
	runnerSelectFlags = []string{"-t", "--testNamePattern", "--testPathPattern", "--testPathPatterns", "--testPathIgnorePatterns", "--project", "--selectProjects", "--shard", "--changed", "--changedSince", "--onlyChanged", "-o", "--related", "--findRelatedTests", "--dir", "--exclude",
		"-k", "-m", "--lf", "--last-failed", "--ff", "--deselect", "--ignore", "--ignore-glob", "--pyargs",
		"-w", "--workspace", "--workspaces", "--filter", "-F", "--scope"}
	runnerReportFlags = []string{"--junitxml", "--junit-xml", "--outputFile", "--json", "--outputJson", "--resultlog", "--report-log"}
	// runnerValueFlags take the next word as their value (written without "="); any
	// other word that is not a flag names tests to run (a file filter, a directory).
	runnerValueFlags = []string{"--reporter", "--reporters", "--config", "-c", "--environment", "--testEnvironment", "--pool", "--maxWorkers", "--maxConcurrency", "--testTimeout", "--root", "--mode", "--color",
		"-n", "--numprocesses", "--dist", "-p", "--rootdir", "--tb", "--maxfail", "--durations", "-W", "--cov-report", "--timeout", "--log-level", "--log-cli-level", "--import-mode", "--basetemp", "--capture", "-r"}
)

// envName reports whether s is a shell variable name.
func envName(s string) bool {
	if s == "" || s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// goCarryFlags are the flags of an inline GOFLAGS the smates step can stand in for:
// the carried ones (goCarry) and those that do not change what runs or passes.
var goCarryFlags = []string{"p", "tags", "race", "timeout", "count", "v", "failfast", "shuffle"}

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
	// Words before the test command: variable assignments (after an optional env)
	// apply to it only, and the smates step replacing the step would lose them. An
	// inline GOFLAGS is read as go test flags (carried, or refused like the command's
	// own); any other variable must move to the step's env:, which the smates step keeps.
	var goflags []string
	pre := words[:at]
	if len(pre) > 0 && path.Base(pre[0]) == "env" {
		pre = pre[1:]
		if len(pre) > 0 && strings.HasPrefix(pre[0], "-") {
			c.skip = "it runs the tests through env " + pre[0] + ", which changes their environment: run them without it, then use the smates action"
			return c
		}
	}
	for _, w := range pre {
		name, val, ok := strings.Cut(w, "=")
		if !ok || !envName(name) {
			continue
		}
		lit, literal := shellLiteral(val)
		if name != "GOFLAGS" || !strings.HasPrefix(match, "go test") {
			c.skip = "it sets " + name + " for the test command on its line, which the smates step would lose: move it to the step's env: (the smates step keeps it), then rerun"
			return c
		}
		if !literal {
			c.skip = "its GOFLAGS value comes from the environment: set the flags in the suite's command in smates.yml, then switch the step by hand"
			return c
		}
		for _, f := range strings.Fields(lit) {
			fname, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
			if !slices.Contains(goCarryFlags, strings.TrimPrefix(fname, "test.")) && fname != "run" && fname != "skip" && fname != "short" {
				c.skip = "it sets GOFLAGS=" + lit + " on its line, and smates does not carry " + f + " into the suite's command: move GOFLAGS to the step's env: (the smates step keeps it), then rerun"
				return c
			}
		}
		goflags, c.inlineGoflags, c.inlineValue = strings.Fields(lit), true, lit
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
		// GOFLAGS come first: the command line's flags win over them.
		gc := checkGoTestArgs(append(goflags, args...))
		gc.inlineGoflags, gc.inlineValue = c.inlineGoflags, c.inlineValue
		return gc
	case strings.Contains(match, "turbo"):
		c.skip = "turbo runs the tests through its own task graph and cache (and --filter picks packages): smates does not replace it; switch to `uses: " + actSmates + "` with `args: --suite <name>` per package by hand"
		return c
	case match == "node --test":
		c.skip = "node --test runs the files it is given: switch it to the smates action by hand if a node suite covers them"
		return c
	}
	for i := 0; i < len(args); i++ {
		w := args[i]
		name, _, hasValue := strings.Cut(w, "=")
		switch {
		case w == "--" || w == "run" && i == 0:
		case slices.Contains(runnerSelectFlags, name):
			c.skip = "it passes " + name + ", which picks the tests (or packages) to run: smates selects among all of the suite's tests"
			return c
		case slices.Contains(runnerReportFlags, name):
			c.skip = "it passes " + name + ", which writes a report a later step may read: drop it if nothing does, then rerun"
			return c
		case slices.Contains(runnerValueFlags, name) && !hasValue:
			i++ // its value
		case strings.HasPrefix(w, "-"):
		default:
			// A word naming tests: vitest and jest take it as a filter on file paths
			// (matching anywhere in them), pytest as a path to collect from.
			lit, ok := shellLiteral(w)
			switch {
			case ok && lit == ".":
			case ok && match == "pytest" && !strings.ContainsAny(lit, "*?[:"):
				c.paths = append(c.paths, path.Clean(lit))
			default:
				c.skip = "it runs only the tests matching " + w + ": smates selects among all of the suite's tests; switch it by hand if those are the suite's tests"
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
		case name == "short":
			note(&skip, "it passes -short, which skips the long tests: smates would run them; switch the step by hand if its job can run them (or add -short to the suite's command)")
		case name == "json":
			note(&skip, "its -json output feeds another tool (a report, or a sharding pipeline): smates does not replace it")
		case name == "coverprofile" || strings.HasSuffix(name, "profile") || name == "trace" || name == "outputdir":
			note(&skip, "it passes -"+name+", which writes a file a later step may read; drop it if nothing does, then rerun")
		case (name == "p" || name == "tags" || name == "timeout") && (dynamic[first] || len(a.Words) > 1 && dynamic[first+1]):
			note(&skip, "its "+a.Words[0]+" value comes from the environment: set it in the suite's command in smates.yml, then switch the step by hand")
		}
	}
	c.carry = goCarryOf(known)
	c.full = other == "" && skip == "" && len(known) == len(args)
	switch {
	case other != "":
		c.skip, c.other = other, true
	case skip != "":
		c.skip = skip
	}
	return c
}
