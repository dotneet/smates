package runner

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/trace"
)

func init() { Register(bun{}) }

type bun struct{}

func (bun) Name() string { return "bun" }

func (bun) Defaults(repo *gitx.Repo, root string) Defaults {
	return Defaults{
		Tests:   []string{"**/*.{test,spec}." + jsTestExts, "**/*_{test,spec}." + jsTestExts},
		Exclude: jsExclude,
		Inputs:  jsLockInputs(root),
		Command: "bun test",
	}
}

func (bun) Units(s *Suite, testFiles []string) []string { return unitsAreFiles(testFiles) }

func (bun) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	if !rc.Record {
		start := time.Now()
		code, err := runBatches(ctx, rc, nil, rc.RelArgs("./"), nil)
		res := &RunResult{ExitCode: code, Duration: time.Since(start)}
		if err == nil {
			// Only a JUnit report (junit: in smates.yml) gives per-file status.
			res.Observations, res.Warnings = reported(rc)
		}
		return res, err
	}
	// Bun runs all files in one process and does not support NODE_V8_COVERAGE, so while
	// recording each file runs in its own process with lcov coverage (loaded modules)
	// and the preload shim (files read). The shim must load before the project's own
	// preloads (bunfig [test].preload), which may import node:fs first: smates passes a
	// copy of bunfig.toml with the shim prepended. Child processes get the shim through
	// BUN_OPTIONS (bun, also preloading it into bun test itself: loaded once) and
	// NODE_OPTIONS (node), both set by NodeEnv.
	shim := rc.Shims().Node
	env := trace.Env(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
	if err := os.MkdirAll(trace.V8Dir(rc.TraceDir), 0o755); err != nil {
		return nil, err
	}
	cfg, cleanup, err := bunConfigWithShim(rc, shim)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	pre := []string{"--config=" + cfg}
	start := time.Now()
	// bun test runs files one after another; running them concurrently could make
	// tests that share ports, databases or directories collide.
	codes, err := isolatedRun(ctx, rc, rc.Units, 1, func(u string, i int) ([]string, []string, error) {
		cov := filepath.Join(rc.TraceDir, fmt.Sprintf("bun-%d", i))
		extra := append(append([]string{}, pre...), "--coverage", "--coverage-reporter=lcov", "--coverage-dir="+cov)
		argv, err := rc.CommandLine(extra, []string{"./" + relToSuite(rc.Suite, u)})
		return argv, env, err
	})
	worst := worstCode(codes)
	res := &RunResult{ExitCode: worst, Duration: time.Since(start)}
	if err != nil {
		return res, err
	}
	obs, warns, err := observe(rc, worst, traceOptions{unitsFromArgv: true})
	if err != nil {
		return res, err
	}
	for i, u := range rc.Units {
		o := obs[u]
		if o == nil {
			o = &Observation{Files: map[string]bool{}}
			obs[u] = o
		}
		files, err := readLcov(filepath.Join(rc.TraceDir, fmt.Sprintf("bun-%d", i), "lcov.info"), rc.Dir(), rc.Repo.Root)
		if err != nil && codes[u] == 0 && bunImportsSource(rc, u) {
			// Bun writes no report when no source file was covered: a passing test
			// that imports some would be recorded without them.
			delete(obs, u)
			continue
		}
		// Otherwise a missing report is no loss: the test is self-contained (bun skips
		// test files in coverage), or it failed, also when it could not import a
		// module, which ends bun before it writes coverage.
		for _, f := range files {
			o.AddFile(f)
		}
		o.AddFile(u)
		// Read by bun natively, not through node:fs; without one, bun would read one
		// created later.
		if snap := bunSnapshotFile(u); exists(filepath.Join(rc.Repo.Root, filepath.FromSlash(snap))) {
			o.AddFile(snap)
		} else {
			o.AddFile(depkey.Existence(snap))
		}
		setUnitExit(o, codes[u])
	}
	res.Observations, res.Warnings = obs, warns
	return res, nil
}

