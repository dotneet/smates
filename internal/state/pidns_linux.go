package state

import "os"

// pidNamespace identifies the PID namespace of this process (see holderDead): a pid
// means the same process only within one. ok is false when it cannot be read.
func pidNamespace() (ns string, ok bool) {
	ns, err := os.Readlink("/proc/self/ns/pid")
	return ns, err == nil && ns != ""
}
