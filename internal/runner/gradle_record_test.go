package runner

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
	"github.com/dotneet/smates/internal/trace"
)

func TestParseCollected(t *testing.T) {
	out := strings.Join([]string{
		"class\torphan.Before", // before any jvm line: ignored
		"jvm\tjvm-41.jfr",
		"command\tworker.org.gradle.process.internal.worker.GradleWorkerMain 'Gradle Test Executor 7'",
		"class\tcom.example.A",
		"read\t/r/a\\tb.txt",
		"proc\t99\t/r/app\tsh -c printf 'x\\ny' \\\\ z",
		"jvm\tjvm-42.jfr",
		"error\tjava.io.IOException: truncated",
		"jvm\tother.jfr",
	}, "\n")
	recs := parseCollected([]byte(out))
	if len(recs) != 3 {
		t.Fatalf("recordings = %d", len(recs))
	}
	a := recs[0]
	if a.Pid != 41 || a.Executor != "Gradle Test Executor 7" || !reflect.DeepEqual(a.Classes, []string{"com.example.A"}) {
		t.Errorf("first: %+v", a)
	}
	if !reflect.DeepEqual(a.Reads, []string{"/r/a\tb.txt"}) {
		t.Errorf("reads = %q", a.Reads)
	}
	if want := []jvmProc{{Pid: 99, Dir: "/r/app", Command: "sh -c printf 'x\ny' \\ z"}}; !reflect.DeepEqual(a.Procs, want) {
		t.Errorf("procs = %+v", a.Procs)
	}
	if recs[1].Pid != 42 || recs[1].Err == "" || recs[2].Pid != 0 {
		t.Errorf("rest: %+v %+v", recs[1], recs[2])
	}
}

// gradleRecordRepo is a repository with two projects, as facts describe them.
func gradleRecordRepo(t *testing.T) (string, []gradleFacts, *gradleMapper) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	files := map[string]string{
		"app/src/main/java/com/example/Store.java":       "package com.example;\npublic class Store {}\nclass Helper {}",
		"app/src/test/java/com/example/StoreTest.java":   "package com.example;\nclass StoreTest { @Test void t() {} }",
		"app/src/test/java/com/example/OtherTest.java":   "package com.example;\nclass OtherTest { @Test void t() {} }",
		"lib/src/main/java/com/example/lib/Money.java":   "package com.example.lib;\npublic class Money {}",
		"other/src/main/java/com/example/lib/Money.java": "package com.example.lib;\npublic class Money {}",
		"app/src/main/resources/db/schema.sql":           "create",
		"app/src/main/resources-extra/extra.txt":         "x",
		"app/data/in.txt":                                "in",
		"lib/src/main/resources/lib.properties":          "a=b",
	}
	testutil.WriteTree(t, root, files)
	var java []string
	for f := range files {
		if strings.HasSuffix(f, ".java") {
			java = append(java, f)
		}
	}
	sort.Strings(java)
	j := func(p string) string { return filepath.Join(root, filepath.FromSlash(p)) }
	facts := []gradleFacts{{
		Task: ":app:test", WorkingDir: j("app"), Java: "java", JavaVersion: 21, JFR: true,
		Projects: []gradleProject{
			{Path: ":app", Dir: j("app"), Jar: j("app/build/libs/app.jar"), SourceSets: []gradleSourceSet{
				{Name: "main", Resources: []string{j("app/src/main/resources"), j("app/src/main/resources-extra")}, ResourcesOut: j("app/build/resources/main"), Classes: []string{j("app/build/classes/java/main")}},
				{Name: "test", Resources: []string{j("app/src/test/resources")}, ResourcesOut: j("app/build/resources/test"), Classes: []string{j("app/build/classes/java/test")}},
			}},
			{Path: ":lib", Dir: j("lib"), Jar: j("lib/build/libs/lib.jar"), SourceSets: []gradleSourceSet{
				{Name: "main", Resources: []string{j("lib/src/main/resources")}, ResourcesOut: j("lib/build/resources/main"), Classes: []string{j("lib/build/classes/java/main")}},
			}},
		},
	}}
	read := func(f string) []byte { return []byte(files[f]) }
	return root, facts, newGradleMapper(root, java, read, facts)
}

func TestGradleMapperClass(t *testing.T) {
	_, _, m := gradleRecordRepo(t)
	for _, tc := range []struct {
		class string
		want  []string
	}{
		{"com.example.Store", []string{"app/src/main/java/com/example/Store.java"}},
		{"com.example.Store$Inner", []string{"app/src/main/java/com/example/Store.java"}},
		{"com.example.Store$$Lambda+0x0000000801001234", []string{"app/src/main/java/com/example/Store.java"}},
		{"com.example.Store$$Lambda/0x0000000801001234", []string{"app/src/main/java/com/example/Store.java"}},
		{"com.example.Helper", []string{"app/src/main/java/com/example/Store.java"}}, // a secondary top-level class
		// Two projects declare the class: both are dependencies.
		{"com.example.lib.Money", []string{"lib/src/main/java/com/example/lib/Money.java", "other/src/main/java/com/example/lib/Money.java"}},
		{"java.lang.String", nil},
		{"org.junit.jupiter.api.Test", nil},
	} {
		got := m.class(tc.class)
		sort.Strings(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("class(%q) = %v, want %v", tc.class, got, tc.want)
		}
	}
}

