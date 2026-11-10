package detect

import (
	"fmt"
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

var (
	gradleSettingsFiles = []string{"settings.gradle", "settings.gradle.kts"}
	gradleBuildFiles    = []string{"build.gradle", "build.gradle.kts"}
)

// otherJVMTests are the test source roots of JVM languages smates does not read: their
// test classes are never units.
var otherJVMTests = []struct{ dir, ext, lang string }{
	{"src/test/kotlin/", ".kt", "Kotlin"},
	{"src/test/groovy/", ".groovy", "Groovy"},
	{"src/test/scala/", ".scala", "Scala"},
}

// gradleProject is a project of a Gradle build.
type gradleProject struct {
	path string // ":app:core"; ":" for the root project
	dir  string // repo-relative
}

// gradleSuites returns a gradle suite per project of a Gradle build with Java tests
// (src/test/java), and notes on what smates cannot run there: tests in other JVM
// languages, and Maven builds.
//
// A build is a directory with a settings file, or a build script without one in it or
// above it. Its projects are the root project and those its settings include. When the
// settings name a project in a way smates cannot read (a computed name or directory),
// every directory of the build with a build script counts as the project its path names
// instead, with a note: the suites' commands assume that layout.
func gradleSuites(repo *gitx.Repo, dirs []string, dirFiles map[string][]string, files []string) ([]config.Suite, []string) {
	has := func(d string, names []string) bool {
		return slices.ContainsFunc(dirFiles[d], func(n string) bool { return slices.Contains(names, n) })
	}
	// dirs is sorted, so a build with settings comes before the directories below it.
	var builds []string
	for _, d := range dirs {
		if underTestdata(d) {
			continue
		}
		if has(d, gradleSettingsFiles) || has(d, gradleBuildFiles) && !slices.ContainsFunc(builds, func(b string) bool { return pathutil.Under(d, b) && has(b, gradleSettingsFiles) }) {
			builds = append(builds, d)
		}
	}
	javaTests := testRoots(files, "src/test/java/", ".java")
	var suites []config.Suite
	var notes []string
	for _, b := range builds {
		projects, ok := gradleProjects(repo, b, dirs, dirFiles, builds)
		if !ok {
			notes = append(notes, fmt.Sprintf("%s: the settings of this Gradle build include projects smates cannot read (a computed name or directory): its suites assume each project's directory is its path (app/core is :app:core); check their commands", b))
		}
		for _, p := range projects {
			label := "Gradle project " + p.path
			if javaTests[p.dir] {
				s := config.Suite{Root: p.dir, Runner: "gradle"}
				if p.dir != defaultProjectDir(b, p.path) {
					s.Command = runner.GradleCommand(repo.Root, p.dir, p.path)
				}
				suites = append(suites, s)
			}
			for _, o := range otherJVMTests {
				if !testRoots(files, o.dir, o.ext)[p.dir] {
					continue
				}
				if javaTests[p.dir] {
					notes = append(notes, fmt.Sprintf("%s: the %s tests of %s (%s) are no test units: smates reads Java tests only, so they run only when the suite runs in full", p.dir, o.lang, label, strings.TrimSuffix(o.dir, "/")))
				} else {
					notes = append(notes, fmt.Sprintf("%s: %s has %s tests only (%s): smates reads Java tests only, so it gets no suite%s", p.dir, label, o.lang, strings.TrimSuffix(o.dir, "/"), ignoreHint(p.dir)))
				}
			}
		}
	}
	notes = append(notes, mavenNotes(dirs, dirFiles)...)
	return suites, notes
}

// ignoreHint suggests ignoring the tree of a project smates cannot run (not the whole
// repository).
func ignoreHint(dir string) string {
	if dir == "." {
		return "; changes to it are orphans, which run every suite in full"
	}
	return fmt.Sprintf("; changes to it are orphans, which run every suite in full: if no other suite's tests use it, add %q to ignore: in smates.yml", dir+"/**")
}

// mavenNotes notes the Maven builds (the topmost pom.xml of a tree) that have tests:
// smates has no runner for them. A directory that is also built by Gradle (a build
// script next to the pom.xml) is left to the gradle suites.
func mavenNotes(dirs []string, dirFiles map[string][]string) []string {
	var poms, notes []string
	for _, d := range dirs {
		names := dirFiles[d]
		if underTestdata(d) || !slices.Contains(names, "pom.xml") ||
			slices.ContainsFunc(poms, func(p string) bool { return pathutil.Under(d, p) }) ||
			slices.ContainsFunc(names, func(n string) bool {
				return slices.Contains(gradleBuildFiles, n) || slices.Contains(gradleSettingsFiles, n)
			}) {
			continue
		}
		poms = append(poms, d)
		tests := false
		for _, t := range dirs {
			if pathutil.Under(t, d) && (strings.HasSuffix(t, "/src/test") || strings.Contains(t, "/src/test/") || strings.HasPrefix(t, "src/test")) {
				tests = true
				break
			}
		}
		if tests {
			notes = append(notes, fmt.Sprintf("%s: a Maven build (pom.xml): smates has no Maven runner, so its tests get no suite%s", d, ignoreHint(d)))
		}
	}
	return notes
}

// testRoots returns the project directories that hold test files with extension ext
// under root (src/test/java/): the directory above root ("." at the top).
func testRoots(files []string, root, ext string) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f, ext) || pathutil.IsVendored(f) {
			continue
		}
		i := strings.Index("/"+f, "/"+root)
		if i < 0 {
			continue
		}
		dir := strings.TrimSuffix(f[:i], "/")
		if dir == "" {
			dir = "."
		}
		out[dir] = true
	}
	return out
}

