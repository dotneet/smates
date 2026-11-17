//go:build unix

package trace

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Files a script names that are not regular files (a pipe, a device, /dev/stdin) or
// too big are never read: reading them may block or never end. What they hold is
// unknown, so the code may read anything.
func TestExecDepsSpecialFilesNotRead(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "x")
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(root, "zero")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("fifo", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	writeScript(t, root, "big.sh", strings.Repeat("# filler\n", 40000)+"cat x\n")
	writeScript(t, root, "small.sh", "cat x\n")
	for _, c := range []struct {
		name, sh string
		want     []string
	}{
		{"source a pipe", ". ./fifo", []string{"./**", "fifo"}},
		{"source through a symlink to a pipe", "source link", []string{"./**", "fifo"}},
		{"source /dev/zero", ". /dev/zero", []string{"./**"}},
		{"source a symlink to /dev/zero", ". ./zero", []string{"./**"}},
		{"source /dev/stdin", ". /dev/stdin < small.sh", []string{"./**"}},
		{"source a big file", ". ./big.sh", []string{"./**", "big.sh"}},
		{"run a pipe", "sh ./fifo", []string{"./**", "fifo"}},
		{"run /dev/zero", "sh /dev/zero", []string{"./**"}},
		{"stdin from a pipe", "bash < fifo", []string{"./**", "fifo"}},
		{"program by path", "./fifo", []string{"./**", "fifo"}},
		{"launched program by path", "uv run ./fifo", []string{"./**", "fifo"}},
		{"sed script from a pipe", "sed -f fifo x", []string{"./**", "fifo", "x"}},
		{"sed script from /dev/zero", "sed -f /dev/zero x", []string{"./**", "x"}},
		{"awk script from a pipe", "awk -f fifo x", []string{"./**", "fifo", "x"}},
		{"awk script from a device", "awk -f zero x", []string{"./**", "x"}},
		{"file list from a pipe", "wc --files0-from=fifo", []string{"./**", "fifo"}},
		{"file list from /dev/zero", "sort --files0-from=/dev/zero", []string{"./**"}},
		// Only named, not read by the analysis.
		{"cat a pipe", "cat fifo", []string{"fifo"}},
		// Must keep: a regular script is still followed.
		{"source a regular file", ". ./small.sh", []string{"small.sh", "x"}},
	} {
		done := make(chan []string, 1)
		go func() { done <- execKeys(root, []string{c.sh}, true) }()
		select {
		case got := <-done:
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("%s: %q = %q, want %q", c.name, c.sh, got, c.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: %q did not return in 5s", c.name, c.sh)
		}
	}
}

// scriptRuntime reads the #! line of a regular file only: a pipe or a device named
// like a program (a console script in .venv/bin or node_modules/.bin) is not read.
func TestScriptRuntimeSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "tool")
	if err := syscall.Mkfifo(fifo, 0o755); err != nil {
		t.Skip("mkfifo:", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "py"), []byte("#!/usr/bin/env python3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path, want string }{
		{"a pipe", fifo, ""},
		{"a device", "/dev/zero", ""},
		// Must keep: a regular script is still read.
		{"a python script", filepath.Join(dir, "py"), "python"},
	} {
		done := make(chan string, 1)
		go func() { done <- scriptRuntime(c.path, true) }()
		select {
		case got := <-done:
			if got != c.want {
				t.Errorf("%s: scriptRuntime = %q, want %q", c.name, got, c.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: scriptRuntime did not return in 5s", c.name)
		}
	}
	// uv run of a console script that is a pipe returns.
	root := repoDir(t)
	if err := os.MkdirAll(filepath.Join(root, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, ".venv", "bin", "tool"), 0o755); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan []string, 1)
	go func() { done <- execKeys(root, []string{"uv run tool"}, true) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("uv run of a pipe did not return in 5s")
	}
}
