package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/dotneet/smates/internal/config"
)

// CheckRequirement checks one requires entry of suite s (see config.Requirement): nil
// when it is met. Anything that keeps the check from telling (a command that cannot
// start, an address that does not parse once expanded) is an error, so the requirement
// counts as unmet: its tests do not run and stay selected.
func CheckRequirement(ctx context.Context, repoRoot string, s *Suite, r config.Requirement) error {
	env := MergeEnv(baseEnv(), suiteEnv(s))
	lookup := func(name string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(env[i], "="); ok && k == name {
				return v
			}
		}
		return ""
	}
	dir := filepath.Join(repoRoot, filepath.FromSlash(s.Root))
	switch r.Kind() {
	case "tcp":
		return checkTCP(ctx, os.Expand(r.TCP, lookup), r.TimeoutOrDefault())
	case "cmd":
		return checkCmd(ctx, dir, env, r.Cmd, r.TimeoutOrDefault())
	case "env":
		if lookup(r.Env) == "" {
			return fmt.Errorf("$%s is not set", r.Env)
		}
		return nil
	case "file":
		return checkFile(dir, os.Expand(r.File, lookup))
	}
	return fmt.Errorf("unknown requirement %q", r.String())
}

// checkTCP dials addr until it accepts a connection or timeout passes (a server that
// is still starting refuses connections at first).
func checkTCP(ctx context.Context, addr string, timeout time.Duration) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	for {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("nothing accepts connections on %s (waited %s): %v", addr, timeout, dialCause(err))
			}
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// dialCause shortens a dial error to its cause ("connection refused").
func dialCause(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err
	}
	return err
}

// checkCmd runs command with the shell in dir; it must exit 0 within timeout. The
// whole process group is stopped when it takes longer.
func checkCmd(ctx context.Context, dir string, env []string, command string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := []string{"sh", "-c", command}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/C", command}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = withPWD(env, dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = time.Second // a background child keeping the output open
	err := runCmd(WithProcessGroup(ctx, 500*time.Millisecond), cmd)
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("did not finish within %s", timeout)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if last := lastLine(out.String()); last != "" {
		return fmt.Errorf("%v: %s", err, last)
	}
	return err
}

// checkFile reports whether pattern (relative to dir, or absolute) matches an existing
// file or directory.
func checkFile(dir, pattern string) error {
	pattern = filepath.FromSlash(pattern)
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(dir, pattern)
	}
	matches, err := doublestar.FilepathGlob(pattern)
	if err != nil {
		return fmt.Errorf("bad glob %q: %v", pattern, err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no file matches %s", pattern)
	}
	return nil
}
