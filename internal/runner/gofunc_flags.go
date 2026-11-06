package runner

// Translation of the suite's go test command into flags for building a test binary
// and running it directly (granularity: func recording).

import (
	"path/filepath"
	"strconv"
	"strings"
)

// goFlags is the suite command split for building a test binary and running it.
type goFlags struct {
	build []string // flags for go list / go test -c
	test  []string // -test.* flags for the binary
	tail  []string // arguments after -args
	exec  []string // a user -exec program
	p     int      // -p: packages built and tested at once (0 = the default)
	vet   string   // the -vet setting ("" = go test's default)
	// why is set when per-function recording cannot be used with this command.
	why string
}

// Test flags of go test (go help testflag) that take a value.
var goTestValueFlags = map[string]bool{
	"bench": true, "benchtime": true, "blockprofile": true, "blockprofilerate": true, "count": true,
	"coverprofile": true, "cpu": true, "cpuprofile": true, "fuzz": true, "fuzzcachedir": true,
	"fuzzminimizetime": true, "fuzztime": true, "list": true, "memprofile": true, "memprofilerate": true,
	"mutexprofile": true, "mutexprofilefraction": true, "outputdir": true, "parallel": true, "run": true,
	"shuffle": true, "skip": true, "timeout": true, "trace": true,
	// Flags of the go command that take a value and only matter when running tests.
	"exec": true, "covermode": true, "coverpkg": true, "vet": true, "o": true,
}

// Boolean test flags.
var goTestBoolFlags = map[string]bool{"v": true, "short": true, "failfast": true, "fullpath": true, "benchmem": true, "json": true, "cover": true, "c": true}

// Build flags taking a value in a separate word.
var goBuildValueFlags = map[string]bool{
	"asmflags": true, "buildmode": true, "compiler": true, "gccgoflags": true, "gcflags": true, "installsuffix": true,
	"ldflags": true, "mod": true, "modfile": true, "overlay": true, "pgo": true, "pkgdir": true, "tags": true,
	"toolexec": true, "C": true, "p": true,
}

// parseGoFlags splits the suite command (with the run's extra arguments).
func parseGoFlags(rc *RunContext) goFlags {
	var g goFlags
	argv, err := rc.CommandLine(nil, nil)
	if err != nil {
		g.why = err.Error()
		return g
	}
	if len(argv) < 2 || strings.TrimSuffix(filepath.Base(argv[0]), ".exe") != "go" || argv[1] != "test" {
		g.why = "the suite command is not `go test ...`"
		return g
	}
	// GOFLAGS first: flags on the command line come later and win (the binary, like the
	// flag package, takes the last value of a repeated flag).
	timeout := false
	g.words(strings.Fields(Getenv(suiteEnv(rc.Suite), "GOFLAGS")), true, &timeout)
	g.words(argv[2:], false, &timeout)
	if !timeout {
		g.test = append(g.test, "-test.timeout=10m0s") // go test's default
	}
	return g
}

// GoArg is one argument of a go test command line: a flag with its value, or a
// package pattern (Name "").
type GoArg struct {
	Words    []string // the word(s) it was read from
	Name     string   // the flag without its dashes ("test.run" stays as is)
	Value    string
	HasValue bool
}

// ParseGoTestArgs splits the arguments of go test (the words after "go test") into
// flags and package patterns, reading the value of a flag that takes one from the
// next word unless oneWord (GOFLAGS: one flag per word). tail holds the words after
// -args (nil when there is none, and never with oneWord).
func ParseGoTestArgs(words []string, oneWord bool) (args []GoArg, tail []string) {
	for i := 0; i < len(words); i++ {
		w := words[i]
		if !oneWord && (w == "-args" || w == "--args") {
			return args, append([]string{}, words[i+1:]...)
		}
		if !strings.HasPrefix(w, "-") || w == "-" || w == "--" {
			args = append(args, GoArg{Words: []string{w}})
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(w, "-"), "=")
		short := strings.TrimPrefix(name, "test.")
		takesValue := goTestValueFlags[short] || (goBuildValueFlags[name] && !strings.HasPrefix(name, "test."))
		a := GoArg{Words: []string{w}, Name: name, Value: val, HasValue: hasVal}
		if takesValue && !hasVal && !oneWord && i+1 < len(words) {
			i++
			a.Words = append(a.Words, words[i])
			a.Value, a.HasValue = words[i], true
		}
		args = append(args, a)
	}
	return args, nil
}

// words translates go test arguments: from GOFLAGS (fromEnv: one flag per word; build
// flags are left to the go command, which reads GOFLAGS itself) or from the command.
func (g *goFlags) words(words []string, fromEnv bool, timeout *bool) {
	where := "the suite command"
	if fromEnv {
		where = "GOFLAGS"
	}
	args, tail := ParseGoTestArgs(words, fromEnv)
	if tail != nil {
		g.tail = tail
	}
	for _, a := range args {
		if a.Name == "" {
			continue // package patterns: smates names the package itself
		}
		name, val, hasVal := a.Name, a.Value, a.HasValue
		short := strings.TrimPrefix(name, "test.")
		if strings.HasPrefix(name, "test.") {
			switch short {
			case "run", "skip", "bench", "fuzz", "list":
				g.why = where + " passes -" + name
			case "timeout":
				*timeout = true
			}
			g.test = append(g.test, "-"+name+suffixValue(val, hasVal))
			continue
		}
		switch {
		case name == "run" || name == "skip" || name == "bench" || name == "fuzz" || name == "list" || name == "c" || name == "o":
			g.why = where + " passes -" + name
		case name == "exec":
			g.exec = strings.Fields(val)
		case name == "timeout" || name == "parallel" || name == "cpu" || name == "count" || name == "shuffle":
			*timeout = *timeout || name == "timeout"
			g.test = append(g.test, "-test."+name+"="+val)
		case name == "v" || name == "short" || name == "failfast" || name == "fullpath":
			g.test = append(g.test, "-test."+name+suffixValue(val, hasVal))
		case name == "vet":
			g.vet = val // for compile checks (gofunc_check.go)
		case goTestValueFlags[name] || goTestBoolFlags[name]:
			// Profiles, -json, coverage settings (smates sets its own): not used.
		default:
			if name == "p" {
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					g.p = n
				}
			}
			if fromEnv {
				continue
			}
			g.build = append(g.build, a.Words...)
		}
	}
}

func suffixValue(v string, has bool) string {
	if !has {
		return ""
	}
	return "=" + v
}
