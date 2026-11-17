package trace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type execCase struct {
	name  string
	cwd   string
	args  []string
	shell bool
	want  []string
}

func runExecCases(t *testing.T, root string, cases []execCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen := map[string]bool{}
			for _, k := range execDeps(c.args, c.shell, c.cwd, root, relCache(root)) {
				seen[k] = true
			}
			if got := keys(seen); !reflect.DeepEqual(got, c.want) {
				t.Errorf("execDeps(%q) in %s = %v, want %v", c.args, c.cwd, got, c.want)
			}
		})
	}
}

// Whatever the analysis cannot resolve may name any path: run outside the repository,
// the command may still reach into it through the unknown value, so the whole
// repository is recorded. Known commands outside the repository still record only the
// paths they name.
func TestExecDepsUnresolvedOutsideRepo(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "data.txt", "sub/data.txt", "fix/data.txt")
	tmp := t.TempDir()
	gone := root + "/scripts/gone.sh" // a script deleted before the traces are collected
	sh := func(name, code string, want ...string) execCase {
		if len(want) == 0 {
			want = nil
		}
		return execCase{name, tmp, []string{code}, true, want}
	}
	all := "./**"
	runExecCases(t, root, []execCase{
		// Unresolved: the whole repository.
		sh("variable in a reader's operand", `cat "$FIX/data.txt"`, all),
		sh("cd to a variable", `cd "$FIX" && cat data.txt`, all),
		sh("program in a variable", `$TOOL run`, all),
		sh("exported variable in sh -c", `F=$R/x; export F; sh -c 'cat "$F"'`, all),
		sh("here-document to a shell", "bash <<EOF\ncat data.txt\nEOF", all),
		sh("pipe to a shell", "echo make | sh", all),
		{"deleted script, argv", tmp, []string{"bash", gone}, false, []string{all, "scripts/gone.sh"}},
		sh("deleted script, shell code", "sh "+gone, all, "scripts/gone.sh"),
		sh("source of a variable", `. "$LIB"`, all),
		sh("input from a variable", `wc -l < "$1"`, all),
		sh("substitution after an unknown cd", `X=$(cd "$Y" && cat data.txt)`, all),
		sh("option value of an opaque program", `mytool --input="$X"`, all),
		sh("operand of an opaque program", `make -C "$DIR"`, all),
		sh("file list in a variable", `wc --files0-from="$L"`, all),
		sh("file list from stdin", `wc --files0-from=-`, all),
		sh("sed r of a variable", `sed "r $F" data.txt`, all),
		sh("launcher directory in a variable", `uv --directory "$D" run make`, all),
		sh("script path in a variable", `sh "$SCRIPT"`, all),
		{"sh -c without code", tmp, []string{"sh", "-c"}, false, []string{all}},
		sh("cd -", "cd -; cat x", all),
		sh("cd without operand", "cd; cat .profile", all),
		sh("cd ~", "cd ~ && cat .profile", all),
		sh("~/path", "cat ~/x", all),
		sh("~ in an assignment", "X=~/f; cat $X", all),
		sh("~user", "cat ~user/x", all),
		sh("eval and a space", "eval 'cat x'", all),
		sh("eval and a tab", "eval\t'cat x'", all),
		sh("eval and spaces after a separator", "true;eval   'cat x'", all),
		sh("too many words, unknown variable", "cat $V{1..2000}", all),
		sh("find -exec, too many words, unknown variable", "find "+root+"/sub "+root+"/fix -exec cat \"$V\" "+strings.Repeat("{} ", 1100)+`\;`, all, "fix/**", "sub/**"),
		sh("find -exec, too many words, known", "find "+root+"/sub "+root+"/fix -exec cat "+strings.Repeat("{} ", 1100)+`\;`, "fix/**", "sub/**"),
		sh("unterminated substitution", "cat $(ls", all),

		// Must keep: known commands outside the repository record only what they name.
		sh("program on PATH", "tool"),
		sh("reader of outside files", "cat /etc/hosts"),
		sh("name-only command with a variable", `echo "$X"`),
		sh("traced runtime with a variable", `node "$X"`),
		sh("cd to a known directory", "cd /nonexistent/x && make"),
		sh("resolved variable", `F=x; cat "$F"`),
		sh("quoted ~", `cat "~/x" '~/y'`),
		sh("words containing eval", "echo medieval x; evaluate y"),
		sh("HOME assigned", "HOME="+root+"/sub; cd ~; cat data.txt", "sub/data.txt"),
		sh("too many words, known variable", "for i in {1..40}; do :; done\nfor j in {1..40}; do :; done\ncat '"+root+"'/sub/$i$j", "sub/**"),
	})
}

