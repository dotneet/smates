package trace

import (
	"testing"
)

// semanticsRepo makes a repository with x, a/x, imp.txt, a/imp.txt, b/imp.txt and
// data.txt, and a builder of the cases of shell code run in it.
func semanticsRepo(t *testing.T) (root string, sh func(name, code string, want ...string) execCase) {
	root = repoDir(t)
	touch(t, root, "x", "a/x", "imp.txt", "a/imp.txt", "b/imp.txt", "data.txt")
	return root, func(name, code string, want ...string) execCase {
		if len(want) == 0 {
			want = nil
		}
		return execCase{name, root, []string{code}, true, want}
	}
}

// A negated pipeline (! cmd) may end either way, whatever the status of cmd: the
// commands after it run in the states where cmd succeeded and where it failed.
func TestExecDepsNegatedPipeline(t *testing.T) {
	root, sh := semanticsRepo(t)
	runExecCases(t, root, []execCase{
		// Must keep a/x: the cd succeeds when a exists.
		sh("negated cd", "! cd a; cat x", "a/x", "x"),
		sh("negated cd with set -e", "set -e; ! cd a; cat x", "a/x", "x"),
		sh("negated cd in a function", "f() { ! cd a; }; f; cat x", "a/x", "x"),
		sh("negated cd in a group", "{ ! cd a; }; cat x", "a/x", "x"),
		sh("negated pipeline of several commands", "! echo | cd a; cat x", "a/x", "x"),
		// The status of the negation decides && and ||.
		sh("negated cd and", "! cd a && cat x", "x"),
		sh("negated cd or", "! cd a || cat x", "a/x"),
		sh("negated loop condition", "while ! cd a; do :; done; cat x", "a/x"),
		sh("double negation", "! ! cd a; cat x", "a/x"),
		sh("no negation", "cd a; cat x", "a/x"),
	})
}

// $'...' strings decode their backslash escapes as bash does; an escape the analysis
// cannot decode exactly makes the word unresolved.
func TestExecDepsANSICQuoting(t *testing.T) {
	root, sh := semanticsRepo(t)
	runExecCases(t, root, []execCase{
		sh("hexadecimal", `cat $'im\x70.txt'`, "imp.txt"),
		sh("one hexadecimal digit", `cat $'a\x2f'imp.txt`, "a/imp.txt"),
		sh("octal", `cat $'im\160.txt'`, "imp.txt"),
		sh("octal slash", `cat $'a\057imp.txt'`, "a/imp.txt"),
		sh("\\u", "cat $'im\\u0070.txt'", "imp.txt"),
		sh("\\U", `cat $'im\U00000070.txt'`, "imp.txt"),
		sh("\\U with fewer digits", `cat $'im\U70.txt'`, "imp.txt"),
		sh("tab", `cat $'a\tb'`, "a\tb?"),
		sh("newline", `cat $'a\nb'`, "a\nb?"),
		sh("backslash", `cat $'a\\b'`, `a\b?`),
		sh("single quote", `cat $'it\'s'`, "it's?"),
		sh("double quote", `cat $'q\"q'`, `q"q?`),
		sh("question mark", `cat $'imp\?.txt'`, "imp?.txt?"),
		sh("backspace", `cat $'a\bb'`, "a\bb?"),
		sh("escape", `cat $'\e'`, "\x1b?"),
		sh("\\E", `cat $'\E'`, "\x1b?"),
		sh("form feed, vertical tab, carriage return", `cat $'\f\v\r'`, "\f\v\r?"),
		sh("control character", `cat $'\cH'`, "\b?"),
		sh("lower-case control character", `cat $'\ch'`, "\b?"),
		// Decoded characters are literal: no glob, no variable.
		sh("decoded star", `cat $'\x2a'`, "*?"),
		sh("decoded dollar", `cat $'\x24HOME'`, "$HOME?"),
		sh("in double quotes, not decoded", `cat "$'im\x70.txt'"`, `$'im\x70.txt'?`),
		// Not decoded exactly: may name anything.
		sh("unknown escape", `cat $'imp\z.txt'`, "./**"),
		sh("\\x without digits", `cat $'\x'`, "./**"),
		sh("\\u without digits", `cat $'\u'`, "./**"),
		sh("NUL", `cat $'imp.txt\0'`, "./**"),
		sh("octal past a byte", `cat $'\777'`, "./**"),
		sh("beyond ASCII", "cat $'imp\\u00e9.txt'", "./**"),
		sh("bell, a mark of the words", `cat $'\a'`, "./**"),
		sh("control character of a mark", `cat $'\cA'`, "./**"),
		sh("unknown control character", `cat $'\c1'`, "./**"),
	})
}

