//go:build windows

package trace

import "os"

var forwardedSignals = []os.Signal{os.Interrupt}
