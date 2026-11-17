package trace

import (
	"path/filepath"
	"testing"
)

// Branches merge: a command after if/elif/else, case, && and ||, loops, pipelines and
// function returns is analyzed for every working directory and value of each variable
// the paths reaching it may have.
func TestExecDepsControlFlow(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "a.json", "b.json", "c.json", "a/x.txt", "b/x.txt", "c/x.txt", "sub/a.txt", "other/a.txt",
		"a.txt", "data.txt", "list.txt", "sub/data.txt", "sub/x", "x", "y", "sub/y")
	writeScript(t, root, "lib/cd.sh", "cd sub\n")
	writeScript(t, root, "lib/set.sh", "CFG=b.json\n")
	sh := func(name, code string, want ...string) execCase {
		if len(want) == 0 {
			want = nil
		}
		return execCase{name, root, []string{code}, true, want}
	}
	runExecCases(t, root, []execCase{
		// if / elif / else.
		sh("if else", `if [ -f flag ]; then CFG=a.json; else CFG=b.json; fi; cat "$CFG"`, "a.json", "b.json", "flag?"),
		sh("if without else", `CFG=a.json; if [ -f flag ]; then CFG=b.json; fi; cat "$CFG"`, "a.json", "b.json", "flag?"),
		sh("elif", "if [ -f f1 ]; then C=a.json\nelif [ -f f2 ]; then C=b.json\nelse C=c.json\nfi\ncat $C", "a.json", "b.json", "c.json", "f1?", "f2?"),
		sh("if on one line, no separator before then", "if [ -f flag ]\nthen D=a\nelse D=b\nfi; cat $D/x.txt", "a/x.txt", "b/x.txt", "flag?"),
		sh("nested if", `D=a; if [ -f f1 ]; then if [ -f f2 ]; then D=b; fi; else D=c; fi; cat $D/x.txt`, "a/x.txt", "b/x.txt", "c/x.txt", "f1?", "f2?"),
		sh("cd in a branch", "if [ -f flag ]; then cd sub; fi; cat a.txt", "a.txt", "flag?", "sub/a.txt"),
		sh("if cd", "if cd sub; then cat x; fi; cat y", "sub/x", "sub/y", "y"),

		// && and ||.
		sh("&& ||", `[ -d a ] && D=a || D=b; cat $D/x.txt`, "a/", "a/x.txt", "b/x.txt"),
		sh("&&", `D=a; [ -f flag ] && D=b; cat $D/x.txt`, "a/x.txt", "b/x.txt", "flag?"),
		sh("||", `D=a; [ -f flag ] || D=b; cat $D/x.txt`, "a/x.txt", "b/x.txt", "flag?"),
		// Must keep: both may have failed.
		sh("cd || cd", "cd sub || cd other; cat a.txt", "a.txt", "other/a.txt", "sub/a.txt"),
		sh("cd || exit", "cd sub || exit 1; cat x", "sub/x"),
		sh("cd && on its own", "cd sub && cat x", "sub/x"),
		// Must keep: the cd may have failed and the script gone on.
		sh("cd && then more", "cd sub && cat x; cat y", "sub/x", "sub/y", "y"),
		sh("! negates", `D=a; ! [ -f flag ] || D=b; cat $D/x.txt`, "a/x.txt", "b/x.txt", "flag?"),

		// case.
		sh("case with a catch-all", `D=c; case "$1" in x) D=a;; *) D=b;; esac; cat $D/x.txt`, "a/x.txt", "b/x.txt"),
		sh("case without a catch-all", `D=c; case "$1" in x) D=a;; y) D=b;; esac; cat $D/x.txt`, "a/x.txt", "b/x.txt", "c/x.txt"),
		sh("case fall-through", `D=b; case "$1" in x) D=a;& y) cat $D/x.txt;; esac`, "a/x.txt", "b/x.txt"),
		sh("case ;;&", `D=b; case "$1" in x) D=a;;& *) cat $D/x.txt;; esac`, "a/x.txt", "b/x.txt"),
		sh("case on lines", "D=c\ncase \"$1\" in\n  x)\n    D=a\n    ;;\n  (y|z) D=b ;;\nesac\ncat $D/x.txt", "a/x.txt", "b/x.txt", "c/x.txt"),
		sh("quoted * is no catch-all", `D=c; case "$1" in "*") D=a;; esac; cat $D/x.txt`, "a/x.txt", "c/x.txt"),

		// Loops: the body may run any number of times.
		sh("while", `D=a; while [ -f flag ]; do D=b; done; cat $D/x.txt`, "a/x.txt", "b/x.txt", "flag?"),
		sh("until", `D=a; until [ -f flag ]; do D=b; done; cat $D/x.txt`, "a/x.txt", "b/x.txt", "flag?"),
		sh("for over a list", `for d in a b; do D=$d; done; cat $D/x.txt`, "a/x.txt", "b/x.txt"),
		sh("for over a possibly empty list", `D=c; for d in $L; do D=a; done; cat $D/x.txt`, "a/x.txt", "c/x.txt"),
		sh("break", "while true; do cd sub; break; cd ..; done; cat x", "sub/x"),
		sh("break 2", "while true; do for i in 1; do cd sub; break 2; done; done; cat x", "sub/x"),
		sh("continue", "D=a; for i in 1 2; do if [ -f flag ]; then continue; fi; D=b; done; cat $D/x.txt", "a/x.txt", "b/x.txt", "flag?"),
		sh("for (( ))", "D=a; for ((i=0; i<2; i++)); do D=b; done; cat $D/x.txt", "a/x.txt", "b/x.txt"),
		sh("program from a for list", "for t in cat make; do $t data.txt; done", "./**", "data.txt"),
		// Must keep: what changes on every iteration is unknown.
		sh("cd on every iteration", "while [ -f flag ]; do cd sub; done; cat x", "./**", "flag?",
			"sub/flag?", "sub/sub/flag?", "sub/sub/sub/flag?", "sub/sub/sub/sub/flag?"),
		sh("value growing on every iteration", "D=sub; while [ -f flag ]; do D=$D/sub; done; cat $D/x", "./**", "flag?"),
		sh("variable counted in a loop", "i=0; while [ -f flag ]; do i=$((i+1)); done; cat f$i", "./**", "flag?"),

		// Functions: return, exit, local variables.
		sh("return in a function", "f() { cd sub || return; cat x; }; f; cat y", "sub/x", "sub/y", "y"),
		sh("exit in a function", "f() { cd sub || exit 1; }; f; cat x", "sub/x"),
		sh("local variable", `D=a; f() { local D=b; cat $D/x.txt; }; f; cat $D/x.txt`, "a/x.txt", "b/x.txt"),
		// Must keep: a local may be declared on some paths only.
		sh("local unknown before", `f() { local D=a; }; f; cat $D/x.txt`, "./**"),

		// Pipelines: bash runs the last command in a subshell, zsh and ksh in the shell.
		sh("cd in a pipeline", "echo | cd sub; cat x", "sub/x", "x"),
		sh("cd before a pipe", "cd sub | cat; cat x", "x"),
		sh("background", "cd sub & cat x", "x"),

		// Sourced files run in the shell itself.
		sh("source changes the directory", ". lib/cd.sh; cat data.txt", "lib/cd.sh", "sub/data.txt"),
		sh("source assigns", "CFG=a.json; . ./lib/set.sh; cat $CFG", "b.json", "lib/set.sh"),
		sh("source in a branch", "CFG=a.json; [ -f flag ] && . ./lib/set.sh; cat $CFG", "a.json", "b.json", "flag?", "lib/set.sh"),

		// Assignments after a reserved word.
		sh("then X=", "if true; then X=data.txt; fi; cat $X", "data.txt"),
		sh("else X=", "if false; then :; else X=data.txt; fi; cat $X", "data.txt"),
		sh("do X=", "for i in 1; do X=data.txt; done; cat $X", "data.txt"),
		sh("{ X= }", "{ X=data.txt; }; cat $X", "data.txt"),
		sh("then X= on its own line", "if true\nthen\n  X=data.txt\nfi\ncat $X", "data.txt"),
		sh("! X=", "! X=data.txt; cat $X", "data.txt"),
		sh("assignment and redirection", "X=list.txt; X=data.txt >/dev/null; cat $X", "data.txt"),
		sh("X+=", "X=data; X+=.txt; cat $X", "data.txt"),
		sh("X+= unknown", "X+=.txt; cat $X", "./**"),
		sh("array element", "X=data.txt; X[1]=y; cat $X", "./**"),

		// Unknown values reached through a branch.
		sh("cd to an unknown directory in a branch", `if [ -f flag ]; then cd "$X"; fi; cat data.txt`, "./**", "flag?"),
		sh("eval in a branch", `[ -f flag ] && eval "$CMD"; cat data.txt`, "./**", "flag?"),
		sh("read in a branch", `D=a; [ -f flag ] && read D; cat $D/x.txt`, "./**", "flag?"),
	})
}

