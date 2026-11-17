package gitx

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

// testRepo is a temporary git repository.
type testRepo struct {
	*testutil.GitRepo
	t    *testing.T
	dir  string
	repo *Repo
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	g := testutil.NewGitRepo(t, nil)
	repo, err := Open(g.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return &testRepo{GitRepo: g, t: t, dir: g.Dir, repo: repo}
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	return r.Git(args...)
}

func (r *testRepo) write(rel, content string) {
	r.t.Helper()
	r.Write(rel, content)
}

func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	return r.Commit(msg)
}

func TestOpenResolvesRootFromSubdir(t *testing.T) {
	r := newTestRepo(t)
	r.write("a/b/c.txt", "x")
	repo, err := Open(filepath.Join(r.dir, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(r.dir)
	if repo.Root != want {
		t.Errorf("root %q, want %q", repo.Root, want)
	}
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("Open outside a repository must fail")
	}
}

func TestSnapshotMatchesHashObject(t *testing.T) {
	r := newTestRepo(t)
	r.write("clean.txt", "clean\n")
	r.write("modified.txt", "v1\n")
	r.write("staged.txt", "v1\n")
	r.write("deleted.txt", "bye\n")
	r.write("staged-deleted.txt", "bye\n")
	r.write("dir/nested file.txt", "spaces\n")
	r.write("ünïcode.txt", "u\n")
	r.write(".gitignore", "ignored.log\nbuild/\n")
	if runtime.GOOS != "windows" {
		if err := os.Symlink("clean.txt", filepath.Join(r.dir, "link")); err != nil {
			t.Fatal(err)
		}
	}
	r.commit("init")

	r.write("modified.txt", "v2\n")
	r.write("staged.txt", "v2\n")
	r.git("add", "staged.txt")
	r.write("staged.txt", "v3\n") // staged and modified again: the working tree wins
	os.Remove(filepath.Join(r.dir, "deleted.txt"))
	r.git("rm", "-q", "staged-deleted.txt")
	r.write("untracked.txt", "new\n")
	r.write("newdir/deep/untracked.txt", "new2\n")
	r.write("ignored.log", "ignored\n")
	r.write("build/out.js", "ignored\n")
	r.write("added.txt", "added\n")
	r.git("add", "added.txt")

	snap, err := r.repo.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"clean.txt", "modified.txt", "staged.txt", "dir/nested file.txt", "ünïcode.txt", ".gitignore", "untracked.txt", "newdir/deep/untracked.txt", "added.txt"}
	if runtime.GOOS != "windows" {
		want = append(want, "link")
	}
	for _, f := range want {
		got, ok := snap[f]
		if !ok {
			t.Errorf("%s missing from snapshot", f)
			continue
		}
		exp := r.git("hash-object", "--no-filters", "--", f)
		if f == "link" {
			exp = r.git("rev-parse", "HEAD:link")
		}
		if got != exp {
			t.Errorf("%s: %s, want %s", f, got, exp)
		}
	}
	for _, f := range []string{"deleted.txt", "staged-deleted.txt", "ignored.log", "build/out.js"} {
		if _, ok := snap[f]; ok {
			t.Errorf("%s must not be in the snapshot", f)
		}
	}
	if len(snap) != len(want) {
		var keys []string
		for k := range snap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("snapshot has %d entries, want %d: %v", len(snap), len(want), keys)
	}
}

func TestSnapshotEmptyRepo(t *testing.T) {
	r := newTestRepo(t)
	snap, err := r.repo.Snapshot()
	if err != nil || len(snap) != 0 {
		t.Fatalf("%v %v", snap, err)
	}
	r.write("a.txt", "a")
	snap, err = r.repo.Snapshot()
	if err != nil || snap["a.txt"] != HashBytes([]byte("a")) {
		t.Fatalf("untracked file in a repo without commits: %v %v", snap, err)
	}
	if r.repo.Head() != "" {
		t.Error("Head of an empty repo must be empty")
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	os.WriteFile(p, []byte("hello\n"), 0o644)
	h, err := HashFile(p)
	// git hash-object of "hello\n"
	if err != nil || h != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("%s %v", h, err)
	}
	if HashBytes([]byte("hello\n")) != h {
		t.Error("HashBytes != HashFile")
	}
	if HashBytes(nil) != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Error("empty blob hash")
	}
	if _, err := HashFile(dir); err == nil {
		t.Error("directories are not hashable")
	}
	if _, err := HashFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file must error")
	}
	if runtime.GOOS != "windows" {
		os.Symlink("f", filepath.Join(dir, "l"))
		lh, err := HashFile(filepath.Join(dir, "l"))
		if err != nil || lh != HashBytes([]byte("f")) {
			t.Errorf("symlink hashes its target path: %s %v", lh, err)
		}
	}
	res := HashFiles(dir, []string{"f", "missing", "f"})
	if res[0] != h || res[1] != "" || res[2] != h {
		t.Errorf("HashFiles %v", res)
	}
	if len(HashFiles(dir, nil)) != 0 {
		t.Error("HashFiles(nil)")
	}
}

