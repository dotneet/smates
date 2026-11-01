package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestFormatFlag runs every command that takes --format through Main: --format json
// prints one JSON document on stdout, --format bogus is a usage error (exit 2) that
// prints nothing on stdout.
func TestFormatFlag(t *testing.T) {
	g, _ := configRepo(t)
	cases := []struct {
		name string
		// cmd are the command words, args the flags and arguments that follow -C and
		// --format.
		cmd, args []string
		// code is the exit code with --format json (-1: 0 or 1).
		code int
	}{
		{"init", []string{"init"}, []string{"--stdout"}, 0},
		{"why", []string{"why"}, []string{"a/x_check.sh"}, 0},
		{"deps", []string{"deps"}, []string{"a/x_check.sh"}, 0},
		{"list", []string{"list"}, nil, 0},
		{"flaky", []string{"flaky"}, nil, 0},
		{"stats", []string{"stats"}, nil, 0},
		{"orphans", []string{"orphans"}, []string{"--history", "1"}, 0},
		{"config add-ignore", []string{"config", "add-ignore"}, []string{"--dry-run", "tools/**"}, 0},
		{"config set", []string{"config", "set"}, []string{"--dry-run", "orphans", "ignore"}, 0},
		{"state show", []string{"state", "show"}, nil, 0},
		{"select", []string{"select"}, []string{"--no-agent"}, 0},
		{"doctor", []string{"doctor"}, nil, -1}, // depends on the tools installed
	}
	run := func(cmd, args []string, format string) (int, string, string) {
		var out, errb bytes.Buffer
		full := append(append(append([]string{}, cmd...), "-C", g.dir, "--format", format), args...)
		code := Main(full, &out, &errb)
		return code, out.String(), errb.String()
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errs := run(c.cmd, c.args, "json")
			if (c.code >= 0 && code != c.code) || (c.code < 0 && code != 0 && code != 1) {
				t.Fatalf("--format json: exit %d\nstdout: %s\nstderr: %s", code, out, errs)
			}
			var v any
			dec := json.NewDecoder(strings.NewReader(out))
			if err := dec.Decode(&v); err != nil {
				t.Fatalf("--format json: stdout is not JSON: %v\n%s", err, out)
			}
			if dec.More() {
				t.Fatalf("--format json: more than one JSON document on stdout:\n%s", out)
			}

			code, out, errs = run(c.cmd, c.args, "bogus")
			if code != ExitUsage {
				t.Fatalf("--format bogus: exit %d, want %d\nstderr: %s", code, ExitUsage, errs)
			}
			if out != "" {
				t.Errorf("--format bogus: stdout %q, want nothing", out)
			}
			// flaky, stats and select do not list the formats they accept.
			want := fmt.Sprintf("smates %s: unknown format \"bogus\" (want text or json)\n", c.cmd[0])
			if c.cmd[0] == "flaky" || c.cmd[0] == "stats" || c.cmd[0] == "select" {
				want = fmt.Sprintf("smates %s: unknown format \"bogus\"\n", c.cmd[0])
			}
			if !strings.HasSuffix(errs, want) {
				t.Errorf("--format bogus: stderr %q, want it to end with %q", errs, want)
			}
		})
	}

	// state show --json is the deprecated spelling of --format json.
	var out, errb bytes.Buffer
	if code := Main([]string{"state", "show", "-C", g.dir, "--json"}, &out, &errb); code != 0 {
		t.Fatalf("state show --json: exit %d: %s", code, errb.String())
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("state show --json: %v\n%s", err, out.String())
	}
}