// bunImportsSource reports whether the static closure of test file unit has repository
// code bun's coverage would list: a JavaScript or TypeScript file that is neither a test
// file nor a suite input (true when the closure is unknown).
func bunImportsSource(rc *RunContext, unit string) bool {
	if rc.Known == nil {
		return true
	}
	for _, k := range rc.Known(unit) {
		kind, p := depkey.Parse(k)
		if kind != depkey.KindContent || p == unit || !pathutil.Match([]string{"**/*." + jsTestExts}, p) {
			continue
		}
		if (rc.IsTest == nil || !rc.IsTest(p)) && !rc.Suite.InputMatch(p) {
			return true
		}
	}
	return false
}

// bunSnapshotFile is the snapshot file of test file unit (repo-relative): bun keeps
// toMatchSnapshot's snapshots in __snapshots__/<file>.snap next to the test file. A
// file that exists after the run (also one the test just wrote) is recorded as read,
// a missing one as probed (creating it selects the test): bun reads it natively, which
// the preload shim cannot see.
func bunSnapshotFile(unit string) string {
	return path.Join(path.Dir(unit), "__snapshots__", path.Base(unit)+".snap")
}

// bunConfigWithShim writes a copy of the suite's bunfig.toml whose [test].preload starts
// with the shim (relative preload paths made absolute). It returns "" when the project
// has no bunfig.toml (then --preload is enough).
func bunConfigWithShim(rc *RunContext, shim string) (string, func(), error) {
	src := filepath.Join(rc.Dir(), "bunfig.toml")
	data, err := os.ReadFile(src)
	if err != nil && !os.IsNotExist(err) {
		return "", nil, err
	}
	// A byte order mark is no TOML statement: left in front, it would hide a [test]
	// header or a test.preload key on the first line (bun accepts the file without it).
	text := withoutCoverageThresholdTables(strings.TrimPrefix(string(data), "\ufeff"))
	abs := func(p string) string {
		if filepath.IsAbs(p) || !strings.HasPrefix(p, ".") {
			return p // absolute, or a package specifier
		}
		return filepath.Join(rc.Dir(), p)
	}
	render := func(items []string) string {
		q := make([]string, len(items))
		for i, it := range items {
			q[i] = strconv.Quote(it)
		}
		return "preload = [" + strings.Join(q, ", ") + "]"
	}
	// preloads returns the paths of a preload value (a string or an array of strings).
	preloads := func(value string) []string {
		var out []string
		for _, s := range tomlStrings(value) {
			out = append(out, abs(s))
		}
		return out
	}
	// Locate the [test] table: from the end of its header to the next header.
	stmts := tomlStatements(text)
	top, start, end := len(text), -1, len(text)
	for _, s := range stmts {
		if !s.header {
			continue
		}
		if top == len(text) {
			top = s.start
		}
		if start >= 0 {
			end = s.start
			break
		}
		if !s.array && len(s.key) == 1 && s.key[0] == "test" {
			start = s.end
		}
	}
	// The top level: its preload paths move with the file too. Without a [test] table it
	// may set the table's keys as dotted keys (test.preload = [...]) or an inline table
	// (test = { preload = [...] }); a table is defined once in TOML, so they move into
	// the [test] table appended.
	var head strings.Builder
	var keys []string
	last := 0
	for _, s := range stmts {
		if s.start >= top {
			break
		}
		if s.header || s.key == nil {
			continue
		}
		switch {
		case len(s.key) == 1 && s.key[0] == "preload":
			head.WriteString(text[last:s.start] + render(preloads(text[s.value:s.end])))
		case start < 0 && len(s.key) > 1 && s.key[0] == "test":
			keys = append(keys, tomlKeyString(s.key[1:])+" = "+strings.TrimSpace(text[s.value:s.end]))
			head.WriteString(text[last:s.start])
		case start < 0 && len(s.key) == 1 && s.key[0] == "test":
			keys = append(keys, tomlInlineTable(text[s.value:s.end])...)
			head.WriteString(text[last:s.start])
		default:
			continue
		}
		last = s.end
	}
	head.WriteString(text[last:top])
	// The [test] table: the shim's preload first, and no coverage threshold (recording
	// runs one file per process with coverage on: a project threshold would fail every
	// one of them).
	testTable := func(table string) string {
		var b strings.Builder
		items := []string{shim}
		last := 0
		for _, s := range tomlStatements(table) {
			// coverageThreshold also as dotted keys (coverageThreshold.lines = 0.9): any of
			// them would redefine the coverageThreshold = 0 set here.
			if s.header || s.key == nil || !((len(s.key) == 1 && s.key[0] == "preload") || s.key[0] == "coverageThreshold") {
				continue
			}
			if s.key[0] == "preload" {
				items = append(items, preloads(table[s.value:s.end])...)
			}
			b.WriteString(table[last:s.start])
			last = s.end
		}
		b.WriteString(table[last:])
		return "\n" + render(items) + "\ncoverageThreshold = 0" + b.String()
	}
	var out string
	if start < 0 {
		out = head.String() + text[top:] + "\n[test]" + testTable("\n"+strings.Join(keys, "\n")+"\n")
	} else {
		out = head.String() + text[top:start] + testTable(text[start:end]) + text[end:]
	}
	f, err := os.CreateTemp(rc.TraceDir, "bunfig-*.toml")
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(out); err != nil {
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// withoutCoverageThresholdTables removes [test.coverageThreshold] tables (their header
// and body, up to the next header) from a TOML document: the recording bunfig sets
// test.coverageThreshold = 0, which a table of that name would redefine.
func withoutCoverageThresholdTables(text string) string {
	var b strings.Builder
	last, skip := 0, false
	for _, s := range tomlStatements(text) {
		if !s.header {
			continue
		}
		drop := len(s.key) >= 2 && s.key[0] == "test" && s.key[1] == "coverageThreshold"
		if drop && !skip {
			b.WriteString(text[last:s.start])
		} else if !drop && skip {
			last = s.start
		}
		skip = drop
	}
	if !skip {
		b.WriteString(text[last:])
	}
	return b.String()
}

// tomlStmt is a statement of a TOML document: a table header or a key = value pair.
type tomlStmt struct {
	start, end int      // text[start:end], with its comment, without the line break
	header     bool     // [table] or [[array of tables]]
	array      bool     // [[array of tables]]
	key        []string // the table's or the key's dotted parts, unquoted
	value      int      // where a key's value starts
}

// tomlStatements splits a TOML document into its statements. Strings (also multi-line
// ones) and comments are skipped, so that nothing inside them is taken for a statement.
func tomlStatements(text string) []tomlStmt {
	var out []tomlStmt
	for i := 0; i < len(text); {
		if c := text[i]; c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if text[i] == '#' {
			for i < len(text) && text[i] != '\n' {
				i++
			}
			continue
		}
		end := tomlScan(text, i, func(c byte, depth int) bool { return depth == 0 && c == '\n' })
		s := tomlStmt{start: i, end: len(strings.TrimRight(text[:end], "\r"))}
		if text[i] == '[' {
			j := i + 1
			if j < end && text[j] == '[' {
				s.array = true
				j++
			}
			k := tomlScan(text, j, func(c byte, depth int) bool { return depth == 0 && (c == ']' || c == '\n') })
			s.header, s.key = true, tomlKey(text[j:min(k, end)])
		} else if eq := tomlScan(text, i, func(c byte, depth int) bool { return depth == 0 && (c == '=' || c == '\n') }); eq < end {
			s.key, s.value = tomlKey(text[i:eq]), eq+1
		}
		out = append(out, s)
		i = end
	}
	return out
}

// tomlKey returns the dotted parts of a key or table name, unquoted ("test".preload is
// test, preload).
func tomlKey(s string) []string {
	var parts []string
	for i := 0; i <= len(s); {
		j := tomlScan(s, i, func(c byte, depth int) bool { return c == '.' })
		p := strings.TrimSpace(s[i:j])
		if len(p) >= 2 && (p[0] == '"' || p[0] == '\'') && p[len(p)-1] == p[0] {
			if p[0] == '"' {
				if u, err := strconv.Unquote(p); err == nil {
					p = u
				} else {
					p = p[1 : len(p)-1]
				}
			} else {
				p = p[1 : len(p)-1]
			}
		}
		parts = append(parts, p)
		i = j + 1
	}
	return parts
}

var tomlBareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// tomlKeyString renders dotted key parts, quoting those that are not bare keys.
func tomlKeyString(parts []string) string {
	q := make([]string, len(parts))
	for i, p := range parts {
		if q[i] = p; !tomlBareKey.MatchString(p) {
			q[i] = strconv.Quote(p)
		}
	}
	return strings.Join(q, ".")
}

// tomlInlineTable returns the key = value pairs of an inline table value ({ a = 1, b = [2,
// 3] }), split at its top-level commas; nothing when the value is no inline table.
func tomlInlineTable(value string) []string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") {
		return nil
	}
	end := tomlScan(value, 1, func(c byte, depth int) bool { return depth < 0 })
	var pairs []string
	for i := 1; i < end; {
		j := min(tomlScan(value, i, func(c byte, depth int) bool { return depth == 0 && c == ',' }), end)
		if kv := strings.TrimSpace(value[i:j]); kv != "" {
			pairs = append(pairs, kv)
		}
		i = j + 1
	}
	return pairs
}

