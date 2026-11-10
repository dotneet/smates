package cli

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/runner"
)

// Gradle versions the gradle runner relies on.
var (
	// gradleAssumptions is the first Gradle that tells test listeners about assumption
	// failures (TestResult.getAssumptionFailure): before it, smates cannot tell a test an
	// assumption aborted from a disabled one.
	gradleAssumptions = [3]int{8, 14, 0}
	// javaRecording is the first Java whose flight recorder has what recording needs.
	javaRecording = 17
)

// doctorGradle checks what the gradle suites need: the launcher of their command, the
// Gradle version, the Java version the tests run on, and test sources in other JVM
// languages, which are no units.
func doctorGradle(p *project, r *doctorReport) {
	checked := map[string]bool{} // launchers: a wrapper's path, or a command on PATH
	javaChecked := map[string]bool{}
	for _, s := range p.suites {
		if s.Runner != "gradle" {
			continue
		}
		words := shellWords(s.Command)
		for len(words) > 1 && strings.Contains(words[0], "=") { // FOO=1 ./gradlew test
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		exe := words[0]
		dir := filepath.Join(p.repo.Root, filepath.FromSlash(s.Root))
		l := exe
		if strings.ContainsAny(exe, `/\`) {
			l = filepath.Join(dir, filepath.FromSlash(exe))
		}
		if !checked[l] {
			checked[l] = true
			doctorGradleLauncher(s.Name, exe, dir, r)
		}
		build := gradleBuildOf(p.repo.Root, s.Root)
		if !javaChecked[build] {
			javaChecked[build] = true
			doctorGradleJava(p, s.Name, build, r)
		}
		if p.cfg.Path != "" { // without smates.yml, detection's notes said it
			doctorGradleOtherTests(p, s, r)
		}
	}
}

// doctorGradleLauncher checks the suite command's executable (a wrapper relative to the
// suite root, or a command on PATH) and the Gradle version it runs.
func doctorGradleLauncher(suite, exe, dir string, r *doctorReport) {
	const id = "toolchain-gradle"
	version, from := "", ""
	if strings.ContainsAny(exe, `/\`) {
		p := filepath.Join(dir, filepath.FromSlash(exe))
		if _, err := os.Stat(p); err != nil {
			r.add(checkFail, id, "suite %s: its command runs %s, which does not exist in %s", suite, exe, dir).about(suite).
				fix(doctorFix{Doc: "https://docs.gradle.org/current/userguide/gradle_wrapper.html"})
			return
		}
		props := filepath.Join(filepath.Dir(p), "gradle", "wrapper", "gradle-wrapper.properties")
		if data, err := os.ReadFile(props); err == nil {
			version, from = wrapperGradleVersion(string(data)), props
		}
	} else {
		if _, err := exec.LookPath(exe); err != nil {
			r.add(checkFail, id, "suite %s: %s not found on PATH (add the Gradle wrapper to the repository, or install Gradle)", suite, exe).about(suite).
				fix(doctorFix{Doc: "https://docs.gradle.org/current/userguide/installation.html"})
			return
		}
		out, _ := exec.Command(exe, "--version").Output()
		version, from = gradleVersionOutput(string(out)), exe+" --version"
	}
	if version == "" {
		r.add(checkWarn, id, "suite %s: the Gradle version %s runs is unknown (smates is tested with Gradle 9)", suite, exe).about(suite)
		return
	}
	v, _ := parsePartial(version)
	if cmpVersion(v, gradleAssumptions) < 0 {
		r.add(checkWarn, id, "suite %s: Gradle %s (%s) does not tell smates about assumption failures (Gradle 8.14 does): a test class with a skipped test is recorded as not proven and runs every time", suite, version, from).about(suite).
			fix(doctorFix{Doc: "https://docs.gradle.org/current/userguide/upgrading_version_8.html"})
		return
	}
	r.add(checkOK, id, "suite %s: Gradle %s (%s)", suite, version, from).about(suite)
}

var (
	wrapperDistRe   = regexp.MustCompile(`(?m)^\s*distributionUrl\s*[=:].*gradle-([0-9][0-9A-Za-z.\-]*?)-(?:bin|all)\.zip`)
	gradleVersionRe = regexp.MustCompile(`(?m)^Gradle ([0-9][0-9A-Za-z.\-]*)\s*$`)
	javaVersionRe   = regexp.MustCompile(`version "([0-9][^"]*)"`)
	toolchainRe     = regexp.MustCompile(`JavaLanguageVersion\s*\.\s*of\s*\(\s*["']?(\d+)["']?\s*\)`)
)

// wrapperGradleVersion is the Gradle version of a gradle-wrapper.properties
// (distributionUrl=...gradle-9.4.1-bin.zip), "" when it names none.
func wrapperGradleVersion(props string) string {
	if m := wrapperDistRe.FindStringSubmatch(props); m != nil {
		return m[1]
	}
	return ""
}

// gradleVersionOutput is the version `gradle --version` prints ("Gradle 9.4.1").
func gradleVersionOutput(out string) string {
	if m := gradleVersionRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// javaMajor is the major version `java -version` prints (1.8.0_402 is 8, 21.0.2 is
// 21), 0 when it prints none.
func javaMajor(out string) int {
	m := javaVersionRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	digits := strings.FieldsFunc(strings.TrimPrefix(m[1], "1."), func(r rune) bool { return r < '0' || r > '9' })
	if len(digits) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(digits[0])
	return n
}

// gradleBuildOf is the root of the Gradle build a project directory belongs to: the
// nearest directory with a settings file, else the project directory.
func gradleBuildOf(repoRoot, root string) string {
	for d := root; ; d = path.Dir(d) {
		if fileExists(repoRoot, joinRel(d, "settings.gradle")) || fileExists(repoRoot, joinRel(d, "settings.gradle.kts")) {
			return d
		}
		if d == "." || d == "/" || d == "" {
			return root
		}
	}
}

// toolchainVersions returns the Java versions the build scripts of the build at build
// request with JavaLanguageVersion.of (a toolchain), sorted, and the files requesting
// each.
func toolchainVersions(root, build string, files []string) ([]int, map[int][]string) {
	by := map[int][]string{}
	for _, f := range files {
		if !pathutil.Under(f, build) || !strings.HasSuffix(f, ".gradle") && !strings.HasSuffix(f, ".gradle.kts") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		for _, m := range toolchainRe.FindAllStringSubmatch(runner.StripGroovyComments(string(data)), -1) {
			n, _ := strconv.Atoi(m[1])
			if !slices.Contains(by[n], f) {
				by[n] = append(by[n], f)
			}
		}
	}
	versions := slices.Sorted(maps.Keys(by))
	for _, fs := range by {
		slices.Sort(fs)
	}
	return versions, by
}

// doctorGradleJava checks the Java version the tests of a build run on: the toolchain
// its build scripts request, else the JDK Gradle runs on (JAVA_HOME, else java on
// PATH). Recording needs Java 17; below it every pass stays not proven.
func doctorGradleJava(p *project, suite, build string, r *doctorReport) {
	const id = "toolchain-java"
	subject := "gradle build " + build
	below := func(what, who string) {
		r.add(checkWarn, id, "%s: the tests run on %s; recording needs Java %d or later, so %s: every pass is recorded as not proven and the test class runs every time. Run the tests on a Java %d+ toolchain (the compiled bytecode level can stay)", subject, what, javaRecording, who, javaRecording).about(subject).
			fix(doctorFix{Doc: docURL + "guides/runners/#gradle"})
	}
	java, how := "java", "java on PATH"
	if home := os.Getenv("JAVA_HOME"); home != "" {
		java, how = filepath.Join(home, "bin", "java"), "JAVA_HOME"
	}
	out, err := exec.Command(java, "-version").CombinedOutput()
	if err != nil {
		r.add(checkFail, id, "%s: no Java found (%s): Gradle needs a JDK to run; a missing one fails the build before any test runs, which smates reports as failing tests", subject, how).about(subject).
			fix(doctorFix{Doc: "https://docs.gradle.org/current/userguide/toolchains.html"})
		return
	}
	if versions, by := toolchainVersions(p.repo.Root, build, p.files); len(versions) > 0 {
		// Every project's tests run on its own toolchain: one below 17 leaves that
		// project's passes unproven, whatever the others request.
		if low := versions[0]; low < javaRecording {
			who := "no test class is recorded"
			if len(versions) > 1 {
				who = "the test classes of the projects on it are not recorded"
			}
			below(fmt.Sprintf("the Java %d toolchain %s requests%s", low, strings.Join(by[low], ", "), ifMore(len(versions) > 1, " (and some projects request others)")), who)
			return
		}
		r.add(checkOK, id, "%s: the tests run on the Java %s toolchain(s) the build scripts request", subject, joinInts(versions)).about(subject)
		return
	}
	v := javaMajor(string(out))
	switch {
	case v == 0:
		r.add(checkWarn, id, "%s: the Java version of %s is unknown; recording needs Java %d or later", subject, how, javaRecording).about(subject)
	case v < javaRecording:
		below(fmt.Sprintf("Java %d (%s)", v, how), "no test class is recorded")
	default:
		r.add(checkOK, id, "%s: the tests run on Java %d (%s; no toolchain requested)", subject, v, how).about(subject)
	}
}

// doctorGradleOtherTests warns about test sources of a gradle suite in JVM languages
// smates does not read: they are no units, so a selective run filters them out.
func doctorGradleOtherTests(p *project, s *runner.Suite, r *doctorReport) {
	for _, o := range []struct{ dir, ext, lang string }{
		{"src/test/kotlin/", ".kt", "Kotlin"}, {"src/test/groovy/", ".groovy", "Groovy"}, {"src/test/scala/", ".scala", "Scala"},
	} {
		prefix := joinRel(s.Root, o.dir)
		n := 0
		for _, f := range p.files {
			if strings.HasPrefix(f, prefix) && strings.HasSuffix(f, o.ext) {
				n++
			}
		}
		if n > 0 {
			r.add(checkWarn, "suite-gradle-other-tests", "suite %s: %d %s test file(s) in %s are no test units: smates reads Java tests only, so a selective run leaves them out and they run only when the suite runs in full", s.Name, n, o.lang, strings.TrimSuffix(prefix, "/")).about(s.Name).
				fix(doctorFix{Doc: docURL + "guides/runners/#gradle"})
		}
	}
}

func ifMore(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}
