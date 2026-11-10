package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/static/java"
	"github.com/dotneet/smates/internal/trace"
)

func init() { Register(gradle{}) }

// gradle runs the Java tests of a Gradle project on the JUnit Platform.
//
// A unit is a test class's source file (src/test/java/**/*.java by default) that
// declares a concrete top-level class that may run tests (see gradleUnits); @Nested
// classes belong to the file of their outer class. smates passes an init script
// (shims/gradle/smates.init.gradle) that, for every Test task of the build, turns off
// up-to-date checks and the build cache (an up-to-date task runs nothing and reports
// nothing), narrows the task to the selected classes (Test.setTestNameIncludePatterns,
// the --tests option, which Gradle intersects with the task's own filters), and writes
// one JSON line per test and per class from a TestListener. Without recording (there
// is none yet), a pass is reported but recorded as unknown: nothing traced what the
// class used (Observation.NoTrace).
type gradle struct{}

func (gradle) Name() string { return "gradle" }

// gradleSettings are the files that make a directory the root of a Gradle build.
var gradleSettings = []string{"settings.gradle", "settings.gradle.kts"}

func (gradle) Defaults(repo *gitx.Repo, root string) Defaults {
	d := Defaults{
		Tests:   []string{"src/test/java/**/*.java"},
		Exclude: []string{"**/build/**"},
	}
	if repo == nil {
		d.Command = "gradle test"
		return d
	}
	build := gradleBuildRoot(repo.Root, root)
	d.Command = gradleCommand(repo.Root, root, build)
	d.Inputs = gradleInputs(repo.Root, build)
	return d
}

// gradleBuildRoot returns the repo-relative directory of the Gradle build a project
// directory belongs to: the nearest one holding a settings file, from root up to the
// repository root; root itself when there is none (a build without settings).
func gradleBuildRoot(repoRoot, root string) string {
	for d := root; ; d = path.Dir(d) {
		for _, s := range gradleSettings {
			if exists(filepath.Join(repoRoot, filepath.FromSlash(d), s)) {
				return d
			}
		}
		if d == "." || d == "/" || d == "" {
			return root
		}
	}
}

// gradleCommand is the default command of a suite rooted at root: the nearest wrapper
// between root and the repository root (else gradle on PATH) with the project's test
// task, by its path from the build root (":app:test"; "test" for the root project).
func gradleCommand(repoRoot, root, build string) string {
	exe := "gradle"
	name := "gradlew"
	if runtime.GOOS == "windows" {
		name = "gradlew.bat"
	}
	for d := root; ; d = path.Dir(d) {
		if exists(filepath.Join(repoRoot, filepath.FromSlash(d), name)) {
			rel, err := filepath.Rel(filepath.Join(repoRoot, filepath.FromSlash(root)), filepath.Join(repoRoot, filepath.FromSlash(d), name))
			if err == nil {
				exe = filepath.ToSlash(rel)
				if !strings.Contains(exe, "/") {
					exe = "./" + exe
				}
			}
			break
		}
		if d == "." || d == "/" || d == "" {
			break
		}
	}
	return exe + " " + gradleTaskPath(root, build, "test")
}

// gradleTaskPath returns the path of a task of the project in directory root, in the
// build rooted at build. It assumes the default layout (a project's directory is its
// path); a build that maps projects elsewhere names the task in the suite's command.
func gradleTaskPath(root, build, task string) string {
	rel := pathutil.RelTo(root, build)
	if rel == "." || !pathutil.Under(root, build) {
		return task
	}
	return ":" + strings.ReplaceAll(rel, "/", ":") + ":" + task
}

// gradleInputs are the files that can change the outcome of every test of a project
// of the build rooted at build: its settings, every build script and properties file
// of the build (a project's tests may depend on any other project), version catalogs,
// lockfiles, the wrapper, buildSrc and included builds. They are repo-relative (a
// leading "/").
func gradleInputs(repoRoot, build string) []string {
	j := func(p string) string { return "/" + pathutil.Join(build, p) }
	in := []string{"/.tool-versions", "/.java-version", "/.sdkmanrc"}
	for _, p := range []string{
		"settings.gradle", "settings.gradle.kts", "settings-gradle.lockfile",
		"**/build.gradle", "**/build.gradle.kts", "**/gradle.properties", "**/gradle.lockfile",
		"**/gradle/dependency-locks/**", "gradle/*.versions.toml", "gradle/wrapper/gradle-wrapper.properties",
		"gradlew", "gradlew.bat", "buildSrc/**",
	} {
		in = append(in, j(p))
	}
	for _, dir := range includedBuilds(repoRoot, build) {
		in = append(in, j(dir))
	}
	return in
}

