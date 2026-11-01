// Package detect infers test suites from a repository layout, so smates works without a
// config file and `smates init` can write one.
package detect

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
			if rr == r && root != d && pathutil.Under(d, root) && (r == "vitest" || r == "jest") && rootOwnsWorkspace(dirFiles[root], r) &&
				rootCovers(repo, root, r, d, files) {
				covered = true
			}
		}
		if covered {
			continue
		}
		jsRoots[d] = r
		out = append(out, config.Suite{Root: d, Runner: r})
	}

	// Python: one suite per project with pytest configuration or tests.
	for _, d := range dirs {
		isProject := has(d, "pyproject.toml") || has(d, "pytest.ini") || has(d, "setup.cfg") || has(d, "setup.py")
		if !isProject {
			continue
		}
		nested := false
		for _, s := range out {
			if s.Runner == "pytest" && pathutil.Under(d, s.Root) {
				nested = true
			}
		}
		if !nested {
			out = append(out, config.Suite{Root: d, Runner: "pytest"})
		}
	}

	// Keep suites that have tests (as the runner's configuration or the default
	// patterns describe them); give them unique names.
	var kept []config.Suite
	for _, s := range out {
		// Exclude tests that belong to nested suites of the same runner.
		for _, o := range out {
			if o.Runner == s.Runner && o.Root != s.Root && pathutil.Under(o.Root, s.Root) {
				s.Exclude = append(s.Exclude, pathutil.RelTo(o.Root, s.Root)+"/**")
			}
		}
		rs, err := runner.NewSuite(repo, s)
		if err != nil || len(rs.TestFiles(files)) == 0 {
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
