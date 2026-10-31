package version

import (
	"os/exec"
	"strings"
	"testing"
)

// The release tooling (make version) must stamp the same version the code reports.
func TestReleaseStampMatches(t *testing.T) {
	out, err := exec.Command("make", "-s", "-C", "../..", "version").CombinedOutput()
	if err != nil {
		t.Fatalf("make version: %v\n%s", err, out)
	}
	want := Product + " v" + Version
	if got := strings.TrimSpace(string(out)); got != want {
		t.Fatalf("release stamp %q, code says %q", got, want)
	}
}