// declare, typeset and local options giving a variable an attribute that changes the
// values assigned to it (-l, -u, -c, -i) make it unknown, at later assignments too; a
// reference (-n) may assign any variable.
func TestExecDepsDeclareAttributes(t *testing.T) {
	root, sh := semanticsRepo(t)
	runExecCases(t, root, []execCase{
		sh("lower case", "declare -l X=IMP.TXT; cat $X", "./**"),
		sh("lower case, later assignment", "declare -l X; X=IMP.TXT; cat $X", "./**"),
		sh("lower case, appended", "declare -l X; X=IMP; X+=.TXT; cat $X", "./**"),
		sh("upper case", "typeset -u X=imp.txt; cat $X", "./**"),
		sh("capitalized", "declare -c X=imp.txt; cat $X", "./**"),
		sh("local in a function", "f() { local -l X=IMP.TXT; cat $X; }; f", "./**"),
		sh("integer", "declare -i X=1+1; cat a/$X", "./**"),
		sh("integer, later assignment", "declare -i X; X=1+1; cat a/$X", "./**"),
		sh("option cluster", "declare -lx X=IMP.TXT; cat $X", "./**"),
		sh("separate options", "declare -x -l X=IMP.TXT; cat $X", "./**"),
		sh("unknown option", "declare -Z X=imp.txt; cat $X", "./**"),
		sh("attribute on one path", "if [ -f f ]; then declare -l X; fi; X=IMP.TXT; cat $X", "./**", "f?"),
		sh("attribute set in a function", "f() { declare -gl X; }; f; X=IMP.TXT; cat $X", "./**"),
		sh("attribute, then read", "declare -l X; read X; cat $X", "./**"),
		sh("attribute, then for", "declare -u X; for X in imp.txt; do cat $X; done", "./**"),
		// References.
		sh("reference assigned", "X=a; declare -n R=X; R=b; cat $X/imp.txt", "./**"),
		sh("reference read", "declare -n R=X; X=a; cat $R/imp.txt", "./**"),
		sh("reference read into", "X=a; declare -n R=X; read R; cat $X/imp.txt", "./**"),
		sh("reference as a loop variable", "X=a; declare -n R=X; for R in b; do :; done; cat $X/imp.txt", "./**"),
		sh("reference unset", "X=a/; declare -n R=X; unset R; cat ${X}imp.txt", "./**"),
		sh("reference assigned in a function", "f() { R=b; }; X=a; declare -n R=X; f; cat $X/imp.txt", "./**"),
		sh("reference, call not replayed", "f() { R=b/; }; declare -n R=X; f; X=a/; f; cat ${X}imp.txt", "./**"),
		sh("local reference", "f() { local -n R=X; R=b; }; X=a; f; cat $X/imp.txt", "./**"),
		// Attributes that keep the values.
		sh("export", "declare -x X=imp.txt; cat $X", "imp.txt"),
		sh("readonly option", "declare -r X=imp.txt; cat $X", "imp.txt"),
		sh("global", "declare -g X=imp.txt; cat $X", "imp.txt"),
		sh("export builtin", "export X=imp.txt; cat $X", "imp.txt"),
		sh("export -n is no reference", "export -n X=imp.txt; cat $X", "imp.txt"),
		sh("readonly builtin", "readonly X=imp.txt; cat $X", "imp.txt"),
		sh("removing an attribute", "declare +l X=imp.txt; cat $X", "imp.txt"),
		sh("functions", "declare -f X; X=imp.txt; cat $X", "imp.txt"),
	})
}

// A trap's code runs in any state the shell is in after the trap is set (at exit, at
// an error, at a signal), not only where it is set.
func TestExecDepsTrapState(t *testing.T) {
	root, sh := semanticsRepo(t)
	runExecCases(t, root, []execCase{
		// Must keep a/x: the shell exits in a.
		sh("exit trap, cd after", "trap 'cat x' EXIT; cd a", "a/x", "x"),
		sh("trap --", "trap -- 'cat x' EXIT; cd a", "a/x", "x"),
		sh("exit in a", "trap 'cat x' EXIT; cd a; exit 1", "a/x", "x"),
		sh("error trap", "trap 'cat x' ERR; cd a; false", "a/x", "x"),
		sh("signal trap", "trap 'cat x' INT TERM; cd a; sleep 1; cd ..", "a/x", "x"),
		sh("variable assigned after the trap", "trap 'cat $F' EXIT; F=data.txt", "./**"),
		sh("trap calling a function", "cleanup() { cat x; }; trap cleanup EXIT; cd a", "a/x", "x"),
		sh("function defined after the trap", "trap cleanup EXIT; cleanup() { cat x; }; cd a", "a/x", "x"),
		sh("trap set in a function", "f() { trap 'cat x' EXIT; }; f; cd a", "a/x", "x"),
		sh("trap set in a function never called", "setup() { trap 'cat x' EXIT; cd a; }", "a/x", "x"),
		sh("call replayed after the trap", "f() { cd a; exit 1; }; if [ -f c ]; then f; fi; trap 'cat x' EXIT; f", "a/x", "c?", "x"),
		sh("trap set in a loop", "for d in a b; do trap 'cat x' EXIT; done; cd a", "a/x", "x"),
		sh("trap of a subshell", "(trap 'cat x' EXIT; cd a); cat data.txt", "a/x", "data.txt", "x"),
		sh("trap of sh -c", `sh -c "trap 'cat x' EXIT; cd a"`, "a/x", "x"),
		sh("unresolved trap", `trap "$CLEANUP" EXIT`, "./**"),
		// A subshell does not run its parent's traps.
		sh("parent trap, subshell cd", "trap 'cat x' EXIT; (cd a; :)", "x"),
	})
}

