package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// envPath returns the PATH of a KEY=VALUE list (the last one: later entries win, as in
// MergeEnv); ok is false when the list sets none. Windows compares names without case
// (Path).
func envPath(env []string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, _ := strings.Cut(env[i], "=")
		if k == "PATH" || runtime.GOOS == "windows" && strings.EqualFold(k, "PATH") {
			return v, true
		}
	}
	return "", false
}

// lookPathIn resolves the command name the way a process whose PATH is path finds it,
// relative directories in it counting from dir: a name with a path separator is
// returned as it is (exec.Cmd resolves a relative one against its Dir), any other is
// looked up in path's directories. exec.LookPath would search smates' own PATH, which
// the suite's env: may replace, so that the command run would not be the one the
// suite's processes see (or none at all). A relative directory in a PATH the suite's
// env: sets counts from dir, as a shell started there finds commands: the suite asked
// for it (exec.LookPath refuses one, exec.ErrDot, against a "." smates' own PATH may
// hold by accident; that PATH is searched by exec.LookPath).
func lookPathIn(name, path, dir string) (string, error) {
	if name == "" || strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return name, nil
	}
	if path == os.Getenv("PATH") {
		return exec.LookPath(name) // smates' own PATH: exactly what exec.Command does
	}
	for _, d := range filepath.SplitList(path) {
		if d == "" {
			d = "." // POSIX: an empty entry is the current directory
		}
		cand := filepath.Join(d, name)
		if !filepath.IsAbs(d) {
			if dir == "" {
				continue
			}
			if p, err := exec.LookPath(filepath.Join(dir, cand)); err == nil {
				if abs, err := filepath.Abs(p); err == nil {
					return abs, nil
				}
				return p, nil
			}
			continue
		}
		// With a separator in it, exec.LookPath checks only that file (adding the
		// extensions of PATHEXT on Windows).
		if p, err := exec.LookPath(cand); err == nil {
			return p, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// commandIn is exec.CommandContext for argv run in dir with the complete environment
// env: argv[0] is found through env's PATH (lookPathIn) rather than smates' own, and
// argv[0] stays what the process sees as its name.
func commandIn(ctx context.Context, dir string, argv, env []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	name := argv[0]
	if path, ok := envPath(env); ok {
		p, err := lookPathIn(name, path, dir)
		if err != nil {
			return nil, err
		}
		name = p
	}
	cmd := exec.CommandContext(ctx, name, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Dir = dir
	cmd.Env = env
	return cmd, nil
}
