package runner

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/trace"
)

func init() { Register(unittestRunner{}) }

// unittestRunner runs Python's standard library unittest (`python -m unittest`).
//
// A unit is a test module (file). Selected files are always passed as dotted module
// names relative to the discovery top-level directory, also for a full run, so the files
// smates knows are exactly the ones that run (unittest's own discovery skips directories
// without __init__.py). While recording, `-m unittest` becomes `-m smates_unittest` (a
// wrapper in the Python shims) that runs unittest's main with per-module attribution
// contexts and per-module results; every module still runs in one process.
type unittestRunner struct{}

func (unittestRunner) Name() string { return "unittest" }

func (u unittestRunner) Defaults(repo *gitx.Repo, root string) Defaults {
	return u.DefaultsFor(repo, root, "")
}

func (unittestRunner) DefaultsFor(repo *gitx.Repo, root, command string) Defaults {
	dir := filepath.Join(repo.Root, filepath.FromSlash(root))
	if command == "" {
		_, py := pythonInterpreter(repo.Root, dir)
		command = py + " -m unittest"
	}
	uc, _ := parseUnittestCommand(command)
	return Defaults{
		Tests:   []string{uc.testGlob()},
		Exclude: []string{"**/.venv/**", "**/venv/**", "**/site-packages/**", "**/node_modules/**", "**/build/**", "**/.tox/**", "**/.nox/**"},
		Inputs: []string{
			"pyproject.toml", "uv.lock", "setup.cfg", "setup.py", "tox.ini", "requirements*.txt", ".python-version",
			"/pyproject.toml", "/uv.lock", "/.python-version", "/requirements*.txt", "/.tool-versions",
		},
		Command: command,
	}
}

// Units keeps the test files unittest can load by module name: under the top-level
// directory, with every path segment a Python identifier.
func (unittestRunner) Units(s *Suite, testFiles []string) []string {
	uc, _ := parseUnittestCommand(s.Command)
	var out []string
	for _, f := range testFiles {
		if _, ok := uc.moduleName(relToSuite(s, f)); ok {
			out = append(out, f)
		}
	}
	return unitsAreFiles(out)
}

func (unittestRunner) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	uc, err := parseUnittestCommand(rc.Suite.Command)
	if err != nil {
		return nil, fmt.Errorf("suite %q: %w", rc.Suite.Name, err)
	}
	var modules, moduleUnits []string
	for _, u := range rc.Units {
		if m, ok := uc.moduleName(relToSuite(rc.Suite, u)); ok {
			modules = append(modules, m)
			moduleUnits = append(moduleUnits, u)
		}
	}
	start := time.Now()
	res := &RunResult{}
	if len(modules) == 0 {
		return res, nil
	}
	base := suiteEnv(rc.Suite)
	var env []string
	if uc.top != "." {
		// Discovery puts the top-level directory first on sys.path; module names are
		// relative to it.
		pp := filepath.Join(rc.Dir(), filepath.FromSlash(uc.top))
		if cur := Getenv(base, "PYTHONPATH"); cur != "" {
			pp += string(os.PathListSeparator) + cur
		}
		env = append(env, "PYTHONPATH="+pp)
		base = MergeEnv(base, env)
	}
	if rc.Record {
		// Node and Bun children of the tests trace themselves too.
		if err := os.MkdirAll(trace.V8Dir(rc.TraceDir), 0o755); err != nil {
			return nil, err
		}
		env = MergeEnv(env, trace.Env(rc.TraceDir, rc.Repo.Root, rc.Shims(), base))
	}
	if rc.Record && config.Bool(rc.Suite.Isolate, false) {
		byUnit := map[string]string{}
		var units []string
		for _, u := range rc.Units {
			if m, ok := uc.moduleName(relToSuite(rc.Suite, u)); ok {
				byUnit[u] = m
				units = append(units, u)
			}
		}
		codes, err := isolatedRun(ctx, rc, units, runtime.NumCPU(), func(u string, _ int) ([]string, []string, error) {
			return uc.argv(rc, true, []string{byUnit[u]}), env, nil
		})
		// A process exits 5 when its module ran no test. That fails the run only when
		// no module ran any test and one of them looks written for pytest, as in a
		// single process.
		empty := 0
		for _, c := range codes {
			if c == unittestNoTests {
				empty++
			}
		}
		worst := 0
		for _, c := range codes {
			if c != unittestNoTests {
				worst = max(worst, c)
			}
		}
		if empty > 0 && empty == len(codes) {
			if code, warn := noTestsOutcome(rc, units); code != 0 {
				worst = code
				res.Warnings = append(res.Warnings, warn)
			}
		}
		res.ExitCode, res.Duration = worst, time.Since(start)
		if err != nil {
			return res, err
		}
		obs, warns, readsBy, err := observeAll(rc, res.ExitCode, traceOptions{})
		for u, o := range obs {
			if c, ok := codes[u]; ok && o.Inferred {
				// No result from the wrapper (it failed to start): the exit code is the unit's own.
				setUnitExit(o, c)
			}
		}
		res.Observations, res.ReadsBy = obs, readsBy
		res.Warnings = append(res.Warnings, warns...)
		return res, err
	}
	worst, done := 0, 0
	for _, b := range chunk(modules) {
		batchUnits := moduleUnits[done : done+len(b)]
		done += len(b)
		code, err := rc.Exec(ctx, uc.argv(rc, rc.Record, b), env)
		if err != nil {
			res.ExitCode, res.Duration = code, time.Since(start)
			return res, err
		}
		if code == unittestNoTests {
			// "No tests ran" (Python 3.12+): helper modules matching the pattern are
			// fine; pytest-style tests under the wrong runner are not.
			var warn string
			if code, warn = noTestsOutcome(rc, batchUnits); warn != "" {
				res.Warnings = append(res.Warnings, warn)
			}
		}
		worst = max(worst, code)
	}
	res.ExitCode, res.Duration = worst, time.Since(start)
	if !rc.Record {
		return res, nil
	}
	var warns []string
	res.Observations, warns, res.ReadsBy, err = observeAll(rc, worst, traceOptions{})
	res.Warnings = append(res.Warnings, warns...)
	return res, err
}

