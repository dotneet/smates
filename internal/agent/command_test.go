package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeScript writes an executable shell script outside the repository.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommandBackend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh scripts")
	}
	f := newFixture(t)
	promptCopy := filepath.Join(t.TempDir(), "prompt.txt")
	tests := []struct {
		name    string
		script  string
		timeout time.Duration
		check   func(t *testing.T, d *Decision)
		wantErr string
	}{
		{
			name: "fx --json envelope",
			script: `cat > ` + promptCopy + `
pwd > ` + promptCopy + `.cwd
cat <<'JSON'
{"final_output":"Done.\n` + "```json" + `\n{\"decision\":\"tests\",\"tests\":[{\"suite\":\"web\",\"test\":\"db.test.ts\",\"why\":\"seed\"},{\"suite\":\"web\",\"test\":\"ghost.test.ts\",\"why\":\"x\"}],\"no_impact\":[\"scripts/gone.sh\"],\"summary\":\"seed affects db\"}\n` + "```" + `","status":"completed"}
JSON
`,
			check: func(t *testing.T, d *Decision) {
				if len(d.Add) != 1 || d.Add[0].Test != "src/db.test.ts" || d.Add[0].Detail != "seed" {
					t.Errorf("Add = %+v", d.Add)
				}
				if len(d.Dropped) != 1 || d.Model != "command" || d.Summary != "seed affects db" {
					t.Errorf("decision = %+v", d)
				}
				prompt, _ := os.ReadFile(promptCopy)
				for _, w := range []string{"## Orphan changes", "scripts/seed.sh (modified)", "+set -e", "src/other.test.ts (selected)", "## Answer format", `"enum":["none","tests","run_all"]`, "untrusted"} {
					if !strings.Contains(string(prompt), w) {
						t.Errorf("prompt lacks %q", w)
					}
				}
				if strings.Contains(string(prompt), "submit_selection") {
					t.Error("prompt mentions the built-in submit tool")
				}
				cwd, _ := os.ReadFile(promptCopy + ".cwd")
				if real, _ := filepath.EvalSymlinks(strings.TrimSpace(string(cwd))); real != f.repo.Root {
					t.Errorf("cwd = %s, want %s", cwd, f.repo.Root)
				}
			},
		},
		{
			name:   "claude -p json",
			script: `cat >/dev/null; echo '{"type":"result","result":"{\"decision\":\"run_all\",\"tests\":[],\"no_impact\":[],\"summary\":\"ci\"}","num_turns":4,"usage":{"input_tokens":5,"output_tokens":6}}'`,
			check: func(t *testing.T, d *Decision) {
				if !d.RunAll || d.Usage.Turns != 4 || d.Usage.OutputTokens != 6 {
					t.Errorf("decision = %+v", d)
				}
			},
		},
		{name: "non-zero exit", script: "cat >/dev/null; echo 'boom: no credentials' >&2; exit 3", wantErr: "boom: no credentials"},
		{name: "no answer", script: "cat >/dev/null; echo 'I could not decide.'", wantErr: `no JSON object with a "decision" field`},
		{name: "timeout", script: "cat >/dev/null; sleep 5", timeout: 100 * time.Millisecond, wantErr: "deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Command = writeScript(t, tt.script)
			if tt.timeout > 0 {
				cfg.Timeout = tt.timeout
			}
			var log bytes.Buffer
			req := f.request(cfg)
			req.Log = &log
			started := time.Now()
			d, err := New(cfg).Decide(context.Background(), req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if time.Since(started) > 4*time.Second {
					t.Errorf("command was not stopped in time")
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			tt.check(t, d)
		})
	}
}

// The command that runs is the one of the request's configuration, which the cache
// key is computed from, not the one the backend was created with.
func TestCommandBackendUsesRequestCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh scripts")
	}
	f := newFixture(t)
	backend := testConfig()
	backend.Command = writeScript(t, "cat >/dev/null; exit 3")
	cfg := testConfig()
	cfg.Command = writeScript(t, `cat >/dev/null; echo '{"decision":"none","tests":[],"no_impact":[],"summary":"request"}'`)
	d, err := New(backend).Decide(context.Background(), f.request(cfg))
	if err != nil || d.Summary != "request" {
		t.Fatalf("Decide = %+v, %v", d, err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	head := &limitedBuffer{max: 4}
	if n, err := head.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := head.Write([]byte("de")); err == nil {
		t.Fatal("expected overflow error")
	}
	tail := &limitedBuffer{max: 4, keepTail: true}
	tail.Write([]byte("abc"))
	tail.Write([]byte("defg"))
	if tail.String() != "defg" {
		t.Fatalf("tail = %q", tail.String())
	}
}