func TestGradleMapperRead(t *testing.T) {
	root, _, m := gradleRecordRepo(t)
	wd := filepath.Join(root, "app")
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"relative to the working directory", "data/in.txt", []string{"app/data/in.txt", "app/data/"}},
		{"absolute", filepath.Join(root, "app/data/in.txt"), []string{"app/data/in.txt", "app/data/"}},
		{"a resource, mapped to its source", filepath.Join(root, "app/build/resources/main/db/schema.sql"), []string{"app/src/main/resources/db/schema.sql", "app/src/main/resources/db/"}},
		{"a resource of a second resource directory", filepath.Join(root, "app/build/resources/main/extra.txt"), []string{"app/src/main/resources-extra/extra.txt", "app/src/main/resources-extra/"}},
		// Generated: no source directory has it, every resource of the set counts.
		{"a generated resource", filepath.Join(root, "app/build/resources/main/version.txt"), []string{"app/src/main/resources/**", "app/src/main/resources-extra/**"}},
		{"a project's jar: its main resources", filepath.Join(root, "lib/build/libs/lib.jar"), []string{"lib/src/main/resources/**"}},
		{"class output", filepath.Join(root, "app/build/classes/java/main/com/example/Store.class"), nil},
		{"outside the repository", "/usr/lib/jvm/lib/modules", nil},
		{"the repository root itself", root, nil},
		{"empty", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.read(tc.path, wd); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("read(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestGradleRecorded(t *testing.T) {
	root, facts, m := gradleRecordRepo(t)
	const store, other, third = "app/src/test/java/com/example/StoreTest.java", "app/src/test/java/com/example/OtherTest.java", "app/src/test/java/com/example/ThirdTest.java"
	classes := map[string]string{"com.example.StoreTest": store, "com.example.OtherTest": other, "com.example.ThirdTest": third}
	ev := func(class, executor string) gradleEvent {
		return gradleEvent{K: "test", Task: ":app:test", Class: class, Result: "SUCCESS", Executor: executor}
	}
	newObs := func() map[string]*Observation {
		return map[string]*Observation{
			store: {Status: state.Pass, NoTrace: true}, other: {Status: state.Pass, NoTrace: true}, third: {Status: state.Fail},
		}
	}
	rec := func(pid int, executor string, classes ...string) *jvmRecording {
		return &jvmRecording{Pid: pid, Executor: "Gradle Test Executor " + executor, Classes: classes}
	}
	t.Run("one JVM per class", func(t *testing.T) {
		obs := newObs()
		recs := []*jvmRecording{
			rec(1, "1", "com.example.StoreTest", "com.example.Store"),
			rec(2, "2", "com.example.OtherTest", "com.example.lib.Money"),
			rec(3, "3", "com.example.Store"), // ran no class of a unit (an abstract class): dropped
		}
		recs[0].Reads = []string{"data/in.txt"}
		events := []gradleEvent{ev("com.example.StoreTest", "Gradle Test Executor 1"), ev("com.example.OtherTest$Nested", "Gradle Test Executor 2")}
		gradleRecorded(obs, events, classes, recs, facts, m, nil)
		if got := keys(obs[store].Files); !reflect.DeepEqual(got, []string{"app/data/", "app/data/in.txt", "app/src/main/java/com/example/Store.java", "app/src/test/java/com/example/StoreTest.java"}) || obs[store].NoTrace {
			t.Errorf("store: %v notrace=%v", got, obs[store].NoTrace)
		}
		if got := keys(obs[other].Files); !reflect.DeepEqual(got, []string{"app/src/test/java/com/example/OtherTest.java", "lib/src/main/java/com/example/lib/Money.java", "other/src/main/java/com/example/lib/Money.java"}) || obs[other].NoTrace {
			t.Errorf("other: %v", got)
		}
	})
	t.Run("isolate false: one JVM's dependencies for all its classes", func(t *testing.T) {
		obs := newObs()
		recs := []*jvmRecording{rec(1, "1", "com.example.Store")}
		events := []gradleEvent{ev("com.example.StoreTest", "Gradle Test Executor 1"), ev("com.example.OtherTest", "Gradle Test Executor 1")}
		gradleRecorded(obs, events, classes, recs, facts, m, nil)
		for _, u := range []string{store, other} {
			if !obs[u].Files["app/src/main/java/com/example/Store.java"] || obs[u].NoTrace {
				t.Errorf("%s: %v", u, keys(obs[u].Files))
			}
		}
	})
	// The cases smates cannot tell keep the pass untraced: recorded as unknown.
	for _, tc := range []struct {
		name   string
		recs   []*jvmRecording
		events []gradleEvent
	}{
		{"no recording for the executor", []*jvmRecording{rec(1, "9", "com.example.Store")}, []gradleEvent{ev("com.example.StoreTest", "Gradle Test Executor 1")}},
		{"unreadable recording", []*jvmRecording{{Pid: 1, Executor: "Gradle Test Executor 1", Err: "truncated"}}, []gradleEvent{ev("com.example.StoreTest", "Gradle Test Executor 1")}},
		{"no executor in the events", []*jvmRecording{rec(1, "1")}, []gradleEvent{ev("com.example.StoreTest", "")}},
		{"one of two JVMs unreadable", []*jvmRecording{rec(1, "1", "com.example.Store"), {Pid: 2, Executor: "Gradle Test Executor 2", Err: "x"}},
			[]gradleEvent{ev("com.example.StoreTest", "Gradle Test Executor 1"), ev("com.example.StoreTest$Inner", "Gradle Test Executor 2")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := newObs()
			gradleRecorded(obs, tc.events, classes, tc.recs, facts, m, nil)
			if !obs[store].NoTrace {
				t.Errorf("store traced: %v", keys(obs[store].Files))
			}
		})
	}
	t.Run("child processes", func(t *testing.T) {
		obs := newObs()
		recs := []*jvmRecording{rec(1, "1"), rec(2, "2")}
		recs[0].Procs = []jvmProc{{Pid: 50, Command: "cat data/in.txt"}} // no directory: the JVM's
		recs[1].Procs = []jvmProc{{Pid: 60, Dir: filepath.Join(root, "app"), Command: "node tool.js"}}
		events := []gradleEvent{ev("com.example.StoreTest", "Gradle Test Executor 1"), ev("com.example.OtherTest", "Gradle Test Executor 2")}
		mk := func(pid, ppid int, anc string, files ...string) *trace.Proc {
			p := &trace.Proc{Pid: pid, Ppid: ppid, Anc: anc, Code: map[string]bool{}, Reads: map[string]bool{}}
			for _, f := range files {
				p.Reads[f] = true
			}
			return p
		}
		procs := []*trace.Proc{
			mk(60, 2, "", "app/tool.js"),              // started by JVM 2 directly
			mk(61, 777, "60-0", "app/tool-data.json"), // through a traced ancestor
			mk(70, 5, "", "app/global.json"),          // linked to no JVM: every unit
		}
		gradleRecorded(obs, events, classes, recs, facts, m, procs)
		if f := obs[store].Files; !f["app/data/in.txt"] || f["app/tool.js"] || !f["app/global.json"] {
			t.Errorf("store: %v", keys(f))
		}
		if f := obs[other].Files; !f["app/tool.js"] || !f["app/tool-data.json"] || !f["app/global.json"] || f["app/data/in.txt"] {
			t.Errorf("other: %v", keys(f))
		}
	})
}

// A test JVM before Java 17 is not recorded: its passes stay untraced, with the reason.
func TestGradleRecordOldJava(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, dir, "facts-_app_test.json", `{"task":":app:test","java":"/jdk11/bin/java","javaVersion":11,"jfr":false,"projects":[]}`)
	r := testutil.NewGitRepo(t, nil)
	rc := &RunContext{Repo: &gitx.Repo{Root: r.Dir}, Suite: &Suite{Suite: config.Suite{Name: "app"}}, TraceDir: t.TempDir()}
	obs := map[string]*Observation{"a": {Status: state.Pass, NoTrace: true}}
	why := gradle{}.record(context.Background(), rc, dir, obs, nil, nil)
	if !strings.Contains(why, "Java 11") || !obs["a"].NoTrace {
		t.Errorf("why = %q, obs %+v", why, obs["a"])
	}
	if why := (gradle{}).record(context.Background(), rc, t.TempDir(), obs, nil, nil); !strings.Contains(why, "no Test task") {
		t.Errorf("no facts: %q", why)
	}
	// Facts the init script could not gather (another kind of source set): no mapping.
	bad := t.TempDir()
	testutil.Write(t, bad, "facts-_app_test.json", `{"task":":app:test","java":"java","javaVersion":21,"jfr":true,"error":"groovy.lang.MissingPropertyException: resources"}`)
	if why := (gradle{}).record(context.Background(), rc, bad, obs, nil, nil); !strings.Contains(why, "could not describe") || !obs["a"].NoTrace {
		t.Errorf("facts error: %q", why)
	}
	if n := untracedPasses(obs); n != 1 {
		t.Errorf("untracedPasses = %d", n)
	}
}
