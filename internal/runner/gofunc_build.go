package runner

// Build constraints of test files for granularity: func. A test function declared only
// in a test file this configuration does not compile (a //go:build linux file on
// darwin, a tag the suite does not pass) is not a unit: go test would run nothing for
// it, and a pass recorded for it here would hide it from a configuration that compiles
// it and shares the state.

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/gitx"
)

// buildInfo is what is known about one test file (absolute path), valid while its size
// and modification time and the build flags stay the same.
type buildInfo struct {
	stat        fileStat
	flags       string
	constrained bool // a GOOS/GOARCH file name or a //go:build line
	listed      bool // ignored is known (go list ran with these flags)
	ignored     bool // excluded by its build constraints
}

var buildInfos sync.Map // absolute path -> buildInfo

// excludedTestFiles returns the test files (repo-relative) of suite s that go list
// reports as excluded by build constraints in the suite's configuration (GOFLAGS and
// command flags, its environment). Only files that may be constrained are looked up,
// with one go list for all their packages; results are cached by file stat. When go
// list fails nothing is excluded.
func excludedTestFiles(s *Suite, testFiles []string) map[string]bool {
	if s.RepoRoot == "" {
		return nil
	}
	rc := &RunContext{Repo: &gitx.Repo{Root: s.RepoRoot}, Suite: s}
	env := MergeEnv(baseEnv(), suiteEnv(s), []string{"GOFLAGS="})
	flags := strings.Join(append(goListFlags(rc), Getenv(env, "GOOS"), Getenv(env, "GOARCH"), Getenv(env, "CGO_ENABLED")), "\x00")
	infos := map[string]buildInfo{}
	var dirs []string
	seenDir := map[string]bool{}
	for _, f := range testFiles {
		abs := filepath.Join(s.RepoRoot, filepath.FromSlash(f))
		fi, err := os.Stat(abs)
		if err != nil {
			continue
		}
		st := fileStat{fi.Size(), fi.ModTime()}
		info := buildInfo{stat: st, flags: flags}
		if v, ok := buildInfos.Load(abs); ok && v.(buildInfo).stat == st && time.Since(st.mod) > 2*time.Second {
			info = v.(buildInfo)
			if info.flags != flags {
				info = buildInfo{stat: st, flags: flags, constrained: info.constrained}
			}
		} else {
			info.constrained = mayBeConstrained(abs)
		}
		infos[abs] = info
		if info.constrained && !info.listed && !seenDir[filepath.Dir(abs)] {
			seenDir[filepath.Dir(abs)] = true
			dirs = append(dirs, filepath.Dir(abs))
		}
	}
	if len(dirs) > 0 {
		ignored, ok := goIgnoredFiles(rc, env, dirs)
		for abs, info := range infos {
			if info.constrained && !info.listed && seenDir[filepath.Dir(abs)] {
				info.listed, info.ignored = true, ok && ignored[abs]
				infos[abs] = info
			}
		}
	}
	out := map[string]bool{}
	for _, f := range testFiles {
		abs := filepath.Join(s.RepoRoot, filepath.FromSlash(f))
		info, ok := infos[abs]
		if !ok {
			continue
		}
		buildInfos.Store(abs, info)
		if info.ignored {
			out[f] = true
		}
	}
	return out
}

// goIgnoredFiles runs go list for the package directories dirs and returns the
// absolute paths of their files that build constraints exclude; ok is false when go
// list fails.
func goIgnoredFiles(rc *RunContext, env, dirs []string) (map[string]bool, bool) {
	args := append(append([]string{"list", "-e", "-find"}, goListFlags(rc)...), "-f", "{{.Dir}}{{range .IgnoredGoFiles}}|{{.}}{{end}}", "--")
	cmd := exec.Command("go", append(args, dirs...)...)
	cmd.Dir = rc.Dir()
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	ignored := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		for _, name := range parts[1:] {
			ignored[filepath.Join(parts[0], name)] = true
		}
	}
	return ignored, true
}

// mayBeConstrained reports whether a Go file may be excluded by build constraints: its
// name ends in a GOOS or GOARCH (x_linux_test.go, x_windows_amd64_test.go), or a
// //go:build or // +build line comes before its package clause.
func mayBeConstrained(abs string) bool {
	base := strings.TrimSuffix(filepath.Base(abs), ".go")
	if i := strings.IndexByte(base, '_'); i >= 0 {
		l := strings.Split(base[i+1:], "_")
		if n := len(l); n > 0 && l[n-1] == "test" {
			l = l[:n-1]
		}
		if n := len(l); n > 0 && (knownOS[l[n-1]] || knownArch[l[n-1]]) {
			return true
		}
	}
	f, err := os.Open(abs)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		switch {
		case bytes.HasPrefix(line, []byte("//go:build")) || bytes.HasPrefix(line, []byte("// +build")):
			return true
		case bytes.HasPrefix(line, []byte("package")):
			return false
		}
	}
	return false
}

// knownOS and knownArch are the GOOS and GOARCH values of file name constraints.
var (
	knownOS = map[string]bool{
		"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true, "illumos": true,
		"ios": true, "js": true, "linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true,
		"solaris": true, "wasip1": true, "windows": true, "zos": true,
	}
	knownArch = map[string]bool{
		"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true, "arm64be": true,
		"loong64": true, "mips": true, "mipsle": true, "mips64": true, "mips64le": true, "mips64p32": true,
		"mips64p32le": true, "ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
		"s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true,
	}
)
