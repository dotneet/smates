package runner

import (
	"path"
	"regexp"
	"strings"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
)

// Which files are tests is, without `tests:` in smates.yml, read from the runner's own
// configuration (jest testMatch/testRegex/roots, vitest include/exclude/projects, pytest
// testpaths/python_files), falling back to the adapter's default globs. The derived set
// does not narrow the suite (Suite.Narrowed): it describes what the runner discovers by
// itself, so a full run still lets the runner discover its tests.

// Matcher is a set of test files described by a runner's configuration.
type Matcher struct {
	// Base is the repo-relative directory the patterns are relative to (jest rootDir,
	// vitest project root, the pytest ini file's directory).
	Base string
	// Roots, relative to Base: a test file must be under one of them (empty: anywhere).
	Roots []string
	// Tests and Exclude are globs relative to Base.
	Tests   []string
	Exclude []string
	// TestRegex replaces Tests when set; regexes are matched against "/" followed by the
	// path relative to Base (jest matches the absolute path).
	TestRegex []*regexp.Regexp
	// IgnoreRegex excludes paths matched like TestRegex.
	IgnoreRegex []*regexp.Regexp
	// Browser is set when the project runs its tests in a browser (Vitest's browser
	// mode, Storybook's Vitest plugin): no trace sees what they load.
	Browser bool
	// Whole is set when the files the project runs could not be listed (a Storybook
	// project whose stories smates cannot read): Tests are a guess, so whenever one of
	// its tests is selected the whole suite runs, which is sure to run them all.
	Whole bool
	// Deps are repo-relative globs of files every test the matcher selects depends on,
	// and Starts repo-relative files whose static import closure they depend on: what a
	// browser project loads besides the test file (Storybook's configuration directory
	// and static directories; its preview.*).
	Deps, Starts []string
	// Unsure says why Deps, Starts and the static import closure of a test may not be
	// all it loads ("" when they are): its passes are then never trusted (see
	// config.Suite.TrustBrowserPasses).
	Unsure string
}

