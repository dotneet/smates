package runner

import (
	"maps"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

func TestGradleUnits(t *testing.T) {
	const dir = "src/test/java/com/example/"
	base := map[string]string{
		// Shared by the cases below.
		dir + "support/Fixtures.java":    "package com.example.support;\npublic class Fixtures { public static int one() { return 1; } }",
		dir + "support/BaseTest.java":    "package com.example.support;\nimport org.junit.jupiter.api.Test;\npublic abstract class BaseTest { @Test void inherited() {} }",
		dir + "support/PlainBase.java":   "package com.example.support;\npublic abstract class PlainBase { protected int x; }",
		dir + "support/FastTest.java":    "package com.example.support;\nimport org.junit.jupiter.api.Test;\n@Test\npublic @interface FastTest {}",
		dir + "support/Same.java":        "package com.example;\npublic abstract class Same { @org.junit.jupiter.api.Test void t() {} }",
		dir + "support/MidBase.java":     "package com.example.support;\npublic abstract class MidBase extends BaseTest {}",
		dir + "support/CycleA.java":      "package com.example.support;\npublic class CycleA extends CycleB {}",
		dir + "support/CycleB.java":      "package com.example.support;\npublic class CycleB extends CycleA {}",
		dir + "support/Generic.java":     "package com.example.support;\npublic abstract class Generic<T> { @org.junit.jupiter.api.RepeatedTest(2) void r() {} }",
		dir + "support/SomeLib.java":     "package com.example.support;\npublic class SomeLib {}",
		dir + "support/Annotated.java":   "package com.example.support;\n@Deprecated public class Annotated {}",
		dir + "support/Interface.java":   "package com.example.support;\nimport org.junit.jupiter.api.Test;\npublic interface Interface { @Test default void t() {} }",
		dir + "support/EnumOnly.java":    "package com.example.support;\npublic enum EnumOnly { A }",
		dir + "support/Error.java":       "package com.example.support;\npublic class Error extends RuntimeException {}",
		dir + "support/TimerJob.java":    "package com.example.support;\nimport java.util.TimerTask;\npublic class TimerJob extends TimerTask { public void run() {} }",
		dir + "support/SwingThing.java":  "package com.example.support;\npublic class SwingThing extends javax.swing.JPanel {}",
		dir + "support/SubContract.java": "package com.example.support;\npublic interface SubContract extends Interface {}",
		dir + "support/Plain.java":       "package com.example.support;\npublic interface Plain { int size(); }",
	}
	// Java sources outside the test files: main code, which cannot declare tests, and
	// test fixtures, which can.
	others := map[string]string{
		"src/main/java/com/example/repo/UserRepository.java":     "package com.example.repo;\npublic interface UserRepository extends org.lib.CrudRepository { }",
		"src/main/java/com/example/repo/AbstractRepo.java":       "package com.example.repo;\npublic abstract class AbstractRepo { }",
		"lib/src/main/java/com/example/lib/Clock.java":           "package com.example.lib;\npublic interface Clock { long now(); }",
		"src/testFixtures/java/com/example/fx/ListContract.java": "package com.example.fx;\nimport org.junit.jupiter.api.Test;\npublic interface ListContract { @Test default void adds() {} }",
		"src/testFixtures/java/com/example/fx/Holder.java":       "package com.example.fx;\npublic interface Holder { }",
		// A test-support project: contract tests in its main source set.
		"testing/src/main/java/com/example/testing/MainContract.java": "package com.example.testing;\nimport org.junit.jupiter.api.Test;\npublic interface MainContract { @Test default void adds() {} }",
		"testing/src/main/java/com/example/testing/RepoSpec.java":     "package com.example.testing;\nimport org.junit.jupiter.api.Test;\npublic abstract class RepoSpec { @Test void saves() {} }",
		"testing/src/main/java/com/example/testing/Layered.java":      "package com.example.testing;\npublic interface Layered extends MainContract { }",
		"testing/src/main/java/com/example/testing/LibBacked.java":    "package com.example.testing;\npublic abstract class LibBacked extends org.lib.Base { }",
	}
	for _, tc := range []struct {
		name string
		file string
		src  string
		want bool
	}{
		{"test method", "CartTest.java", "package com.example;\nimport org.junit.jupiter.api.Test;\nclass CartTest { @Test void adds() {} }", true},
		{"qualified annotation", "Q.java", "package com.example;\nclass Q { @org.junit.jupiter.api.Test void t() {} }", true},
		{"parameterized", "P.java", "package com.example;\nclass P { @ParameterizedTest @ValueSource(ints = 1) void t(int i) {} }", true},
		{"nested only", "Outer.java", "package com.example;\nclass Outer { @Nested class Inner { } }", true},
		{"junit 4 runner", "R.java", "package com.example;\n@RunWith(Foo.class) public class R {}", true},
		{"abstract with tests", "AbstractCart.java", "package com.example;\nabstract class AbstractCart { @Test void t() {} }", false},
		{"inherits tests through import", "Sub.java", "package com.example;\nimport com.example.support.BaseTest;\nclass Sub extends BaseTest {}", true},
		{"inherits tests through two levels", "Sub2.java", "package com.example;\nimport com.example.support.MidBase;\nclass Sub2 extends MidBase {}", true},
		{"inherits tests from same package", "Sub3.java", "package com.example;\nclass Sub3 extends Same {}", true},
		{"inherits tests through on-demand import", "Sub4.java", "package com.example;\nimport com.example.support.*;\nclass Sub4 extends BaseTest {}", true},
		{"inherits tests through qualified name", "Sub5.java", "package com.example;\nclass Sub5 extends com.example.support.BaseTest {}", true},
		{"generic base with tests", "Sub6.java", "package com.example;\nimport com.example.support.Generic;\nclass Sub6 extends Generic<String> {}", true},
		{"plain base", "Helper.java", "package com.example;\nimport com.example.support.PlainBase;\nclass Helper extends PlainBase {}", false},
		{"plain helper", "Util.java", "package com.example;\npublic class Util { static int two() { return 2; } }", false},
		{"other annotations only", "Ann.java", "package com.example;\n@Deprecated class Ann { @Override public String toString() { return \"\"; } }", false},
		{"composed annotation", "Composed.java", "package com.example;\nimport com.example.support.FastTest;\nclass Composed { @FastTest void quick() {} }", true},
		{"named like a test", "OrderIT.java", "package com.example;\nclass OrderIT { @Property void p() {} }", true},
		{"named like a test, no annotation", "LegacyTests.java", "package com.example;\nclass LegacyTests { public void testIt() {} }", true},
		{"secondary class is the test", "Pair.java", "package com.example;\nclass Pair {}\nclass PairCheck { @Test void t() {} }", true},
		// smates cannot tell: they stay units.
		{"library base class", "LibSub.java", "package com.example;\nimport org.lib.SpringTestBase;\nclass LibSub extends SpringTestBase {}", true},
		{"unknown simple base", "LibSub2.java", "package com.example;\nclass LibSub2 extends Unknown {}", true},
		{"library class named like java.lang", "LibThread.java", "package com.example;\nimport org.lib.Thread;\nclass LibThread extends Thread {}", true},
		{"cycle", "Cyc.java", "package com.example;\nimport com.example.support.CycleA;\nclass Cyc extends CycleA {}", true},
		// JDK superclasses run no test.
		{"java.lang base", "Ex.java", "package com.example;\nclass Ex extends RuntimeException {}", false},
		{"java.util base", "Job.java", "package com.example;\nimport java.util.TimerTask;\nclass Job extends TimerTask { public void run() {} }", false},
		{"qualified javax base", "Panel.java", "package com.example;\nclass Panel extends javax.swing.JPanel {}", false},
		{"interface", "Contract.java", "package com.example;\ninterface Contract { @Test default void t() {} }", false},
		// Tests inherited from an interface's default methods.
		{"implements a test interface", "ListBehaviour.java", "package com.example;\nimport com.example.support.Interface;\nclass ListBehaviour implements Interface {}", true},
		{"implements a test interface through two levels", "Deep.java", "package com.example;\nimport com.example.support.*;\nclass Deep extends Object implements Runnable, SubContract { public void run() {} }", true},
		{"implements a test-fixtures contract", "FxBehaviour.java", "package com.example;\nimport com.example.fx.ListContract;\nclass FxBehaviour implements ListContract {}", true},
		{"implements an unknown library interface", "LibImpl.java", "package com.example;\nimport org.lib.ContractTests;\nclass LibImpl implements ContractTests {}", true},
		{"abstract, implements a test interface", "AbsImpl.java", "package com.example;\nimport com.example.support.Interface;\nabstract class AbsImpl implements Interface {}", false},
		{"implements a plain test-file interface", "PlainImpl.java", "package com.example;\nimport com.example.support.Plain;\nclass PlainImpl implements Plain { public int size() { return 0; } }", false},
		{"implements a main interface (a fake)", "FakeRepo.java", "package com.example;\nimport com.example.repo.UserRepository;\nclass FakeRepo implements UserRepository {}", false},
		{"implements a main interface of another project", "FixedClock.java", "package com.example;\nimport com.example.lib.Clock;\nclass FixedClock implements Clock { public long now() { return 0; } }", false},
		{"implements a contract in a main source set", "MainImpl.java", "package com.example;\nimport com.example.testing.MainContract;\nclass MainImpl implements MainContract {}", true},
		{"extends a contract class in a main source set", "Spec.java", "package com.example;\nimport com.example.testing.RepoSpec;\nclass Spec extends RepoSpec {}", true},
		{"main interface extending a main contract", "LayeredImpl.java", "package com.example;\nimport com.example.testing.Layered;\nclass LayeredImpl implements Layered {}", true},
		{"main class extending a library class", "Backed.java", "package com.example;\nimport com.example.testing.LibBacked;\nclass Backed extends LibBacked {}", false},
		{"extends a main class", "MemRepo.java", "package com.example;\nimport com.example.repo.AbstractRepo;\nclass MemRepo extends AbstractRepo {}", false},
		{"implements a plain test-fixtures interface", "Held.java", "package com.example;\nimport com.example.fx.Holder;\nclass Held implements Holder {}", false},
		{"implements JDK interfaces", "Task.java", "package com.example;\nimport java.util.function.Supplier;\nclass Task implements Runnable, Supplier<String>, java.io.Closeable { public void run() {} public String get() { return \"\"; } public void close() {} }", false},
		{"implements a JUnit callback", "Ext.java", "package com.example;\nimport org.junit.jupiter.api.extension.BeforeEachCallback;\nclass Ext implements BeforeEachCallback {}", false},
		{"enum", "Kind.java", "package com.example;\nenum Kind { A }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{dir + tc.file: tc.src}
			for k, v := range base {
				files[k] = v
			}
			var list []string
			for f := range files {
				list = append(list, f)
			}
			slices.Sort(list)
			read := func(f string) []byte {
				if src, ok := files[f]; ok {
					return []byte(src)
				}
				return []byte(others[f])
			}
			units := gradleUnits(list, read, func() []string { return slices.Sorted(maps.Keys(others)) })
			if got := slices.Contains(units, dir+tc.file); got != tc.want {
				t.Errorf("unit = %v, want %v (units %v)", got, tc.want, units)
			}
		})
	}
	// None of the support files is a unit.
	var list []string
	for f := range base {
		list = append(list, f)
	}
	slices.Sort(list)
	if units := gradleUnits(list, func(f string) []byte { return []byte(base[f]) }, nil); !reflect.DeepEqual(units, []string{dir + "support/CycleA.java", dir + "support/CycleB.java"}) {
		t.Errorf("support units = %v, want only the cycle", units)
	}
}