// The executable bit is part of a file's hash: a test running a script that lost it
// fails differently (EACCES).
func TestSnapshotExecutableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no executable bit")
	}
	r := newTestRepo(t)
	testutil.WriteMode(t, r.dir, "run.sh", "#!/bin/sh\necho hi\n", 0o755)
	r.write("data.txt", "d\n")
	base := r.commit("init")
	blob := r.git("hash-object", "--no-filters", "--", "run.sh")
	snapshot := func() Snapshot {
		t.Helper()
		snap, err := r.repo.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	hashFile := func(rel string) string {
		t.Helper()
		h, err := HashFile(filepath.Join(r.dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	snap := snapshot()
	if snap["run.sh"] != blob+ExecSuffix || hashFile("run.sh") != snap["run.sh"] {
		t.Errorf("executable file: snapshot %q, HashFile %q, want %q", snap["run.sh"], hashFile("run.sh"), blob+ExecSuffix)
	}
	if Blob(snap["run.sh"]) != blob || Blob(snap["data.txt"]) != snap["data.txt"] {
		t.Error("Blob must strip the executable marker only")
	}

	// chmod -x in the working tree: a change git status reports, and so does smates.
	os.Chmod(filepath.Join(r.dir, "run.sh"), 0o644)
	if snap := snapshot(); snap["run.sh"] != blob || hashFile("run.sh") != blob {
		t.Errorf("after chmod -x: snapshot %q, HashFile %q, want %q", snap["run.sh"], hashFile("run.sh"), blob)
	}
	got, err := r.repo.ChangedSince(base)
	if err != nil || !slices.Equal(got, []string{"run.sh"}) {
		t.Errorf("ChangedSince after chmod -x = %v %v", got, err)
	}
	// Committed: the index has mode 100644 now.
	r.git("commit", "-q", "-am", "chmod")
	if snap := snapshot(); snap["run.sh"] != blob {
		t.Errorf("committed chmod -x: %q, want %q", snap["run.sh"], blob)
	}
	if got, err := r.repo.ChangedSince(base); err != nil || !slices.Equal(got, []string{"run.sh"}) {
		t.Errorf("ChangedSince over a mode-only commit = %v %v", got, err)
	}
	head := r.repo.Head()
	if ch, err := r.repo.ChangedInCommit(Commit{SHA: head, Parent: base}); err != nil || len(ch) != 1 || ch[0].Path != "run.sh" {
		t.Errorf("ChangedInCommit of a mode-only commit = %v %v", ch, err)
	}

	// An untracked executable file.
	testutil.WriteMode(t, r.dir, "new.sh", "#!/bin/sh\n", 0o755)
	if snap := snapshot(); snap["new.sh"] != HashBytes([]byte("#!/bin/sh\n"))+ExecSuffix {
		t.Errorf("untracked executable: %q", snap["new.sh"])
	}

	// core.fileMode false: git ignores the bit on disk, and so does the snapshot (an
	// edited file keeps the index's mode, an untracked one would be added as 100644).
	r.git("config", "core.fileMode", "false")
	os.Chmod(filepath.Join(r.dir, "run.sh"), 0o755)
	r.write("data.txt", "d2\n")
	os.Chmod(filepath.Join(r.dir, "data.txt"), 0o755)
	snap = snapshot()
	if snap["run.sh"] != blob {
		t.Errorf("core.fileMode false, chmod +x: %q, want %q", snap["run.sh"], blob)
	}
	if want := HashBytes([]byte("d2\n")); snap["data.txt"] != want {
		t.Errorf("core.fileMode false, edited: %q, want %q", snap["data.txt"], want)
	}
	if snap["new.sh"] != HashBytes([]byte("#!/bin/sh\n")) {
		t.Errorf("core.fileMode false, untracked executable: %q", snap["new.sh"])
	}
	r.git("add", "new.sh")
	if got := r.git("ls-files", "-s", "new.sh"); !strings.HasPrefix(got, "100644 ") {
		t.Errorf("git adds it as %q", got)
	}
}

// TestCompare pins every rule of comparing a recorded hash with the current one, at
// both levels: a dependency key (ByKey) and an entry of the tree snapshot (ByContent,
// also SameHash).
func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		recorded, cur string
		key, content  bool
	}{
		{"ab", "ab", true, true},
		{"ab+x", "ab+x", true, true},
		{"ab", "ab+x", true, true},   // recorded without the marker (an older smates)
		{"ab+x", "ab", false, false}, // lost the bit
		{"ab", "cd+x", false, false},
		{"ab+exists", "ab+x+exists", true, true},
		{"", "ab+x", false, false},
		{"", "", true, true},
		{"commit:ab", "commit:ab", true, true},
		{"commit:ab", "commit:cd", false, false},
		// A skip-worktree file not on disk: the same content for git, not for a test
		// that read it (must keep: narrowing or widening the sparse cone selects it).
		{"ab", OffDisk("ab"), false, true},
		{OffDisk("ab"), "ab", false, true},
		{OffDisk("ab"), OffDisk("ab"), true, true},
		{OffDisk("ab+x"), OffDisk("ab+x"), true, true},
		{"ab+x", OffDisk("ab+x"), false, true},
		{"ab", OffDisk("ab+x"), false, true},
		{OffDisk("ab"), OffDisk("ab+x"), true, true}, // recorded without the marker
		{OffDisk("ab"), OffDisk("cd"), false, false},
		{OffDisk("ab"), "cd", false, false},
		{"", OffDisk("ab"), false, false},
		{OffDisk("ab"), "", false, false},
	} {
		if got := ByKey.Same(tc.recorded, tc.cur); got != tc.key {
			t.Errorf("ByKey.Same(%q, %q) = %v, want %v", tc.recorded, tc.cur, got, tc.key)
		}
		if got := ByContent.Same(tc.recorded, tc.cur); got != tc.content {
			t.Errorf("ByContent.Same(%q, %q) = %v, want %v", tc.recorded, tc.cur, got, tc.content)
		}
		if got := SameHash(tc.recorded, tc.cur); got != tc.content {
			t.Errorf("SameHash(%q, %q) = %v, want %v", tc.recorded, tc.cur, got, tc.content)
		}
	}
	// What the agent reads and what an older smates recorded.
	for h, want := range map[string][2]string{
		"ab":            {"ab", "ab"},
		"ab+x":          {"ab", "ab"},
		OffDisk("ab+x"): {"ab", OffDisk("ab")},
		OffDisk("ab"):   {"ab", OffDisk("ab")},
		"commit:ab":     {"commit:ab", "commit:ab"},
	} {
		if got := Blob(h); got != want[0] {
			t.Errorf("Blob(%q) = %q, want %q", h, got, want[0])
		}
		if got := LegacyHash(h); got != want[1] {
			t.Errorf("LegacyHash(%q) = %q, want %q", h, got, want[1])
		}
	}
}

func TestChangedSinceAndMergeBase(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a")
	r.write("b.txt", "b")
	r.write("c.txt", "c")
	base := r.commit("base")
	r.git("checkout", "-q", "-b", "feature")
	r.write("a.txt", "a2")
	r.commit("feature")
	r.write("b.txt", "b2") // unstaged
	r.write("s.txt", "s")
	r.git("add", "s.txt") // staged new
	os.Remove(filepath.Join(r.dir, "c.txt"))
	r.write("u/new.txt", "u") // untracked
	r.write(".gitignore", "*.log\n")
	r.write("x.log", "ignored")

	got, err := r.repo.ChangedSince(base)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{".gitignore", "a.txt", "b.txt", "c.txt", "s.txt", "u/new.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ChangedSince = %v, want %v", got, want)
	}
	if _, err := r.repo.ChangedSince("no-such-ref"); err == nil {
		t.Error("unknown base must fail")
	}

	// main moves on; merge base stays at base.
	r.git("stash", "-u", "-q")
	r.git("checkout", "-q", "main")
	r.write("m.txt", "m")
	r.commit("main moves")
	r.git("checkout", "-q", "feature")
	if mb := r.repo.MergeBase("main"); mb != base {
		t.Errorf("MergeBase = %q, want %q", mb, base)
	}
	if mb := r.repo.MergeBase("nope"); mb != "" {
		t.Errorf("MergeBase of unknown ref = %q", mb)
	}
	if !r.repo.RefExists("main") || r.repo.RefExists("nope") || !r.repo.RefExists("HEAD~1") || r.repo.RefExists("HEAD~5") {
		t.Error("RefExists")
	}
	if r.repo.CurrentBranch() != "feature" {
		t.Errorf("CurrentBranch %q", r.repo.CurrentBranch())
	}
	r.git("checkout", "-q", "--detach")
	if r.repo.CurrentBranch() != "" {
		t.Error("detached HEAD has no branch")
	}
	if b, ok := r.repo.BlobContent(r.git("rev-parse", base+":a.txt")); !ok || string(b) != "a" {
		t.Errorf("BlobContent %q %v", b, ok)
	}
	if _, ok := r.repo.BlobContent("0000000000000000000000000000000000000000"); ok {
		t.Error("missing blob")
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Run("main local", func(t *testing.T) {
		r := newTestRepo(t)
		r.write("a", "a")
		r.commit("x")
		if b := r.repo.DefaultBranch(); b != "main" {
			t.Errorf("got %q", b)
		}
	})
	t.Run("master local", func(t *testing.T) {
		r := newTestRepo(t)
		r.write("a", "a")
		r.commit("x")
		r.git("branch", "-m", "master")
		if b := r.repo.DefaultBranch(); b != "master" {
			t.Errorf("got %q", b)
		}
	})
	t.Run("unknown falls back to main", func(t *testing.T) {
		r := newTestRepo(t)
		r.write("a", "a")
		r.commit("x")
		r.git("branch", "-m", "trunk")
		if b := r.repo.DefaultBranch(); b != "main" {
			t.Errorf("got %q", b)
		}
	})
	t.Run("origin HEAD wins", func(t *testing.T) {
		up := newTestRepo(t)
		up.write("a", "a")
		up.commit("x")
		up.git("branch", "-m", "develop")
		r := newTestRepo(t)
		r.git("remote", "add", "origin", up.dir)
		r.git("fetch", "-q", "origin")
		r.git("remote", "set-head", "origin", "develop")
		r.git("checkout", "-q", "-b", "main", "origin/develop")
		if b := r.repo.DefaultBranch(); b != "develop" {
			t.Errorf("got %q", b)
		}
	})
}

func TestFirstParentHistory(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "1")
	r.write("b.txt", "1")
	root := r.commit("root")
	r.write("a.txt", "2")
	r.git("mv", "b.txt", "c.txt")
	second := r.commit("second\twith tab")
	r.git("checkout", "-q", "-b", "feat")
	r.write("feat/x.txt", "x")
	r.commit("feat")
	r.git("checkout", "-q", "main")
	r.write("d.txt", "d")
	r.commit("third")
	r.git("merge", "-q", "--no-ff", "-m", "merge feat", "feat")

	commits, err := r.repo.FirstParentCommits("", 0)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, c := range commits {
		subjects = append(subjects, c.Subject)
	}
	if strings.Join(subjects, "|") != "merge feat|third|second\twith tab|root" {
		t.Fatalf("subjects %q", subjects)
	}
	if commits[3].SHA != root || commits[3].Parent != "" || commits[2].SHA != second || commits[2].Parent != root {
		t.Errorf("commits %+v", commits)
	}
	if c, _ := r.repo.FirstParentCommits("", 2); len(c) != 2 {
		t.Errorf("limit: %d", len(c))
	}
	if c, _ := r.repo.FirstParentCommits(second, 0); len(c) != 2 || c[1].Subject != "third" {
		t.Errorf("since: %+v", c)
	}

	show := func(c Commit) string {
		ch, err := r.repo.ChangedInCommit(c)
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, x := range ch {
			s = append(s, string(x.Status)+" "+x.Path)
		}
		sort.Strings(s)
		return strings.Join(s, ",")
	}
	if got := show(commits[3]); got != "A a.txt,A b.txt" {
		t.Errorf("root: %s", got)
	}
	if got := show(commits[2]); got != "A c.txt,D b.txt,M a.txt" {
		t.Errorf("second: %s", got)
	}
	if got := show(commits[0]); got != "A feat/x.txt" {
		t.Errorf("merge (first-parent diff): %s", got)
	}
	for _, c := range commits {
		if c.Shallow {
			t.Errorf("full clone: %s marked shallow", c.Subject)
		}
	}
}

