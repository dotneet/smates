package trace

import (
	"reflect"
	"sort"
	"strings"
	"testing"

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