// unset empties a variable; with -f, or without options, it may remove a function, and
// the name runs a program again.
func TestExecDepsUnset(t *testing.T) {
	root, sh := semanticsRepo(t)
	runExecCases(t, root, []execCase{
		sh("unset", "X=a/; unset X; cat ${X}imp.txt", "imp.txt"),
		sh("unset -v", "X=a/; unset -v X; cat ${X}imp.txt", "imp.txt"),
		sh("unset --", "X=a/; unset -- X; cat ${X}imp.txt", "imp.txt"),
		sh("several names", "X=a/; Y=b/; unset X Y; cat ${X}${Y}imp.txt", "imp.txt"),
		sh("in a function", "f() { unset X; }; X=a/; f; cat ${X}imp.txt", "imp.txt"),
		sh("on one path", "X=a/; if [ -f c ]; then unset X; fi; cat ${X}imp.txt", "a/imp.txt", "c?", "imp.txt"),
		sh("functions only", "X=a/; unset -f X; cat ${X}imp.txt", "a/imp.txt"),
		// Unknown afterwards: may name anything.
		sh("a reference itself", "X=a/; unset -n X; cat ${X}imp.txt", "./**"),
		sh("an unknown name", `X=a/; unset "$V"; cat ${X}imp.txt`, "./**"),
		sh("an array element", "X=a/; unset 'X[1]'; cat ${X}imp.txt", "./**"),
		sh("an attributed variable", "declare -l X=A; unset X; cat ${X}imp.txt", "./**"),
		// Must keep x: the program runs once the function is gone, or before it exists.
		sh("unset -f", "cat() { :; }; unset -f cat; cat x", "x"),
		sh("unset without options", "cat() { :; }; unset cat; cat x", "x"),
		sh("unset of an unknown name", `cat() { :; }; unset -f "$F"; cat x`, "x"),
		sh("function defined after the call", "cat x; cat() { :; }", "x"),
		sh("function called", "cat() { :; }; cat x"),
	})
}

// Names beyond ASCII (é, CJK letters) keep their bytes in every quoting form: a word
// re-encoded byte by byte would name a file that does not exist, and a change to the
// real file would select nothing. Must keep each read.
func TestExecDepsNonASCIINames(t *testing.T) {
	root := repoDir(t)
	touch(t, root, "données.txt", "dé/x", "データ.txt", "répertoire/データ.txt")
	sh := func(name, code string, want ...string) execCase {
		return execCase{name, root, []string{code}, true, want}
	}
	runExecCases(t, root, []execCase{
		sh("unquoted", `cat données.txt`, "données.txt"),
		sh("single quotes", `cat 'données.txt'`, "données.txt"),
		sh("double quotes", `cat "données.txt"`, "données.txt"),
		sh("backslash before the character", `cat donn\ées.txt`, "données.txt"),
		sh("ANSI-C quoting", `cat $'données.txt'`, "données.txt"),
		sh("ANSI-C byte escapes", `cat $'donn\xc3\xa9es.txt'`, "données.txt"),
		sh("ANSI-C octal escapes", `cat $'donn\303\251es.txt'`, "données.txt"),
		sh("CJK in double quotes", `cat "データ.txt"`, "データ.txt"),
		sh("CJK in ANSI-C quoting", `cat $'データ.txt'`, "データ.txt"),
		sh("cd to a quoted directory", `cd "dé"; cat "x"`, "dé/x"),
		sh("variable in double quotes", `D="répertoire"; cat "$D/データ.txt"`, "répertoire/データ.txt"),
		sh("here-document word", "cat \"données.txt\" <<'É'\nbody\nÉ", "données.txt"),
	})
}
