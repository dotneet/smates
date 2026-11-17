package trace

import (
	"os"
	"path/filepath"
	"strings"
)

// tracedPrograms trace themselves (or only start programs that do). npm, npx, pnpm and
// yarn are Node programs: the commands they start go through child_process and are
// analyzed there. Native launchers (uv run, uvx, bunx) execute their command directly
// and are analyzed as the command they run (launched).
var tracedPrograms = map[string]bool{
	"node": true, "nodejs": true, "bun": true, "python": true, "python3": true, "pypy3": true,
	"npm": true, "npx": true, "pnpm": true, "yarn": true,
}

// runtimeOf names the runtime a traced program runs on: its shim must reach the child
// (exec events list the runtimes the child's environment traces) for the child to trace
// itself.
func runtimeOf(prog string) string {
	switch {
	case prog == "bun":
		return "bun"
	case strings.HasPrefix(prog, "python") || prog == "pypy3":
		return "python"
	}
	return "node"
}

// skipsSite reports whether the options of a Python command line (before its script,
// -c or -m) keep the interpreter from loading the shim: -I and -E ignore PYTHONPATH,
// -S skips the site module that imports sitecustomize.
func skipsSite(args []string) bool {
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "-" || w == "--" || !strings.HasPrefix(w, "-"):
			return false
		case strings.HasPrefix(w, "--"):
			if w == "--check-hash-based-pycs" {
				i++
			}
			continue
		}
		for j := 1; j < len(w); j++ {
			switch w[j] {
			case 'I', 'E', 'S':
				return true
			case 'c', 'm':
				return false // the rest is the code or the module
			case 'W', 'X':
				if j+1 == len(w) {
					i++ // the argument is the next word
				}
				j = len(w)
			}
		}
	}
	return false
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true}

// launcherOpts are the options of uv (global, run and tool run options) and bun that
// take a separate argument.
var launcherOpts = map[string]bool{
	"--directory": true, "--project": true, "--cache-dir": true, "--config-file": true, "--color": true,
	"--python": true, "-p": true, "--with": true, "-w": true, "--with-editable": true,
	"--with-requirements": true, "--from": true, "--package": true, "--extra": true, "--group": true,
	"--no-group": true, "--only-group": true, "--env-file": true, "--index": true, "--default-index": true,
	"--index-url": true, "-i": true, "--extra-index-url": true, "--find-links": true, "-f": true,
	"--index-strategy": true, "--keyring-provider": true, "--resolution": true, "--prerelease": true,
	"--exclude-newer": true, "--link-mode": true, "--refresh-package": true, "--reinstall-package": true,
	"--upgrade-package": true, "-P": true, "--config-setting": true, "-C": true,
	"--no-binary-package": true, "--no-build-package": true, "--allow-insecure-host": true,
	"--python-preference": true, "--python-platform": true, "--cwd": true,
}

// launched returns the command a native launcher runs (uv run, uv tool run, uvx, bunx,
// bun x) and the directory it runs in, relative to the launcher's ("" for the same);
// ok is false when seg does not start one. uv run --module and --script run Python.
func launched(seg []string) (cmd []string, dir string, ok bool) {
	prog, args := filepath.Base(seg[0]), seg[1:]
	py := ""
	// options skips the launcher options at the start of args.
	options := func() {
		for len(args) > 0 {
			w := args[0]
			if w == "--" {
				args = args[1:]
				return
			}
			if !strings.HasPrefix(w, "-") || w == "-" {
				return
			}
			args = args[1:]
			name, val, inline := strings.Cut(w, "=")
			switch {
			case prog == "uv" && (w == "-m" || w == "--module"):
				py = "-m"
			case prog == "uv" && (w == "-s" || w == "--script" || w == "--gui-script"):
				py = "script"
			case !launcherOpts[name]:
			case !inline && len(args) == 0:
				return
			default:
				if !inline {
					val, args = args[0], args[1:]
				}
				if name == "--directory" || name == "--cwd" {
					dir = filepath.Join(dir, literal(val)) // relative to the previous one, or absolute
					if filepath.IsAbs(val) {
						dir = literal(val)
					}
				}
			}
		}
	}
	switch prog {
	case "uvx", "bunx":
	case "uv", "bun":
		options()
		switch {
		case prog == "uv" && len(args) > 0 && args[0] == "run":
			args = args[1:]
		case prog == "uv" && len(args) > 1 && args[0] == "tool" && args[1] == "run":
			args = args[2:]
		case prog == "bun" && len(args) > 0 && args[0] == "x":
			args = args[1:]
		default:
			return nil, "", false
		}
	default:
		return nil, "", false
	}
	options()
	switch {
	case len(args) == 0:
	case py == "-m":
		args = append([]string{"python", "-m"}, args...)
	case py == "script":
		args = append([]string{"python"}, args...)
	}
	return args, dir, true
}

// consoleScript returns the runtime of the console script name in a .venv/bin or
// node_modules/.bin from dir up to the repository root (see scriptRuntime), or "" when
// there is none (a native tool, one the launcher downloads).
func (a *execAnalysis) consoleScript(name, dir string) string {
	for d := dir; ; {
		for _, c := range []string{filepath.Join(d, ".venv", "bin", name), filepath.Join(d, "node_modules", ".bin", name)} {
			if _, err := os.Stat(c); err == nil {
				return scriptRuntime(c, true)
			}
		}
		parent := filepath.Dir(d)
		if d == a.root || parent == d || !within(d, a.root) {
			return ""
		}
		d = parent
	}
}

// scriptRuntime returns the runtime ("python", "node", "bun") the #! line of the file p
// names, or "" (a native binary, a shell script, a missing file). With wrappers, the
// shell wrappers package managers install are recognized too: pnpm's node_modules/.bin
// shims and uv's trampoline for long interpreter paths.
func scriptRuntime(p string, wrappers bool) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	head := make([]byte, 1024)
	n, _ := f.Read(head)
	f.Close()
	text := string(head[:n])
	line, _, _ := strings.Cut(text, "\n")
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if !strings.HasPrefix(line, "#!") || len(fields) == 0 {
		return ""
	}
	interp := filepath.Base(fields[0])
	if interp == "env" {
		for _, w := range fields[1:] {
			if !strings.HasPrefix(w, "-") {
				interp = filepath.Base(w)
				break
			}
		}
	}
	switch {
	case strings.HasPrefix(interp, "python") || strings.HasPrefix(interp, "pypy"):
		return "python"
	case interp == "node" || interp == "nodejs":
		return "node"
	case interp == "bun":
		return "bun"
	case !wrappers || !shells[interp]:
		return ""
	case strings.Contains(text, "exec node ") || strings.Contains(text, `/node" `):
		return "node"
	case strings.Contains(text, "'''exec'") && strings.Contains(text, "python"):
		return "python"
	}
	return ""
}
