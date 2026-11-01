package trace

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/testutil"
)

// execKeys runs execDeps in root and returns the sorted, deduplicated keys.
func execKeys(root string, args []string, shell bool) []string {
	seen := map[string]bool{}
	for _, k := range execDeps(args, shell, root, root, relCache(root)) {
		seen[k] = true
	}
	return keys(seen)
}

func writeScript(t *testing.T, root, rel, body string) {
	t.Helper()
	testutil.WriteMode(t, root, rel, body, 0o755)
}

func TestExecDeps(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "data.txt", "list.txt", "other.txt", "sub/data.txt", "scripts/data.txt")
	writeScript(t, root, "lib/env.sh", "export A=1\n")
	writeScript(t, root, "scripts/cat.sh", "#!/bin/sh\ncat data.txt\n")
	writeScript(t, root, "scripts/nested.sh", "#!/bin/sh\nsh -c 'cat data.txt'\n")
	writeScript(t, root, "scripts/nested-make.sh", "#!/bin/bash\nbash -e -c \"make\"\n")
	writeScript(t, root, "scripts/run.sh", "#!/bin/bash\n"+
		`DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"`+"\n"+
		`cat "$DIR/data.txt"`+"\n"+
		`cd "$DIR"`+"\n"+
		"./build\n")
	writeScript(t, root, "scripts/toplevel.sh", "#!/bin/bash\n"+
		`ROOT="$(git rev-parse --show-toplevel)"`+"\n"+
		`cat "$ROOT/data.txt"`+"\n")
	writeScript(t, root, "scripts/src.sh", "#!/bin/sh\n. \"$(dirname \"$0\")/../lib/env.sh\"\n")

	cases := []struct {
		name  string
		args  []string
		shell bool
		want  []string
	}{
		// Shells: -c after options, combined options, nested -c strings.
		{"sh -c", []string{"sh", "-c", "cat data.txt"}, false, []string{"data.txt"}},
		{"bash -e -c", []string{"bash", "-e", "-c", "cat data.txt; make"}, false, []string{"./**", "data.txt"}},
		{"bash long options -c", []string{"/bin/bash", "--noprofile", "--norc", "-c", "cat data.txt"}, false, []string{"data.txt"}},
		{"bash -ec", []string{"bash", "-ec", "cat data.txt"}, false, []string{"data.txt"}},
		{"bash -o pipefail -c", []string{"bash", "-o", "pipefail", "-c", "cat data.txt"}, false, []string{"data.txt"}},
		{"-c positional args are not files", []string{"bash", "-c", "cat data.txt", "other.txt", "list.txt"}, false, []string{"data.txt"}},
		{"-c with unresolved positional", []string{"bash", "-c", `cat "$1"`, "_", "other.txt"}, false, []string{"./**"}},
		{"-c without text", []string{"bash", "-c"}, false, []string{"./**"}},
		{"sh -c in a shell string", []string{`sh -c 'cat data.txt'`}, true, []string{"data.txt"}},
		{"bash -e -c in a shell string", []string{`bash -e -c "cat data.txt"`}, true, []string{"data.txt"}},
		{"nested sh -c make", []string{`bash -c "sh -c 'make'"`}, true, []string{"./**"}},
		{"single-quoted $ in -c string", []string{`F=data.txt; sh -c 'cat $F'`}, true, []string{"./**"}},
		{"sh -c in a script", []string{"bash", "scripts/nested.sh"}, false, []string{"data.txt", "scripts/nested.sh"}},
		{"bash -e -c in a script", []string{"scripts/nested-make.sh"}, false, []string{"./**", "scripts/nested-make.sh"}},

		// Shells running script files.
		{"bash script", []string{"bash", "scripts/cat.sh"}, false, []string{"data.txt", "scripts/cat.sh"}},
		{"bash -o pipefail script", []string{"bash", "-o", "pipefail", "scripts/cat.sh"}, false, []string{"data.txt", "scripts/cat.sh"}},
		{"script arguments are not files", []string{"bash", "-e", "--", "scripts/cat.sh", "other.txt"}, false, []string{"data.txt", "scripts/cat.sh"}},
		{"sh < script", []string{"sh < scripts/cat.sh"}, true, []string{"data.txt", "scripts/cat.sh"}},
		{"commands from a pipe", []string{"echo make | sh"}, true, []string{"./**"}},
		{"sh -s", []string{"sh", "-s", "scripts/cat.sh"}, false, []string{"./**"}},
		{"unresolved script path", []string{`sh "$SCRIPT"`}, true, []string{"./**"}},

		// Command substitutions.
		{"$(pwd)", []string{`cat "$(pwd)/data.txt"`}, true, []string{"data.txt"}},
		{"$(pwd) after cd", []string{`cd sub; cat "$(pwd)/data.txt"`}, true, []string{"sub/**"}},
		{"$(cat list)", []string{`cat $(cat list.txt)`}, true, []string{"./**", "list.txt"}},
		{"backticks", []string{"cat `cat list.txt`"}, true, []string{"./**", "list.txt"}},
		{"$(basename) output", []string{`cat "$(basename data.txt)"`}, true, []string{"./**", "data.txt"}},
		{"for over $(ls)", []string{`for f in $(ls sub); do cat "sub/$f"; done`}, true, []string{"./**", "sub/"}},
		{"unknown command output", []string{`ROOT="$(git rev-parse --show-toplevel)"; cat "$ROOT/data.txt"; cd "$ROOT"; ./build`}, true, []string{"./**"}},
		{"BASH_SOURCE dirname idiom in a script", []string{"bash", "scripts/run.sh"}, false, []string{"scripts/**", "scripts/data.txt", "scripts/run.sh"}},
		{"BASH_SOURCE outside a script", []string{"bash", "-c", `DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"; cat "$DIR/data.txt"; cd "$DIR"; ./build`}, false, []string{"./**"}},
		{"unknown output in a script", []string{"scripts/toplevel.sh"}, false, []string{"./**", "scripts/toplevel.sh"}},
		{"source $(dirname $0)", []string{"sh", "scripts/src.sh"}, false, []string{"lib/env.sh", "scripts/src.sh"}},
		{"source unresolved", []string{`. "$(dirname "$0")/env.sh"`}, true, []string{"./**"}},

		// System information (platform.processor() runs uname -p in pytest-xdist workers).
		{"uname -p", []string{"uname", "-p"}, false, nil},
		{"$(uname -m)", []string{`ARCH="$(uname -m)"; cat data.txt`}, true, []string{"data.txt"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := execKeys(root, c.args, c.shell)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("execDeps(%q, shell=%v) = %v, want %v", c.args, c.shell, got, c.want)
			}
			for _, k := range got {
				if k == "_" || strings.HasPrefix(k, "_/") || strings.ContainsAny(k, "$`") || strings.Contains(k, "smates_subst") {
					t.Errorf("placeholder leaked into key %q", k)
				}
			}
		})
	}
}