// A file smates cannot read is a unit: it cannot tell what it declares.
func TestGradleUnitsUnreadable(t *testing.T) {
	units := gradleUnits([]string{"src/test/java/A.java"}, func(string) []byte { return nil }, nil)
	if !reflect.DeepEqual(units, []string{"src/test/java/A.java"}) {
		t.Errorf("units = %v", units)
	}
}

func TestGradleDefaults(t *testing.T) {
	wrapper := "gradlew"
	if runtime.GOOS == "windows" {
		wrapper = "gradlew.bat"
	}
	for _, tc := range []struct {
		name  string
		files []string
		root  string
		want  string
	}{
		{"root project with wrapper", []string{"settings.gradle", wrapper}, ".", "./" + wrapper + " :test"},
		{"subproject, wrapper at the build root", []string{"settings.gradle.kts", wrapper, "app/build.gradle.kts"}, "app", "../" + wrapper + " :app:test"},
		{"nested subproject", []string{"settings.gradle", wrapper, "libs/core/build.gradle"}, "libs/core", "../../" + wrapper + " :libs:core:test"},
		{"no wrapper", []string{"settings.gradle", "app/build.gradle"}, "app", "gradle :app:test"},
		{"build in a subdirectory", []string{"backend/settings.gradle", "backend/" + wrapper, "backend/app/build.gradle"}, "backend/app", "../" + wrapper + " :app:test"},
		{"build root in a subdirectory", []string{"backend/settings.gradle", "backend/" + wrapper}, "backend", "./" + wrapper + " :test"},
		{"no settings", []string{"build.gradle"}, ".", "gradle :test"},
		{"nearest wrapper wins", []string{"settings.gradle", wrapper, "app/" + wrapper}, "app", "./" + wrapper + " :app:test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				testutil.Write(t, dir, f, "")
			}
			d := gradle{}.Defaults(&gitx.Repo{Root: dir}, tc.root)
			if d.Command != tc.want {
				t.Errorf("command = %q, want %q", d.Command, tc.want)
			}
		})
	}
}

