package cli

import "syscall"

const ioctlGetTermios, ioctlSetTermios = syscall.TCGETS, syscall.TCSETS
