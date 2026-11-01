package notest

// Version has no tests: go test -cover still reports it.
const Version = "1"

// Major returns the major version.
func Major() string { return Version }
