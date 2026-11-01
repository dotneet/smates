// Package detect infers test suites from a repository layout, so smates works without a
// config file and `smates init` can write one.
package detect

import (
	"encoding/json"
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

func (p *packageJSON) has(dep string) bool {
	_, a := p.Dependencies[dep]
	_, b := p.DevDependencies[dep]
	return a || b
}

var vitestConfigRe = regexp.MustCompile(`^(vitest\.config|vite\.config|vitest\.workspace)\.(ts|mts|cts|js|mjs|cjs|json)$`)
var jestConfigRe = regexp.MustCompile(`^jest\.config\.(ts|mts|cts|js|mjs|cjs|json)$`)

// Suites detects suites from the files of the working tree.
func Suites(repo *gitx.Repo, files []string) []config.Suite {
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
		r := ""
		switch {
		case pj.has("vitest") || strings.Contains(test, "vitest") || hasMatch(d, vitestConfigRe):
			r = "vitest"
		case pj.has("jest") || strings.Contains(test, "jest") || hasMatch(d, jestConfigRe) || len(pj.Jest) > 0:
			r = "jest"
		case strings.Contains(test, "bun test"):
			r = "bun"
		case strings.Contains(test, "node --test") || strings.Contains(test, "node --experimental-test"):
			r = "node"
		case has(d, "bun.lock") || has(d, "bun.lockb") || has(d, "bunfig.toml"):
			r = "bun"
		}
		if r == "" {
			continue
		}
		covered := false
		for root, rr := range jsRoots {
			if rr == r && root != d && pathutil.Under(d, root) && (r == "vitest" || r == "jest") && rootOwnsWorkspace(dirFiles[root], r) {
				covered = true
			}
		}
		if covered {
			continue
		}
		jsRoots[d] = r
		out = append(out, config.Suite{Root: d, Runner: r})
	}

	// Python: one suite per project with pytest configuration or tests. Projects that
	// show no sign of pytest (no dependency, configuration or conftest.py) use the
	// standard library's unittest.
	for _, d := range dirs {
		isProject := has(d, "pyproject.toml") || has(d, "pytest.ini") || has(d, "setup.cfg") || has(d, "setup.py")
		if !isProject {
			continue
		}
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
		}
		out = append(out, config.Suite{Root: d, Runner: r})
	}

	// Keep suites that have tests; give them unique names.
	var kept []config.Suite
	for _, s := range out {
		a, _ := runner.Get(s.Runner)
		rs := &runner.Suite{Suite: s, Adapter: a}
		d := a.Defaults(repo, s.Root)
		rs.Tests, rs.Exclude = d.Tests, d.Exclude
		// Exclude tests that belong to nested suites of the same runner.
		for _, o := range out {
			if o.Runner == s.Runner && o.Root != s.Root && pathutil.Under(o.Root, s.Root) {
				rs.Exclude = append(rs.Exclude, pathutil.RelTo(o.Root, s.Root)+"/**")
				s.Exclude = append(s.Exclude, pathutil.RelTo(o.Root, s.Root)+"/**")
			}
		}
		if len(rs.TestFiles(files)) == 0 {
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
	return kept
}

var (
	// pytestManifests are manifests, lockfiles and task files whose mention of pytest
	// means the project uses it.
	pytestManifests = regexp.MustCompile(`^(pyproject\.toml|setup\.cfg|setup\.py|tox\.ini|noxfile\.py|Pipfile|Pipfile\.lock|poetry\.lock|pdm\.lock|uv\.lock|.*requirements.*\.(txt|in))$`)
	// requirementsFile is any pip requirements file inside a requirements/ directory.
	requirementsFile = regexp.MustCompile(`\.(txt|in)$`)
	workflowFile     = regexp.MustCompile(`\.ya?ml$`)
	pytestWord       = regexp.MustCompile(`\bpytest\b`)
	// pytestStyle marks a test file written for pytest. A TestCase class next to it does
	// not make it a unittest file: unittest runs neither module-level test functions nor
	// pytest marks, fixtures or parametrization, while pytest runs TestCase classes too.
	pytestStyle = regexp.MustCompile(`(?m)^(import pytest\b|from pytest import|(async )?def test_)`)
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
//   - a GitHub workflow that runs pytest;
//   - a test file written for pytest: `import pytest` or a module-level `def test_`
//     function (even next to TestCase classes).
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
	for _, n := range dirFiles[".github/workflows"] {
		if workflowFile.MatchString(n) && mentions(".github/workflows/"+n) {
			return true
		}
	}
	return hasPytestStyleTests(repo, dir, dirFiles)
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
		if pytestStyle.Match(src) {
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

func underTestdata(d string) bool {
	for _, seg := range strings.Split(d, "/") {
		if seg == "testdata" || seg == "vendor" {
			return true
		}
	}
	return false
}