// Match reports whether the repo-relative path f is selected by m.
func (m *Matcher) Match(f string) bool {
	if !pathutil.Under(f, m.Base) {
		return false
	}
	rel := pathutil.RelTo(f, m.Base)
	if len(m.Roots) > 0 {
		in := false
		for _, r := range m.Roots {
			in = in || pathutil.Under(rel, r)
		}
		if !in {
			return false
		}
	}
	if len(m.TestRegex) > 0 {
		if !matchAny(m.TestRegex, "/"+rel) {
			return false
		}
	} else if !pathutil.Match(m.Tests, rel) {
		return false
	}
	return !pathutil.Match(m.Exclude, rel) && !matchAny(m.IgnoreRegex, "/"+rel)
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// Patterns describes m for people, with globs relative to the suite root.
func (m *Matcher) Patterns(root string) []string {
	prefix := ""
	if m.Base != root {
		if pathutil.Under(m.Base, root) {
			prefix = pathutil.RelTo(m.Base, root) + "/"
		} else {
			prefix = "/" + m.Base + "/"
			if m.Base == "." {
				prefix = "/"
			}
		}
	}
	var out []string
	add := func(s string) { out = append(out, s) }
	var roots []string
	for _, r := range m.Roots {
		if r != "." {
			roots = append(roots, prefix+r)
		}
	}
	if len(roots) > 0 {
		add("roots " + strings.Join(roots, ","))
	}
	if len(m.TestRegex) > 0 {
		for _, re := range m.TestRegex {
			add("regex " + re.String())
		}
	} else {
		for _, g := range m.Tests {
			add(prefix + g)
		}
	}
	for _, g := range m.Exclude {
		add("!" + prefix + g)
	}
	for _, re := range m.IgnoreRegex {
		add("!regex " + re.String())
	}
	return out
}

// Discovery is what a runner's configuration says about the suite's test files.
type Discovery struct {
	// Matchers select test files (any of them); none: use the adapter's default globs.
	Matchers []Matcher
	// Source names where they come from, e.g. "jest.config.js testMatch".
	Source string
	// Exclude, when non-nil, replaces the adapter's default exclude globs.
	Exclude []string
	// Warning says why a configuration that exists could not be read (the defaults are
	// used then).
	Warning string
	// Files are the configuration files read (repo-relative): a project's file may have
	// any name.
	Files []string
}

// discoverer is implemented by adapters that read their runner's configuration.
type discoverer interface {
	Discover(repo *gitx.Repo, s *Suite) Discovery
}

// SourceDefaults and SourceConfig are the TestsSource of suites whose test globs are the
// runner defaults or come from smates.yml.
const (
	SourceDefaults = "runner defaults"
	SourceConfig   = "smates.yml tests"
)

// matchersOf returns the Discovered matchers that select one of files.
func (s *Suite) matchersOf(files []string) []*Matcher {
	var out []*Matcher
	for i := range s.Discovered {
		m := &s.Discovered[i]
		for _, f := range files {
			if m.Match(f) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// BrowserUnit reports whether the unit made of the test files files runs only in a
// browser: every project of the suite that runs one of them is a browser project
// (Matcher.Browser).
func (s *Suite) BrowserUnit(files []string) bool {
	ms := s.matchersOf(files)
	for _, m := range ms {
		if !m.Browser {
			return false
		}
	}
	return len(ms) > 0
}

// RunsWhole reports whether a project that runs one of files cannot be run file by
// file (Matcher.Whole).
func (s *Suite) RunsWhole(files []string) bool {
	for _, m := range s.matchersOf(files) {
		if m.Whole {
			return true
		}
	}
	return false
}

// BrowserDeps returns what the browser projects running one of files make it depend
// on besides its static import closure (Matcher.Deps and Matcher.Starts), and why
// that may not be all ("" when it is).
func (s *Suite) BrowserDeps(files []string) (globs, starts []string, unsure string) {
	for _, m := range s.matchersOf(files) {
		if !m.Browser {
			continue
		}
		globs = append(globs, m.Deps...)
		starts = append(starts, m.Starts...)
		if unsure == "" {
			unsure = m.Unsure
		}
	}
	return globs, starts, unsure
}

// TestPatterns describes the suite's test file patterns for people.
func (s *Suite) TestPatterns() []string {
	if len(s.Discovered) == 0 {
		return append([]string{}, s.Tests...)
	}
	var out []string
	for i := range s.Discovered {
		out = append(out, s.Discovered[i].Patterns(s.Root)...)
	}
	return out
}

// ---- glob and regex conversion ----

// micromatchToGlob converts a micromatch/picomatch glob (jest, vitest) to doublestar
// syntax: extglobs ?(a|b), @(a|b), +(a), *(a) and groups (a|b) become alternatives. It
// reports false for constructs doublestar cannot express (!(...), POSIX classes).
func micromatchToGlob(g string) (string, bool) {
	if strings.Contains(g, "!(") || strings.Contains(g, "[[:") {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		ext := (c == '?' || c == '@' || c == '+' || c == '*') && i+1 < len(g) && g[i+1] == '('
		if !ext && c != '(' {
			if c == ')' || c == '|' {
				return "", false
			}
			b.WriteByte(c)
			continue
		}
		open := i
		if ext {
			open = i + 1
		}
		end := closingParen(g, open)
		if end < 0 {
			return "", false
		}
		var alts []string
		for _, a := range splitAlternatives(g[open+1 : end]) {
			conv, ok := micromatchToGlob(a)
			if !ok {
				return "", false
			}
			alts = append(alts, conv)
		}
		if ext && (c == '?' || c == '*') {
			// Zero or one / zero or more occurrences. One or more (+, and the more of *)
			// is approximated by one: repetitions such as .test.test.js are vanishingly
			// rare.
			alts = append([]string{""}, alts...)
		}
		b.WriteString("{" + strings.Join(alts, ",") + "}")
		i = end
	}
	return b.String(), true
}

// splitAlternatives splits s at the '|' outside parentheses.
func splitAlternatives(s string) []string {
	var out []string
	depth, last := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	return append(out, s[last:])
}

func closingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// jsRegex compiles a JavaScript regular expression with Go's regexp. Go lacks
// lookaround and backreferences; such patterns report false.
func jsRegex(src, flags string) (*regexp.Regexp, bool) {
	if strings.Contains(src, "(?=") || strings.Contains(src, "(?!") || strings.Contains(src, "(?<=") || strings.Contains(src, "(?<!") {
		return nil, false
	}
	// Named groups (?<name>...) are (?P<name>...) in older Go; both work since Go 1.22.
	prefix := ""
	if strings.Contains(flags, "i") {
		prefix += "(?i)"
	}
	if strings.Contains(flags, "s") {
		prefix += "(?s)"
	}
	if strings.Contains(flags, "m") {
		prefix += "(?m)"
	}
	// \/ is a valid escape in Go too; \d, \w, \s match ASCII in both.
	re, err := regexp.Compile(prefix + src)
	if err != nil {
		return nil, false
	}
	return re, true
}

// repoRel returns the repo-relative form of p (relative to the repo-relative dir), and
// false if it leaves the repository.
func repoRel(dir, p string) (string, bool) {
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	j := path.Clean(path.Join(dir, p))
	if j == ".." || strings.HasPrefix(j, "../") {
		return "", false
	}
	return j, true
}
