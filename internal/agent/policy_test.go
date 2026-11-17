package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

func TestHasGitSegment(t *testing.T) {
	for p, want := range map[string]bool{
		".git":             true,
		".git/config":      true,
		".GIT/config":      true,
		".Git/HEAD":        true,
		"sub/.git/config":  true,
		"sub/.gIt":         true,
		".git./config":     true,
		".git /config":     true,
		".gitignore":       false,
		".github/workflow": false,
		"src/git/x":        false,
		"a.git/x":          false,
		"x/.gitkeep":       false,
	} {
		if got := hasGitSegment(p); got != want {
			t.Errorf("hasGitSegment(%q) = %v, want %v", p, got, want)
		}
	}
}

// policyRepo is a repository whose working tree has, beside readable files, every kind
// of path the tools must refuse: ignored files, .git in other letter cases, and
// symbolic links (of files and of directories) leading outside the repository, into
// .git or to an ignored file.
func policyRepo(t *testing.T) (*testutil.GitRepo, *toolbox) {
	t.Helper()
	g := testutil.NewGitRepo(t, map[string]string{
		".gitignore": ".env\nignored/\n",
		"a.txt":      "hello readable\n",
		"d/f.txt":    "tracked inner\n",
	})
	outside := t.TempDir()
	testutil.Write(t, outside, "secret.txt", "OUTSIDE SECRET\n")
	testutil.Write(t, outside, "f.txt", "OUTSIDE SECRET via dir\n")
	g.Write(".env", "ENV SECRET\n")
	g.Write("ignored/x.txt", "IGNORED SECRET\n")
	for link, target := range map[string]string{
		"to-a":       "a.txt",
		"to-env":     ".env",
		"to-ignored": "ignored/x.txt",
		"to-git":     ".git/config",
		"upper-git":  ".GIT/config",
		"to-outside": filepath.Join(outside, "secret.txt"),
		"linkdir":    outside,
	} {
		if err := os.Symlink(target, filepath.Join(g.Dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	// A tracked directory replaced by a link to the outside: git still lists d/f.txt.
	if err := os.RemoveAll(filepath.Join(g.Dir, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(g.Dir, "d")); err != nil {
		t.Fatal(err)
	}
	return g, &toolbox{root: g.Dir, orphans: map[string]Orphan{}}
}

func TestToolsFileAccessPolicy(t *testing.T) {
	_, tb := policyRepo(t)
	ctx := context.Background()
	tests := []struct {
		path    string
		wantErr string // "" when readable
		want    string
	}{
		{path: "a.txt", want: "hello readable"},
		{path: "to-a", want: "hello readable"},
		{path: ".env", wantErr: "git-ignored"},
		{path: "ignored/x.txt", wantErr: "git-ignored"},
		{path: ".git/config", wantErr: "not readable"},
		{path: ".GIT/config", wantErr: "not readable"},
		{path: ".Git/HEAD", wantErr: "not readable"},
		{path: "sub/.git/config", wantErr: "not readable"},
		{path: "to-env", wantErr: "not readable"},
		{path: "to-ignored", wantErr: "not readable"},
		{path: "to-git", wantErr: "not readable"},
		{path: "upper-git", wantErr: "upper-git"}, // refused, how depends on the file system's case sensitivity
		{path: "to-outside", wantErr: "points outside"},
		{path: "linkdir/secret.txt", wantErr: "not a repository file"},
		{path: "d/f.txt", wantErr: "points outside"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			out, err := tb.readFile(tt.path, 0, 0)
			if tt.wantErr == "" {
				if err != nil || !strings.Contains(out, tt.want) {
					t.Fatalf("read_file = %q, %v; want %q", out, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || strings.Contains(out, "SECRET") {
				t.Fatalf("read_file = %q, %v; want error %q", out, err, tt.wantErr)
			}
			// git_diff shows a link's target text, as git records it, but never what
			// a refused path leads to.
			if out, err := tb.gitDiff(ctx, tt.path); strings.Contains(out, "SECRET") || err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("git_diff leaks: %q, %v", out, err)
			}
		})
	}
	out, err := tb.grep(ctx, "SECRET|readable", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SECRET") || !strings.Contains(out, "a.txt:1: hello readable") {
		t.Fatalf("grep = %q", out)
	}
}

// When git cannot list the repository's files, nothing is readable: whether a file is
// ignored is unknown.
func TestToolsFailClosedWithoutListing(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, dir, "a.txt", "SECRET\n")
	t.Setenv("GIT_DIR", filepath.Join(dir, "no-such-git-dir"))
	tb := &toolbox{root: dir, orphans: map[string]Orphan{}}
	ctx := context.Background()
	if out, err := tb.readFile("a.txt", 0, 0); err == nil || strings.Contains(out, "SECRET") {
		t.Errorf("read_file = %q, %v", out, err)
	}
	if out, err := tb.gitDiff(ctx, "a.txt"); err == nil || strings.Contains(out, "SECRET") {
		t.Errorf("git_diff = %q, %v", out, err)
	}
	if out, err := tb.grep(ctx, "SECRET", "", 0); err == nil || strings.Contains(out, "SECRET") {
		t.Errorf("grep = %q, %v", out, err)
	}
	if _, err := tb.repoFile("a.txt"); err == nil || !strings.Contains(err.Error(), "cannot list") {
		t.Errorf("repoFile error = %v", err)
	}
}

func TestRepoFileSizeLimit(t *testing.T) {
	g := testutil.NewGitRepo(t, map[string]string{"a.txt": "0123456789"})
	tb := &toolbox{root: g.Dir, orphans: map[string]Orphan{}}
	rf, err := tb.repoFile("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	var big *tooLargeError
	if _, err := rf.read(9); !errors.As(err, &big) || big.size != 10 {
		t.Errorf("read(9) = %v", err)
	}
	if _, err := rf.open(9); !errors.As(err, &big) {
		t.Errorf("open(9) = %v", err)
	}
	if data, err := rf.read(10); err != nil || string(data) != "0123456789" {
		t.Errorf("read(10) = %q, %v", data, err)
	}
	if _, err := tb.catBlob(context.Background(), "HEAD:a.txt", 9); !errors.As(err, &big) || big.size != 10 {
		t.Errorf("catBlob(9) = %v", err)
	}
	if data, err := tb.catBlob(context.Background(), "HEAD:a.txt", 10); err != nil || string(data) != "0123456789" {
		t.Errorf("catBlob(10) = %q, %v", data, err)
	}
}

func TestGitDiffTooLarge(t *testing.T) {
	big := strings.Repeat("x", maxDiffFileSize) + "\n"
	g := testutil.NewGitRepo(t, map[string]string{"old-big.txt": big, "new-big.txt": "small\n"})
	g.Write("old-big.txt", "small\n")
	g.Write("new-big.txt", big)
	g.Write("untracked-big.txt", big)
	tb := &toolbox{root: g.Dir, orphans: map[string]Orphan{}}
	for _, p := range []string{"old-big.txt", "new-big.txt", "untracked-big.txt"} {
		out, err := tb.gitDiff(context.Background(), p)
		if err != nil || !strings.Contains(out, "too large to diff") || len(out) > 1000 {
			t.Errorf("%s: git_diff = %.200q, %v", p, out, err)
		}
	}
}

func TestReadFileLongLine(t *testing.T) {
	g := testutil.NewGitRepo(t, map[string]string{
		"long.txt":   strings.Repeat("y", 3<<20) + "\nsecond\n",
		"nolf.txt":   strings.Repeat("é", 1<<20),
		"exact.txt":  strings.Repeat("z", maxReadLine) + "\n",
		"crlf.txt":   "a\r\nb\r\n",
		"no-eol.txt": "last",
	})
	tb := &toolbox{root: g.Dir, orphans: map[string]Orphan{}}
	tests := []struct {
		path string
		want []string
	}{
		{"long.txt", []string{"     1\t" + strings.Repeat("y", maxReadLine) + "…\n", "     2\tsecond\n"}},
		{"nolf.txt", []string{"…\n"}},
		{"exact.txt", []string{"     1\t" + strings.Repeat("z", maxReadLine) + "\n"}},
		{"crlf.txt", []string{"     1\ta\n", "     2\tb\n"}},
		{"no-eol.txt", []string{"     1\tlast\n"}},
	}
	for _, tt := range tests {
		out, err := tb.readFile(tt.path, 0, 0)
		if err != nil {
			t.Fatalf("%s: %v", tt.path, err)
		}
		for _, w := range tt.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: output lacks %q: %.300q", tt.path, w, out)
			}
		}
		if len(out) > 2*maxReadLine+200 {
			t.Errorf("%s: %d bytes of output", tt.path, len(out))
		}
	}
}

func TestReadLine(t *testing.T) {
	// The first line is longer than the reader's buffer and than what is kept.
	r := bufio.NewReaderSize(strings.NewReader("abcdefghijklmnopqrstuvwxyz0123456789\nshort\nend"), 16)
	for _, want := range []string{"abcdef", "short\n", "end"} {
		if got, err := readLine(r, 6); string(got) != want || err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("readLine = %q, %v; want %q", got, err, want)
		}
	}
	if got, err := readLine(r, 6); len(got) != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("at end: %q, %v", got, err)
	}
}
