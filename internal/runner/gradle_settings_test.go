package runner

import (
	"reflect"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

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
		got, ok := gradleIncludes(tc.src, includeWord)
		if !reflect.DeepEqual(got, tc.want) || ok != tc.ok {
			t.Errorf("%q: got %v %v, want %v %v", tc.src, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReadGradleSettings(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		want           []GradleProject
		ok             bool
	}{
		{"includes", "include 'app', ':libs:core'", []GradleProject{{":", "."}, {":app", "app"}, {":libs", "libs"}, {":libs:core", "libs/core"}}, true},
		{"moved inside the build", "include ':core'\nproject(':core').projectDir = file('modules/core-impl')", []GradleProject{{":", "."}, {":core", "modules/core-impl"}}, true},
		{"child of a moved project", "include ':core', ':core:api'\nproject(':core').projectDir = new File(settingsDir, 'modules/core')", []GradleProject{{":", "."}, {":core", "modules/core"}, {":core:api", "modules/core/api"}}, true},
		{"moved outside the build", "include ':shared'\nproject(':shared').projectDir = file('../shared')", []GradleProject{{":", "."}, {":shared", "../shared"}}, true},
		{"includeFlat", "includeFlat 'sibling'", []GradleProject{{":", "."}, {":sibling", "../sibling"}}, true},
		{"projectDir set otherwise", "include 'a'\nrootProject.children.each { it.projectDir = file(\"x/${it.name}\") }", []GradleProject{{":", "."}, {":a", "a"}}, false},
		{"computed includeFlat", "includeFlat(*names)", []GradleProject{{":", "."}}, false},
		{"no settings", "", []GradleProject{{":", "."}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.settings != "" {
				testutil.Write(t, dir, "b/settings.gradle", tc.settings)
			}
			st := ReadGradleSettings(dir, "b")
			if !reflect.DeepEqual(st.Projects, tc.want) || st.OK != tc.ok {
				t.Errorf("got %+v ok=%v, want %+v ok=%v", st.Projects, st.OK, tc.want, tc.ok)
			}
		})
	}
}
