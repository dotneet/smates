package trace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// V8 coverage written by traced Node processes and coverage tools sharing it.
//
// smates points NODE_V8_COVERAGE at V8Dir of its trace directory. Coverage tools that
// read the same variable keep working on top of it:
//
//   - c8 uses NODE_V8_COVERAGE as its temporary directory when it is set, so it writes
//     there and reads its report from smates' files; with --clean (its default) it
//     empties the directory when it starts. Every runner invocation therefore gets a
//     directory of its own (V8DirN), so one invocation never deletes another's files.
//   - A NODE_V8_COVERAGE the user set (to collect raw coverage themselves) is replaced
//     while recording; the coverage files are copied there afterwards (CopyV8).
//   - Tools driving the V8 profiler through node:inspector (vitest's v8 provider, jest's
//     v8 coverage) are handled by the Node shim.

// V8DirN is the coverage directory of the n-th runner invocation of a trace directory
// (V8Dir for the first).
func V8DirN(traceDir string, n int) string {
	if n == 0 {
		return V8Dir(traceDir)
	}
	return fmt.Sprintf("%s-%d", V8Dir(traceDir), n)
}

// v8Dirs lists the coverage directories of a trace directory.
func v8Dirs(traceDir string) []string {
	dirs := []string{V8Dir(traceDir)}
	more, _ := filepath.Glob(V8Dir(traceDir) + "-*")
	return append(dirs, more...)
}

// SeparateV8 returns env with the coverage directory of invocation n (created) when env
// points NODE_V8_COVERAGE at V8Dir(traceDir); env unchanged otherwise.
func SeparateV8(env []string, traceDir string, n int) ([]string, error) {
	if n == 0 {
		return env, nil
	}
	key := "NODE_V8_COVERAGE=" + V8Dir(traceDir)
	for i, kv := range env {
		if kv != key {
			continue
		}
		dir := V8DirN(traceDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		out := append([]string{}, env...)
		out[i] = "NODE_V8_COVERAGE=" + dir
		return out, nil
	}
	return env, nil
}

// UserV8Dir returns the NODE_V8_COVERAGE directory the user set for a run (in base, the
// suite's environment, or else smates' own environment), resolved against dir, the
// directory the runner starts in; "" when none is set. The directory of an outer smates
// run tracing this one is not the user's.
func UserV8Dir(base []string, dir string) string {
	v := getenv(base, "NODE_V8_COVERAGE")
	if v == "" {
		return ""
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(dir, v)
	}
	if outer := getenv(base, "SMATES_TRACE_DIR"); outer != "" {
		if rel, err := filepath.Rel(outer, v); err == nil && !strings.HasPrefix(rel, "..") {
			return ""
		}
	}
	return filepath.Clean(v)
}

// CopyV8 copies the V8 coverage files of a trace directory into dst (created), where
// the user asked Node to write them. Existing files are never overwritten or removed.
func CopyV8(traceDir, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	var first error
	for _, d := range v8Dirs(traceDir) {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !v8FileRe.MatchString(e.Name()) {
				continue
			}
			if err := copyNew(filepath.Join(d, e.Name()), filepath.Join(dst, e.Name())); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func copyNew(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
