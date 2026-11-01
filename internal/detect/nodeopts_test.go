package detect

import (
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Options taking a value as the next word keep it, watch options drop it.
func TestNodeTestOptionsValues(t *testing.T) {
	args := strings.Fields("--allow-fs-read . --diagnostic-dir /tmp/d --watch-kill-signal SIGINT --max-old-space-size-percentage 50 -e 1 --report-dir r --test-only --test x.test.js")
	got, ok := nodeTestOptions(args)
	want := strings.Fields("--allow-fs-read . --diagnostic-dir /tmp/d --max-old-space-size-percentage 50 -e 1 --report-dir r --test-only")
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("nodeTestOptions = %q, %v; want %q", got, ok, want)
	}
}

// Every option node 24's --help lists as --name=... is known to take a value (the
// list was taken from it).
func TestNodeValueOptsCoverNodeHelp(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	if v, err := exec.Command(node, "--version").Output(); err != nil || !strings.HasPrefix(string(v), "v24.") {
		t.Skipf("node %s: the list is node 24's", strings.TrimSpace(string(v)))
	}
	help, err := exec.Command(node, "--help").Output()
	if err != nil {
		t.Fatal(err)
	}
	// Not the optional values (--name[=...]): those are never the next word.
	entry := regexp.MustCompile(`(?m)^\s+(-[^\s\[]*(?:, -[^\s\[]*)*)=\.\.\.`)
	entries := entry.FindAllStringSubmatch(string(help), -1)
	if len(entries) < 50 {
		t.Fatalf("only %d options with a value in node --help", len(entries))
	}
	for _, m := range entries {
		names := strings.Split(m[1], ", ")
		name := names[len(names)-1] // the one =... applies to ("--loader, --experimental-loader=...")
		if name == "--watch-path" || name == "--watch-kill-signal" {
			continue
		}
		if !nodeValueOpts[name] {
			t.Errorf("%s takes a value (node --help) but is not in nodeValueOpts", name)
		}
	}
}