// An unknown cd leaves the working directory unknown: the commands after it do not run
// in the old one. ~ is $HOME, not a directory named "~" in the working directory.
func TestExecDepsUnknownDirectory(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "data.txt", "sub/data.txt", "~/x")
	sub := filepath.Join(root, "sub")
	runExecCases(t, root, []execCase{
		{"cd to a variable", sub, []string{`cd "$X"; cat data.txt`}, true, []string{"./**"}},
		{"pushd to a variable", sub, []string{`pushd "$X"; cat data.txt`}, true, []string{"./**"}},
		{"cd -", sub, []string{"cd -; cat data.txt"}, true, []string{"./**"}},
		{"cd ~", root, []string{"cd ~; cat x"}, true, []string{"./**"}},
		// Must keep: a subshell confines the unknown directory; a quoted ~ is a name.
		{"cd to a variable in a subshell", sub, []string{`(cd "$X"; make); cat data.txt`}, true, []string{"./**", "sub/data.txt"}},
		{"quoted ~", root, []string{`cd "~"; cat x`}, true, []string{"~/x"}},
		{"~ inside a word", root, []string{"cat sub~ a~/x"}, true, []string{"a~/x?", "sub~?"}},
		// An unknown value may name a path outside the working directory ($X=../x).
		{"unresolved in a subdirectory", sub, []string{`cat "$X"`}, true, []string{"./**"}},
	})
}

// The repository root reached through a symlink, and its ancestors, cover the whole
// repository; a directory beside it does not.
func TestExecDepsRootAndAncestors(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "data.txt")
	tmp := t.TempDir()
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip(err)
	}
	parent := filepath.Dir(root)
	runExecCases(t, root, []execCase{
		{"opaque program in the root through a symlink", link, []string{"make"}, false, []string{"./**"}},
		{"find in the root through a symlink", link, []string{"find . -name x"}, true, []string{"./**"}},
		{"ls in the root through a symlink", link, []string{"ls"}, true, []string{"./"}},
		{"opaque program in an ancestor", parent, []string{"make"}, false, []string{"./**"}},
		{"opaque program in /", "/", []string{"make"}, false, []string{"./**"}},
		{"glob tree in an ancestor", tmp, []string{"cat " + parent + "/*/data.txt"}, true, []string{"./**", "data.txt"}},
		{"unresolved through a symlink", link, []string{`cat "$X"`}, true, []string{"./**"}},
		// Must not widen: a directory beside the repository.
		{"opaque program beside the repository", tmp, []string{"make"}, false, nil},
	})
}

// Words of an argv are no shell code, even after the analysis followed shell code (a
// find -exec running sh -c): a shell expanded no glob in them.
func TestExecDepsArgvAfterShellCode(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "other/f", "sub/")
	runExecCases(t, root, []execCase{
		{"argv", root, []string{"find", "other", "-exec", "sh", "-c", ":", ";", "-exec", "cat", "sub/*.log", ";"}, false, []string{"other/**"}},
		{"shell code", root, []string{`find other -exec sh -c : \; -exec cat sub/*.log \;`}, true, []string{"other/**", "sub/"}},
	})
}

func TestTreeKey(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "sub/")
	tmp := t.TempDir()
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip(err)
	}
	rel := relCache(root)
	for _, c := range []struct {
		dir     string
		unknown bool
		want    string
	}{
		{filepath.Join(root, "sub"), false, "sub/**"},
		{filepath.Join(root, "sub"), true, "sub/**"},
		{filepath.Join(link, "sub"), false, "sub/**"},
		{root, false, "./**"},
		{root + "/", false, "./**"},
		{link, false, "./**"},
		{filepath.Dir(root), false, "./**"},
		{"/", false, "./**"},
		{tmp, false, ""},
		{tmp, true, "./**"},
		{root + "-sibling", false, ""},
		{"", true, "./**"},
	} {
		if got := treeKey(c.dir, root, rel, c.unknown); got != c.want {
			t.Errorf("treeKey(%q, unknown=%v) = %q, want %q", c.dir, c.unknown, got, c.want)
		}
	}
}

// A command line the shim cut short may have named anything: outside the repository,
// the whole repository.
func TestCollectTruncatedOutsideRepo(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "a.txt", "sub/")
	td := t.TempDir()
	for _, c := range []struct {
		cwd  string
		want []string
	}{
		{"/definitely/outside", []string{"./**"}},
		{filepath.Dir(root), []string{"./**"}},
		// The words cut off may name a path outside the working directory.
		{filepath.Join(root, "sub"), []string{"./**"}},
	} {
		stream(t, td, 100, 0,
			ev{"k": "start", "pid": 100, "ppid": 1, "cwd": root, "t": 1000, "lang": "python"},
			ev{"k": "exec", "args": []string{"cat", "a.tx"}, "cwd": c.cwd, "trunc": true},
			ev{"k": "exit", "code": 0, "t": 1500},
		)
		procs, err := Collect(td, root)
		if err != nil {
			t.Fatal(err)
		}
		if len(procs) != 1 {
			t.Fatalf("procs %v", procs)
		}
		if got := keys(procs[0].Reads); !reflect.DeepEqual(got, c.want) {
			t.Errorf("truncated command line in %s: reads %v, want %v", c.cwd, got, c.want)
		}
	}
}
