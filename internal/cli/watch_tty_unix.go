//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// keyMode switches a terminal to reading single key presses (no line buffering, no
// echo; Ctrl-C still raises SIGINT). It returns false when f is not a terminal.
func keyMode(f *os.File) (restore func(), perKey, ok bool) {
	var old syscall.Termios
	if ioctl(f.Fd(), ioctlGetTermios, &old) != nil {
		return nil, false, false
	}
	raw := old
	raw.Lflag &^= syscall.ICANON | syscall.ECHO
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if ioctl(f.Fd(), ioctlSetTermios, &raw) != nil {
		return func() {}, false, true // a terminal, but line mode
	}
	return func() { _ = ioctl(f.Fd(), ioctlSetTermios, &old) }, true, true
}

func ioctl(fd, req uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}
