package trace

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// GoTestLogs adds what the programs a Go test started may have read: re-executions of
// the test binary through their own test logs (all of the module when one is missing or
// cut short), other programs as the trees of the directories they may run in; Node and
// Python trace themselves.
func TestGoTestLogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bits")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string, perm os.FileMode) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("go.mod", "module m\n", 0o644)
	write("pkg/data.txt", "x", 0o644)
	write("shared.txt", "x", 0o644)
	write("work/.keep", "", 0o644)
	bin := write("build/pkg.test", "", 0o755)
	tool := write("bin/tool", "", 0o755)
	write("bin/node", "", 0o755)
	write("bin/run.js", "#!/usr/bin/env node\n", 0o755)
	write("bin/script.sh", "#!/bin/sh\n", 0o644) // read, not run: not executable
	pkg := filepath.Join(root, "pkg")
	log := write("parent.log", strings.Join([]string{
		"# test log",
		"open " + bin, // a re-execution
		"open data.txt",
		"open ../bin/tool", // run in the test's directory
		"open " + tool,     // run with cmd.Dir
		"stat ../work",
		"open ../bin/node", // traces itself
		"open ../bin/run.js",
		"open ../bin/script.sh",
		"",
	}, "\n"), 0o644)
	children := filepath.Join(root, "children")
	os.MkdirAll(children, 0o755)
	os.WriteFile(filepath.Join(children, "100.dir"), []byte(pkg), 0o644)
	os.WriteFile(filepath.Join(children, "100.testlog"), []byte("# test log\nopen ../shared.txt\ngetenv MODE\n"), 0o644)

	sep := string(filepath.Separator)
	tree := func(rel string) string { return filepath.Join(root, rel, "**") }
	base := []string{
		filepath.Join(root, "pkg/data.txt"),
		tool,
		tree("pkg"),
		filepath.Join(root, "work") + "?",
		tree("work"),
		filepath.Join(root, "bin/node"),
		filepath.Join(root, "bin/run.js"),
		filepath.Join(root, "bin/script.sh"),
		filepath.Join(root, "shared.txt"),
	}
	check := func(name string, want []string, wantEnv ...string) {
		t.Helper()
		files, env := GoTestLogs(log, pkg, bin, children)
		files = slices.DeleteFunc(files, func(f string) bool { return f == bin })
		slices.Sort(files)
		want = slices.Clone(want)
		slices.Sort(want)
		if !slices.Equal(files, want) {
			t.Errorf("%s: files\n %q\nwant\n %q", name, files, want)
		}
		if !slices.Equal(env, wantEnv) {
			t.Errorf("%s: env %q, want %q", name, env, wantEnv)
		}
	}
	check("complete child", base, "MODE")

	// A child whose log was cut short (os.Exit before its tests ended): the module.
	os.WriteFile(filepath.Join(children, "200.dir"), []byte(pkg), 0o644)
	os.WriteFile(filepath.Join(children, "200.testlog"), []byte(strings.Repeat("x", 4096)), 0o644)
	check("cut child", append(slices.Clone(base), filepath.Join(root, "**")), "MODE")
	os.Remove(filepath.Join(children, "200.dir"))
	os.Remove(filepath.Join(children, "200.testlog"))

	// A re-execution that left no log at all.
	os.WriteFile(log, []byte("# test log\nopen "+bin+"\nopen "+bin+"\n"), 0o644)
	check("missing child", []string{filepath.Join(root, "shared.txt"), filepath.Join(root, "**")}, "MODE")

	// Without children, nothing is added.
	os.RemoveAll(children)
	os.WriteFile(log, []byte("# test log\nopen data.txt\nstat "+sep+"nope\n"), 0o644)
	check("no children", []string{filepath.Join(root, "pkg/data.txt"), sep + "nope?"})
}
