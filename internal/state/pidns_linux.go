package state

import (
	"os"
	"strings"
)

// pidNamespace identifies the PID namespace of this process (see holderDead): a pid
// means the same process only within one. A namespace's inode is unique within one
// kernel only (the initial one is the same on every boot), so the kernel's boot id is
// part of it: "pid:[4026531836]/<boot id>". ok is false when either cannot be read.
func pidNamespace() (ns string, ok bool) {
	pidns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil || pidns == "" {
		return "", false
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	id := strings.TrimSpace(string(boot))
	if err != nil || id == "" {
		return "", false
	}
	return pidns + "/" + id, true
}
