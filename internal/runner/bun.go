package runner

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/trace"
)

func init() { Register(bun{}) }

type bun struct{}

func (bun) Name() string { return "bun" }

func (bun) Defaults(repo *gitx.Repo, root string) Defaults {
	return Defaults{
		Tests:   []string{"**/*.{test,spec}." + jsTestExts, "**/*_{test,spec}." + jsTestExts},
		Exclude: jsExclude,
		Inputs:  jsLockInputs(root),
		Command: "bun test",
	}
}

func (bun) Units(s *Suite, testFiles []string) []string { return unitsAreFiles(testFiles) }

func (bun) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	if !rc.Record {
		start := time.Now()
		code, err := runBatches(ctx, rc, nil, rc.RelArgs("./"), nil)
		return &RunResult{ExitCode: code, Duration: time.Since(start)}, err
	}
	// Bun runs all files in one process and does not support NODE_V8_COVERAGE, so while
	// recording each file runs in its own process with lcov coverage (loaded modules)
	// and the preload shim (files read). The shim must load before the project's own
	// preloads (bunfig [test].preload), which may import node:fs first: smates passes a
	// copy of bunfig.toml with the shim prepended. Child processes get the shim through
	// BUN_OPTIONS (bun) and NODE_OPTIONS (node).
	shim := rc.Shims().Node
	env := trace.NodeEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
	env = append(env, "BUN_OPTIONS="+strings.TrimSpace(Getenv(suiteEnv(rc.Suite), "BUN_OPTIONS")+" --preload="+shim))
	if err := os.MkdirAll(trace.V8Dir(rc.TraceDir), 0o755); err != nil {
		return nil, err
	}
	cfg, cleanup, err := bunConfigWithShim(rc, shim)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	pre := []string{"--config=" + cfg}
	start := time.Now()
	// bun test runs files one after another; running them concurrently could make
	// tests that share ports, databases or directories collide.
	codes, err := isolatedRun(ctx, rc, rc.Units, 1, func(u string, i int) ([]string, []string, error) {
		cov := filepath.Join(rc.TraceDir, fmt.Sprintf("bun-%d", i))
		extra := append(append([]string{}, pre...), "--coverage", "--coverage-reporter=lcov", "--coverage-dir="+cov)
		argv, err := rc.CommandLine(extra, []string{"./" + relToSuite(rc.Suite, u)})
		return argv, env, err
	})
	worst := worstCode(codes)
	res := &RunResult{ExitCode: worst, Duration: time.Since(start)}
	if err != nil {
		return res, err
	}
	obs, warns, err := observe(rc, worst, traceOptions{unitsFromArgv: true})
	if err != nil {
		return res, err
	}
	for i, u := range rc.Units {
		o := obs[u]
		if o == nil {
			o = &Observation{Files: map[string]bool{}}
			obs[u] = o
		}
		files, err := readLcov(filepath.Join(rc.TraceDir, fmt.Sprintf("bun-%d", i), "lcov.info"), rc.Dir(), rc.Repo.Root)
		if err != nil {
			delete(obs, u) // no coverage: do not record a partial picture
			continue
		}
		for _, f := range files {
			o.AddFile(f)
		}
		o.AddFile(u)
		setUnitExit(o, codes[u])
	}
	res.Observations, res.Warnings = obs, warns
	return res, nil
}

var (
	tomlSection = regexp.MustCompile(`(?m)^\s*\[([^\]]+)\]\s*$`)
	tomlPreload = regexp.MustCompile(`(?ms)^[ \t]*preload[ \t]*=[ \t]*(\[.*?\]|"[^"\n]*"|'[^'\n]*')[ \t]*$`)
	tomlString  = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)
	// coverageThreshold = 0.9 or = { lines = 0.9, functions = 0.8 }
	tomlThreshold = regexp.MustCompile(`(?m)^[ \t]*coverageThreshold[ \t]*=[ \t]*(\{[^}]*\}|[^\n]*)[ \t]*$`)
)

// bunConfigWithShim writes a copy of the suite's bunfig.toml whose [test].preload starts
// with the shim (relative preload paths made absolute). It returns "" when the project
// has no bunfig.toml (then --preload is enough).
func bunConfigWithShim(rc *RunContext, shim string) (string, func(), error) {
	src := filepath.Join(rc.Dir(), "bunfig.toml")
	data, err := os.ReadFile(src)
	if err != nil && !os.IsNotExist(err) {
		return "", nil, err
	}
	text := string(data)
	abs := func(p string) string {
		if filepath.IsAbs(p) || !strings.HasPrefix(p, ".") {
			return p // absolute, or a package specifier
		}
		return filepath.Join(rc.Dir(), p)
	}
	list := func(value string) []string {
		var out []string
		for _, m := range tomlString.FindAllStringSubmatch(value, -1) {
			out = append(out, abs(m[1]+m[2]))
		}
		return out
	}
	render := func(items []string) string {
		q := make([]string, len(items))
		for i, it := range items {
			q[i] = strconv.Quote(it)
		}
		return "preload = [" + strings.Join(q, ", ") + "]"
	}
	// Locate the [test] section.
	secs := tomlSection.FindAllStringSubmatchIndex(text, -1)
	start, end := -1, len(text)
	for i, m := range secs {
		if strings.TrimSpace(text[m[2]:m[3]]) == "test" {
			start = m[1]
			if i+1 < len(secs) {
				end = secs[i+1][0]
			}
			break
		}
	}
	// Top-level preload paths move with the file too.
	top := len(text)
	if len(secs) > 0 {
		top = secs[0][0]
	}
	head := tomlPreload.ReplaceAllStringFunc(text[:top], func(m string) string {
		return render(list(m[strings.Index(m, "=")+1:]))
	})
	// Recording runs one file per process with coverage on: a project coverage
	// threshold would fail every one of them.
	const noThreshold = "coverageThreshold = 0"
	var out string
	switch {
	case start < 0:
		out = head + text[top:] + "\n[test]\n" + render([]string{shim}) + "\n" + noThreshold + "\n"
	default:
		section := tomlThreshold.ReplaceAllString(text[start:end], "")
		if loc := tomlPreload.FindStringIndex(section); loc != nil {
			m := section[loc[0]:loc[1]]
			section = section[:loc[0]] + render(append([]string{shim}, list(m[strings.Index(m, "=")+1:])...)) + "\n" + noThreshold + section[loc[1]:]
		} else {
			section = "\n" + render([]string{shim}) + "\n" + noThreshold + section
		}
		out = head + text[top:start] + section + text[end:]
	}
	f, err := os.CreateTemp(rc.TraceDir, "bunfig-*.toml")
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(out); err != nil {
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// readLcov returns repo-relative source files listed in an lcov report.
func readLcov(file, cwd, root string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		p, ok := strings.CutPrefix(sc.Text(), "SF:")
		if !ok {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if r, ok := pathutil.Rel(root, p); ok && !pathutil.IsVendored(r) {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
