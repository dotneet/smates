// Command smates runs only the tests a change can affect.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/dotneet/smates/internal/cli"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/trace"
)

// version is set with -ldflags "-X main.version=..." (releases: the tag without its
// "v", e.g. 0.5.0).
var version = "dev"

func main() {
	// `go test -exec` program of a compile check: the test binary is not run.
	if runner.IsGoCheck() {
		os.Exit(0)
	}
	// `go test -exec` wrapper mode (see trace.GoExec).
	if trace.IsGoExec() {
		os.Exit(trace.GoExec(os.Args[1:]))
	}
	cli.Version = buildVersion(version, debug.ReadBuildInfo)
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}

// buildVersion is the version smates reports: the one set at link time, else the
// module version Go recorded in the binary (`go install
// github.com/dotneet/smates/cmd/smates@v0.5.0` records v0.5.0), without its "v" as
// releases print it. It stays "dev" when neither is known.
func buildVersion(linked string, buildInfo func() (*debug.BuildInfo, bool)) string {
	if linked != "dev" {
		return linked
	}
	info, ok := buildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return linked
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}
