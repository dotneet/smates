package detect

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/testutil"
)

const javaTest = "package p;\nclass ATest { @Test void a() {} }\n"

func TestDetectGradle(t *testing.T) {
	type suite struct{ Name, Root, Command string }
	cases := []struct {
		name  string
		files map[string]string
		want  []suite
		// notes are substrings expected in the notes, in order; nil expects none.
		notes []string
	}{
		{
			name: "groovy settings",
			files: map[string]string{
				"settings.gradle":  "rootProject.name = 'shop'\ninclude 'lib', 'app'\n",
				"lib/build.gradle": "", "lib/src/test/java/p/LibTest.java": javaTest,
				"app/build.gradle": "", "app/src/test/java/p/AppTest.java": javaTest,
				"docs/build.gradle":                   "", // not included: no project of the build
				"docs/src/test/java/p/DocTest.java":   javaTest,
				"app/src/main/java/p/App.java":        "package p; class App {}",
				"internal/testdata/g/settings.gradle": "include 'x'", "internal/testdata/g/x/src/test/java/XTest.java": javaTest,
			},
			want: []suite{{"gradle:app", "app", ""}, {"gradle:lib", "lib", ""}},
		},
		{
			name: "kotlin settings over several lines, nested project, root project tests",
			files: map[string]string{
				"settings.gradle.kts": "include(\n    \":services:api\",\n    \":tools\",\n)\n",
				"build.gradle.kts":    "", "src/test/java/p/RootTest.java": javaTest,
				"services/api/src/test/java/p/ApiTest.java": javaTest,
				"tools/src/test/java/p/ToolTest.java":       javaTest,
			},
			want: []suite{{"gradle:root", ".", ""}, {"gradle:services/api", "services/api", ""}, {"gradle:tools", "tools", ""}},
		},
		{
			name: "build in a subdirectory, project moved by the settings",
			files: map[string]string{
				"backend/settings.gradle": "include ':core'\nproject(':core').projectDir = file('modules/core-impl')\n",
				"backend/gradlew":         "#!/bin/sh\n",
				"backend/modules/core-impl/src/test/java/p/CoreTest.java": javaTest,
			},
			want: []suite{{"gradle", "backend/modules/core-impl", "../../gradlew :core:test"}},
		},
		{
			name: "computed include: every directory with a build script, with a note",
			files: map[string]string{
				"settings.gradle": "file('.').eachDir { if (new File(it, 'build.gradle').exists()) include it.name }\n",
				"a/build.gradle":  "", "a/src/test/java/p/ATest.java": javaTest,
				"b/c/build.gradle.kts": "", "b/c/src/test/java/p/CTest.java": javaTest,
				"buildSrc/build.gradle": "", "buildSrc/src/test/java/p/BTest.java": javaTest,
				"tests-only/src/test/java/p/TTest.java": javaTest, // no build script
			},
			want:  []suite{{"gradle:a", "a", ""}, {"gradle:b/c", "b/c", ""}},
			notes: []string{".: the settings of this Gradle build include projects smates cannot read"},
		},
		{
			name: "projectDir set in a way smates cannot read",
			files: map[string]string{
				"settings.gradle": "include 'a'\nrootProject.children.each { it.projectDir = new File(rootDir, \"modules/${it.name}\") }\n",
				"a/build.gradle":  "", "a/src/test/java/p/ATest.java": javaTest,
			},
			want:  []suite{{"gradle", "a", ""}},
			notes: []string{"cannot read"},
		},
		{
			name: "single project without settings",
			files: map[string]string{
				"build.gradle": "plugins { id 'java' }", "src/test/java/p/ATest.java": javaTest,
			},
			want: []suite{{"gradle", ".", ""}},
		},
		{
			name: "Kotlin tests only, Java and Groovy tests, Maven",
			files: map[string]string{
				"settings.gradle":                        "include 'kt', 'mixed'",
				"kt/src/test/kotlin/p/KTest.kt":          "",
				"mixed/src/test/java/p/MTest.java":       javaTest,
				"mixed/src/test/groovy/p/GSpec.groovy":   "",
				"legacy/pom.xml":                         "<project/>",
				"legacy/core/pom.xml":                    "<project/>",
				"legacy/core/src/test/java/p/LTest.java": javaTest,
				"mixed/build.gradle":                     "", "mixed/pom.xml": "<project/>", // built by both: left to Gradle
				"site/pom.xml": "<project/>", // no tests
			},
			want: []suite{{"gradle", "mixed", ""}},
			notes: []string{
				`kt: Gradle project :kt has Kotlin tests only (src/test/kotlin): smates reads Java tests only, so it gets no suite; changes to it are orphans, which run every suite in full: if no other suite's tests use it, add "kt/**" to ignore:`,
				"mixed: the Groovy tests of Gradle project :mixed (src/test/groovy) are no test units",
				`legacy: a Maven build (pom.xml): smates has no Maven runner, so its tests get no suite; changes to it are orphans, which run every suite in full: if no other suite's tests use it, add "legacy/**" to ignore:`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var list []string
			for f, c := range tc.files {
				testutil.Write(t, dir, f, c)
				list = append(list, f)
			}
			sort.Strings(list)
			suites, notes := Detect(&gitx.Repo{Root: dir}, list)
			var got []suite
			for _, s := range suites {
				got = append(got, suite{s.Name, s.Root, s.Command})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("suites:\n got %+v\nwant %+v", got, tc.want)
			}
			if len(notes) != len(tc.notes) {
				t.Fatalf("notes: got %q, want %d", notes, len(tc.notes))
			}
			for i, n := range tc.notes {
				if !strings.Contains(notes[i], n) {
					t.Errorf("note %d: %q lacks %q", i, notes[i], n)
				}
			}
		})
	}
}

func TestGradleIncludes(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want []string
		ok   bool
	}{
		{"include 'a'", []string{":a"}, true},
		{"include ':a:b', 'c'", []string{":a", ":a:b", ":c"}, true},
		{"include(\"a\", \"b\")", []string{":a", ":b"}, true},
		{"include 'a',\n        'b'\ninclude 'c'", []string{":a", ":b", ":c"}, true},
		{"include(\n  \":a\",\n  \":b\",\n)", []string{":a", ":b"}, true},
		{"if (x) { include 'a' }", []string{":a"}, true},
		{"include 'a'; include 'b'", []string{":a", ":b"}, true},
		{"includeBuild 'x'\nincludeFlat 'y'", []string{}, true},
		{"include(*modules)", []string{}, false},
		{"include \"svc-$name\"", []string{}, false},
		{"include 'a' + suffix", []string{}, false},
		{"include modules.toTypedArray()", []string{}, false},
		{"include 'a', 'b'\ninclude listOf('c')", []string{":a", ":b"}, false},
		{"include('a',)", []string{":a"}, true},
		{"include()", []string{}, false},
	} {
		got, ok := gradleIncludes(tc.src)
		if !reflect.DeepEqual(got, tc.want) || ok != tc.ok {
			t.Errorf("%q: got %v %v, want %v %v", tc.src, got, ok, tc.want, tc.ok)
		}
	}
}
