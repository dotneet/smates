package detect

// Suites that no manifest announces but the code or the repository's task files do: test
// files of Node's own runner (they import node:test) outside every JS package with a test
// runner, and directories a CI or task file runs with `python -m unittest discover -s`.

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
)

var (
	// nodeTestName is a file name node --test collects by default (with the TypeScript
	// extensions Node strips types from): group 1 is the glob it is collected as.
	nodeTestName = regexp.MustCompile(`^(?:.+(\.test|-test|_test)|(test-).+|(test))\.(js|mjs|cjs|ts|mts|cts)$`)
	// nodeTestImport is an import of Node's test runner.
	nodeTestImport = regexp.MustCompile(`(?:\bfrom\s*|\brequire\(\s*|\bimport\(\s*)['"]node:test['"]`)
	// unittestDiscover is a `python -m unittest discover` command line; group 1 holds its
	// arguments, up to the end of the shell command.
	unittestDiscover = regexp.MustCompile(`\bpython[0-9.]*\s+-m\s*unittest\s+discover\b([^\n;&|)` + "`" + `]*)`)
	// taskFiles are the CI and task files at the repository root whose commands count.
	taskFiles = regexp.MustCompile(`^(\.github/workflows/[^/]+\.ya?ml|\.gitlab-ci\.ya?ml|\.circleci/config\.ya?ml|Makefile|GNUmakefile|makefile|justfile|Taskfile\.ya?ml|tox\.ini|noxfile\.py|package\.json)$`)
)

// maxEvidenceBytes bounds how much of a file the evidence scans read.
const maxEvidenceBytes = 1 << 20

// nodeTestSuites returns a node suite for the test files that import node:test and lie
// outside every suite in taken: rooted at their package (the nearest directory with a
// package.json), else at their directory (above a test/ directory), with the globs that
// match them.
func nodeTestSuites(repo *gitx.Repo, files []string, taken []config.Suite) []config.Suite {
	globs := map[string][]string{} // root -> test globs
	var roots []string
	for _, f := range files {
		name := path.Base(f)
		inTestDir := slices.Contains(strings.Split(path.Dir(f), "/"), "test")
		if !nodeTestName.MatchString(name) && !(inTestDir && nodeTestExt(name)) || pathutil.IsVendored(f) || inFixtureDir(path.Dir(f)) ||
			slices.ContainsFunc(taken, func(s config.Suite) bool { return isJS(s.Runner) && pathutil.Under(f, s.Root) }) ||
			!nodeTestImport.Match(readHead(repo.Root, f)) {
			continue
		}
		root := nodeTestRoot(f, files)
		glob := nodeTestGlob(pathutil.RelTo(f, root))
		if _, ok := globs[root]; !ok {
			roots = append(roots, root)
		}
		if !slices.Contains(globs[root], glob) {
			globs[root] = append(globs[root], glob)
		}
	}
	slices.Sort(roots)
	var out []config.Suite
	for _, r := range roots {
		if i := slices.IndexFunc(out, func(s config.Suite) bool { return pathutil.Under(r, s.Root) }); i >= 0 {
			// A root below another one: its files go to the outer suite.
			for _, g := range globs[r] {
				if g2 := pathutil.RelTo(path.Join(r, g), out[i].Root); !slices.Contains(out[i].Tests, g2) {
					out[i].Tests = append(out[i].Tests, g2)
				}
			}
			continue
		}
		out = append(out, config.Suite{Root: r, Runner: "node", Tests: globs[r]})
	}
	return out
}

func isJS(r string) bool {
	return r == "vitest" || r == "jest" || r == "bun" || r == "node"
}

func nodeTestExt(name string) bool {
	switch path.Ext(name) {
	case ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts":
		return true
	}
	return false
}