// defaultProjectDir is the directory of a project of the build at build when the
// settings do not move it: its path below the build root (":app:core" is app/core).
func defaultProjectDir(build, project string) string {
	rel := strings.ReplaceAll(strings.Trim(project, ":"), ":", "/")
	if rel == "" {
		return build
	}
	return pathutil.Join(build, rel)
}

var (
	includeWord     = regexp.MustCompile(`\binclude\b`)
	projectDirWord  = regexp.MustCompile(`\bprojectDir\b`)
	projectDirAssig = regexp.MustCompile(`\bproject\s*\(\s*['"](:?[\w.:-]+)['"]\s*\)\s*\.\s*projectDir\s*=\s*(?:file\s*\(|new\s+File\s*\(\s*(?:settingsDir|rootDir)\s*,)\s*['"]([^'"$]+)['"]\s*\)`)
)

// gradleProjects returns the projects of the build rooted at build: the root project,
// the projects its settings include (and their parents) in their directories, moved
// where the settings set projectDir. ok is false when the settings include or move a
// project smates cannot read; the projects are then the directories of the build with a
// build script, at the paths their directories name.
func gradleProjects(repo *gitx.Repo, build string, dirs []string, dirFiles map[string][]string, builds []string) ([]gradleProject, bool) {
	var src string
	for _, s := range gradleSettingsFiles {
		if slices.Contains(dirFiles[build], s) {
			data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(build), s))
			if err == nil {
				src += runner.StripGroovyComments(string(data)) + "\n"
			}
		}
	}
	paths, ok := gradleIncludes(src)
	moved := map[string]string{}
	for _, m := range projectDirAssig.FindAllStringSubmatch(src, -1) {
		moved[":"+strings.TrimPrefix(m[1], ":")] = m[2]
	}
	if len(projectDirWord.FindAllStringIndex(src, -1)) > len(projectDirAssig.FindAllStringIndex(src, -1)) {
		ok = false
	}
	if !ok {
		var out []gradleProject
		for _, d := range dirs {
			if d == build || !pathutil.Under(d, build) || !slices.ContainsFunc(dirFiles[d], func(n string) bool { return slices.Contains(gradleBuildFiles, n) }) ||
				slices.ContainsFunc(builds, func(b string) bool { return b != build && pathutil.Under(d, b) }) || buildOutput(pathutil.RelTo(d, build)) {
				continue
			}
			out = append(out, gradleProject{path: ":" + strings.ReplaceAll(pathutil.RelTo(d, build), "/", ":"), dir: d})
		}
		return append([]gradleProject{{path: ":", dir: build}}, out...), false
	}
	var dirOf func(p string) (string, bool)
	dirOf = func(p string) (string, bool) {
		if d, ok := moved[p]; ok {
			d = path.Clean(d)
			if path.IsAbs(d) || d == ".." || strings.HasPrefix(d, "../") {
				return "", false // outside the build: no suite smates could run from it
			}
			return pathutil.Join(build, d), true
		}
		i := strings.LastIndexByte(p, ':')
		if i <= 0 {
			return pathutil.Join(build, p[1:]), true
		}
		parent, ok := dirOf(p[:i])
		return pathutil.Join(parent, p[i+1:]), ok
	}
	out := []gradleProject{{path: ":", dir: build}}
	for _, p := range paths {
		if d, ok := dirOf(p); ok {
			out = append(out, gradleProject{path: p, dir: d})
		}
	}
	return out, true
}

// buildOutput reports whether a directory relative to a build root is one Gradle owns
// rather than a project (buildSrc, build outputs, caches).
func buildOutput(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == "buildSrc" || seg == "build" || seg == ".gradle" {
			return true
		}
	}
	return false
}

// gradleIncludes returns the project paths the include statements of settings (comments
// stripped) name, with their parents (":a:b" includes ":a"), sorted. ok is false when
// an include names a project by anything but string literals.
func gradleIncludes(src string) ([]string, bool) {
	set := map[string]bool{}
	ok := true
	for _, loc := range includeWord.FindAllStringIndex(src, -1) {
		lits, good := includeArgs(src[loc[1]:])
		if !good {
			ok = false
			continue
		}
		for _, l := range lits {
			p := ":" + strings.TrimPrefix(l, ":")
			for ; p != ""; p = p[:strings.LastIndexByte(p, ':')] {
				set[p] = true
			}
		}
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, ok
}

// includeArgs reads the arguments of an include call: string literals separated by
// commas, in parentheses or not (Groovy's `include 'a', 'b'` may continue on the next
// line after a comma). ok is false for anything else (a variable, an interpolated
// string, a spread list).
func includeArgs(s string) ([]string, bool) {
	i := 0
	space := func(newlines bool) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || newlines && s[i] == '\n') {
			i++
		}
	}
	space(false)
	paren := i < len(s) && s[i] == '('
	if paren {
		i++
	}
	var out []string
	for {
		space(true)
		if paren && len(out) > 0 && i < len(s) && s[i] == ')' {
			return out, true // a trailing comma (Kotlin)
		}
		if i >= len(s) || s[i] != '\'' && s[i] != '"' {
			return nil, false
		}
		q := s[i]
		end := strings.IndexByte(s[i+1:], q)
		if end < 0 {
			return nil, false
		}
		lit := s[i+1 : i+1+end]
		if strings.ContainsAny(lit, "$\n") || lit == "" {
			return nil, false
		}
		out = append(out, lit)
		i += end + 2
		space(paren)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if paren {
			return out, i < len(s) && s[i] == ')'
		}
		return out, i >= len(s) || s[i] == '\n' || s[i] == ';' || s[i] == '}'
	}
}
