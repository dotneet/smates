// Package detect infers test suites from a repository layout, so smates works without a
// config file and `smates init` can write one.
package detect

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
)

type packageJSON struct {
	Name            string            `json:"name"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Jest            json.RawMessage   `json:"jest"`
}

// jestHasKey reports whether the "jest" object of a package.json sets key.
func jestHasKey(raw json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

func (p *packageJSON) has(dep string) bool {
	_, a := p.Dependencies[dep]
	_, b := p.DevDependencies[dep]
	return a || b
}

// vitestProjectConfigRe matches a per-project vitest configuration file
// (vitest.stories.config.ts), which configures vitest when a command names it.
var vitestProjectConfigRe = regexp.MustCompile(`^vitest\.[^.]+\.config\.(ts|mts|cts|js|mjs|cjs)$`)

// vitestMentionRe matches what makes a vite.config.* configure vitest: an import of
// vitest, or a test key.
var vitestMentionRe = regexp.MustCompile(`["']vitest(?:/[^"']*)?["']|\btest\s*:`)

var vitestConfigRe = regexp.MustCompile(`^(vitest\.config|vite\.config|vitest\.workspace)\.(ts|mts|cts|js|mjs|cjs|json)$`)
var jestConfigRe = regexp.MustCompile(`^jest\.config\.(ts|mts|cts|js|mjs|cjs|json)$`)

// Suites detects suites from the files of the working tree.
func Suites(repo *gitx.Repo, files []string) []config.Suite {
	s, _ := Detect(repo, files)
	return s
}

// Detect is Suites that also returns notes on the directories that configure a test
// runner but got no suite, because no test file was found there (a package whose only
// tests are Storybook stories smates cannot read, say) or its suite is invalid.
func Detect(repo *gitx.Repo, files []string) ([]config.Suite, []string) {
	var out []config.Suite
	dirFiles := map[string][]string{}
	for _, f := range files {
		if pathutil.IsVendored(f) {
			continue
		}
		dirFiles[path.Dir(f)] = append(dirFiles[path.Dir(f)], path.Base(f))
	}
	has := func(dir, name string) bool {
		for _, n := range dirFiles[dir] {
			if n == name {
				return true
			}
		}
		return false
	}
	hasMatch := func(dir string, re *regexp.Regexp) bool {
		for _, n := range dirFiles[dir] {
			if re.MatchString(n) {
				return true
			}
		}
		return false
	}
	var dirs []string
	for d := range dirFiles {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs) // parents before children

	// Go: one suite per module.
	for _, d := range dirs {
		if has(d, "go.mod") && !underTestdata(d) {
			out = append(out, config.Suite{Root: d, Runner: "go"})
		}
	}

	// JS: one suite per package that uses a test runner, unless an ancestor suite with
	// the same runner already covers it (workspace-level vitest/jest config).
	jsRoots := map[string]string{}
	// vitestConfigured: the package has a vitest configuration (a vitest.config.*, a
	// workspace or per-project file, or a vite.config.* importing vitest or with a test
	// key), so finding no test file is worth a note; a plain Vite app is not.
	vitestConfigured := map[string]bool{}
	for _, d := range dirs {
		if !has(d, "package.json") {
			continue
		}
		var pj packageJSON
		data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(d), "package.json"))
		if err != nil || json.Unmarshal(data, &pj) != nil {
			continue
		}
		test := pj.Scripts["test"]
		r, command, env := "", "", map[string]string(nil)
		var tests []string
		_, craAt, craBin := craTest(test)
		nodeCommand, nodeEnv, isNodeTest := nodeTest(test)
		vitestConfigured[d] = hasMatch(d, vitestProjectConfigRe) ||
			slices.ContainsFunc(dirFiles[d], func(n string) bool {
				if !vitestConfigRe.MatchString(n) {
					return false
				}
				if !strings.HasPrefix(n, "vite.config.") {
					return true
				}
				src, _ := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(d), n))
				return vitestMentionRe.Match(src)
			})
		jestNamed, bunNamed := strings.Contains(test, "jest"), strings.Contains(test, "bun test")
		if hasMatch(d, vitestProjectConfigRe) {
			// Over a per-project vitest file, jest and bun win only when the script (or
			// a package script it runs) invokes them: a script named test:jest-x, or
			// bun test:unit running a package script, names no runner.
			run := scriptClosure(pj.Scripts, test)
			jestNamed = slices.ContainsFunc(run, runsJest)
			bunNamed = slices.ContainsFunc(run, runsBunTest)
		}
		switch {
		case pj.has("vitest") || strings.Contains(test, "vitest") || hasMatch(d, vitestConfigRe):
			r = "vitest"
		case craAt >= 0 || strings.Contains(test, "react-scripts test"):
			// Create React App runs jest with a configuration react-scripts generates,
			// which a bare jest would not see: only src/ is searched for tests, unless
			// package.json overrides testMatch (which smates then reads itself) or a
			// wrapper's configuration (craco.config.js, config-overrides.js) may.
			r = "jest"
			command, env = craCommand(repo.Root, d, test)
			if !jestHasKey(pj.Jest, "testMatch") && (craBin == "react-scripts" || craBin == "" || !hasMatch(d, craWrapperConfig)) {
				tests = craTests
			}
		case pj.has("jest") || jestNamed || hasMatch(d, jestConfigRe) || len(pj.Jest) > 0:
			r = "jest"
		case bunNamed:
			r = "bun"
		case isNodeTest || strings.Contains(test, "node --experimental-test"):
			r = "node"
			command, env = nodeCommand, nodeEnv
		case hasMatch(d, vitestProjectConfigRe):
			// A per-project file (vitest.stories.config.ts) configures vitest only
			// when named: it picks vitest only when nothing else names the runner of the
			// package's tests (a test script, a dependency, a configuration).
			r = "vitest"
		case has(d, "bun.lock") || has(d, "bun.lockb") || has(d, "bunfig.toml"):
			r = "bun"
		}
		if r == "" {
			continue
		}
		covered := false
		for root, rr := range jsRoots {
			if rr == r && root != d && pathutil.Under(d, root) && (r == "vitest" || r == "jest") && rootOwnsWorkspace(dirFiles[root], r) &&
				rootCovers(repo, root, r, d, files) {
				covered = true
			}
		}
		if covered {
			continue
		}
		jsRoots[d] = r
		out = append(out, config.Suite{Root: d, Runner: r, Command: command, Env: env, Tests: tests})
	}

	// Node's own runner where no package announces it (see nodeTestSuites).
	out = append(out, nodeTestSuites(repo, files, out)...)

	// Python: one suite per project with pytest configuration or tests. Projects that
	// show no sign of pytest (no dependency, configuration or conftest.py) use the
	// standard library's unittest, except Django projects: their TestCase classes need
	// the settings `manage.py test` sets up, which `python -m unittest` (the only
	// command the unittest runner drives) does not, so they get no suite.
	for _, d := range pythonProjects(repo, dirs, dirFiles) {
		nested := false
		for _, s := range out {
			if (s.Runner == "pytest" || s.Runner == "unittest") && pathutil.Under(d, s.Root) {
				nested = true
			}
		}
		if nested {
			continue
		}
		r := "unittest"
		if usesPytest(repo, d, dirFiles) {
			r = "pytest"
		} else if isDjango(d, dirFiles) {
			continue
		}
		out = append(out, config.Suite{Root: d, Runner: r})
	}

	// unittest directories a CI or task file discovers (see unittestSuites).
	out = append(out, unittestSuites(repo, files, out)...)

	// Keep suites that have tests (as the runner's configuration or the default
	// patterns describe them); give them unique names.
	var kept []config.Suite
	var notes []string
	for _, s := range out {
		// Exclude tests that belong to nested suites of the same runner.
		for _, o := range out {
			if o.Runner == s.Runner && o.Root != s.Root && pathutil.Under(o.Root, s.Root) {
				s.Exclude = append(s.Exclude, pathutil.RelTo(o.Root, s.Root)+"/**")
			}
		}
		rs, err := runner.NewSuite(repo, s)
		configured := s.Runner == "vitest" && vitestConfigured[s.Root] || s.Runner == "jest" && hasMatch(s.Root, jestConfigRe)
		switch {
		case err != nil:
			if configured {
				notes = append(notes, fmt.Sprintf("%s: no %s suite: %v", s.Root, s.Runner, err))
			}
			continue
		case len(rs.TestFiles(files)) == 0:
			if configured {
				why := ""
				if rs.DiscoveryWarning != "" {
					why = " (" + rs.DiscoveryWarning + ")"
				}
				if s.Runner == "vitest" && !hasMatch(s.Root, vitestConfigRe) {
					// Only per-project files: vitest reads one when the command names it.
					for _, n := range dirFiles[s.Root] {
						if vitestProjectConfigRe.MatchString(n) {
							why += fmt.Sprintf(" (%s configures vitest only when named: set command: vitest run --config %s)", n, n)
							break
						}
					}
				}
				notes = append(notes, fmt.Sprintf("%s: no %s suite: its configuration names no test file smates can find (test patterns: %s)%s; write the suite in smates.yml with tests: if it has tests",
					s.Root, s.Runner, strings.Join(rs.TestPatterns(), " "), why))
			}
			continue
		}
		kept = append(kept, s)
	}
	count := map[string]int{}
	for _, s := range kept {
		count[s.Runner]++
	}
	for i := range kept {
		s := &kept[i]
		switch {
		case count[s.Runner] == 1:
			s.Name = s.Runner
		case s.Root == ".":
			s.Name = s.Runner + ":root"
		default:
			s.Name = s.Runner + ":" + s.Root
		}
	}
	return kept, notes
}

// pythonProjects returns the Python project directories, sorted (parents before
// children). A project file (pyproject.toml, setup.cfg, setup.py) or a pytest config
// (pytest.ini, a tox.ini with a [pytest] section) marks its directory unless it is
// inside a testdata or vendor directory (as go.mod does; fixture directories, which
// hold sample projects other suites may well run, still count); whether that project
// has tests is checked by the caller.
//
// Weaker signs mark a directory too: a pip requirements file (*requirements*.txt, or
// any .txt/.in in a requirements/ directory, which marks the directory above), a
// Pipfile, a Django manage.py, or a conftest.py (inside a test/ or tests/ directory it
// marks the directory above, the one pytest runs from). As such files also appear in
// repositories that are not Python projects (mkdocs' requirements.txt next to a Go
// module), a weakly marked directory is a project only when it is outside testdata,
// vendor and fixture directories, is not inside a marked project, and holds Python
// test files of its own outside such directories and outside the marked projects
// below it.
func pythonProjects(repo *gitx.Repo, dirs []string, dirFiles map[string][]string) []string {
	strong, weak := map[string]bool{}, map[string]bool{}
	var tests []string // Python test files outside fixture directories
	for _, d := range dirs {
		if underTestdata(d) {
			// Like Go modules there: testdata and vendor hold data of other tests and
			// third-party code, never a project of this repository.
			continue
		}
		fixture := inFixtureDir(d)
		for _, n := range dirFiles[d] {
			switch {
			case n == "pyproject.toml", n == "setup.cfg", n == "setup.py", n == "pytest.ini":
				strong[d] = true
			case n == "tox.ini":
				data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(d), n))
				if err == nil && toxPytestSection.Match(data) {
					strong[d] = true
				}
			case fixture:
			case requirementsTxt.MatchString(n), n == "Pipfile", n == "manage.py":
				weak[d] = true
			case n == "conftest.py":
				weak[conftestProject(d)] = true
			case path.Base(d) == "requirements" && requirementsFile.MatchString(n):
				weak[path.Dir(d)] = true
			}
			if !fixture && pyTestFile.MatchString(n) {
				tests = append(tests, path.Join(d, n))
			}
		}
	}
	out := make([]string, 0, len(strong)+len(weak))
	for d := range strong {
		out = append(out, d)
	}
	// Test files outside every marked project, sorted so that the ones under a
	// directory are one range.
	var unowned []string
	for _, t := range tests {
		if !underAny(path.Dir(t), strong) {
			unowned = append(unowned, t)
		}
	}
	sort.Strings(unowned)
	for d := range weak {
		if inFixtureDir(d) || underAny(d, strong) || !hasUnder(unowned, d) {
			continue
		}
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// underAny reports whether directory d is in set or below a directory of it.
func underAny(d string, set map[string]bool) bool {
	for {
		if set[d] {
			return true
		}
		if d == "." || d == "" {
			return false
		}
		d = path.Dir(d)
	}
}

// hasUnder reports whether the sorted file list has a file under directory d.
func hasUnder(sorted []string, d string) bool {
	if d == "." || d == "" {
		return len(sorted) > 0
	}
	i := sort.SearchStrings(sorted, d+"/")
	return i < len(sorted) && strings.HasPrefix(sorted[i], d+"/")
}

// isDjango reports whether the project at d is a Django project (manage.py in it).
func isDjango(d string, dirFiles map[string][]string) bool {
	return slices.Contains(dirFiles[d], "manage.py")
}

// inFixtureDir reports whether d is inside a testdata, vendor or fixtures directory,
// whose files are data of other tests rather than a project.
func inFixtureDir(d string) bool {
	if underTestdata(d) {
		return true
	}
	for _, seg := range strings.Split(d, "/") {
		if seg == "fixtures" || seg == "fixture" || seg == "__fixtures__" {
			return true
		}
	}
	return false
}

// conftestProject returns the directory a conftest.py in dir marks as a project: the
// parent of its outermost test/tests directory, or dir itself.
func conftestProject(dir string) string {
	segs := strings.Split(dir, "/")
	for i, s := range segs {
		if s == "tests" || s == "test" {
			if i == 0 {
				return "."
			}
			return strings.Join(segs[:i], "/")
		}
	}
	return dir
}

var (
	// toxPytestSection is the [pytest] section of a tox.ini that configures pytest.
	toxPytestSection = regexp.MustCompile(`(?m)^\s*\[pytest\]`)
	// requirementsTxt is a pip requirements file (requirements.txt, dev-requirements.txt,
	// requirements-test.txt).
	requirementsTxt = regexp.MustCompile(`^.*requirements.*\.txt$`)
	// pytestManifests are manifests, lockfiles and task files whose mention of pytest
	// means the project uses it.
	pytestManifests = regexp.MustCompile(`^(pyproject\.toml|setup\.cfg|setup\.py|tox\.ini|noxfile\.py|Pipfile|Pipfile\.lock|poetry\.lock|pdm\.lock|uv\.lock|.*requirements.*\.(txt|in))$`)
	// requirementsFile is any pip requirements file inside a requirements/ directory.
	requirementsFile = regexp.MustCompile(`\.(txt|in)$`)
	workflowFile     = regexp.MustCompile(`\.ya?ml$`)
	pytestWord       = regexp.MustCompile(`\bpytest\b`)
	// pyTestFile is a file either runner would collect.
	pyTestFile = regexp.MustCompile(`^(test.*|.*_test)\.py$`)
)

// maxStyleChecks bounds the test files usesPytest reads (and maxStyleBytes how much of
// each).
const (
	maxStyleChecks = 300
	maxStyleBytes  = 256 << 10
)

// usesPytest reports whether the Python project at dir uses pytest, so that a project
// is never left to unittest when it has pytest tests (unittest would run none of them):
//   - a pytest.ini or conftest.py in the project;
//   - pytest named in a manifest, lockfile or task file (pyproject.toml, setup.cfg,
//     setup.py, tox.ini, noxfile.py, Pipfile, poetry/pdm/uv locks, *requirements*.txt or
//     .in, any .txt/.in under a requirements/ directory) in the project or a directory
//     above it up to the repository root;
//   - a GitHub workflow that runs pytest, for the project at the repository root or
//     one whose directory the workflow names;
//   - a test file written for pytest: `import pytest` or a module-level `def test_`
//     function other than unittest's `test_suite()` (even next to TestCase classes).
func usesPytest(repo *gitx.Repo, dir string, dirFiles map[string][]string) bool {
	mentions := func(rel string) bool {
		data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(rel)))
		return err == nil && pytestWord.Match(data)
	}
	for d, names := range dirFiles {
		if !pathutil.Under(d, dir) {
			continue
		}
		for _, n := range names {
			if n == "pytest.ini" || n == "conftest.py" {
				return true
			}
		}
	}
	// Manifests of the project and of the directories above it (a monorepo's shared
	// requirements-dev.txt), and requirements/ directories below the project.
	for d := dir; ; d = path.Dir(d) {
		for _, n := range dirFiles[d] {
			if pytestManifests.MatchString(n) && mentions(path.Join(d, n)) {
				return true
			}
		}
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	for d, names := range dirFiles {
		if !pathutil.Under(d, dir) || !slices.Contains(strings.Split(d, "/"), "requirements") {
			continue
		}
		for _, n := range names {
			if (requirementsFile.MatchString(n) || pytestManifests.MatchString(n)) && mentions(path.Join(d, n)) {
				return true
			}
		}
	}
	// Workflows run pytest somewhere in the repository: they count for the project at
	// the root, or for a project whose directory they name.
	for _, n := range dirFiles[".github/workflows"] {
		if !workflowFile.MatchString(n) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repo.Root, ".github", "workflows", n))
		if err == nil && pytestWord.Match(data) && (dir == "." || mentionsDir(data, dir)) {
			return true
		}
	}
	return hasPytestStyleTests(repo, dir, dirFiles)
}

// mentionsDir reports whether data names the repository directory dir as a whole path
// (not as part of a longer name: "svc" is not in "svc2" or "my-svc").
func mentionsDir(data []byte, dir string) bool {
	re := regexp.MustCompile(`(^|[^\w./-])(\./)?` + regexp.QuoteMeta(dir) + `($|[^\w.-])`)
	return re.Match(data)
}

// hasPytestStyleTests reports whether a test file under dir is written for pytest
// (`import pytest` or module-level test functions).
func hasPytestStyleTests(repo *gitx.Repo, dir string, dirFiles map[string][]string) bool {
	var files []string
	for d, names := range dirFiles {
		if !pathutil.Under(d, dir) {
			continue
		}
		for _, n := range names {
			if pyTestFile.MatchString(n) {
				files = append(files, path.Join(d, n))
			}
		}
	}
	sort.Strings(files)
	if len(files) > maxStyleChecks {
		files = files[:maxStyleChecks]
	}
	buf := make([]byte, maxStyleBytes)
	for _, f := range files {
		fh, err := os.Open(filepath.Join(repo.Root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		n, _ := io.ReadFull(fh, buf)
		fh.Close()
		src := buf[:n]
		// A TestCase class next to pytest-style code does not make it a unittest file:
		// pytest runs TestCase classes too, unittest none of pytest's.
		if runner.PytestStyle(src) {
			return true
		}
	}
	return false
}

// rootOwnsWorkspace reports whether a package directory configures the runner for
// nested packages (vitest workspace/projects, jest projects).
func rootOwnsWorkspace(names []string, r string) bool {
	for _, n := range names {
		if r == "vitest" && (strings.HasPrefix(n, "vitest.workspace.") || vitestConfigRe.MatchString(n)) {
			return true
		}
		if r == "jest" && jestConfigRe.MatchString(n) {
			return true
		}
	}
	return false
}

// rootCovers reports whether the suite of runner r at root counts test files under the
// package directory d as its configuration (include, testMatch, projects) describes
// them; with the default patterns (or no test files under d at all) it does.
func rootCovers(repo *gitx.Repo, root, r, d string, files []string) bool {
	rs, err := runner.NewSuite(repo, config.Suite{Root: root, Runner: r})
	if err != nil || len(rs.Discovered) == 0 {
		return true
	}
	any := false
	for _, f := range files {
		if pathutil.Under(f, d) && !pathutil.IsVendored(f) {
			if rs.IsTestFile(f) {
				return true
			}
			any = true
		}
	}
	return !any
}

func underTestdata(d string) bool {
	for _, seg := range strings.Split(d, "/") {
		if seg == "testdata" || seg == "vendor" {
			return true
		}
	}
	return false
}
