package detect

import (
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/testutil"
)

const javaTest = "package p;\nclass ATest { @Test void a() {} }\n"

func TestDetectGradle(t *testing.T) {
	wrapper := "gradlew"
	if runtime.GOOS == "windows" {
		wrapper = "gradlew.bat"
	}
	// Command is the suite's command as the runner resolves it; smates.yml gets none
	// (a written command would name the wrapper of the platform init ran on).
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
			want: []suite{{"gradle:app", "app", "gradle :app:test"}, {"gradle:lib", "lib", "gradle :lib:test"}},
		},
		{
			name: "kotlin settings over several lines, nested project, root project tests",
			files: map[string]string{
				"settings.gradle.kts": "include(\n    \":services:api\",\n    \":tools\",\n)\n",
				"build.gradle.kts":    "", "src/test/java/p/RootTest.java": javaTest,
				"services/api/src/test/java/p/ApiTest.java": javaTest,
				"tools/src/test/java/p/ToolTest.java":       javaTest,
			},
			want: []suite{{"gradle:root", ".", "gradle :test"}, {"gradle:services/api", "services/api", "gradle :services:api:test"}, {"gradle:tools", "tools", "gradle :tools:test"}},
		},
		{
			name: "build in a subdirectory, project moved by the settings",
			files: map[string]string{
				"backend/settings.gradle": "include ':core'\nproject(':core').projectDir = file('modules/core-impl')\n",
				"backend/gradlew":         "#!/bin/sh\n",
				"backend/gradlew.bat":     "@rem\n",
				"backend/modules/core-impl/src/test/java/p/CoreTest.java": javaTest,
			},
			want: []suite{{"gradle", "backend/modules/core-impl", "../../" + wrapper + " :core:test"}},
		},
		{
			name: "projects outside the build: projectDir and includeFlat",
			files: map[string]string{
				"jvm/settings.gradle":                    "include ':shared'\nproject(':shared').projectDir = file('../shared')\nincludeFlat 'tools'\ninclude 'app'\n",
				"jvm/app/src/test/java/p/AppTest.java":   javaTest,
				"shared/src/test/java/p/SharedTest.java": javaTest,
				"tools/src/test/java/p/ToolTest.java":    javaTest,
			},
			want: []suite{{"gradle", "jvm/app", "gradle :app:test"}},
			notes: []string{
				"jvm: Gradle project :shared is in shared, outside the build (projectDir or includeFlat): smates runs a suite from inside its build, so the project gets no suite",
				"jvm: Gradle project :tools is in tools, outside the build",
			},
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
			want:  []suite{{"gradle:a", "a", "gradle :a:test"}, {"gradle:b/c", "b/c", "gradle :b:c:test"}},
			notes: []string{".: the settings of this Gradle build include projects smates cannot read"},
		},
		{
			name: "projectDir set in a way smates cannot read",
			files: map[string]string{
				"settings.gradle": "include 'a'\nrootProject.children.each { it.projectDir = new File(rootDir, \"modules/${it.name}\") }\n",
				"a/build.gradle":  "", "a/src/test/java/p/ATest.java": javaTest,
			},
			want:  []suite{{"gradle", "a", "gradle :a:test"}},
			notes: []string{"cannot read"},
		},
		{
			name: "single project without settings",
			files: map[string]string{
				"build.gradle": "plugins { id 'java' }", "src/test/java/p/ATest.java": javaTest,
			},
			want: []suite{{"gradle", ".", "gradle :test"}},
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
			want: []suite{{"gradle", "mixed", "gradle :mixed:test"}},
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
			repo := &gitx.Repo{Root: dir}
			suites, notes := Detect(repo, list)
			var got []suite
			for _, s := range suites {
				if s.Command != "" {
					t.Errorf("suite %s: command %q written", s.Name, s.Command)
				}
				rs, err := runner.NewSuite(repo, s)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, suite{s.Name, s.Root, rs.Command})
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
