// Command smates runs only the tests a change can affect.
package main

import (
	"os"

	"github.com/dotneet/smates/internal/cli"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/trace"
)

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
	cli.Version = version
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