func TestGradleInputs(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteTree(t, dir, map[string]string{
		"backend/settings.gradle": `rootProject.name = 'x'
include 'app', 'lib'
includeBuild 'build-logic'
includeBuild("tools/plugins")
pluginManagement { includeBuild(file('conventions')) }
// includeBuild 'commented-out'
/* includeBuild 'also-commented' */
`,
	})
	s := &Suite{Suite: config.Suite{Name: "app", Runner: "gradle", Root: "backend/app"}}
	s.Inputs = gradle{}.Defaults(&gitx.Repo{Root: dir}, "backend/app").Inputs
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"backend/settings.gradle", true},
		{"backend/build.gradle", true},
		{"backend/app/build.gradle.kts", true},
		{"backend/lib/build.gradle", true}, // a project the suite may depend on
		{"backend/gradle.properties", true},
		{"backend/app/gradle.properties", true},
		{"backend/gradle/libs.versions.toml", true},
		{"backend/gradle/wrapper/gradle-wrapper.properties", true},
		{"backend/gradlew", true},
		{"backend/app/gradle.lockfile", true},
		{"backend/settings-gradle.lockfile", true},
		{"backend/buildSrc/src/main/groovy/Conventions.groovy", true},
		{"backend/build-logic/src/main/kotlin/Plugin.kt", true},
		{"backend/tools/plugins/build.gradle", true},
		{"backend/conventions/src/main/java/C.java", true},
		{".tool-versions", true},
		{"backend/commented-out/x.java", false},
		{"backend/also-commented/x.java", false},
		{"backend/app/src/main/java/A.java", false},
		{"frontend/build.gradle", false},
	} {
		if got := s.InputMatch(tc.path); got != tc.want {
			t.Errorf("InputMatch(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// An included build smates cannot locate makes the whole build an input.
func TestGradleIncludedBuilds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		want     []string
	}{
		{"literal", `includeBuild 'a'`, []string{"a/**"}},
		{"parenthesized, double quotes", `includeBuild("b/c")`, []string{"b/c/**"}},
		{"file()", `includeBuild(file("d"))`, []string{"d/**"}},
		{"none", `include 'app'`, nil},
		{"outside the repository", `includeBuild '../other'`, nil},
		{"variable", `def dir = 'x'
includeBuild dir`, []string{"**"}},
		{"interpolated", `includeBuild "${rootDir}/x"`, []string{"**"}},
		{"the build itself", `includeBuild '.'`, []string{"**"}},
		{"mixed", `includeBuild 'a'
includeBuild(someDir)`, []string{"**"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			testutil.Write(t, dir, "settings.gradle", tc.settings)
			if got := includedBuilds(dir, "."); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("includedBuilds = %v, want %v", got, tc.want)
			}
		})
	}
	// A build in a subdirectory: a sibling build of the repository is kept, one that
	// leaves the repository is not.
	dir := t.TempDir()
	abs := filepath.Join(dir, "libs", "common")
	testutil.Write(t, dir, "jvm/settings.gradle", "includeBuild '../shared'\nincludeBuild('../../elsewhere')\nincludeBuild '..'\nincludeBuild '"+filepath.ToSlash(abs)+"'\nincludeBuild '"+filepath.ToSlash(filepath.Dir(dir))+"/outside'\n")
	if got, want := includedBuilds(dir, "jvm"), []string{"../**", "../libs/common/**", "../shared/**"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sibling builds: includedBuilds = %v, want %v", got, want)
	}
}

