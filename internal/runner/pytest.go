package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/trace"
)

func init() { Register(pytest{}) }

type pytest struct{}

func (pytest) Name() string { return "pytest" }

func (pytest) Defaults(repo *gitx.Repo, root string) Defaults {
	dir := filepath.Join(repo.Root, filepath.FromSlash(root))
	return Defaults{
		Tests:   []string{"**/test_*.py", "**/*_test.py"},
		Exclude: []string{"**/.venv/**", "**/venv/**", "**/site-packages/**", "**/node_modules/**", "**/build/**", "**/.tox/**", "**/.nox/**"},
		Inputs: []string{
			"pyproject.toml", "uv.lock", "setup.cfg", "setup.py", "pytest.ini", "tox.ini", "requirements*.txt", ".python-version",
			"/pyproject.toml", "/uv.lock", "/.python-version", "/requirements*.txt", "/pytest.ini", "/.tool-versions",
		},
		Command: pythonCommand(repo.Root, dir),
	}
}

// pythonCommand picks how to start pytest: uv when the project uses it, else a local
// virtualenv, else python3 from PATH.
func pythonCommand(repoRoot, dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if exists(filepath.Join(d, "uv.lock")) {
			return "uv run pytest"
		}
		if data, err := os.ReadFile(filepath.Join(d, "pyproject.toml")); err == nil && strings.Contains(string(data), "[tool.uv") {
			return "uv run pytest"
		}
		for _, venv := range []string{".venv", "venv"} {
			py := filepath.Join(d, venv, "bin", "python")
			if exists(py) {
				if r, err := filepath.Rel(dir, py); err == nil {
					return "./" + filepath.ToSlash(r) + " -m pytest"
				}
			}
		}
		if d == repoRoot || len(d) <= len(repoRoot) {
			break
		}
	}
	return "python3 -m pytest"
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (pytest) Units(s *Suite, testFiles []string) []string { return unitsAreFiles(testFiles) }

func (pytest) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	var env []string
	var extra []string
	if rc.Record {
		env = trace.PythonEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
		// One module failing to import would otherwise stop the whole session, leaving
		// every other test file without a result.
		if !hasArg(rc, "-x") && !hasArg(rc, "--exitfirst") && !hasArg(rc, "--continue-on-collection-errors") {
			extra = []string{"--continue-on-collection-errors"}
		}
	}
	start := time.Now()
	// Exit code 5 means "no tests collected": not a failure for an explicit selection.
	var ok []int
	if !rc.All {
		ok = []int{5}
	}
	code, err := runBatches(ctx, rc, extra, rc.RelArgs(""), env, ok...)
	res := &RunResult{ExitCode: code, Duration: time.Since(start)}
	if err != nil || !rc.Record {
		return res, err
	}
	res.Observations, res.Warnings, res.ReadsBy, err = observeAll(rc, code, traceOptions{})
	return res, err
}
