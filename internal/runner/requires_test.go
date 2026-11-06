package runner

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
)

func TestCheckRequirement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "pkg", "fixtures"), 0o755)
	os.WriteFile(filepath.Join(root, "pkg", ".env.test"), []byte("A=1\n"), 0o644)
	os.WriteFile(filepath.Join(root, "pkg", "fixtures", "data.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, "top.txt"), []byte("x"), 0o644)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	open := ln.Addr().String()
	_, openPort, _ := net.SplitHostPort(open)
	// A port nothing listens on: listen, then close.
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l2.Addr().String()
	l2.Close()

	t.Setenv("SMATES_REQ_SET", "yes")
	t.Setenv("SMATES_REQ_EMPTY", "")
	t.Setenv("SMATES_REQ_PORT", openPort)
	t.Setenv("ANTHROPIC_API_KEY", "secret") // tests never get it, so it is not set for them
	short := 300 * time.Millisecond
	cases := []struct {
		name string
		r    config.Requirement
		env  map[string]string // the suite's env
		err  string            // substring; "" = met
	}{
		{"tcp open", config.Requirement{TCP: open}, nil, ""},
		{"tcp closed", config.Requirement{TCP: closed, Timeout: short}, nil, "nothing accepts connections on " + closed},
		{"tcp from the environment", config.Requirement{TCP: "127.0.0.1:${SMATES_REQ_PORT}"}, nil, ""},
		{"tcp from the suite env", config.Requirement{TCP: "$DB_ADDR"}, map[string]string{"DB_ADDR": open}, ""},
		{"tcp unset variable", config.Requirement{TCP: "$SMATES_REQ_UNSET_X"}, nil, "$SMATES_REQ_UNSET_X not set"},
		// Must keep the tests deferred: ":<port>" would dial the local machine, where
		// something else may listen.
		{"tcp unset host variable", config.Requirement{TCP: "${SMATES_REQ_UNSET_X}:" + openPort}, nil, "$SMATES_REQ_UNSET_X not set"},
		{"tcp empty host variable", config.Requirement{TCP: "${SMATES_REQ_EMPTY}:" + openPort}, nil, "$SMATES_REQ_EMPTY not set"},
		{"tcp unset port variable", config.Requirement{TCP: "127.0.0.1:${SMATES_REQ_UNSET_X}"}, nil, "$SMATES_REQ_UNSET_X not set"},
		{"tcp host emptied by the suite env", config.Requirement{TCP: "${DB_HOST}:" + openPort}, map[string]string{"DB_HOST": ""}, "$DB_HOST not set"},
		{"tcp expands to a bad port", config.Requirement{TCP: "127.0.0.1:${SMATES_REQ_SET}"}, nil, `bad port "yes"`},
		{"tcp unresolvable host", config.Requirement{TCP: "no-such-host.invalid:1", Timeout: short}, nil, "nothing accepts connections"},
		{"cmd exits 0", config.Requirement{Cmd: "test -f .env.test"}, nil, ""},
		{"cmd runs in the suite root", config.Requirement{Cmd: "test -f fixtures/data.json"}, nil, ""},
		{"cmd sees the suite env", config.Requirement{Cmd: `test "$MODE" = db`}, map[string]string{"MODE": "db"}, ""},
		{"cmd exits 1", config.Requirement{Cmd: "echo the database is down >&2; exit 1"}, nil, "exit status 1: the database is down"},
		{"cmd not found", config.Requirement{Cmd: "smates-no-such-command-x7"}, nil, "exit status 127"},
		{"cmd times out", config.Requirement{Cmd: "sleep 30", Timeout: short}, nil, "did not finish within 300ms"},
		// It exits at once; the check does not wait for the child.
		{"cmd background child holds the output", config.Requirement{Cmd: "sleep 30 & exit 1"}, nil, "exit status 1"},
		{"env set", config.Requirement{Env: "SMATES_REQ_SET"}, nil, ""},
		{"env empty", config.Requirement{Env: "SMATES_REQ_EMPTY"}, nil, "$SMATES_REQ_EMPTY is not set"},
		{"env unset", config.Requirement{Env: "SMATES_REQ_UNSET_X"}, nil, "is not set"},
		{"env from the suite env", config.Requirement{Env: "DB_URL"}, map[string]string{"DB_URL": "x"}, ""},
		{"env emptied by the suite env", config.Requirement{Env: "SMATES_REQ_SET"}, map[string]string{"SMATES_REQ_SET": ""}, "is not set"},
		{"env agent secret", config.Requirement{Env: "ANTHROPIC_API_KEY"}, nil, "is not set"},
		{"file relative to the suite root", config.Requirement{File: ".env.test"}, nil, ""},
		{"file missing", config.Requirement{File: ".env.local"}, nil, "no file matches"},
		{"file glob", config.Requirement{File: "fixtures/**/*.json"}, nil, ""},
		{"file glob without match", config.Requirement{File: "fixtures/*.yaml"}, nil, "no file matches"},
		{"file directory", config.Requirement{File: "fixtures"}, nil, ""},
		{"file outside the suite root", config.Requirement{File: "../top.txt"}, nil, ""},
		{"file absolute", config.Requirement{File: filepath.ToSlash(filepath.Join(root, "top.txt"))}, nil, ""},
		{"file from the environment", config.Requirement{File: "${SMATES_REQ_DIR}/top.txt"}, nil, ""},
		// Must keep: an empty path would name the suite directory, which exists.
		{"file unset variable", config.Requirement{File: "${SMATES_REQ_UNSET_X}"}, nil, "$SMATES_REQ_UNSET_X not set"},
		{"file unset directory variable", config.Requirement{File: "${SMATES_REQ_UNSET_X}/top.txt"}, nil, "$SMATES_REQ_UNSET_X not set"},
	}
	t.Setenv("SMATES_REQ_DIR", root)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Suite{Suite: config.Suite{Name: "s", Root: "pkg", Env: tc.env}}
			start := time.Now()
			err := CheckRequirement(context.Background(), root, s, tc.r)
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("the check took %s", took)
			}
			switch {
			case tc.err == "" && err != nil:
				t.Errorf("unmet: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("got %v, want an error with %q", err, tc.err)
			}
		})
	}
}

// A cancelled run stops a check at once, and the check counts as unmet.
func TestCheckRequirementCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Suite{Suite: config.Suite{Name: "s", Root: "."}}
	for _, r := range []config.Requirement{{Cmd: "sleep 30", Timeout: time.Minute}, {TCP: "127.0.0.1:1", Timeout: time.Minute}} {
		start := time.Now()
		if err := CheckRequirement(ctx, t.TempDir(), s, r); err == nil || time.Since(start) > 5*time.Second {
			t.Errorf("%s: %v after %s", r, err, time.Since(start))
		}
	}
}
