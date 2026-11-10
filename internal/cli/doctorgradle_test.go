package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

func TestGradleVersions(t *testing.T) {
	for _, tc := range []struct{ props, want string }{
		{"distributionUrl=https\\://services.gradle.org/distributions/gradle-9.4.1-bin.zip\n", "9.4.1"},
		{"distributionBase=GRADLE_USER_HOME\ndistributionUrl=https\\://example.com/gradle-8.14-all.zip", "8.14"},
		{"distributionUrl=https\\://services.gradle.org/distributions/gradle-9.0-rc-1-bin.zip", "9.0-rc-1"},
		{"#distributionUrl=gradle-1.0-bin.zip\n", ""},
		{"", ""},
	} {
		if got := wrapperGradleVersion(tc.props); got != tc.want {
			t.Errorf("wrapperGradleVersion(%q) = %q, want %q", tc.props, got, tc.want)
		}
	}
	if got := gradleVersionOutput("\n------\nGradle 9.4.1\n------\n\nBuild time: x\n"); got != "9.4.1" {
		t.Errorf("gradleVersionOutput = %q", got)
	}
	for _, tc := range []struct {
		out  string
		want int
	}{
		{`openjdk version "21.0.2" 2024-01-16`, 21},
		{`java version "1.8.0_402"`, 8},
		{`openjdk version "17" 2021-09-14`, 17},
		{`openjdk version "25-ea" 2025-09-16`, 25},
		{`java version "1."`, 0},
		{"command not found", 0},
	} {
		if got := javaMajor(tc.out); got != tc.want {
			t.Errorf("javaMajor(%q) = %d, want %d", tc.out, got, tc.want)
		}
	}
}

func TestToolchainVersions(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"app/build.gradle":                  "java { toolchain { languageVersion = JavaLanguageVersion.of(11) } }",
		"lib/build.gradle.kts":              "java { toolchain { languageVersion.set(JavaLanguageVersion.of(\"17\")) } }",
		"buildSrc/src/main/groovy/c.gradle": "// languageVersion = JavaLanguageVersion.of(8)\n",
		"other/build.gradle":                "JavaLanguageVersion.of(21)", // another build
		"app/Notes.java":                    "JavaLanguageVersion.of(25)", // no build script
	}
	var list []string
	for f, c := range files {
		testutil.Write(t, dir, f, c)
		list = append(list, f)
	}
	for _, f := range []string{"app/build.gradle", "lib/build.gradle.kts", "buildSrc/src/main/groovy/c.gradle", "app/Notes.java"} {
		list = append(list, "b/"+f)
		testutil.Write(t, dir, "b/"+f, files[f])
	}
	got, by := toolchainVersions(dir, "b", list)
	if len(got) != 2 || got[0] != 11 || got[1] != 17 || !reflect.DeepEqual(by[11], []string{"b/app/build.gradle"}) || !reflect.DeepEqual(by[17], []string{"b/lib/build.gradle.kts"}) {
		t.Errorf("toolchainVersions = %v, %v", got, by)
	}
}

// doctor checks a gradle suite's wrapper, Gradle and Java versions, and its tests in
// other JVM languages.
func TestDoctorGradle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake java is a shell script")
	}
	jdk := t.TempDir()
	testutil.WriteMode(t, jdk, "bin/java", "#!/bin/sh\necho 'openjdk version \"21.0.2\" 2024-01-16' >&2\n", 0o755)
	t.Setenv("JAVA_HOME", jdk)
	g := testutil.NewGitRepo(t, map[string]string{
		"settings.gradle": "include 'app', 'lib'",
		"gradlew":         "#!/bin/sh\n",
		"gradle/wrapper/gradle-wrapper.properties": "distributionUrl=https\\://services.gradle.org/distributions/gradle-8.10-bin.zip\n",
		"app/build.gradle":                         "java { toolchain { languageVersion = JavaLanguageVersion.of(11) } }",
		"app/src/test/java/p/ATest.java":           "package p; class ATest { @Test void a() {} }",
		"app/src/test/kotlin/p/KTest.kt":           "class KTest",
		"lib/src/test/java/p/LTest.java":           "package p; class LTest { @Test void a() {} }",
		"lib/build.gradle":                         "java { toolchain { languageVersion = JavaLanguageVersion.of(21) } }", // mixed: 11 still warns
	})
	g.Write("smates.yml", "suites:\n  - {name: app, runner: gradle, root: app}\n  - {name: lib, runner: gradle, root: lib}\n")
	var out, errb bytes.Buffer
	Main([]string{"doctor", "-C", g.Dir}, &out, &errb)
	for _, want := range []string{
		"suite app: Gradle 8.10 (",
		"does not tell smates about assumption failures",
		"gradle build .: the tests run on the Java 11 toolchain app/build.gradle requests (and some projects request others); recording needs Java 17",
		"suite app: 1 Kotlin test file(s) in app/src/test/kotlin are no test units",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor lacks %q:\n%s", want, out.String())
		}
	}
	// One check per launcher and build, not per suite.
	if n := strings.Count(out.String(), "Gradle 8.10"); n != 1 {
		t.Errorf("the wrapper is checked %d times:\n%s", n, out.String())
	}
	if strings.Contains(out.String(), "suite lib: 1 Kotlin") {
		t.Errorf("lib has no Kotlin tests:\n%s", out.String())
	}

	// No wrapper: gradle on PATH (a fake one), and a JDK at 17 without a toolchain.
	bin := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	testutil.WriteMode(t, bin, "gradle", "#!/bin/sh\nprintf '\\n-----\\nGradle 9.4.1\\n-----\\n'\n", 0o755)
	t.Setenv("PATH", bin)
	g.Remove("gradlew")
	g.Write("app/build.gradle", "plugins { id 'java' }")
	g.Remove("lib/build.gradle")
	testutil.WriteMode(t, jdk, "bin/java", "#!/bin/sh\necho 'openjdk version \"17.0.9\"' >&2\n", 0o755)
	out.Reset()
	Main([]string{"doctor", "-C", g.Dir}, &out, &errb)
	for _, want := range []string{
		"suite app: Gradle 9.4.1 (gradle --version)",
		"gradle build .: the tests run on Java 17 (JAVA_HOME; no toolchain requested)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor lacks %q:\n%s", want, out.String())
		}
	}

	// Neither a wrapper nor gradle, and no Java.
	os.Remove(filepath.Join(bin, "gradle"))
	t.Setenv("JAVA_HOME", filepath.Join(jdk, "missing"))
	out.Reset()
	Main([]string{"doctor", "-C", g.Dir}, &out, &errb)
	for _, want := range []string{
		"FAIL  suite app: gradle not found on PATH",
		"FAIL  gradle build .: no Java found (JAVA_HOME)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor lacks %q:\n%s", want, out.String())
		}
	}
}