func TestFirstParentHistoryShallow(t *testing.T) {
	up := newTestRepo(t)
	for _, msg := range []string{"one", "two", "three"} {
		up.write("f"+msg, msg)
		up.write("common", msg)
		up.commit(msg)
	}
	dir := filepath.Join(t.TempDir(), "clone")
	up.git("clone", "-q", "--depth", "2", "file://"+up.dir, dir)
	repo, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &testRepo{t: t, dir: dir, repo: repo}
	commits, err := r.repo.FirstParentCommits("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || commits[0].Shallow || !commits[1].Shallow || commits[1].Parent != "" {
		t.Fatalf("commits %+v", commits)
	}
	if ch, err := r.repo.ChangedInCommit(commits[0]); err != nil || len(ch) != 2 {
		t.Errorf("inside the clone: %v %v", ch, err)
	}
	if ch, err := r.repo.ChangedInCommit(commits[1]); err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Errorf("boundary commit must not be diffed as a root: %v %v", ch, err)
	}
}

// TestSnapshotFlaggedFiles: git status does not report edits to files flagged
// skip-worktree or assume-unchanged, so the snapshot hashes them from disk; one that is
// not on disk hashes as OffDisk of its index hash when skip-worktree (a sparse
// checkout: the same content for git, ByContent, but not for a test, ByKey) and is
// deleted otherwise. ChangedSince reports the same edits.
func TestSnapshotFlaggedFiles(t *testing.T) {
	type row struct {
		name  string
		flags []string // update-index flags
		edit  func(r *testRepo, f string)
		// want: "disk" (hash-object of the file), "index" (HEAD's blob), "offdisk"
		// (HEAD's blob, off disk), "" (absent).
		want string
		exec bool // the expected hash carries ExecSuffix
	}
	write := func(r *testRepo, f string) { r.write(f, "edited\n") }
	remove := func(r *testRepo, f string) { os.Remove(filepath.Join(r.dir, f)) }
	chmodX := func(r *testRepo, f string) { os.Chmod(filepath.Join(r.dir, f), 0o755) }
	rows := []row{
		{"skip-worktree edited", []string{"--skip-worktree"}, write, "disk", false},
		{"assume-unchanged edited", []string{"--assume-unchanged"}, write, "disk", false},
		{"both flags edited", []string{"--skip-worktree", "--assume-unchanged"}, write, "disk", false},
		{"skip-worktree unedited", []string{"--skip-worktree"}, nil, "index", false},
		{"assume-unchanged unedited", []string{"--assume-unchanged"}, nil, "index", false},
		// must keep: a sparse checkout leaves skip-worktree files off disk.
		{"skip-worktree not on disk", []string{"--skip-worktree"}, remove, "offdisk", false},
		{"assume-unchanged deleted", []string{"--assume-unchanged"}, remove, "", false},
		{"unflagged edited", nil, write, "disk", false},
	}
	if runtime.GOOS != "windows" {
		rows = append(rows,
			row{"skip-worktree chmod +x", []string{"--skip-worktree"}, chmodX, "index", true},
			row{"assume-unchanged chmod +x", []string{"--assume-unchanged"}, chmodX, "index", true},
		)
	}
	r := newTestRepo(t)
	file := func(i int) string { return "f" + strconv.Itoa(i) + ".txt" }
	for i := range rows {
		r.write(file(i), "original\n")
	}
	r.commit("init")
	for i, c := range rows {
		if len(c.flags) > 0 {
			r.git(append(append([]string{"update-index"}, c.flags...), file(i))...)
		}
		if c.edit != nil {
			c.edit(r, file(i))
		}
	}
	snap, err := r.repo.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := r.repo.ChangedSince("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range rows {
		f := file(i)
		head := r.git("rev-parse", "HEAD:"+f)
		got, ok := snap[f]
		var want string
		switch c.want {
		case "disk":
			want = r.git("hash-object", "--no-filters", "--", f)
		case "index":
			want = head
		case "offdisk":
			want = OffDisk(head)
		}
		if c.exec {
			want += ExecSuffix
		}
		if off := c.want == "offdisk"; IsOffDisk(got) != off || off && (!ByContent.Same(head, got) || ByKey.Same(head, got)) {
			t.Errorf("%s: %s hashes %q: off disk %v, want %v (the same content as HEAD, not the same for a key)", c.name, f, got, IsOffDisk(got), off)
		}
		if c.want == "" && ok {
			t.Errorf("%s: %s in the snapshot (%s), want absent", c.name, f, got)
		} else if c.want != "" && got != want {
			t.Errorf("%s: %s hashes %q, want %q", c.name, f, got, want)
		}
		// Off disk is no edit for git.
		if wantChanged := strings.TrimPrefix(got, OffDiskPrefix) != head; slices.Contains(changed, f) != wantChanged {
			t.Errorf("%s: ChangedSince lists %s: %v, want %v (%v)", c.name, f, !wantChanged, wantChanged, changed)
		}
	}
}

