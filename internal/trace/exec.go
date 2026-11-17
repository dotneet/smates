package trace

import (
	"strings"

	"github.com/dotneet/smates/internal/depkey"
)

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
//   - shell code is parsed and followed through cd, pushd/popd, source, variables
//     (also those export, local and declare assign), brace expansion, input
//     redirections, command substitutions (in the state where they occur), the bodies
//     of the functions it calls (in the caller's directory), a script's positional
//     parameters ($1, "$@"), unset and the code of traps (in every state after the
//     trap); where branches (if, case, && and ||, loops) merge, every directory and
//     value of each variable any of them may leave is kept;
//   - any other program may read anything below the directory it runs in, so the test
//     depends on that whole tree ("<dir>/**"), plus the files named on its command line
//     (the script itself, option values such as --config=cfg/app.ini, response files
//     @args.txt) and the program when it is named by path (./build, ../bin/tool).
//
// This over-approximates on purpose: an opaque command is exactly where a dependency
// could hide. Whatever the analysis cannot resolve (an unset variable, a command's
// output, an unknown cd, commands read from stdin, code that cannot be parsed) may name
// any path ("../x" too): the whole repository is recorded.
//
// The analysis is split by responsibility: exec_lex.go parses shell code into a syntax
// tree, exec_interp.go follows its state (cd, variables, functions, substitutions),
// exec_commands.go knows what commands read, exec_runtime.go which programs trace
// themselves, and exec_sink.go records the keys.

// execDeps infers dependencies of a child process. It returns repo-relative keys:
// plain paths (content), "dir/" (listing), "dir/**" (whole tree) and "path?" (existence).
func execDeps(args []string, shell bool, cwd, root string, rel func(string) (string, bool)) []string {
	return execDepsIn(args, shell, cwd, root, rel, nil)
}

// execDepsIn is execDeps for a child whose environment traces the runtimes in rt (nil:
// all of them); the Node, Bun and Python programs of the others are opaque.
func execDepsIn(args []string, shell bool, cwd, root string, rel func(string) (string, bool), rt map[string]bool) (deps []string) {
	// A command the analysis cannot handle may read anything: a failure in it must not
	// stop the run, and fails toward running more (the whole repository).
	defer func() {
		if r := recover(); r != nil {
			deps = []string{depkey.Tree(".")}
		}
	}()
	a := &execAnalysis{rel: rel, root: root, funcs: map[string]bool{}, defs: map[string]*funcDef{}, analyzed: map[string]bool{},
		calling: map[string]bool{}, memo: map[string]callResult{}, relevant: map[string][]string{}, changes: map[string]bool{},
		leaves: map[string]bool{}, unsetFuncs: map[string]bool{}, rt: rt, sourced: map[string]*shList{}}
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
	leaves   map[string]bool       // whether a function's body may break or continue the caller's loops (mayLeave)
	// unsetFuncs are the functions an unset may have removed ("": any of them).
	unsetFuncs map[string]bool
	traps      []*trap               // the traps the shell being analyzed set
	lost       bool                  // the commands of a state no longer followed were recorded (shellState.unknown)
	rt         map[string]bool       // the runtimes the child's environment traces (nil: all)
	substs     []*shSubst            // the substitutions of the code parsed, by id (see placeholder)
	frames     []*frame              // the loops, functions and subshells being run
	zeros      []string              // $0 of the shells being analyzed ("" when unknown)
	sourced    map[string]*shList    // sourced files parsed, by path and $0
	stats      map[string]statResult // see stat
}
