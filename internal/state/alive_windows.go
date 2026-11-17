package state

// processAlive cannot tell on Windows: every holder counts as alive, and a lock left
// behind is broken after the stale timeout only.
func processAlive(int) bool { return true }