// Command substitutions run in the state of the shell where they occur, and their
// commands are analyzed there.
func TestExecDepsSubstitutionState(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "sub/v.txt", "v.txt", "shared/x", "data.txt", "sub/data.txt", "sub/list.txt", "pkg/data.txt")
	pkg := filepath.Join(root, "pkg")
	sh := func(name, code string, want ...string) execCase {
		if len(want) == 0 {
			want = nil
		}
		return execCase{name, root, []string{code}, true, want}
	}
	runExecCases(t, root, []execCase{
		sh("after cd", "cd sub\nV=$(cat v.txt)", "sub/v.txt"),
		{"variable assigned before", pkg, []string{`D=../shared; V=$(cat "$D/x")`}, true, []string{"shared/x"}},
		{"variable assigned before, output run", pkg, []string{`D=../shared; $(cat "$D/x")`}, true, []string{"./**", "shared/x"}},
		sh("in the same assignment command", "D=sub V=$(cat $D/v.txt)", "sub/v.txt"),
		sh("in a command's argument", "cd sub; echo $(cat v.txt)", "sub/v.txt"),
		sh("nested", "cd sub; X=$(echo $(cat data.txt))", "sub/data.txt"),
		sh("in a for list", "cd sub; for f in $(cat list.txt); do :; done", "sub/list.txt"),
		sh("in a case word", "cd sub; case $(cat data.txt) in x) ;; esac", "sub/data.txt"),
		sh("in an if condition", "cd sub; if [ -n \"$(cat data.txt)\" ]; then :; fi", "sub/data.txt"),
		sh("in a here-document", "cd sub; cat <<EOF\n$(cat data.txt)\nEOF", "sub/data.txt"),
		sh("not in a quoted here-document", "cd sub; cat <<'EOF'\n$(cat data.txt)\nEOF"),
		sh("in an arithmetic expansion", "cd sub; n=$(( $(wc -l < data.txt) + 1 ))", "sub/data.txt"),
		sh("in a function body", "f() { V=$(cat data.txt); }\ncd sub\nf", "sub/data.txt"),
		sh("in a branch", "if [ -f flag ]; then cd sub; fi; V=$(cat data.txt)", "data.txt", "flag?", "sub/data.txt"),
		sh("backticks after cd", "cd sub; X=`cat data.txt`", "./**", "sub/data.txt"),
		sh("process substitution", "cd sub; diff <(sort data.txt) <(sort list.txt)", "sub/data.txt", "sub/list.txt"),
		sh("$(pwd) in a subshell", `(cd sub; cat "$(pwd)/data.txt"); cat "$(pwd)/data.txt"`, "data.txt", "sub/data.txt"),
		sh("$(cd dir && pwd)", `D="$(cd sub && pwd)"; cat "$D/data.txt"`, "sub/data.txt"),
		// Must keep: the output of other commands is unknown.
		sh("$(cd dir && pwd) then more", `D="$(cd sub && pwd; echo x)"; cat "$D/data.txt"`, "./**"),
		{"output in a subdirectory", pkg, []string{`cat "$(cat data.txt)"`}, true, []string{"./**", "pkg/data.txt"}},
	})
}