// noTestsOutcome decides a unittest run in which none of units (repo-relative) ran a
// test: a failure (unittestNoTests, with a warning) when one of them looks written for
// pytest, which unittest cannot run; a pass otherwise (helper modules matching the test
// pattern, which `python -m unittest discover` accepts too).
func noTestsOutcome(rc *RunContext, units []string) (int, string) {
	var styled []string
	for _, u := range units {
		data, err := os.ReadFile(filepath.Join(rc.Repo.Root, filepath.FromSlash(u)))
		if err == nil && PytestStyle(data) {
			styled = append(styled, u)
		}
	}
	if len(styled) == 0 {
		return 0, ""
	}
	return unittestNoTests, fmt.Sprintf("unittest ran no test, and %s looks written for pytest (`import pytest` or module-level `def test_...` functions, which unittest does not run): set \"runner: pytest\" on suite %q in smates.yml", strings.Join(styled, ", "), rc.Suite.Name)
}

// pytestStyleRe finds what marks a Python test file written for pytest; PytestStyle
// checks the test function names it captures.
var pytestStyleRe = regexp.MustCompile(`(?m)^(?:import pytest\b|from pytest import|(?:async )?def (test_\w*))`)

// PytestStyle reports whether a Python test file is written for pytest: it imports
// pytest or defines module-level test functions (unittest runs neither those functions
// nor pytest's marks and fixtures). unittest's own `def test_suite()` convention does
// not count.
func PytestStyle(src []byte) bool {
	for _, m := range pytestStyleRe.FindAllSubmatch(src, -1) {
		if string(m[1]) != "test_suite" {
			return true
		}
	}
	return false
}

// unittestNoTests is unittest's exit code when no test ran (Python 3.12+).
const unittestNoTests = 5

// unittestCommand is a suite command split around `python -m unittest`.
type unittestCommand struct {
	// launcher is everything up to and including "-m" (e.g. "uv run python -m"); module
	// is the word naming the module ("unittest", or "-munittest" when joined).
	launcher []string
	module   string
	// opts are the unittest options kept for every run (-v, -k NAME, --locals, ...);
	// discovery arguments are removed: smates passes module names instead.
	opts []string
	// placeholder is the index in opts where "{tests}" stood (-1: append).
	placeholder int
	// start, pattern and top are the discovery settings (start and top relative to the
	// suite directory).
	start, pattern, top string
}

