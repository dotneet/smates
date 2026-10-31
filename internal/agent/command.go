package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
)

// maxCommandOutput bounds the stdout read from an external agent.
const maxCommandOutput = 8 << 20

// answerFormat tells an external agent how to answer. It mirrors submit_selection.
const answerFormat = `## Answer format

You may inspect the repository in the current working directory with your own read-only tools. Do not modify any file.

When you are done, print one JSON object (and nothing after it) matching this JSON Schema:

{"type":"object","additionalProperties":false,"required":["decision","tests","no_impact","summary"],"properties":{
 "decision":{"type":"string","enum":["none","tests","run_all"]},
 "tests":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["suite","test","why"],"properties":{
  "suite":{"type":"string","description":"suite name"},
  "test":{"type":"string","description":"exact test ID from the list above, or \"*\" for the whole suite"},
  "why":{"type":"string","description":"short concrete evidence"}}}},
 "no_impact":{"type":"array","items":{"type":"string"},"description":"orphan paths that cannot affect any test"},
 "summary":{"type":"string","description":"one or two sentences for the CI log"}}}

Example: {"decision":"tests","tests":[{"suite":"web","test":"src/db.test.ts","why":"reads fixtures/seed.sql changed here"}],"no_impact":[],"summary":"seed.sql is loaded by the db tests."}
`

// commandBackend runs an external agent (agent.command): the prompt goes to stdin
// and the answer is read from stdout.
type commandBackend struct {
	cfg config.AgentConfig
}

// Decide runs the configured command once and validates its answer.
func (c *commandBackend) Decide(ctx context.Context, req *Request) (*Decision, error) {
	if err := checkRequest(req); err != nil {
		return nil, err
	}
	cfg := effectiveConfig(req, c.cfg)
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(cfg))
	defer cancel()
	log := logWriter(req)
	started := time.Now()

	tb := newToolbox(req)
	prompt := systemPrompt(cfg, true) + "\n\n" + buildBrief(ctx, req, tb, commandBrief) + "\n" + answerFormat

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", c.cfg.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", c.cfg.Command)
	}
	cmd.Dir = req.Repo.Root
	cmd.Stdin = strings.NewReader(prompt)
	stdout := &limitedBuffer{max: maxCommandOutput}
	stderr := &limitedBuffer{max: 64 << 10, keepTail: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second // do not hang on grandchildren holding the pipes

	logf(log, "running %s", c.cfg.Command)
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("agent command: %w after %s", ctx.Err(), time.Since(started).Round(time.Millisecond))
	}
	if err != nil {
		tail := strings.TrimSpace(stderr.String())
		if tail != "" {
			tail = ": " + clip(lastLines(tail, 10), 2000)
		}
		return nil, fmt.Errorf("agent command failed: %v%s", err, tail)
	}
	sub, usage, err := parseCommandOutput(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("agent command: %w", err)
	}
	d := toDecision(sub, req)
	d.Usage = usage
	d.Model = "command"
	logf(log, "%s in %s", describe(d), time.Since(started).Round(time.Millisecond))
	return d, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// limitedBuffer keeps at most max bytes: the head, or the tail when keepTail is set.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	keepTail bool
}

var errOutputTooLarge = errors.New("agent command output too large")

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.keepTail {
		l.buf.Write(p)
		if extra := l.buf.Len() - l.max; extra > 0 {
			l.buf.Next(extra)
		}
		return len(p), nil
	}
	if l.buf.Len()+len(p) > l.max {
		return 0, errOutputTooLarge
	}
	return l.buf.Write(p)
}

func (l *limitedBuffer) Bytes() []byte  { return l.buf.Bytes() }
func (l *limitedBuffer) String() string { return l.buf.String() }
