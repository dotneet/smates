package trace

import "strings"

// Child processes a test starts are traced directly when they are Node, Bun or Python
// processes that load the shims (their environment carries them, as the exec event
// says, and Python is not run with -I, -E or -S). Everything else (shell scripts, grep,
// make, a compiled binary) is opaque, so dependencies are inferred from the command line:
//
//   - well-known commands that only read their file arguments (cat, grep, jq, ...)
//     depend on the repository files and directories named on the command line,
//     including shell globs (and the directories they search), on whether the named
//     paths that do not exist yet exist, and on the working directory when they name no
//     path (ls lists it, find, rg and grep -r search it);
//   - launchers (uv run, uvx, bunx) and the commands of find -exec are analyzed as the
//     command they run; sed scripts as the files their r commands read;
//   - shell code is followed through cd, pushd/popd, source, variables, for-loops, brace
//     expansion, case items, input redirections, command substitutions, the bodies of
//     the functions it calls (in the caller's directory) and a script's positional
//     parameters ($1, "$@");
//   - any other program may read anything below the directory it runs in, so the test
//     depends on that whole tree ("<dir>/**"), plus the files named on its command line
//     (the script itself, option values such as --config=cfg/app.ini, response files
//     @args.txt) and the program when it is named by path (./build, ../bin/tool).
//
// This over-approximates on purpose: an opaque command is exactly where a dependency
// could hide. Whatever the analysis cannot resolve (an unset variable, a command's
// output, an unknown cd, commands read from stdin) may name any path: below the working
// directory, or anywhere in the repository when that directory is outside it.
//
// The analysis is split by responsibility: exec_lex.go splits shell code into words,
// exec_interp.go follows its state (cd, variables, functions, substitutions),
// exec_commands.go knows what commands read, exec_runtime.go which programs trace
// themselves, and exec_sink.go records the keys.

// execDeps infers dependencies of a child process. It returns repo-relative keys:
// plain paths (content), "dir/" (listing), "dir/**" (whole tree) and "path?" (existence).
func execDeps(args []string, shell bool, cwd, root string, rel func(string) (string, bool)) []string {
	return execDepsIn(args, shell, cwd, root, rel, nil)
}

// execDepsIn is execDeps for a child whose environment traces the runtimes in rt (nil:
// all of them); the Node, Bun and Python programs of the others are opaque.
func execDepsIn(args []string, shell bool, cwd, root string, rel func(string) (string, bool), rt map[string]bool) []string {
	a := &execAnalysis{rel: rel, root: root, funcs: map[string]bool{}, defs: map[string]*funcDef{}, analyzed: map[string]bool{},
		calling: map[string]bool{}, memo: map[string]callResult{}, relevant: map[string][]string{}, changes: map[string]bool{}, rt: rt}
	if shell {
		a.shellText(strings.Join(args, " "), cwd, "", 0, nil)
	} else {
		a.command(args, cwd, 0, false)
	}
	a.flush()
	return a.out
}

type execAnalysis struct {
	rel  func(string) (string, bool)
	root string
	out  []string
	// Shell functions: the names the analyzed scripts define (funcDefRe), the bodies
	// found (defs, in order of definition in pending), and those analyzed at least once.
	funcs    map[string]bool
	defs     map[string]*funcDef
	pending  []string
	analyzed map[string]bool
	calling  map[string]bool       // functions whose body is being analyzed (recursion)
	calls    int                   // function bodies analyzed at a call (bounded)
	memo     map[string]callResult // calls analyzed, by callKey
	relevant map[string][]string   // the variables a function's body may use (callKey)
	changes  map[string]bool       // whether a function's body may change the caller's state (mayChange)
	lost     bool                  // the commands of a state no longer followed were recorded (shellState.unknown)
	rt       map[string]bool       // the runtimes the child's environment traces (nil: all)
}