// Comments, quotes, escapes, here-documents and substitutions are lexed in one pass:
// a quote in a comment or a here-document, an escaped quote, a ")" in a substitution
// do not change how the rest of the script is read.
func TestExecDepsLexing(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "data.txt", "list.txt", "sub/data.txt", "a", "b/")
	sh := func(name, code string, want ...string) execCase {
		if len(want) == 0 {
			want = nil
		}
		return execCase{name, root, []string{code}, true, want}
	}
	runExecCases(t, root, []execCase{
		// Quotes and comments.
		sh("escaped quote in double quotes", `printf "a \" # b"; cat data.txt`, "data.txt"),
		sh("escaped single quote", `echo \' 'x # y'; cat data.txt`, "data.txt"),
		sh("ANSI-C quoting", `echo $'it\'s # x'; cat data.txt`, "data.txt"),
		sh("quote in a here-document", "cat <<EOF\ndon't\nEOF\ncat data.txt # it's\ncat list.txt", "data.txt", "list.txt"),
		sh("quote in a comment", "# don't\ncat data.txt\n# \"\ncat list.txt", "data.txt", "list.txt"),
		sh("comment after ;", "cat data.txt;# cat list.txt", "data.txt"),
		sh("# inside a word", "echo a#b; cat data.txt", "data.txt"),
		sh("quoted #", `echo "#" '#'; cat data.txt`, "data.txt"),
		sh("${#X} and $#", `X=a; echo ${#X} $#; cat data.txt`, "data.txt"),
		sh("substitution in a comment", "# $(cat list.txt)\ncat data.txt", "data.txt"),
		sh("substitution in single quotes", `echo '$(cat list.txt)'; cat data.txt`, "data.txt"),
		sh("escaped $ in double quotes", `cat "\$X"; cat data.txt`, "$X?", "data.txt"),
		sh("eval in a here-document", "cat <<EOF\neval x\nEOF\ncat data.txt", "data.txt"),

		// Substitutions end at their matching ")".
		sh("quoted ) in $( )", "n=$(grep -c ')' data.txt)\ncat list.txt", "data.txt", "list.txt"),
		sh("double-quoted ) in $( )", `n=$(echo "a)b"); cat data.txt`, "data.txt"),
		sh("case in $( )", "x=$(case y in y) cat data.txt;; esac); cat list.txt", "data.txt", "list.txt"),
		sh("subshell in $( )", "x=$( (cd sub; cat data.txt) ); cat data.txt", "data.txt", "sub/data.txt"),
		sh("$(( )) in a subshell", "(cd sub; m=$((n+1)); cat data.txt); cat data.txt", "data.txt", "sub/data.txt"),
		sh("$(( )) reads nothing", "n=$((1+2)); echo $n"),
		sh("(( )) reads nothing", "(( n = 1 << 2 )); echo $n"),
		sh("$(( )) with a substitution", "n=$(( $(cat data.txt) + 1 ))", "data.txt"),
		sh("comment in $( )", "x=$(cat data.txt # )\n); cat list.txt", "data.txt", "list.txt"),
		sh("array", "A=(a 'b c' $(cat data.txt)); cat list.txt", "data.txt", "list.txt"),

		// [[ ]]: file operators read; && || < > ( ) are part of the test.
		sh("[[ ]]", "[[ -f a && -d b ]] && cat data.txt", "a", "b/", "data.txt"),
		sh("[[ ]] comparison", "[[ $x < list.txt ]] && cat data.txt", "data.txt"),
		sh("[[ ]] grouping", "[[ ( -f missing ) || -n $x ]]; cat data.txt", "data.txt", "missing?"),
		sh("[[ ]] regex", "[[ $x =~ ^(a|b)$ ]]; cat data.txt", "data.txt"),

		// What cannot be parsed may run anything.
		sh("unterminated quote", `cat 'data.txt`, "./**", "data.txt"),
		sh("unterminated if", "if true; then cat data.txt", "./**", "data.txt"),
		sh("stray fi", "fi; cat data.txt", "./**", "data.txt"),
		sh("unterminated ${", "cat ${X", "./**"),

		// Field splitting: a value used unquoted is split.
		sh("value with blanks", `F="data.txt list.txt"; cat $F`, "data.txt", "data.txt list.txt?", "list.txt"),
		sh("quoted blanks are one word", `cat "data.txt list.txt"`, "data.txt list.txt?"),

		// trap code runs at exit.
		sh("trap code", "trap 'cat data.txt' EXIT", "data.txt"),
	})
}