// parseUnittestCommand parses a suite command running `python -m unittest`, with or
// without `discover` and its options (-s/--start-directory, -p/--pattern,
// -t/--top-level-directory or the positional forms).
func parseUnittestCommand(command string) (*unittestCommand, error) {
	uc := &unittestCommand{placeholder: -1, start: ".", pattern: "test*.py"}
	words, err := SplitWords(command)
	if err != nil {
		return uc, err
	}
	at := -1
	for i, w := range words {
		if w == "-munittest" {
			uc.launcher, uc.module, at = words[:i], w, i+1
			break
		}
		if w == "-m" && i+1 < len(words) && words[i+1] == "unittest" {
			uc.launcher, uc.module, at = words[:i+1], words[i+1], i+2
			break
		}
	}
	if at < 0 {
		uc.top = "."
		return uc, fmt.Errorf("unittest runner: command %q does not run `python -m unittest` (use runner: custom for other commands)", command)
	}
	rest := words[at:]
	discover := len(rest) > 0 && rest[0] == "discover"
	if discover {
		rest = rest[1:]
	}
	top := ""
	var positional []string
	for i := 0; i < len(rest); i++ {
		w := rest[i]
		value := func() string {
			if i+1 < len(rest) {
				i++
				return rest[i]
			}
			return ""
		}
		if w == "{tests}" {
			uc.placeholder = len(uc.opts)
			continue
		}
		if discover {
			name, v, joined := strings.Cut(w, "=")
			switch name {
			case "-s", "--start-directory", "-p", "--pattern", "-t", "--top-level-directory":
				if !joined {
					v = value()
				}
				switch name {
				case "-s", "--start-directory":
					uc.start = v
				case "-p", "--pattern":
					uc.pattern = v
				default:
					top = v
				}
				continue
			}
			if len(w) > 2 && (strings.HasPrefix(w, "-s") || strings.HasPrefix(w, "-p") || strings.HasPrefix(w, "-t")) && !strings.HasPrefix(w, "--") {
				switch w[1] {
				case 's':
					uc.start = w[2:]
				case 'p':
					uc.pattern = w[2:]
				case 't':
					top = w[2:]
				}
				continue
			}
		}
		switch {
		case w == "-k" || w == "--durations":
			uc.opts = append(uc.opts, w, value())
		case strings.HasPrefix(w, "-"):
			uc.opts = append(uc.opts, w)
		default:
			positional = append(positional, w)
		}
	}
	if discover {
		// discover [start [pattern [top]]]
		for i, p := range positional {
			switch i {
			case 0:
				uc.start = p
			case 1:
				uc.pattern = p
			case 2:
				top = p
			}
		}
	}
	// Positional test names of a plain `python -m unittest tests.test_x` are dropped:
	// smates passes the selected modules itself.
	uc.start = cleanRel(uc.start)
	if top == "" {
		top = uc.start
	}
	uc.top = cleanRel(top)
	if uc.pattern == "" {
		uc.pattern = "test*.py"
	}
	return uc, nil
}

func cleanRel(p string) string {
	if p == "" {
		return "."
	}
	return path.Clean(filepath.ToSlash(p))
}

// testGlob is the suite-relative glob of the files discovery would load.
func (uc *unittestCommand) testGlob() string {
	if uc.start == "." || path.IsAbs(uc.start) || uc.start == ".." || strings.HasPrefix(uc.start, "../") {
		return "**/" + uc.pattern
	}
	return uc.start + "/**/" + uc.pattern
}

var pyIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// moduleName converts a suite-relative .py file to its dotted module name relative to
// the top-level directory (ok false when unittest cannot load it by name).
func (uc *unittestCommand) moduleName(rel string) (string, bool) {
	rel = filepath.ToSlash(rel)
	if uc.top != "." {
		if !strings.HasPrefix(rel, uc.top+"/") {
			return "", false
		}
		rel = rel[len(uc.top)+1:]
	}
	rel, ok := strings.CutSuffix(rel, ".py")
	if !ok || rel == "" {
		return "", false
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if !pyIdentRe.MatchString(p) {
			return "", false
		}
	}
	return strings.Join(parts, "."), true
}

// argv builds the command line running the given modules; record runs them through
// the smates_unittest wrapper.
func (uc *unittestCommand) argv(rc *RunContext, record bool, modules []string) []string {
	argv := append([]string{}, uc.launcher...)
	switch {
	case record && uc.module == "-munittest":
		argv = append(argv, "-msmates_unittest")
	case record:
		argv = append(argv, "smates_unittest")
	default:
		argv = append(argv, uc.module)
	}
	tests := append(append([]string{}, rc.ExtraArgs...), modules...)
	if uc.placeholder < 0 {
		return append(append(argv, uc.opts...), tests...)
	}
	argv = append(argv, uc.opts[:uc.placeholder]...)
	argv = append(argv, tests...)
	return append(argv, uc.opts[uc.placeholder:]...)
}