func TestParseIndex(t *testing.T) {
	out := "H 100644 aaaa 0\ta.txt\x00S 100755 bbbb 0\tb c.sh\x00h 100644 cccc 0\td.txt\x00s 120000 dddd 0\tlink\x00H 160000 eeee 0\tsub\x00junk\x00"
	got := parseIndex(out)
	want := []struct {
		path          string
		flagged, skip bool
	}{
		{"a.txt", false, false}, {"b c.sh", true, true}, {"d.txt", true, false}, {"link", true, true}, {"sub", false, false},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %+v", got)
	}
	for i, w := range want {
		if got[i].path != w.path || got[i].flagged() != w.flagged || got[i].skipWorktree() != w.skip {
			t.Errorf("entry %d: %+v flagged=%v skip=%v, want %+v", i, got[i], got[i].flagged(), got[i].skipWorktree(), w)
		}
	}
}

// TestOldestBase: the commit changes are taken since when a state has no snapshot: the
// merge base of HEAD and the recorded commits that exist in the clone.
func TestOldestBase(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a")
	c1 := r.commit("one")
	r.write("a.txt", "b")
	c2 := r.commit("two")
	r.git("checkout", "-q", "-b", "side", c1)
	r.write("s.txt", "s")
	side := r.commit("side")
	r.git("checkout", "-q", "-")
	r.write("a.txt", "c")
	c3 := r.commit("three")
	const missing = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name    string
		commits []string
		want    string
	}{
		{"none", nil, ""},
		{"empty", []string{""}, ""},
		{"not in the clone", []string{missing}, ""},
		{"not a commit name", []string{"no such ref\nHEAD"}, ""},
		{"HEAD", []string{c3}, c3},
		{"one older", []string{c2}, c2},
		{"the oldest of several", []string{c3, c2, c1, c2}, c1},
		{"a missing one is left out", []string{missing, c2}, c2},
		{"a branch HEAD does not contain: where it forked", []string{side, c3}, c1},
	} {
		if got := r.repo.OldestBase(tc.commits); got != tc.want {
			t.Errorf("%s: OldestBase(%v) = %q, want %q", tc.name, tc.commits, got, tc.want)
		}
	}
}