// Unknown values and the assignments of declaration builtins.
func TestExecDepsUnknownValues(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "shared/x.txt", "pkg/x.txt", "list.txt")
	pkg := filepath.Join(root, "pkg")
	sh := func(name, code string, want ...string) execCase {
		if len(want) == 0 {
			want = nil
		}
		return execCase{name, pkg, []string{code}, true, want}
	}
	runExecCases(t, root, []execCase{
		// export, local, declare, typeset and readonly assign.
		sh("export", "export D=../shared; cat $D/x.txt", "shared/x.txt"),
		sh("local", "f() { local D=../shared; cat $D/x.txt; }; f", "shared/x.txt"),
		sh("declare", "declare -x D=../shared; cat $D/x.txt", "shared/x.txt"),
		sh("typeset", "typeset D=../shared; cat $D/x.txt", "shared/x.txt"),
		sh("readonly", "readonly D=../shared; cat $D/x.txt", "shared/x.txt"),
		sh("export of a known variable", "D=../shared; export D; cat $D/x.txt", "shared/x.txt"),
		// Must keep: an unknown value may name a path outside the working directory.
		sh("unknown leading variable", "cat $D/x.txt", "./**"),
		sh("unknown inner variable", "cat sub/$D/x.txt", "./**"),
		sh("export without a value", "export D; cat $D/x.txt", "./**"),
		sh("declare -a", "declare -a D=(x); cat $D/x.txt", "./**"),
		sh("read", "D=x; read D; cat $D", "./**"),
		sh("read from a file", "D=x; read -r D < ../list.txt; cat $D", "./**", "list.txt"),
		sh("getopts", "D=x; getopts ab D; cat $D", "./**"),
		sh("mapfile", "MAPFILE=x; mapfile < ../list.txt; cat $MAPFILE", "./**", "list.txt"),
		sh("printf -v", "D=x.txt; printf -v D '%s' y; cat $D", "./**"),
		sh("let", "D=1; let D=D+1; cat f$D", "./**"),
		sh("${X:=default}", "D=../shared; : ${D:=.}; cat $D/x.txt", "pkg/x.txt", "shared/x.txt"),
		sh("${X:=default} unknown", ": ${D:=../shared}; cat $D/x.txt", "./**"),
		sh("arithmetic assignment", "n=1; (( n++ )); cat f$n", "./**"),
		sh("command output", "D=$(cat ../list.txt); cat $D/x.txt", "./**", "list.txt"),
		sh("unknown program", "$TOOL x.txt", "./**"),
	})
}

