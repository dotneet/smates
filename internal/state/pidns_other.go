//go:build !linux

package state

// pidNamespace identifies the PID namespace of this process (see holderDead). Other
// platforms have one per host: its identity is empty.
func pidNamespace() (ns string, ok bool) {
	return "", true
}
