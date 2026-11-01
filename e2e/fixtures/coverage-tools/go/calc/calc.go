package calc

// Add returns a+b.
func Add(a, b int) int { return a + b }

// Sign returns -1, 0 or 1.
func Sign(x int) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}
