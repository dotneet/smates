package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
)

// pytestOptions are the ini options of a pytest configuration that select test files.
type pytestOptions struct {
	file    string // repo-relative path of the config file
	section string
	values  map[string][]string // testpaths, python_files, norecursedirs (whitespace-separated words)
}

// pytestNoRecurse is pytest's default norecursedirs.
var pytestNoRecurse = []string{"*.egg", ".*", "_darcs", "build", "CVS", "dist", "node_modules", "venv", "{arch}"}

// pytestVenvExclude approximates pytest skipping virtualenvs whatever norecursedirs says
// (unless --collect-in-virtualenv).
var pytestVenvExclude = []string{"**/.venv/**", "**/venv/**", "**/site-packages/**", "**/.tox/**", "**/.nox/**"}

// pytestExclude is the exclude globs (relative to the suite directory, where pytest
// starts collecting) of a suite whose pytest skips virtualenvs and the directories
// matching the norecursedirs patterns noRecurse: fnmatch patterns matched against a
// directory's name, or the end of its path when they have a "/", at any depth. fnmatch's
// "*" also matches "/": a "*" path segment of a pattern with a "/" becomes "**" (a "*"
// within a segment stays in it). pytest still collects the testpaths themselves: when a
// pattern matches a testpath or one of its parents, it only excludes what is below the
// testpaths (only their files are tests then).
func pytestExclude(noRecurse, testpaths []string) []string {
	out := append([]string{}, pytestVenvExclude...)
	add := func(g string) {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	var dirs []string // testpaths that are directories
	for _, p := range testpaths {
		if p = strings.Trim(strings.TrimPrefix(p, "./"), "/"); p != "" && p != "." && !strings.HasSuffix(p, ".py") {
			dirs = append(dirs, p)
		}
	}
	for _, pat := range noRecurse {
		// fnmatch has no escapes and no braces.
		pat = strings.NewReplacer(`\`, `\\`, "{", `\{`, "}", `\}`).Replace(strings.Trim(pat, "/"))
		if strings.Contains(pat, "/") {
			segs := strings.Split(pat, "/")
			for i, s := range segs {
				if s == "*" {
					segs[i] = "**"
				}
			}
			pat = strings.Join(segs, "/")
		}
		dir := "**/" + pat
		covers := false // a testpath is below a directory the pattern matches
		for _, p := range testpaths {
			p = strings.Trim(strings.TrimPrefix(p, "./"), "/")
			for q := p; q != "." && q != "" && q != "/"; q = path.Dir(q) {
				covers = covers || pathutil.Match([]string{dir}, q)
			}
		}
		if !covers {
			add(dir + "/**")
			continue
		}
		for _, p := range dirs {
			add(globEscape(p) + "/**/" + pat + "/**")
		}
	}
	return out
}

// globEscape quotes the glob metacharacters of a literal path.
func globEscape(p string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`, "{", `\{`, "}", `\}`).Replace(p)
}

// Discover reads testpaths, python_files and norecursedirs from the pytest
// configuration pytest would use: the first of pytest.ini, .pytest.ini, pyproject.toml
// ([tool.pytest.ini_options] or [tool.pytest]), tox.ini ([pytest]) and setup.cfg
// ([tool:pytest]) found from the suite directory up. testpaths only apply when that
// file is in the suite directory (pytest uses them when started in its rootdir without
// arguments). A configured norecursedirs replaces pytest's default one.
func (pytest) Discover(repo *gitx.Repo, s *Suite) Discovery {
	if c, ok := commandFlag(s, "-c", "--config-file", "--rootdir"); ok {
		return Discovery{Warning: fmt.Sprintf("the suite command sets a pytest configuration (%s): using the default test patterns (set tests: in smates.yml if they differ)", c)}
	}
	opts, err := findPytestConfig(repo, s.Root)
	if err != nil {
		return Discovery{Warning: fmt.Sprintf("%v: using the default test patterns (set tests: in smates.yml if they differ)", err)}
	}
	if opts == nil {
		return Discovery{}
	}
	return pytestDiscovery(opts, s.Root)
}

func pytestDiscovery(opts *pytestOptions, root string) Discovery {
	files, hasFiles := opts.values["python_files"]
	paths, hasPaths := opts.values["testpaths"]
	noRecurse, hasNoRecurse := opts.values["norecursedirs"]
	dir := path.Dir(opts.file)
	if dir != root {
		hasPaths = false
	}
	if (!hasFiles || len(files) == 0) && (!hasPaths || len(paths) == 0) && !hasNoRecurse {
		return Discovery{}
	}
	if !hasNoRecurse {
		noRecurse = pytestNoRecurse
	}
	if !hasPaths {
		paths = nil
	}
	var keys []string
	if !hasFiles || len(files) == 0 {
		files = []string{"test_*.py", "*_test.py"}
	} else {
		keys = append(keys, "python_files")
	}
	m := Matcher{Base: dir}
	var tests []string
	if hasPaths && len(paths) > 0 {
		keys = append([]string{"testpaths"}, keys...)
		for _, p := range paths {
			p = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
			if p == "" || p == "." {
				p = ""
			}
			if strings.HasSuffix(p, ".py") {
				tests = append(tests, p) // a file given explicitly is collected whatever its name
				continue
			}
			for _, f := range files {
				if p == "" {
					tests = append(tests, "**/"+f)
				} else {
					tests = append(tests, p+"/**/"+f)
				}
			}
		}
	} else {
		for _, f := range files {
			tests = append(tests, "**/"+f)
		}
	}
	m.Tests = tests
	if hasNoRecurse {
		keys = append(keys, "norecursedirs")
	}
	return Discovery{Matchers: []Matcher{m}, Source: fmt.Sprintf("%s %s %s", opts.file, opts.section, strings.Join(keys, ", ")), Exclude: pytestExclude(noRecurse, paths)}
}

// findPytestConfig returns the options of the pytest config file for dir (nil if none).
func findPytestConfig(repo *gitx.Repo, dir string) (*pytestOptions, error) {
	for d := dir; ; d = path.Dir(d) {
		for _, c := range []struct{ name, section string }{
			{"pytest.ini", "[pytest]"}, {".pytest.ini", "[pytest]"}, {"pyproject.toml", ""},
			{"tox.ini", "[pytest]"}, {"setup.cfg", "[tool:pytest]"},
		} {
			p := pathutil.Join(d, c.name)
			data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(p)))
			if err != nil {
				continue
			}
			var opts *pytestOptions
			if c.name == "pyproject.toml" {
				opts = parsePyprojectPytest(data)
			} else {
				opts = parseIniSection(data, c.section)
				if opts == nil && strings.HasSuffix(c.name, "pytest.ini") {
					// pytest.ini is the config file even without a [pytest] section.
					opts = &pytestOptions{section: c.section}
				}
			}
			if opts != nil {
				opts.file = p
				return opts, nil
			}
		}
		if d == "." {
			return nil, nil
		}
	}
}