var (
	includeBuildWord    = regexp.MustCompile(`\bincludeBuild\b`)
	includeBuildLiteral = regexp.MustCompile(`\bincludeBuild\s*\(?\s*(?:file\s*\(\s*)?(['"])([^'"$]+)['"]`)
)

// includedBuilds returns the trees (dir/**, relative to the build root) of the builds
// the settings include with includeBuild. One whose directory is not a string literal
// makes the whole build an input ("**"): smates cannot tell which files it holds.
func includedBuilds(repoRoot, build string) []string {
	var out []string
	for _, s := range gradleSettings {
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(build), s))
		if err != nil {
			continue
		}
		src := stripGroovyComments(string(data))
		lits := includeBuildLiteral.FindAllStringSubmatch(src, -1)
		if len(includeBuildWord.FindAllStringIndex(src, -1)) > len(lits) {
			return []string{"**"}
		}
		for _, m := range lits {
			dir := pathutil.Join(m[2])
			if dir == "." || strings.HasPrefix(dir, "../") || path.IsAbs(dir) {
				// The build itself, or a directory outside it: every file the input
				// glob can name (outside the repository there are none to track).
				if dir == "." {
					return []string{"**"}
				}
				continue
			}
			out = append(out, dir+"/**")
		}
	}
	sort.Strings(out)
	return out
}

