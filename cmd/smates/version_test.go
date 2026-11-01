package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	for _, c := range []struct {
		linked string
		// module is the main module's version Go recorded; noInfo: no build info at all.
		module string
		noInfo bool
		want   string
	}{
		{linked: "0.5.0", module: "v0.4.0", want: "0.5.0"},          // release (-ldflags -X): unchanged
		{linked: "source-v1", module: "(devel)", want: "source-v1"}, // the GitHub Action's source build
		{linked: "dev", module: "v0.5.0", want: "0.5.0"},            // go install ...@v0.5.0
		{linked: "dev", module: "v0.5.1-0.20261101000000-abcdef123456", want: "0.5.1-0.20261101000000-abcdef123456"},
		{linked: "dev", module: "(devel)", want: "dev"},
		{linked: "dev", module: "", want: "dev"},
		{linked: "dev", noInfo: true, want: "dev"},
	} {
		info := func() (*debug.BuildInfo, bool) {
			if c.noInfo {
				return nil, false
			}
			return &debug.BuildInfo{Main: debug.Module{Version: c.module}}, true
		}
		if got := buildVersion(c.linked, info); got != c.want {
			t.Errorf("buildVersion(%q) with module version %q: got %q, want %q", c.linked, c.module, got, c.want)
		}
	}
}