// tomlStrings returns the strings of a TOML value (a string or an array of strings),
// leaving out comments.
func tomlStrings(value string) []string {
	var out []string
	for i := 0; i < len(value); i++ {
		switch c := value[i]; c {
		case '#':
			for i < len(value) && value[i] != '\n' {
				i++
			}
		case '"', '\'':
			j := tomlStringEnd(value, i)
			q := value[i:j]
			n := 1
			if strings.HasPrefix(q, strings.Repeat(string(c), 3)) {
				n = 3
			}
			s := q[min(n, len(q)):max(len(q)-n, min(n, len(q)))]
			if c == '"' && n == 1 {
				if u, err := strconv.Unquote(q); err == nil {
					s = u
				}
			}
			out = append(out, s)
			i = j - 1
		}
	}
	return out
}

// tomlStringEnd returns the index just past the string starting at s[i] (a quote).
func tomlStringEnd(s string, i int) int {
	q := s[i]
	if delim := strings.Repeat(string(q), 3); strings.HasPrefix(s[i:], delim) {
		// A multi-line string: basic ones (""") escape quotes with a backslash, and
		// either kind may end in one or two quotes of its own ("""a""""" holds a"").
		for j := i + 3; j < len(s); j++ {
			if s[j] == '\\' && q == '"' {
				j++
				continue
			}
			if strings.HasPrefix(s[j:], delim) {
				j += 3
				for k := 0; k < 2 && j < len(s) && s[j] == q; k++ {
					j++
				}
				return j
			}
		}
		return len(s)
	}
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case q:
			return j + 1
		case '\\':
			if q == '"' {
				j++
			}
		case '\n':
			return j // unterminated
		}
	}
	return len(s)
}

// tomlScan returns the index of the first byte at or after i for which stop reports
// true, given the bracket depth relative to i; strings and comments are skipped.
func tomlScan(s string, i int, stop func(c byte, depth int) bool) int {
	depth := 0
	for i < len(s) {
		c := s[i]
		if stop(c, depth) {
			return i
		}
		switch c {
		case '"', '\'':
			i = tomlStringEnd(s, i)
			continue
		case '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if stop(c, depth) {
				return i
			}
		}
		i++
	}
	return len(s)
}

// readLcov returns repo-relative source files listed in an lcov report.
func readLcov(file, cwd, root string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		p, ok := strings.CutPrefix(sc.Text(), "SF:")
		if !ok {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if r, ok := pathutil.Rel(root, p); ok && !pathutil.IsVendored(r) {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
