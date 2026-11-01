package text

import (
	"os"
	"strings"
)

// Greeting reads the greeting next to the package.
func Greeting() string {
	b, err := os.ReadFile("greeting.txt")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
