package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"
)

type runFlags struct {
	commonFlags
	callerFlags
	all      bool
	selectOn bool
	base     string
	noAgent  bool
	noRecord bool
	shadow   bool
	failFast bool
	shard    string
	dryRun   bool
	explain  bool
	summary  string
	markdown string
	report   string
	// output is the output mode (outputFull, outputFailures); outputFlag is set when
	// --output gave it. only are the --only tests.
	output     string
	outputFlag bool
	only       stringList
	onlySpecs  []onlySpec
	// extra are the arguments passed through to the runner; shardI/shardN the parsed
	// --shard (0/0 when not sharded).
	extra          []string
	shardI, shardN int
	// budget is --budget; heartbeat the progress interval of failures mode (0: none).
	budget    time.Duration
	heartbeat time.Duration
	// maxTime is --max-time: no batch starts that would end after it (0: no limit).
	maxTime time.Duration
	// deferred receives, at the end of a run, one line saying which selected units did
	// not run and stay selected (--max-time, unmet requires); the coding agent hooks
	// print it instead of blocking.
	deferred func(string)
	// skipEmpty returns right after the selection when it runs nothing, saving only
	// the agent's new answer and the orphans the policy handled (saveWithoutRun): the
	// Claude Code Stop hook's fast path.
	skipEmpty bool
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	f, err := parseRunFlags(args, stderr)
	if err != nil {
		return 2, err
	}
	code, err := runCycle(ctx, f, stdout, stderr, nil)
	return testsExitCode(code), err
}

// testsExitCode is the exit code of smates for the exit code of the test runs: 1 when
// they failed, whatever code a runner exited with (codes above 1 tell smates' own
// errors apart, see Main).
func testsExitCode(code int) int {
	return min(code, ExitFailed)
}

// parseRunFlags parses the arguments of `smates run`.
func parseRunFlags(args []string, stderr io.Writer) (*runFlags, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f runFlags
	f.commonFlags.register(fs)
	f.callerFlags.register(fs)
	fs.BoolVar(&f.all, "all", false, "run every test (and rewrite the recorded state)")
	fs.BoolVar(&f.selectOn, "select", false, "select tests even where the default is a full run (default branch on CI)")
	fs.StringVar(&f.base, "base", "", "git revision to diff against when nothing is recorded yet")
	fs.BoolVar(&f.noAgent, "no-agent", false, "do not call the agent for unattributed changes")
	fs.BoolVar(&f.noRecord, "no-record", false, "run without tracing and without updating the state")
	fs.BoolVar(&f.shadow, "shadow", false, "run every test but report which failures the selection would have missed")
	fs.BoolVar(&f.failFast, "fail-fast", false, "start no further test batch after one fails (tests run likeliest-to-fail first)")
	fs.StringVar(&f.shard, "shard", "", "run shard `I/N` of the selected tests (tests are assigned to shards by a stable hash)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the commands instead of running them")
	fs.BoolVar(&f.explain, "explain", false, "print why each test was selected")
	fs.StringVar(&f.summary, "summary", "", "append a markdown summary to `file` (default: $GITHUB_STEP_SUMMARY on GitHub Actions)")
	fs.StringVar(&f.report, "report", "", "write a JSON report (selection, per-test results, shadow escapes) to `file` (- for stdout; all other output then goes to stderr)")
	fs.StringVar(&f.output, "output", "", "`mode`: full streams the runner output; failures prints only the output of failing tests, then a short summary with a command that reruns them (default: $SMATES_OUTPUT, where agent means failures, else full)")
	fs.Var(&f.only, "only", "run exactly the test unit `SUITE:UNIT` (repeatable; recorded as usual)")
	fs.StringVar(&f.markdown, "markdown", "", "write the summary as Markdown to `file`, e.g. for a pull request comment")
	fs.DurationVar(&f.budget, "budget", defaultBudget, "print how to split the run when the recorded durations of the selected tests add up to more than this (0: never)")
	fs.DurationVar(&f.maxTime, "max-time", 0, "start no test batch that would end after `duration` by recorded durations; the tests left out stay selected (0: no limit)")
	if err := fs.Parse(args); err != nil {
		return nil, usageError{err}
	}
	if f.maxTime < 0 {
		return nil, usageError{fmt.Errorf("--max-time must not be negative (got %s)", f.maxTime)}
	}
	f.extra = fs.Args()
	f.heartbeat = heartbeatInterval()
	if err := f.callerFlags.validate(); err != nil {
		return nil, err
	}
	var err error
	fs.Visit(func(fl *flag.Flag) { f.outputFlag = f.outputFlag || fl.Name == "output" })
	if f.output, err = parseOutputMode(f.output, stderr); err != nil {
		return nil, err
	}
	if f.onlySpecs, err = parseOnly(f.only); err != nil {
		return nil, err
	}
	if len(f.onlySpecs) > 0 && (f.all || f.shadow) {
		return nil, usageError{fmt.Errorf("--only runs exactly the named tests and cannot be combined with --all or --shadow")}
	}
	if len(f.onlySpecs) > 0 && (len(f.adds) > 0 || f.orphans != "") {
		return nil, usageError{fmt.Errorf("--only runs exactly the named tests and cannot be combined with --add or --orphans")}
	}
	if f.shardI, f.shardN, err = parseShard(f.shard); err != nil {
		return nil, err
	}
	if f.shadow && f.failFast {
		return nil, usageError{fmt.Errorf("--shadow runs every test and cannot be combined with --fail-fast")}
	}
	if f.shadow && f.maxTime > 0 {
		return nil, usageError{fmt.Errorf("--shadow runs every test and cannot be combined with --max-time")}
	}
	if f.dryRun && (f.report != "" || f.markdown != "" || f.summary != "") {
		return nil, usageError{fmt.Errorf("--dry-run runs nothing and cannot be combined with --report, --markdown or --summary (use `smates select --format json|markdown` for the selection)")}
	}
	return &f, nil
}