// parseIniSection reads the options of one section of an ini file (nil if the section
// is missing). Continuation lines (indented) extend the previous value.
func parseIniSection(data []byte, section string) *pytestOptions {
	var opts *pytestOptions
	in := false
	key := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			in = trimmed == section
			key = ""
			if in && opts == nil {
				opts = &pytestOptions{section: section, values: map[string][]string{}}
			}
			continue
		}
		if !in {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && key != "" {
			opts.values[key] = append(opts.values[key], strings.Fields(stripIniComment(trimmed))...)
			continue
		}
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			k, v, ok = strings.Cut(trimmed, ":")
		}
		if !ok {
			key = ""
			continue
		}
		key = strings.TrimSpace(k)
		opts.values[key] = strings.Fields(stripIniComment(v))
	}
	return opts
}

func stripIniComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, " ;"); i >= 0 {
		s = s[:i]
	}
	return s
}

var tomlStringRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|'([^']*)'`)

// parsePyprojectPytest reads [tool.pytest.ini_options] (or the native [tool.pytest]
// table of pytest 9) of a pyproject.toml: string values are split at whitespace, arrays
// of strings taken as they are. nil if neither table exists.
func parsePyprojectPytest(data []byte) *pytestOptions {
	var opts *pytestOptions
	in := false
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && !strings.Contains(trimmed, "=") {
			h := strings.TrimSpace(strings.Trim(trimmed, "[] "))
			in = h == "tool.pytest.ini_options" || h == "tool.pytest"
			if in && opts == nil {
				opts = &pytestOptions{section: "[" + h + "]", values: map[string][]string{}}
			}
			continue
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "[") {
			// Multi-line arrays: read until the closing bracket.
			for !arrayClosed(v) && i+1 < len(lines) {
				i++
				v += "\n" + lines[i]
			}
			var out []string
			for _, m := range tomlStringRe.FindAllStringSubmatch(stripTomlComments(v), -1) {
				out = append(out, m[1]+m[2])
			}
			opts.values[k] = out
			continue
		}
		if m := tomlStringRe.FindStringSubmatch(v); m != nil {
			opts.values[k] = strings.Fields(m[1] + m[2])
		}
	}
	return opts
}

// arrayClosed reports whether a TOML array value has its closing bracket (outside
// strings and comments).
func arrayClosed(v string) bool {
	depth := 0
	v = stripTomlComments(v)
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '"', '\'':
			q := v[i]
			for i++; i < len(v) && v[i] != q; i++ {
				if v[i] == '\\' && q == '"' {
					i++
				}
			}
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// stripTomlComments drops # comments outside strings, line by line.
func stripTomlComments(v string) string {
	var b strings.Builder
	for _, line := range strings.Split(v, "\n") {
		inStr := byte(0)
		cut := len(line)
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case inStr != 0:
				if c == '\\' && inStr == '"' {
					i++
				} else if c == inStr {
					inStr = 0
				}
			case c == '"' || c == '\'':
				inStr = c
			case c == '#':
				cut = i
				i = len(line)
			}
		}
		b.WriteString(line[:cut])
		b.WriteByte('\n')
	}
	return b.String()
}