// A sibling included build is an input of the suites of the build that includes it.
func TestGradleInputsSiblingBuild(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, dir, "jvm/settings.gradle", "includeBuild('../shared')\n")
	s := &Suite{Suite: config.Suite{Name: "app", Runner: "gradle", Root: "jvm/app"}}
	s.Inputs = gradle{}.Defaults(&gitx.Repo{Root: dir}, "jvm/app").Inputs
	for path, want := range map[string]bool{"shared/src/main/java/com/example/S.java": true, "other/x.java": false, "jvm/app/src/main/java/A.java": false} {
		if got := s.InputMatch(path); got != want {
			t.Errorf("InputMatch(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestGradleObservations(t *testing.T) {
	yes, no := true, false
	classes := map[string]string{
		"com.example.ATest": "a", "com.example.BTest": "b", "com.example.CTest": "c",
		"com.example.DTest": "d", "com.example.ETest": "e", "com.example.Second": "e",
	}
	units := []string{"a", "b", "c", "d", "e", "f"}
	ev := func(k, class, name, result string, assumption *bool) gradleEvent {
		return gradleEvent{K: k, Class: class, Name: name, Result: result, Ms: 5, Assumption: assumption, Msg: "boom"}
	}
	events := []gradleEvent{
		// a: passed, a disabled test, a nested class
		ev("test", "com.example.ATest", "t1", "SUCCESS", nil),
		ev("test", "com.example.ATest", "off", "SKIPPED", &no),
		ev("test", "com.example.ATest$Inner", "inner", "SUCCESS", nil),
		ev("class", "com.example.ATest$Inner", "com.example.ATest$Inner", "SUCCESS", nil),
		ev("class", "com.example.ATest", "com.example.ATest", "SUCCESS", nil),
		// b: an assumption aborted a test
		ev("test", "com.example.BTest", "t1", "SUCCESS", nil),
		ev("test", "com.example.BTest", "needsDb", "SKIPPED", &yes),
		// c: a failure in a nested class
		ev("test", "com.example.CTest", "t1", "SUCCESS", nil),
		ev("test", "com.example.CTest$Inner", "bad", "FAILURE", nil),
		// d: a skip Gradle cannot tell from @Disabled
		ev("test", "com.example.DTest", "maybe", "SKIPPED", nil),
		// e: a class-level failure of a secondary class of the file
		ev("test", "com.example.ETest", "t1", "SUCCESS", nil),
		ev("class", "com.example.Second", "com.example.Second", "FAILURE", nil),
		// a class no unit declares
		ev("test", "com.example.Other", "t", "FAILURE", nil),
	}
	obs, warns := gradleObservations(units, classes, events, 1, true)
	want := map[string]string{"a": state.Pass, "b": state.Unknown, "c": state.Fail, "d": state.Unknown, "e": state.Fail}
	for u, st := range want {
		if obs[u] == nil || obs[u].Status != st {
			t.Errorf("%s: %+v, want %s", u, obs[u], st)
		}
	}
	if obs["f"] != nil {
		t.Errorf("f: %+v, want no observation in a failed build (settled by the caller)", obs["f"])
	}
	if !obs["a"].NoTrace || obs["c"].NoTrace || obs["b"].NoTrace {
		t.Errorf("NoTrace: a=%v b=%v c=%v, want only the pass", obs["a"].NoTrace, obs["b"].NoTrace, obs["c"].NoTrace)
	}
	if obs["a"].DurationMs != 5 {
		t.Errorf("a duration = %d, want the top-level class's", obs["a"].DurationMs)
	}
	if !strings.Contains(obs["c"].Failure, "bad: boom") || !strings.Contains(obs["e"].Failure, "com.example.Second: boom") {
		t.Errorf("failures: %q / %q", obs["c"].Failure, obs["e"].Failure)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "not proven") {
		t.Errorf("warnings = %q", warns)
	}

	// Without recording, a pass is just a pass.
	obs, _ = gradleObservations(units, classes, events, 1, false)
	if obs["a"].NoTrace {
		t.Error("NoTrace without recording")
	}

	// A successful build: a unit without events is unknown, with a warning.
	obs, warns = gradleObservations(units, classes, events[:5], 0, true)
	if obs["f"] == nil || obs["f"].Status != state.Unknown || !slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "ran no test") }) {
		t.Errorf("f: %+v, warnings %q", obs["f"], warns)
	}

	// A build that failed before any test ran (a compile error): every unit fails.
	obs, _ = gradleObservations(units, classes, nil, 1, true)
	for _, u := range units {
		if obs[u] == nil || obs[u].Status != state.Fail || !obs[u].Inferred {
			t.Errorf("%s: %+v, want an inferred failure", u, obs[u])
		}
	}
}