// nodeTestRoot is the root of the suite holding node test file f: the nearest directory
// above it with a package.json, else the directory above its test/ directory, else its
// own directory.
func nodeTestRoot(f string, files []string) string {
	for d := path.Dir(f); ; d = path.Dir(d) {
		if slices.Contains(files, path.Join(d, "package.json")) {
			return d
		}
		if d == "." {
			break
		}
	}
	segs := strings.Split(path.Dir(f), "/")
	if i := slices.Index(segs, "test"); i >= 0 {
		if i == 0 {
			return "."
		}
		return strings.Join(segs[:i], "/")
	}
	return path.Dir(f)
}

// nodeTestGlob is the glob, relative to its suite's root, collecting test file rel and
// its siblings of the same kind. A file that is a test only by sitting in a test/
// directory above its suite's root (the root is a package inside it) matches no
// pattern there: it stays a path of its own.
func nodeTestGlob(rel string) string {
	dir, name := path.Split(rel)
	if segs := strings.Split(strings.TrimSuffix(dir, "/"), "/"); slices.Contains(segs, "test") {
		i := slices.Index(segs, "test")
		return path.Join(append(segs[:i+1], "**", "*"+path.Ext(name))...)
	}
	m := nodeTestName.FindStringSubmatch(name)
	if m == nil {
		return rel
	}
	ext := "." + m[4]
	var g string
	switch {
	case m[1] != "":
		g = "*" + m[1] + ext
	case m[2] != "":
		g = m[2] + "*" + ext
	default:
		g = m[3] + ext
	}
	return path.Join(dir, g)
}

// unittestSuites returns a unittest suite for each start directory that a CI or task
// file at the repository root discovers with `python -m unittest discover -s DIR`, when
// DIR holds files of the repository outside every suite in taken. The suite is rooted
// at DIR with discover's pattern; a command with its own top-level directory (-t) keeps
// it and runs from the repository root.
func unittestSuites(repo *gitx.Repo, files []string, taken []config.Suite) []config.Suite {
	var out []config.Suite
	for _, tf := range files {
		if !taskFiles.MatchString(tf) {
			continue
		}
		for _, m := range unittestDiscover.FindAllSubmatch(readHead(repo.Root, tf), -1) {
			words, err := runner.SplitWords(string(m[1]))
			if err != nil {
				continue
			}
			start, pattern, top := "", "", ""
			for i := 0; i < len(words); i++ {
				name, v, joined := strings.Cut(words[i], "=")
				if !joined && i+1 < len(words) {
					v = words[i+1]
				}
				switch name {
				case "-s", "--start-directory":
					start = v
				case "-p", "--pattern":
					pattern = v
				case "-t", "--top-level-directory":
					top = v
				default:
					continue
				}
				if !joined {
					i++
				}
			}
			start = strings.TrimPrefix(path.Clean(start), "./")
			if start == "" || start == "." || strings.HasPrefix(start, "..") || path.IsAbs(start) || strings.Contains(start, "$") {
				continue
			}
			if !slices.ContainsFunc(files, func(f string) bool { return pathutil.Under(f, start) && f != start }) {
				continue // not a directory of the repository (another working directory)
			}
			s := config.Suite{Root: start, Runner: "unittest"}
			if top != "" {
				s.Root, s.Command = ".", strings.TrimSpace(string(m[0]))
			} else if pattern != "" && pattern != "test*.py" {
				s.Command = "python3 -m unittest discover -p " + quoteWord(pattern)
			}
			if slices.ContainsFunc(append(taken, out...), func(o config.Suite) bool {
				return (o.Runner == "unittest" || o.Runner == "pytest") && (pathutil.Under(start, o.Root) || o.Root == s.Root && o.Command == s.Command)
			}) {
				continue
			}
			out = append(out, s)
		}
	}
	return out
}

func quoteWord(w string) string {
	if strings.ContainsAny(w, " *?[]'\"$\\") {
		return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return w
}

// readHead reads up to maxEvidenceBytes of a repository file.
func readHead(root, rel string) []byte {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, maxEvidenceBytes))
	return data
}
