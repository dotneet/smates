package trace

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

// streamAs writes a trace stream under a given file name (the shims add a nonce).
func streamAs(t *testing.T, dir, name string, events ...ev) {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	testutil.Write(t, dir, name, b.String())
}

// Two processes that had the same pid write streams of their own (a nonce in the name):
// they stay two processes, references to the pid name the one running when the
// referring process started, and coverage goes to the one running when it was written.
// Streams without a nonce (older shims) are still read.
func TestCollectReusedPid(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "a.txt", "b.txt", "early.txt", "late.txt", "src/a.js", "src/b.js", "old.txt", "exec.txt")
	abs := func(r string) string { return filepath.Join(root, r) }
	td := t.TempDir()
	streamAs(t, td, "proc-50-0-aaa.jsonl",
		ev{"k": "start", "pid": 50, "ppid": 1, "tid": 0, "argv": []string{"node", "a.js"}, "t": 100, "lang": "node"},
		ev{"k": "read", "p": abs("a.txt")},
		ev{"k": "exit", "code": 0, "t": 200})
	streamAs(t, td, "proc-50-0-bbb.jsonl",
		ev{"k": "start", "pid": 50, "ppid": 2, "tid": 0, "argv": []string{"node", "b.js"}, "t": 1000, "lang": "node"},
		ev{"k": "read", "p": abs("b.txt")},
		ev{"k": "exit", "code": 1, "t": 2000})
	streamAs(t, td, "proc-60-0-ccc.jsonl",
		ev{"k": "start", "pid": 60, "ppid": 99, "anc": "50-0", "pctx": "50-0:test:" + abs("a.txt"), "t": 150, "lang": "python"},
		ev{"k": "read", "p": abs("early.txt")})
	streamAs(t, td, "proc-61-0-ddd.jsonl",
		ev{"k": "start", "pid": 61, "ppid": 99, "anc": "50-0", "pctx": "50-0:test:" + abs("b.txt"), "t": 1500, "lang": "python"},
		ev{"k": "read", "p": abs("late.txt")})
	// A program an os.exec* replaced a Python process with: same pid, its own stream,
	// the replaced process as its traced ancestor.
	streamAs(t, td, "proc-80-0-e1.jsonl",
		ev{"k": "start", "pid": 80, "ppid": 1, "t": 100, "lang": "python"},
		ev{"k": "exec", "args": []string{"python3", "next.py"}, "cwd": root, "t": 110})
	streamAs(t, td, "proc-80-0-e2.jsonl",
		ev{"k": "start", "pid": 80, "ppid": 1, "anc": "80-0", "argv": []string{"next.py"}, "t": 120, "lang": "python"},
		ev{"k": "read", "p": abs("exec.txt")})
	stream(t, td, 70, 0, ev{"k": "start", "pid": 70, "t": 5}, ev{"k": "read", "p": abs("old.txt")})
	v8File(t, td, 50, 190, 0, v8JSON("file://"+abs("src/a.js")))
	v8File(t, td, 50, 1990, 0, v8JSON("file://"+abs("src/b.js")))

	procs, err := Collect(td, root)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(procs)
	first, second := m["50-0"], m["50-0.1"]
	if first == nil || second == nil || len(procs) != 7 {
		t.Fatalf("procs %v", slices.Sorted(maps.Keys(m)))
	}
	if got := keys(first.Reads); !slices.Equal(got, []string{"a.txt"}) || first.Ppid != 1 || *first.Exit != 0 || !slices.Equal(keys(first.Code), []string{"src/a.js"}) {
		t.Errorf("first: reads %v code %v %+v", got, keys(first.Code), first)
	}
	if got := keys(second.Reads); !slices.Equal(got, []string{"b.txt"}) || second.Ppid != 2 || *second.Exit != 1 || !slices.Equal(keys(second.Code), []string{"src/b.js"}) {
		t.Errorf("second: reads %v code %v %+v", got, keys(second.Code), second)
	}
	for key, want := range map[string]string{"60-0": "50-0", "61-0": "50-0.1", "80-0.1": "80-0"} {
		if p := m[key]; p == nil || p.Anc != want {
			t.Errorf("%s: anc %+v, want %s", key, p, want)
		}
	}
	if p := m["61-0"]; p.SpawnKey != "50-0.1" {
		t.Errorf("spawn key %q", p.SpawnKey)
	}
	if p := m["70-0"]; p == nil || !slices.Equal(keys(p.Reads), []string{"old.txt"}) {
		t.Errorf("stream without a nonce: %+v", p)
	}
	// The exec'd program traced itself: the command is not opaque.
	if got := keys(m["80-0"].Reads); slices.Contains(got, "./**") {
		t.Errorf("exec'd program: reads %v", got)
	}
}

