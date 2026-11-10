package trace

import (
	"path/filepath"
	"strings"
)

// CommandLineDeps infers the dependencies of a child process a JVM started, from the
// command line the JDK Flight Recorder records for it (jdk.ProcessStart): its
// arguments joined with spaces. The words are split on whitespace again, so an argument
// with a space in it is read as several; a shell's -c script is taken whole (the rest
// of the line). The keys are those execDeps returns.
func CommandLineDeps(command, cwd, root string) []string {
	args := strings.Fields(command)
	if len(args) == 0 {
		return nil
	}
	rel := RelFunc(root)
	if len(args) > 2 && shells[filepath.Base(args[0])] && args[1] == "-c" {
		i := strings.Index(command, " -c ")
		return execDeps([]string{strings.TrimSpace(command[i+len(" -c "):])}, true, cwd, root, rel)
	}
	return execDeps(args, false, cwd, root, rel)
}

// Files returns every dependency key the process recorded: the code it executed and
// what it read, listed, probed and ran.
func (p *Proc) Files() map[string]bool { return p.allFiles() }
