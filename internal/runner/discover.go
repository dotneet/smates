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