// A path read through a symlink of the repository that points outside of it records
// the link (retargeting it changes what the test sees), for every kind of access; the
// file outside is not recorded. A link inside the repository keeps both.
func TestCollectSymlinkOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root := repoDir(t)
	out := repoDir(t)
	touch(t, root, "data/v1.json", "plain.txt")
	touch(t, out, "ext/f.txt", "ext/sub/", "v2.json")
	for link, target := range map[string]string{
		"data/current.json": filepath.Join(out, "v2.json"),
		"vendorlink":        filepath.Join(out, "ext"),
		"data/inner.json":   "v1.json",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	abs := func(r string) string { return filepath.Join(root, r) }
	for _, tc := range []struct {
		kind, path string
		want       []string
	}{
		{"read", abs("data/current.json"), []string{"data/current.json"}},
		{"code", abs("data/current.json"), []string{"data/current.json"}},
		{"read", abs("vendorlink/f.txt"), []string{"vendorlink"}},
		{"dir", abs("vendorlink"), []string{"vendorlink"}},
		{"dir", abs("vendorlink/sub"), []string{"vendorlink"}},
		{"tree", abs("vendorlink"), []string{"vendorlink"}},
		{"probe", abs("vendorlink/missing"), []string{"vendorlink"}},
		{"read", abs("data/inner.json"), []string{"data/inner.json", "data/v1.json"}}, // must keep
		{"read", abs("plain.txt"), []string{"plain.txt"}},
		{"read", filepath.Join(out, "v2.json"), nil},
	} {
		t.Run(tc.kind+" "+tc.path, func(t *testing.T) {
			td := t.TempDir()
			stream(t, td, 1, 0, ev{"k": "start", "pid": 1, "t": 1}, ev{"k": tc.kind, "p": tc.path})
			procs, err := Collect(td, root)
			if err != nil {
				t.Fatal(err)
			}
			got := slices.Concat(keys(procs[0].Reads), keys(procs[0].Code))
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A Node, Bun or Python command counts as tracing itself only when a traced process of
// that runtime started under the process that ran it: otherwise (an environment that
// dropped the tracing variables) it is analyzed as an opaque command, which may read
// anything below its working directory.
func TestCollectExecVerified(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "app.js", "b.py")
	abs := func(r string) string { return filepath.Join(root, r) }
	type child struct {
		anc  string
		pid  int
		lang string
		argv []string
		t    int64
	}
	nodeKid := child{anc: "10-0", pid: 11, lang: "node", argv: []string{"/bin/node", abs("app.js")}, t: 1100}
	for _, tc := range []struct {
		name   string
		exec   ev
		kids   []child
		opaque bool
	}{
		{name: "node with a traced child", exec: ev{"args": []string{"node", "app.js"}, "t": 1000}, kids: []child{nodeKid}},
		{name: "node without one (must keep)", exec: ev{"args": []string{"node", "app.js"}, "t": 1000}, opaque: true},
		{name: "child of another process", exec: ev{"args": []string{"node", "app.js"}, "t": 1000}, kids: []child{{anc: "99-0", pid: 11, lang: "node", t: 1100}}, opaque: true},
		{name: "child started before the command", exec: ev{"args": []string{"node", "app.js"}, "t": 5000}, kids: []child{nodeKid}, opaque: true},
		{name: "child with other arguments", exec: ev{"args": []string{"node", "app.js", "--port", "1"}, "t": 1000}, kids: []child{{anc: "10-0", pid: 11, lang: "node", argv: []string{"/bin/node", abs("app.js"), "--port", "2"}, t: 1100}}, opaque: true},
		{name: "child with the same arguments", exec: ev{"args": []string{"node", "app.js", "--port", "1"}, "t": 1000}, kids: []child{{anc: "10-0", pid: 11, lang: "node", argv: []string{"/bin/node", abs("app.js"), "--port", "1"}, t: 1100}}},
		{name: "child of another runtime", exec: ev{"args": []string{"python3", "b.py"}, "t": 1000}, kids: []child{nodeKid}, opaque: true},
		{name: "python child", exec: ev{"args": []string{"python3", "b.py", "x"}, "t": 1000}, kids: []child{{anc: "10-0", pid: 11, lang: "python", argv: []string{"b.py", "x"}, t: 1100}}},
		{name: "through a shell", exec: ev{"args": []string{"node app.js | tr a b"}, "shell": true, "t": 1000}, kids: []child{{anc: "10-0", pid: 11, lang: "node", argv: []string{"/bin/node", abs("app.js"), "expanded"}, t: 1100}}},
		{name: "through a shell, no child", exec: ev{"args": []string{"node app.js | tr a b"}, "shell": true, "t": 1000}, opaque: true},
		{name: "grandchild through npx", exec: ev{"args": []string{"npx", "vitest"}, "t": 1000}, kids: []child{{anc: "10-0", pid: 11, lang: "node", argv: []string{"/bin/node", "/x/npx-cli.js", "vitest"}, t: 1100}, {anc: "11-0", pid: 12, lang: "node", t: 1200}}},
		{name: "older shim: no time", exec: ev{"args": []string{"node", "app.js"}}, kids: []child{nodeKid}},
		{name: "environment without the shim", exec: ev{"args": []string{"node", "app.js"}, "t": 1000, "rt": []string{"python"}}, kids: []child{nodeKid}, opaque: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			td := t.TempDir()
			e := ev{"k": "exec", "cwd": root}
			for k, v := range tc.exec {
				e[k] = v
			}
			stream(t, td, 10, 0, ev{"k": "start", "pid": 10, "t": 900, "lang": "node"}, e)
			for _, k := range tc.kids {
				stream(t, td, k.pid, 0, ev{"k": "start", "pid": k.pid, "ppid": 1, "anc": k.anc, "argv": k.argv, "t": k.t, "lang": k.lang})
			}
			procs, err := Collect(td, root)
			if err != nil {
				t.Fatal(err)
			}
			got := keys(byKey(procs)["10-0"].Reads)
			if slices.Contains(got, "./**") != tc.opaque {
				t.Errorf("reads %v, want opaque %v", got, tc.opaque)
			}
		})
	}
}

// A Node, Bun or Python program a Go test starts traces itself only when as many traced
// processes of that runtime name the test binary's package as their traced ancestor;
// otherwise (a test giving the command an environment of its own) every start of it is
// an opaque program.
func TestGoTestLogsUntracedRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bits")
	}
	root := repoDir(t)
	write := func(rel, content string, perm os.FileMode) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("go.mod", "module m\n", 0o644)
	node := write("bin/node", "", 0o755)
	python := write("bin/python3", "", 0o755)
	bin := write("build/pkg.test", "", 0o755)
	pkg := filepath.Join(root, "pkg")
	os.MkdirAll(pkg, 0o755)
	tree := filepath.Join(pkg, "**")
	for _, tc := range []struct {
		name   string
		starts []string
		kids   []ev // start events in the trace directory
		opaque bool
	}{
		{name: "traced node", starts: []string{node}, kids: []ev{{"anc": "go:" + pkg, "lang": "node"}}},
		{name: "untraced node (must keep)", starts: []string{node}, opaque: true},
		{name: "two starts, one traced", starts: []string{node, node}, kids: []ev{{"anc": "go:" + pkg, "lang": "node"}}, opaque: true},
		{name: "another package's child", starts: []string{node}, kids: []ev{{"anc": "go:" + root, "lang": "node"}}, opaque: true},
		{name: "a grandchild", starts: []string{node}, kids: []ev{{"anc": "5-0", "lang": "node"}}, opaque: true},
		{name: "another runtime's child", starts: []string{python}, kids: []ev{{"anc": "go:" + pkg, "lang": "node"}}, opaque: true},
		{name: "traced python", starts: []string{python}, kids: []ev{{"anc": "go:" + pkg, "lang": "python"}}},
		{name: "a worker thread", starts: []string{node}, kids: []ev{{"anc": "go:" + pkg, "lang": "node", "tid": 1}}, opaque: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			td := t.TempDir()
			children := filepath.Join(td, "children")
			os.MkdirAll(children, 0o755)
			lines := []string{"# test log"}
			for _, s := range tc.starts {
				lines = append(lines, "open "+s)
			}
			log := filepath.Join(td, "test.log")
			os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
			for i, k := range tc.kids {
				e := ev{"k": "start", "pid": 100 + i, "t": 1}
				for key, v := range k {
					e[key] = v
				}
				streamAs(t, td, fmt.Sprintf("proc-%d-0-n.jsonl", 100+i), e)
			}
			files, _ := GoTestLogs(log, pkg, bin, children)
			if slices.Contains(files, tree) != tc.opaque {
				t.Errorf("files %v, want opaque %v", files, tc.opaque)
			}
		})
	}
}

// A JUnit report cannot tell a test that skipped itself from a disabled one: a skipped
// test case makes its file unknown (as Gradle's skips without assumption information),
// and a failure still wins.
func TestJUnitSkipped(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "a_test.py", "b_test.py", "c_test.py", "d_test.py")
	report := filepath.Join(t.TempDir(), "junit.xml")
	os.WriteFile(report, []byte(`<testsuites><testsuite name="s">
<testcase file="a_test.py" name="t1"/><testcase file="a_test.py" name="t2"><skipped message="needs db"/></testcase>
<testcase file="b_test.py" name="t1"><skipped/></testcase>
<testcase file="c_test.py" name="t1"><skipped/></testcase><testcase file="c_test.py" name="t2"><failure message="boom"/></testcase>
<testcase file="d_test.py" name="t1"/>
</testsuite></testsuites>`), 0o644)
	got, err := JUnit(report, root, root)
	if err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{"a_test.py": state.Unknown, "b_test.py": state.Unknown, "c_test.py": state.Fail, "d_test.py": state.Pass} {
		if got[f].Status != want {
			t.Errorf("%s: %q, want %q", f, got[f].Status, want)
		}
	}
}
