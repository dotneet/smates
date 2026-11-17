package trace

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// break and continue leave loops wherever they run: in a function the loop calls (also
// at a later call of the same function, also through another function), with a count
// the analysis does not know (any of the enclosing loops).
func TestExecDepsBreakContinue(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "important.txt", "a/x", "b/x", "c/x")
	sh := func(name, code string, want ...string) execCase {
		return execCase{name, root, []string{code}, true, want}
	}
	var past strings.Builder
	past.WriteString("stop() { echo $X >/dev/null; break; }\n")
	for i := 0; i < 205; i++ {
		fmt.Fprintf(&past, "for i in 1; do X=%d; stop; done\n", i)
	}
	past.WriteString("while true; do X=999; stop; done\ncat important.txt\n")
	runExecCases(t, root, []execCase{
		// A function breaking its caller's loop, called again in another loop.
		sh("break in a function called twice", "stop() { break; }\nfor x in a; do stop; done\nfor y in b; do stop; done\ncat important.txt", "important.txt"),
		sh("continue in a function called twice", "stop() { continue; }\nfor x in a; do stop; done\nD=a; for y in b c; do stop; D=b; done; cat $D/x", "a/x"),
		sh("conditional continue in a function", "stop() { [ -f flag ] && continue; }\nfor x in a; do stop; done\nD=a; for y in b c; do stop; D=b; done; cat $D/x",
			"a/x", "b/x", "flag?"),
		sh("function defined in a loop, called in another", "for x in a; do f() { break; }; f; done; while true; do f; done; cat important.txt", "important.txt"),
		sh("break through two functions", "g() { break; }; f() { g; }\nfor x in a; do f; done\nwhile true; do f; done\ncat important.txt", "important.txt"),
		sh("return in a loop of a function", "f() { while true; do return; done; }; f; f; cat important.txt", "important.txt"),
		// Must keep: past the bound on calls, a function that may break leaves the loops
		// it is called in, in a state no longer followed.
		sh("break in a function past the bound on calls", past.String(), "./**"),

		// break N and continue N.
		sh("break with a known count", "n=2; while true; do while true; do break $n; done; done; cat important.txt", "important.txt"),
		sh("break 2", "while true; do while true; do break 2; done; done; cat important.txt", "important.txt"),
		sh("break with a count past the loops", "while true; do break 5; done; cat important.txt", "important.txt"),
		// Must keep: an unknown count may leave any of the loops.
		sh("break with an unknown count", "while true; do while true; do break $N; done; done; cat important.txt", "important.txt"),
		sh("break with a substitution", "while true; do while true; do break $(echo 2); done; done; cat important.txt", "important.txt"),
		sh("continue with an unknown count", "D=a; while [ -f flag ]; do for i in 1; do D=b; continue $N; done; D=c; done; cat $D/x",
			"a/x", "b/x", "c/x", "flag?"),
		sh("break outside a loop", "break; cat important.txt", "important.txt"),
	})
}

// A command whose variables have more value combinations than are followed one at a
// time (maxAlts) has every value merged into one command line: where one value at a
// time matters (a cd, a function's parameters, the program, a program that is not a
// mere reader), the command may read anything.
func TestExecDepsMergedValues(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "a/x.txt", "b/x.txt")
	nine := "for a in a b c d e f g h i; do for b in 1 2 3 4 5 6 7 8 9; do "
	sh := func(name, code string, want ...string) execCase {
		return execCase{name, root, []string{code}, true, want}
	}
	var many []string
	for i := 1; i <= 70; i++ {
		many = append(many, fmt.Sprintf("f%d?", i))
	}
	slices.Sort(many)
	runExecCases(t, root, []execCase{
		sh("cd", nine+`(cd "$a$b" && cat cfg); done; done`, "./**"),
		sh("pushd", nine+`pushd "$a$b"; cat cfg; popd; done; done`, "./**"),
		sh("function parameter", `f() { cat "$1/cfg"; }; `+nine+`f "$a$b"; done; done`, "./**"),
		sh("one variable", `for d in d{1..70}; do (cd "$d" && make); done`, "./**"),
		sh("program", `for t in t{1..70}; do $t x.txt; done`, "./**"),
		sh("shell code", `for c in c{1..70}; do sh -c "$c"; done`, "./**"),
		sh("source", `for f in f{1..70}; do . "$f"; done`, "./**"),
		// Must keep: readers read every value; under the bound each combination is a
		// command of its own.
		{"reader", root, []string{`for i in {1..70}; do cat f$i; done`}, true, many},
		sh("echo", `for i in {1..70}; do echo "$i"; done`),
		sh("cd under the bound", `for d in a b; do (cd "$d" && cat x.txt); done`, "a/x.txt", "b/x.txt"),
		// Too many words: a cd or a call is lost.
		sh("cd with too many words", `cd {1..2000}; cat a/x.txt`, "./**"),
	})
}