// A sourced file runs in the shell sourcing it: $0 is that shell's script, while
// ${BASH_SOURCE[0]} names the sourced file.
func TestExecDepsSourcedScriptIdioms(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "scripts/data.txt", "lib/data.txt", "lib/own.txt")
	writeScript(t, root, "lib/paths.sh", "cat \"$(dirname \"$0\")/data.txt\"\ncat \"$(dirname \"${BASH_SOURCE[0]}\")/own.txt\"\n")
	writeScript(t, root, "scripts/main.sh", "#!/bin/bash\n. \"$(dirname \"$0\")/../lib/paths.sh\"\n")
	runExecCases(t, root, []execCase{
		{"sourced by a script", root, []string{"bash", "scripts/main.sh"}, false,
			[]string{"lib/own.txt", "lib/paths.sh", "scripts/data.txt", "scripts/main.sh"}},
		// Must keep: $0 of a -c string is unknown.
		{"sourced by a -c string", root, []string{". lib/paths.sh"}, true, []string{"./**", "lib/own.txt", "lib/paths.sh"}},
	})
}

// A panic in the analysis of a command does not stop the run: the command depends on
// the whole repository.
func TestExecDepsPanicFailsTowardRunningMore(t *testing.T) {
	rel := func(string) (string, bool) { panic("analysis bug") }
	got := execDeps([]string{"cat data.txt"}, true, "/repo", "/repo", rel)
	if len(got) != 1 || got[0] != "./**" {
		t.Errorf("execDeps after a panic = %v, want [./**]", got)
	}
}
