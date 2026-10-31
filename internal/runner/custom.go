package runner

import (
	"context"
	"os"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/trace"
)

func init() { Register(custom{}) }

// custom runs any command; selected test files replace "{tests}" (or are appended).
// While recording, Node and Python processes it starts are traced.
type custom struct{}

func (custom) Name() string { return "custom" }

func (custom) Defaults(repo *gitx.Repo, root string) Defaults { return Defaults{} }

func (custom) Units(s *Suite, testFiles []string) []string { return unitsAreFiles(testFiles) }

func (custom) Run(ctx context.Context, rc *RunContext) (*RunResult, error) {
	var env []string
	if rc.Record {
		if err := os.MkdirAll(trace.V8Dir(rc.TraceDir), 0o755); err != nil {
			return nil, err
		}
		base := suiteEnv(rc.Suite)
		env = append(trace.NodeEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), base), trace.PythonEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), base)[2:]...)
	}
	start := time.Now()
	code, err := runBatches(ctx, rc, nil, rc.RelArgs(""), env)
	res := &RunResult{ExitCode: code, Duration: time.Since(start)}
	if err != nil || !rc.Record {
		return res, err
	}
	res.Observations, res.Warnings, res.ReadsBy, err = observeAll(rc, code, traceOptions{})
	return res, err
}