func TestGradleEventsParse(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, dir, "results-_app_test.jsonl", `{"k":"test","task":":app:test","class":"a.B","name":"t()","result":"SKIPPED","ms":3,"assumption":true}
not json
{"k":"test","class":"","name":"x"}
{"k":"class","task":":app:test","class":"a.B","name":"a.B","result":"FAILURE","ms":7,"msg":"x"}
`)
	evs, err := readGradleEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Assumption == nil || !*evs[0].Assumption || evs[1].Result != "FAILURE" {
		t.Errorf("events = %+v", evs)
	}
}

func TestGradleCommandsHaveInitScript(t *testing.T) {
	rc := &RunContext{Suite: &Suite{Suite: config.Suite{Name: "app", Command: "./gradlew :app:test"}}, ShimDir: "/shims", ExtraArgs: []string{"--info"}}
	cmds, err := gradle{}.Commands(rc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"./gradlew", ":app:test", "--init-script", filepath.Join("/shims", "gradle", "smates.init.gradle"), "--info"}
	if !reflect.DeepEqual(cmds, [][]string{want}) {
		t.Errorf("commands = %q, want %q", cmds, want)
	}
}

func TestClassFromPath(t *testing.T) {
	s := &Suite{Suite: config.Suite{Root: "app"}}
	if got := classFromPath(s, "app/src/test/java/com/example/FooTest.java"); got != "com.example.FooTest" {
		t.Errorf("classFromPath = %q", got)
	}
}