// A glob in a cd target (written there, or held by a for loop's variable) is the
// directories it matches.
func TestExecDepsGlobDirectory(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "pk/a/cfg", "pk/b/x", "pk/file")
	sh := func(name, code string, want ...string) execCase {
		return execCase{name, root, []string{code}, true, want}
	}
	runExecCases(t, root, []execCase{
		sh("for over a glob, make", `for d in pk/*/; do (cd "$d" && make); done`, "pk/", "pk/a/**", "pk/b/**"),
		sh("for over a glob, cat", `for d in pk/*/; do (cd "$d" && cat cfg); done`, "pk/", "pk/a/cfg", "pk/b/cfg?"),
		sh("cd to a glob", `cd pk/*; cat cfg`, "pk/", "pk/a/cfg", "pk/b/cfg?"),
		sh("pushd to a glob", `pushd pk/a*; cat cfg`, "pk/", "pk/a/cfg"),
		// Must keep: no match leaves the directory (the cd fails); a quoted pattern is a
		// name.
		sh("glob without a match", `cd pk/z*/ && cat cfg`, "cfg?", "pk/"),
		sh("quoted glob", `cd 'pk/*' && cat cfg`, "pk/*/cfg?"),
	})
}

// command NAME and builtin NAME do not call a shell function NAME.
func TestExecDepsCommandBypassesFunctions(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "data.txt", "sub/x")
	sh := func(name, code string, want ...string) execCase {
		return execCase{name, root, []string{code}, true, want}
	}
	runExecCases(t, root, []execCase{
		sh("wrapper of a program", `make() { command make "$@"; }; make`, "./**"),
		sh("command skips the function", `cat() { :; }; command cat data.txt`, "data.txt"),
		sh("command -p", `cat() { :; }; command -p cat data.txt`, "data.txt"),
		sh("builtin cd", `cd() { builtin cd "$@"; }; cd sub; cat x`, "sub/x"),
		sh("wrapper of ls", `ls() { command ls -la "$@"; }; ls sub`, "sub/"),
		sh("command -v", `command -v make`),
		sh("function called", `f() { cat data.txt; }; f`, "data.txt"),
	})
}

// Parsing and analysis are bounded: input making them repeat work finishes quickly and
// may read anything.
func TestExecDepsWorkBounded(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "x")
	for _, c := range []struct {
		name string
		sh   string
		all  bool // ./** is recorded
	}{
		{"nested unterminated $((", "echo " + strings.Repeat("$((", 24), true},
		// Each substitution's output runs as a program: unknown.
		{"nested $(( that are substitutions", "echo " + strings.Repeat("$((cd a && ", 22) + "pwd" + strings.Repeat(") )", 22), true},
		{"many (", strings.Repeat("(", 80000), true},
		{"many unterminated here-documents", strings.Repeat("cat <<E\n", 20000), true},
		{"nested loops changing state", strings.Repeat(`while [ -f f ]; do D=$D/x; `, 10) + "cat $D" + strings.Repeat("; done", 10), true},
		{"valid nested arithmetic", "echo " + strings.Repeat("$(( 1 + ", 30) + "1" + strings.Repeat(" ))", 30) + "; cat x", false},
	} {
		start := time.Now()
		done := make(chan []string, 1)
		go func() { done <- execKeys(root, []string{c.sh}, true) }()
		select {
		case got := <-done:
			if d := time.Since(start); d > time.Second {
				t.Errorf("%s: took %v", c.name, d)
			}
			if slices.Contains(got, "./**") != c.all {
				t.Errorf("%s: %.200v, want ./** %v", c.name, got, c.all)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: execDeps did not return in 10s", c.name)
		}
	}
}