var groovyComment = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`)

// stripGroovyComments drops comments from a Groovy or Kotlin build script, so a
// commented-out includeBuild is not counted. A "//" inside a string literal (a URL)
// cuts the rest of its line, which can only hide a literal and so add "**".
func stripGroovyComments(s string) string { return groovyComment.ReplaceAllString(s, "") }

// testAnnotations are the annotations that make a class run tests: JUnit Jupiter's
// test methods and @Nested, JUnit 4's @Test and @RunWith, JUnit Platform suites,
// jqwik's properties and examples, and TestNG's @Test.
var testAnnotations = []string{
	"Test", "ParameterizedTest", "RepeatedTest", "TestFactory", "TestTemplate", "Nested",
	"RunWith", "Theory", "Suite", "Property", "Example", "ArchTest",
}

// testClassSuffixes name classes that are tests by convention: a class named so is a
// unit even when no annotation smates knows marks it (an engine's own annotation).
var testClassSuffixes = []string{"Test", "Tests", "TestCase", "IT", "ITCase"}

// javaLang are java.lang classes a test-support class may extend: such a class runs no
// test. Any other superclass smates cannot find among the suite's test files may be a
// test base class of a library, and keeps the class a unit.
var javaLang = map[string]bool{
	"Object": true, "Exception": true, "RuntimeException": true, "Error": true, "Throwable": true,
	"Thread": true, "Number": true, "IllegalArgumentException": true, "IllegalStateException": true,
	"AssertionError": true, "ClassLoader": true, "UnsupportedOperationException": true,
}

func (gradle) Units(s *Suite, testFiles []string) []string {
	read := func(f string) []byte {
		if s.RepoRoot == "" {
			return nil
		}
		b, _ := os.ReadFile(filepath.Join(s.RepoRoot, filepath.FromSlash(f)))
		return b
	}
	return unitsAreFiles(gradleUnits(testFiles, read))
}

// gradleUnits returns the test files that declare a concrete top-level class that may
// run tests: one carrying a test annotation (testAnnotations, and annotations declared
// in the test files that carry one: composed annotations) or named like a test
// (testClassSuffixes), or extending a class that may run tests. A superclass found
// among the files is followed; one that is not (a library's) keeps the class a unit,
// unless it is a java.lang class or imported from java.* or javax.*. A file that
// cannot be read is a unit: smates cannot tell. Other files are test support code,
// dependencies of the units that use them.
func gradleUnits(files []string, read func(string) []byte) []string {
	infos := make(map[string]*java.Info, len(files))
	types := map[string]string{} // qualified top-level type -> file
	var units []string
	for _, f := range files {
		src := read(f)
		if src == nil {
			units = append(units, f)
			continue
		}
		in := java.Describe(src)
		infos[f] = in
		for _, d := range in.Decls {
			types[qualifyJava(in.Package, d.Name)] = f
		}
	}
	anns := map[string]bool{}
	for _, a := range testAnnotations {
		anns[a] = true
	}
	for changed := true; changed; {
		changed = false
		for _, in := range infos {
			if !annotated(in, anns) {
				continue
			}
			for _, d := range in.Decls {
				if d.Kind == "@interface" && !anns[d.Name] {
					anns[d.Name], changed = true, true
				}
			}
		}
	}
	memo := map[string]bool{}
	var runs func(f string, d java.Decl) bool
	runs = func(f string, d java.Decl) bool {
		in := infos[f]
		key := f + "#" + d.Name
		if v, ok := memo[key]; ok {
			return v
		}
		memo[key] = true // a cycle (not valid Java) counts as a test: smates cannot tell
		v := annotated(in, anns) || testName(d.Name) || d.Extends != "" && superRuns(in, d.Extends, types, infos, runs)
		memo[key] = v
		return v
	}
	for _, f := range files {
		in := infos[f]
		if in == nil {
			continue
		}
		for _, d := range in.Decls {
			if d.Kind == "class" && !d.Abstract && runs(f, d) {
				units = append(units, f)
				break
			}
		}
	}
	return units
}

// superRuns reports whether superclass name, as file in writes it, may run tests: the
// class it resolves to among the test files does, or it is none of them and not a JDK
// class (qualified or imported from java.* or javax.*, or a java.lang class neither
// imported nor declared in the file's package).
func superRuns(in *java.Info, name string, types map[string]string, infos map[string]*java.Info, runs func(string, java.Decl) bool) bool {
	for _, q := range in.Resolve(name) {
		f, ok := types[q]
		if !ok {
			continue
		}
		for _, d := range infos[f].Decls {
			if qualifyJava(infos[f].Package, d.Name) == q {
				return runs(f, d)
			}
		}
	}
	jdk := strings.HasPrefix(name, "java.") || strings.HasPrefix(name, "javax.") ||
		in.Imported(name, "java.") || in.Imported(name, "javax.") ||
		javaLang[name] && !in.Imported(name, "")
	return !jdk
}

func annotated(in *java.Info, anns map[string]bool) bool {
	for a := range in.Annotations {
		if anns[a] {
			return true
		}
	}
	return false
}

func testName(name string) bool {
	for _, s := range testClassSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

func qualifyJava(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// gradleInitScript is where the init script is extracted in a shim directory.
func gradleInitScript(shimDir string) string {
	return filepath.Join(shimDir, "gradle", "smates.init.gradle")
}

// Commands implements Commander: the suite command with the init script.
func (gradle) Commands(rc *RunContext) ([][]string, error) {
	argv, err := rc.CommandLine([]string{"--init-script", gradleInitScript(rc.ShimDir)}, nil)
	if err != nil {
		return nil, err
	}
	return [][]string{argv}, nil
}

func (g gradle) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	dir := rc.TraceDir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "smates-gradle-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}
	results := filepath.Join(dir, "gradle")
	if err := os.MkdirAll(results, 0o755); err != nil {
		return nil, err
	}
	classes := gradleClasses(rc)
	env := []string{"SMATES_GRADLE_RESULTS=" + results}
	if !rc.All {
		var names []string
		for c := range classes {
			names = append(names, c)
		}
		sort.Strings(names)
		list := filepath.Join(dir, "gradle-classes.txt")
		if err := os.WriteFile(list, []byte(strings.Join(names, "\n")+"\n"), 0o644); err != nil {
			return nil, err
		}
		env = append(env, "SMATES_GRADLE_CLASSES="+list)
	}
	argvs, err := g.Commands(rc)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	code, err := rc.Exec(ctx, argvs[0], env)
	res := &RunResult{ExitCode: code, Duration: time.Since(start)}
	if err != nil {
		return res, err
	}
	events, err := readGradleEvents(results)
	if err != nil {
		return res, err
	}
	res.Observations, res.Warnings = gradleObservations(rc.Units, classes, events, code, rc.Record)
	return res, nil
}

// gradleClasses maps the qualified names of the top-level classes the units declare to
// their unit. A unit that cannot be read is passed by the class its path names.
func gradleClasses(rc *RunContext) map[string]string {
	out := map[string]string{}
	for _, u := range rc.Units {
		src, err := os.ReadFile(filepath.Join(rc.Repo.Root, filepath.FromSlash(u)))
		if err != nil {
			out[classFromPath(rc.Suite, u)] = u
			continue
		}
		in := java.Describe(src)
		n := 0
		for _, d := range in.Decls {
			if d.Kind == "class" {
				out[qualifyJava(in.Package, d.Name)] = u
				n++
			}
		}
		if n == 0 {
			out[qualifyJava(in.Package, strings.TrimSuffix(path.Base(u), ".java"))] = u
		}
	}
	return out
}

// classFromPath derives a class name from a test file's path below src/<set>/java.
func classFromPath(s *Suite, u string) string {
	rel := strings.TrimSuffix(relToSuite(s, u), ".java")
	if i := strings.Index(rel, "/java/"); i >= 0 {
		rel = rel[i+len("/java/"):]
	}
	return strings.ReplaceAll(rel, "/", ".")
}

// gradleEvent is one line the init script's listener writes: a test ("test") or a
// test class ("class") that finished.
type gradleEvent struct {
	K      string `json:"k"`
	Task   string `json:"task"`
	Class  string `json:"class"`
	Name   string `json:"name"`
	Result string `json:"result"` // SUCCESS, FAILURE or SKIPPED
	Ms     int64  `json:"ms"`
	// Assumption is set for a skipped test: true when an assumption aborted it, false
	// when it was disabled; absent when Gradle cannot tell (before its TestResult had
	// getAssumptionFailure).
	Assumption *bool  `json:"assumption"`
	Msg        string `json:"msg"`
}

func readGradleEvents(dir string) ([]gradleEvent, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "results-*.jsonl"))
	sort.Strings(files)
	var out []gradleEvent
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		for sc.Scan() {
			var ev gradleEvent
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Class != "" {
				out = append(out, ev)
			}
		}
		f.Close()
	}
	return out, nil
}

// gradleObservations turns the listener's events into per-unit outcomes:
//
//   - a failed test or class fails its unit;
//   - otherwise a test an assumption aborted (or a skip Gradle cannot tell from
//     @Disabled) makes it unknown: it did not prove anything;
//   - otherwise a unit with events passes (tests disabled count as passing);
//   - a unit without events is unknown, with a warning, when the build succeeded (the
//     task's own filters or tags exclude it, or it declares no test); when the build
//     failed and no test reported anything, it failed before tests ran (a compile
//     error), and every unit fails (Inferred).
//
// Without tracing, a pass is marked NoTrace when recording: it is recorded as unknown.
func gradleObservations(units []string, classes map[string]string, events []gradleEvent, exitCode int, record bool) (map[string]*Observation, []string) {
	obs := map[string]*Observation{}
	unitOf := func(class string) string {
		if i := strings.IndexByte(class, '$'); i >= 0 {
			class = class[:i]
		}
		return classes[class]
	}
	type acc struct {
		fail, unproven, any bool
		classMs, testMs     int64
		msg                 string
	}
	accs := map[string]*acc{}
	for _, ev := range events {
		u := unitOf(ev.Class)
		if u == "" {
			continue
		}
		a := accs[u]
		if a == nil {
			a = &acc{}
			accs[u] = a
		}
		a.any = true
		switch ev.Result {
		case "FAILURE":
			a.fail = true
			label := ev.Name
			if ev.K == "class" {
				label = ev.Class
			}
			a.msg = trace.JoinMessage(a.msg, label+": "+ev.Msg)
		case "SKIPPED":
			if ev.Assumption == nil || *ev.Assumption {
				a.unproven = true
			}
		}
		if ev.K == "class" && !strings.Contains(ev.Class, "$") {
			a.classMs += ev.Ms
		} else if ev.K == "test" {
			a.testMs += ev.Ms
		}
	}
	var warnings []string
	if len(events) == 0 && exitCode != 0 {
		for _, u := range units {
			obs[u] = &Observation{Status: state.Fail, Inferred: true}
		}
		return obs, nil
	}
	var silent []string
	passed := 0
	for _, u := range units {
		a := accs[u]
		if a == nil {
			if exitCode == 0 {
				obs[u] = &Observation{Status: state.Unknown}
				silent = append(silent, u)
			}
			continue
		}
		o := &Observation{Status: state.Pass, DurationMs: a.classMs, Failure: a.msg}
		if o.DurationMs == 0 {
			o.DurationMs = a.testMs
		}
		switch {
		case a.fail:
			o.Status = state.Fail
		case a.unproven:
			o.Status = state.Unknown
		default:
			o.NoTrace = record
			passed++
		}
		obs[u] = o
	}
	if len(silent) > 0 {
		sort.Strings(silent)
		w := fmt.Sprintf("%d test class file(s) ran no test (the task's filters or tags exclude them, they declare none, or the command runs no Test task); they will run again next time", len(silent))
		if len(silent) <= 3 {
			w += ": " + strings.Join(silent, ", ")
		}
		warnings = append(warnings, w)
	}
	if record && passed > 0 {
		warnings = append(warnings, fmt.Sprintf("%d test class file(s) passed; the gradle runner does not trace tests yet, so their passes are recorded as not proven and they run again next time", passed))
	}
	return obs, warnings
}