// OldestBase folds a long list of commits over chunks: the result is that of one
// octopus merge base over all of them.
func TestOldestBaseChunks(t *testing.T) {
	r := newTestRepo(t)
	var commits []string
	for i := 0; i < oldestBaseChunk*2+3; i++ {
		r.write("a.txt", fmt.Sprint(i))
		commits = append(commits, r.commit(fmt.Sprint("c", i)))
	}
	for _, tc := range []struct {
		name    string
		commits []string
		want    string
	}{
		{"oldest first", commits, commits[0]},
		{"oldest last", append(slices.Clone(commits[1:]), commits[0]), commits[0]},
		{"oldest in a later chunk", append(slices.Clone(commits[oldestBaseChunk+5:]), commits[3]), commits[3]},
	} {
		if got := r.repo.OldestBase(tc.commits); got != tc.want {
			t.Errorf("%s: OldestBase = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TreeSnapshot hashes a commit's files as Snapshot hashes the clean working tree.
func TestTreeSnapshot(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a")
	r.write("dir/b.txt", "b")
	r.write("run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(r.repo.Root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(r.repo.Root, "link")); err != nil {
		t.Fatal(err)
	}
	c := r.commit("one")
	want, err := r.repo.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	r.write("a.txt", "changed later")
	got, err := r.repo.TreeSnapshot(c)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, want) {
		t.Errorf("TreeSnapshot = %v, want the clean snapshot %v", got, want)
	}
	if _, err := r.repo.TreeSnapshot("no-such-rev"); err == nil {
		t.Error("TreeSnapshot of a missing revision: no error")
	}
}
