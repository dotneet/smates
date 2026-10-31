//go:build !darwin && !linux

package cli

import "os"

// keyMode reports whether f is a terminal; keys are then read a line at a time.
func keyMode(f *os.File) (restore func(), perKey, ok bool) {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil, false, false
	}
	return func() {}, false, true
}
