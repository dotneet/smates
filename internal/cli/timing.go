package cli

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/static/js"
)

var (
	// clockRe matches reading a clock to measure elapsed time: performance.now(),
	// process.hrtime (and its bigint form), or Date.now() minus something.
	clockRe = regexp.MustCompile(`\bperformance\.now\s*\(|\bprocess\.hrtime\b|\bDate\.now\s*\(\s*\)\s*-`)
	// orderRe matches an assertion that compares values by order: the ordering
	// matchers of Vitest and Jest (also Bun's), expect(a < b).toBe(true), and
	// node:assert's ok / assert with < or > in the same statement.
	orderRe = regexp.MustCompile(`\.toBe(Less|Greater)Than(OrEqual)?\s*\(|\bexpect\s*\([^;\n]*[<>][^;\n]*\)\s*\.\s*toBe(Truthy)?\s*\(|\bassert(\.strict)?(\.ok)?\s*\([^;\n]*[<>]`)
	// fakeTimersRe matches fake timers, under which the clock a test reads is not real
	// time.
	fakeTimersRe = regexp.MustCompile(`\b(vi|jest)\.useFakeTimers\b|\bmock\.timers\.enable\b|\bsetSystemTime\b`)
)

// maxTimingScan bounds the size of a test file timingTests reads.
const maxTimingScan = 1 << 20

// timingTests returns the test files (relative to the suite root) of a Node-based
// suite that assert on measured time, which the suite's untraced does not list yet:
// recording measures their code with V8 coverage, which slows it down unevenly, so a
// timing or ratio assertion can fail only under smates (see config.Suite.Untraced).
func timingTests(repoRoot string, s *runner.Suite, files []string) []string {
	if !runner.TracesUntraced(s.Runner) {
		return nil
	}
	var out []string
	for _, f := range s.TestFiles(files) {
		if !js.IsSource(f) || selector.UntracedUnit(s, f, []string{f}) {
			continue
		}
		p := filepath.Join(repoRoot, filepath.FromSlash(f))
		if fi, err := os.Stat(p); err != nil || fi.Size() > maxTimingScan {
			continue
		}
		data, err := os.ReadFile(p)
		if err == nil && measuresTime(data) {
			out = append(out, pathutil.RelTo(f, s.Root))
		}
	}
	return out
}

// measuresTime reports whether a JavaScript or TypeScript test source reads a clock
// and asserts on the order of values, with real timers.
func measuresTime(src []byte) bool {
	return clockRe.Match(src) && orderRe.Match(src) && !fakeTimersRe.Match(src)
}