// A command substitution never stands for a resolved path: whatever its output is
// used for is opaque.
func TestExecDepsSubstitutionUnresolved(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "_/data.txt", "data.txt")
	for _, cmd := range []string{
		`cd "$(dirname x)"; ./build`,
		`cat "$(dirname x)/data.txt"`,
		"cd `dirname x`; make",
	} {
		got := execKeys(root, []string{cmd}, true)
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"./**"}) {
			t.Errorf("%q: %v, want [./**]", cmd, got)
		}
	}
}

// Readers without a path operand read the working directory; paths that do not exist
// yet are existence dependencies; launchers are analyzed as the command they run;
// subshells scope cd; awk programs doing their own I/O are opaque.
func TestExecDepsOperands(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "data.txt", "list.txt", "script.py", "sub/data.txt")
	testutil.Write(t, root, "prog.awk", `{ getline line < "list.txt"; print line }`)
	writeScript(t, root, ".venv/bin/pytest", "#!"+root+"/.venv/bin/python\nimport sys\n")
	writeScript(t, root, ".venv/bin/ruff", "\x7fELF")
	writeScript(t, root, "node_modules/.bin/eslint", "#!/usr/bin/env node\nrequire('../eslint')\n")
	// A repository script that ends by exec'ing node is still a shell script.
	writeScript(t, root, "run.sh", "#!/bin/sh\ncat data.txt\nexec node app.js\n")
	writeScript(t, root, "tool", "#!/usr/bin/env -S python3 -u\nimport sys\n")
	touch(t, root, "lib/x.jq", "inc.sed", "f1", "f2", "expected/a.txt", "actual/a.txt", "other/f.txt")
	writeScript(t, root, "bin/tool", "\x7fELF")
	writeScript(t, root, "scripts/gen.sh", "#!/bin/sh\ncat /dev/null\n")
	testutil.Write(t, root, "nul.lst", "data.txt\x00sub/data.txt\x00")

	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		// No operand: the working directory.
		{"ls", "ls", []string{"./"}},
		{"ls -la", "ls -la", []string{"./"}},
		{"ls in a subdirectory", "cd sub; ls", []string{"sub/"}},
		{"ls -R", "ls -lR", []string{"./**"}},
		{"ls .", "ls .", []string{"./"}},
		{"rg pattern", "rg pattern", []string{"./**"}},
		{"rg -e pattern", "rg -e pattern -g '*.py'", []string{"./**"}},
		{"rg --files", "rg --files", []string{"./**"}},
		{"grep -r pattern", "grep -rn pattern", []string{"./**"}},
		{"grep pattern reads stdin", "grep pattern", nil},
		{"find -name", "find -name x", []string{"./**"}},
		{"find in a subdirectory", "cd sub; find . -name '*.txt'", []string{"sub/**"}},
		{"rg pattern path", "rg data sub", []string{"sub/**"}},
		{"grep -r pattern path", "grep -r --include=*.txt data sub", []string{"sub/**"}},
		{"ls -r reverses", "ls -r", []string{"./"}},
		{"find -newer", "find sub -newer list.txt -type f", []string{"list.txt", "sub/**"}},

		// Option arguments.
		{"grep --exclude-from", "grep --exclude-from list.txt -r data sub", []string{"list.txt", "sub/**"}},
		{"grep --label", "grep --label x data data.txt", []string{"data.txt"}},
		{"rg --ignore-file", "rg --ignore-file list.txt data sub", []string{"list.txt", "sub/**"}},
		{"rg -d", "rg -d 2 data sub", []string{"sub/**"}},
		{"rg --pre", "rg --pre ./conv data data.txt", []string{"./**", "data.txt"}},
		{"jq -L", "jq -L lib .x data.txt", []string{"data.txt", "lib/**"}},
		{"sort --key", "sort --key 2 data.txt", []string{"data.txt"}},
		{"BSD stat -f", "stat -f %z data.txt", []string{"data.txt"}},
		{"GNU stat -f", "stat -f data.txt", []string{"data.txt"}},
		{"[ ] after a file operator", `[ "$X" = -f ]`, nil},

		// Paths that do not exist yet.
		{"test -f missing", "test -f plugins/extra.py", []string{"plugins/extra.py?"}},
		{"[ -f missing ]", "[ -f x ]", []string{"x?"}},
		{"[ string comparison ]", `[ "$X" = yes ]`, nil},
		{"cat missing", "cat missing.txt", []string{"missing.txt?"}},
		{"grep pattern missing", "grep -e data missing.txt data.txt", []string{"data.txt", "missing.txt?"}},
		{"option arguments are not paths", "head -n 5 data.txt; cut -d, -f1 data.txt; sort -k2 -o out.txt data.txt", []string{"data.txt"}},
		{"jq filter", "jq -r .x data.txt missing.json", []string{"data.txt", "missing.json?"}},
		{"output redirection", "cat data.txt > out.txt", []string{"data.txt"}},
		{"glob without matches", "cat sub/*.log", []string{"sub/"}},
		{"glob", "cat *.txt", []string{"./", "data.txt", "list.txt"}},
		{"glob in a directory component", "cat */data.txt", []string{"./**", "sub/data.txt"}},
		{"glob of a runtime", "python -m pytest sub/*.txt", []string{"sub/", "sub/data.txt"}},
		{"quoted glob characters", `python -c 'print([1,2])'; python -m pytest "sub/data.txt::t[1]"; printf '[%s]\n' x`, nil},
		{"quoted glob of a reader", `tr '[:upper:]' '[:lower:]' < data.txt; cat 'sub/*.log'`, []string{"data.txt", "sub/*.log?"}},
		{"echo glob", "echo sub/*.log", nil},
		{"here-document", "cat <<EOF\nhello $X\nEOF\ncat data.txt", []string{"data.txt"}},
		{"here-document <<-", "cat <<-'END'\n\tmake\n\tEND\ncat data.txt", []string{"data.txt"}},
		{"here-string", "cat <<< x", nil},
		{"shell reading a here-document", "sh <<EOF\ncat data.txt\nEOF", []string{"./**"}},
		{"unterminated here-document", "cat <<EOF\nhello\ncat data.txt", []string{"./**", "data.txt"}},
		{"arithmetic <<", "cd sub\n(( n <<= 1 ))\n(( m = 1 << 2 ))\ncat ../data.txt", []string{"data.txt"}},
		{"quoted glob in a variable", `P='sub/*.txt'; cat $P; Q="sub/*.txt"; cat $Q`, []string{"sub/", "sub/data.txt"}},
		{"quoted glob in a for list", `for p in 'sub/*.txt'; do cat $p; done`, []string{"sub/", "sub/data.txt"}},
		{"line continuation in double quotes", "cat \"data\\\n.txt\"", []string{"data.txt"}},
		{"trailing backslash", `cat data.txt \`, []string{"data.txt"}},
		{"line continuation", "cat \\\ndata.txt", []string{"data.txt"}},
		{"opaque command, missing path outside its directory", "cd sub; ./build ../missing.cfg", []string{"missing.cfg?", "sub/**"}},
		{"opaque command, missing path inside its directory", "make test", []string{"./**"}},

		// Launchers.
		{"uvx native tool", "uvx ruff check sub", []string{"./**", "sub/**"}},
		{"uv run native tool", "uv run ruff check sub", []string{"./**", "sub/**"}},
		{"uv run options --", "uv run --with x --frozen -- ruff check sub", []string{"./**", "sub/**"}},
		{"uv tool run", "uv tool run ruff check", []string{"./**"}},
		{"uv --directory", "uv --directory sub run make", []string{"sub/**"}},
		{"uv run console script", "uv run pytest sub", nil},
		{"uv run python", "uv run --python 3.12 python -m pytest", nil},
		{"uv run -m", "uv run -m pytest", nil},
		{"uv run script.py", "uv run script.py", []string{"script.py"}},
		{"uv run reader", "uv run cat data.txt", []string{"data.txt"}},
		{"uv sync", "uv sync", []string{"./**"}},
		{"bunx node script", "bunx eslint sub", nil},
		{"bunx unknown tool", "bunx --bun tsc", []string{"./**"}},
		{"bun x reader", "bun x cat data.txt", []string{"data.txt"}},
		{"npx is traced", "npx eslint sub", nil},
		{"uv run shell script", "uv run ./run.sh", []string{"data.txt", "run.sh"}},
		{"bunx shell script", "bunx ./run.sh", []string{"data.txt", "run.sh"}},
		{"bun x shell script", "bun x ./run.sh", []string{"data.txt", "run.sh"}},
		{"uv run python script by path", "uv run ./tool data.txt", []string{"data.txt", "tool"}},

		// Subshells.
		{"cd in a subshell", "(cd sub && cat data.txt); cat data.txt", []string{"data.txt", "sub/data.txt"}},
		{"variables in a subshell", "F=data.txt; (F=sub/data.txt; cat $F); cat $F", []string{"data.txt", "sub/data.txt"}},
		{"brace group keeps cd", "{ cd sub; }; cat data.txt", []string{"sub/data.txt"}},

		// awk programs.
		{"awk", `awk '{print $1}' data.txt`, []string{"data.txt"}},
		{"awk getline", `awk '{ while ((getline l < "list.txt") > 0) print l }' data.txt`, []string{"./**", "data.txt"}},
		{"awk system", `awk 'BEGIN { system("make") }'`, []string{"./**"}},
		{"awk print >", `awk '{ print > "out.txt" }' data.txt`, []string{"./**", "data.txt"}},
		{"awk -f with getline", "awk -F, -f prog.awk data.txt", []string{"./**", "data.txt", "prog.awk"}},
		{"awk comparison", `awk -v n=1 '$1 > n { print }' data.txt`, []string{"data.txt"}},
		{"awk quoted braces", `awk '{print $1} NR > 1' data.txt`, []string{"data.txt"}},

		// Input redirections of builtins and compound commands.
		{"read < file", "read -r V < list.txt", []string{"list.txt"}},
		{"IFS= read < file", "IFS= read -r V < list.txt; echo $V", []string{"list.txt"}},
		{"export < file", "export X=1 < list.txt", []string{"list.txt"}},
		{"brace group < file", "{ cat data.txt; } < list.txt", []string{"data.txt", "list.txt"}},
		{"while read < file", `while read l; do echo "$l"; done < list.txt`, []string{"list.txt"}},

		// sed scripts: r and R read a file, e and s///e run commands.
		{"sed r", "sed '/x/r inc.sed' data.txt", []string{"data.txt", "inc.sed"}},
		{"sed -e R", "sed -e 's/a/b/' -e '1R inc.sed' data.txt", []string{"data.txt", "inc.sed"}},
		{"sed r in a block", "sed '/x/{r inc.sed\nd}' data.txt", []string{"data.txt", "inc.sed"}},
		{"sed w", "sed 's/a/b/w out.txt' data.txt", []string{"data.txt"}},
		{"sed e", "sed '1e make' data.txt", []string{"./**", "data.txt"}},
		{"sed s///e", "sed 's/x/make/e' data.txt", []string{"./**", "data.txt"}},
		{"sed commands", `sed -n '$!N;/a/,/b/p;1~2d;s|/usr|/opt|g;s/[/]/x/;y/ab/cd/' data.txt`, []string{"data.txt"}},

		// diff compares the files two directories have in common.
		{"diff of directories", "diff expected actual", []string{"actual/**", "expected/**"}},
		{"diff -q of directories", "diff -q expected actual", []string{"actual/**", "expected/**"}},

		// find -exec runs a command for the files found ({}), -execdir in their directory.
		{"find -exec", `find sub -exec cat other/f.txt \;`, []string{"other/f.txt", "sub/**"}},
		{"find -exec {}", "find sub -name '*.txt' -exec grep -rl x {} +", []string{"sub/**"}},
		{"find -execdir", `find sub -execdir cat ../list.txt \;`, []string{"list.txt", "sub/**"}},
		{"find -ok opaque", `find sub -ok rm {} \;`, []string{"./**", "sub/**"}},
		{"find -files0-from", "find -files0-from nul.lst -name x", []string{"data.txt", "nul.lst", "sub/data.txt"}},

		// Brace expansion (unquoted, before variables).
		{"brace expansion", "cat expected/{a,b}.txt", []string{"expected/a.txt", "expected/b.txt?"}},
		{"brace sequence", "cat f{1..2}", []string{"f1", "f2"}},
		{"for over a brace sequence", "for i in {1..2}; do cat f$i; done", []string{"f1", "f2"}},
		{"quoted braces", `cat '{a,b}' "{a,b}"; jq '{a: .b}' data.txt; echo {a,b}`, []string{"data.txt", "{a,b}?"}},
		{"braces in a variable", "P={a,b}; cat $P", []string{"{a,b}?"}},
		{"${VAR} in double quotes", `D=sub; cat "${D}/data.txt"`, []string{"sub/data.txt"}},

		// pushd/popd and cd options.
		{"pushd popd", "pushd sub; ls; popd; cat data.txt", []string{"data.txt", "sub/"}},
		{"pushd in a subshell", "(pushd sub; cat data.txt); popd; cat data.txt", []string{"data.txt", "sub/data.txt"}},
		{"cd -P", "cd -P sub; cat data.txt", []string{"sub/data.txt"}},

		// Shell functions run in the caller's state.
		{"function called after cd", "f() { cat data.txt; }\ncd sub\nf", []string{"sub/data.txt"}},
		{"cd in a called function", "f() { cd sub; }; f; cat data.txt", []string{"sub/data.txt"}},
		{"function never called", "f() { cd sub; cat data.txt; }; cat list.txt", []string{"list.txt", "sub/data.txt"}},
		{"function keyword", "function g { cat data.txt; }; if g; then :; fi", []string{"data.txt"}},
		{"recursive function", "f() { f; cat data.txt; }; f", []string{"data.txt"}},
		{"mutually recursive functions", "f() { g; }\ng() { f; cat data.txt; }\nf", []string{"data.txt"}},
		{"trap handler calling a later function", "cleanup() { helper; }\nhelper() { cat data.txt; }\ntrap cleanup EXIT", []string{"data.txt"}},
		{"uncalled function calling a later one", "a() { b; }\nb() { cat data.txt; }", []string{"data.txt"}},
		{"function called before its callee is defined", "main() { helper; }\nhelper() { cat data.txt; }\nmain \"$@\"", []string{"data.txt"}},
		{"function called through a variable", "cmd_test() { run; }\nrun() { cat list.txt; }\n\"cmd_$1\"", []string{"./**", "list.txt"}},
		{"function called in a subshell", "f() { cd sub; }\n(f)\ncat data.txt", []string{"data.txt"}},
		{"helper called many times", "f() { echo hi; }\n" + strings.Repeat("f\n", 250) + "cat data.txt", []string{"data.txt"}},
		{"calls in different directories", "f() { cat data.txt; }\nf\ncd sub\nf", []string{"data.txt", "sub/data.txt"}},
		{"calls with different variables", "f() { cat $F; }\nF=data.txt; f; F=list.txt; f", []string{"data.txt", "list.txt"}},
		{"same variable twice in a word", `for i in 1 2; do cat "f$i" "sub/$i$i"; done`, []string{"f1", "f2", "sub/11?", "sub/22?"}},
		{"case as an argument", "echo case foo in bar; cat data.txt;; cat list.txt", []string{"data.txt", "list.txt"}},

		// case items: patterns are no commands, their ")" closes no subshell.
		{"case", `case "$1" in a|b) cat data.txt;; (c) make;; esac; cat list.txt`, []string{"./**", "data.txt", "list.txt"}},
		{"case in a subshell", "(cd sub; case a in a) cat data.txt;; esac)", []string{"sub/data.txt"}},

		// Command substitutions run their commands.
		{"$(cd dir && cat)", "X=$(cd sub && cat data.txt)", []string{"sub/data.txt"}},
		{"$(cd unknown && pwd)", `X=$(cd "$Y" && pwd); echo $X`, nil},
		{"$(cd unknown && cat)", `X=$(cd "$Y" && cat data.txt)`, []string{"./**"}},
		{"$(<file)", "X=$(<list.txt)", []string{"list.txt"}},
		{"$(script)", "X=$(./run.sh)", []string{"data.txt", "run.sh"}},

		// A program named by its path is read.
		{"program by path", "./bin/tool", []string{"./**", "bin/tool"}},
		{"program outside its directory", "cd sub; ../bin/tool", []string{"bin/tool", "sub/**"}},
		{"missing program outside its directory", "cd sub; ../missing", []string{"missing?", "sub/**"}},

		// grep -d recurse, --files0-from.
		{"grep -d recurse", "grep -d recurse data sub", []string{"sub/**"}},
		{"grep --directories=recurse", "grep --directories=recurse data sub", []string{"sub/**"}},
		{"wc --files0-from", "wc --files0-from=nul.lst", []string{"data.txt", "nul.lst", "sub/data.txt"}},
		{"wc --files0-from stdin", "wc --files0-from=-", []string{"./**"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := execKeys(root, []string{c.cmd}, true); !reflect.DeepEqual(got, c.want) {
				t.Errorf("execDeps(%q) = %v, want %v", c.cmd, got, c.want)
			}
		})
	}
	// The same launcher as an argv (no shell).
	if got, want := execKeys(root, []string{"uvx", "ruff", "check", "sub"}, false), []string{"./**", "sub/**"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv uvx: %v, want %v", got, want)
	}
	// No shell expands an argv's globs: no listing.
	if got := execKeys(root, []string{"cat", "sub/*.log"}, false); got != nil {
		t.Errorf("argv glob: %v, want []", got)
	}

	// Argvs, and commands running elsewhere (in a subdirectory, outside the repository:
	// only the paths they name count).
	tmp := t.TempDir()
	for _, c := range []struct {
		name  string
		cwd   string
		args  []string
		shell bool
		want  []string
	}{
		{"argv sed r", root, []string{"sed", "/x/r inc.sed", "data.txt"}, false, []string{"data.txt", "inc.sed"}},
		{"argv find -exec", root, []string{"find", "sub", "-exec", "cat", "other/f.txt", ";"}, false, []string{"other/f.txt", "sub/**"}},
		{"argv diff of directories", root, []string{"diff", "expected", "actual"}, false, []string{"actual/**", "expected/**"}},
		{"argv grep -d recurse", root, []string{"grep", "-d", "recurse", "data", "sub"}, false, []string{"sub/**"}},
		{"argv bash -c braces", root, []string{"bash", "-c", "cat expected/{a,b}.txt"}, false, []string{"expected/a.txt", "expected/b.txt?"}},
		{"argv braces", root, []string{"cat", "expected/{a,b}.txt"}, false, []string{"expected/{a,b}.txt?"}},
		{"program outside cwd", root + "/sub", []string{"../bin/tool"}, false, []string{"bin/tool", "sub/**"}},
		{"program in the repository, cwd outside", tmp, []string{root + "/bin/tool"}, false, []string{"bin/tool"}},
		{"missing program, cwd outside", tmp, []string{root + "/bin/missing", "x"}, false, []string{"bin/missing?"}},
		{"program on PATH, cwd outside", tmp, []string{"tool"}, false, nil},
		{"$(script), cwd outside", tmp, []string{"X=$(" + root + "/scripts/gen.sh); echo $X"}, true, []string{"scripts/gen.sh"}},
		{"$(<file), cwd outside", tmp, []string{"X=$(<" + root + "/list.txt)"}, true, []string{"list.txt"}},
		{"read < file, cwd outside", tmp, []string{"read -r V < " + root + "/list.txt"}, true, []string{"list.txt"}},
	} {
		seen := map[string]bool{}
		for _, k := range execDeps(c.args, c.shell, c.cwd, root, relCache(root)) {
			seen[k] = true
		}
		if got := keys(seen); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: execDeps(%q) in %s = %v, want %v", c.name, c.args, c.cwd, got, c.want)
		}
	}
}

// Expansions and function calls are bounded: execDeps runs while a run's traces are
// collected, so a script must not make it hang or produce keys by the thousand.
func TestExecDepsBounded(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "x", "sub/x")
	hugeFn := "f() {\n" + strings.Repeat("  cat x; cd sub; cd ..\n", 5000) + "}\n" + strings.Repeat("f\n", 300)
	var chain strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&chain, "f%d() { f%d; cat x; }\n", i, i+1)
	}
	chain.WriteString("f300() { cat sub/x; }\nf0\n")
	for _, c := range []struct {
		name    string
		sh      string
		maxKeys int
		want    []string
	}{
		{"one variable used thrice", `for i in {1..1000}; do cat "$i/$i/$i"; done`, 1000, nil},
		{"two loops", "for i in {1..1000}; do for j in {1..1000}; do cat $i/$j; done; done", 1, []string{"./**"}},
		{"huge range", "cat {1..100000}", 1, []string{"./**"}},
		{"nested braces", "cat " + strings.Repeat("{a,b}", 40), 1, []string{"./**"}},
		{"find {} over many words", "find sub -exec cat " + strings.Repeat("{}{}{} ", 2000) + `\;`, 2, nil},
		{"big function called often", hugeFn, 1, []string{"x"}},
		{"call chain", chain.String(), 2, []string{"sub/x", "x"}},
	} {
		done := make(chan []string, 1)
		go func() { done <- execKeys(root, []string{c.sh}, true) }()
		select {
		case got := <-done:
			if len(got) > c.maxKeys || (c.want != nil && !reflect.DeepEqual(got, c.want)) {
				t.Errorf("%s: %d keys %.200v, want at most %d (%v)", c.name, len(got), got, c.maxKeys, c.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: execDeps did not return in 5s", c.name)
		}
	}
}

// Past the bound on calls, a function that may change the caller's state leaves it
// unknown: the commands after the call may read anything (the cd into sub is not
// followed, but sub/x is covered).
func TestExecDepsCallsPastBoundLoseState(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "x", "sub/x")
	var b strings.Builder
	b.WriteString("f() { echo $X >/dev/null; cd sub; }\n")
	for i := 0; i < 205; i++ {
		fmt.Fprintf(&b, "X=%d; f; cd ..\n", i)
	}
	b.WriteString("X=999; f; cat x\n")
	if got, want := execKeys(root, []string{b.String()}, true), []string{"./**"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cd in a function past the bound: %v, want %v", got, want)
	}
	// A function that changes nothing keeps the state followed, and a subshell confines
	// the loss to itself.
	b.Reset()
	b.WriteString("f() { echo $X >/dev/null; }\ng() { cd sub; }\n")
	for i := 0; i < 205; i++ {
		fmt.Fprintf(&b, "X=%d; f\n", i)
	}
	b.WriteString("(g; cat list)\ncd sub; cat x\n")
	if got, want := execKeys(root, []string{b.String()}, true), []string{"./**", "sub/x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("no change past the bound: %v, want %v", got, want)
	}
}

// A command with too many words to follow run outside the repository records the trees
// of the repository directories its absolute words name.
func TestExecDepsWordBoundOutsideRepo(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "1", "sub/f1")
	tmp := repoDir(t)
	for _, c := range []struct {
		sh   string
		want []string
	}{
		{"cat " + root + "/{1..2000}", []string{"./**"}},
		{"cat " + root + "/sub/f{1..2000} /etc/{1..2}", []string{"sub/**"}},
		{"for i in {1..40}; do :; done\nfor j in {1..40}; do :; done\ncat '" + root + "'/sub/$i$j", []string{"sub/**"}},
	} {
		seen := map[string]bool{}
		for _, k := range execDeps([]string{c.sh}, true, tmp, root, relCache(root)) {
			seen[k] = true
		}
		if got := keys(seen); !reflect.DeepEqual(got, c.want) {
			t.Errorf("execDeps(%.60q) outside the repository = %v, want %v", c.sh, got, c.want)
		}
	}
}
