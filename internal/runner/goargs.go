package runner

// The flags go test gets in a Go suite, read once: GOFLAGS in the suite's environment,
// then the suite command with the arguments passed through to it. Every part of the Go
// adapter that needs one of them (the build flags go list must see, -coverpkg, a user's
// -exec, the go command itself) reads it here, so that they all agree on what the run
// does.

import (
	"path/filepath"
	"slices"
	"strings"
)

// goCommand is the suite's go test configuration (see goCommandOf).
type goCommand struct {
	// argv is the suite command line with the arguments passed through (nil when the
	// command does not parse: err says why).
	argv []string
	err  error
	// goTest is set when argv is `<go> test ...`: smates then sees, and can change,
	// every flag go test gets on its command line. Otherwise the command is a wrapper
	// (gotestsum, make, sh -c), whose words are read as go test flags only as a guess.
	goTest bool
	// bin is the go command: argv[0] when it is one (any path whose base name is go),
	// else "go" (found through the suite's PATH).
	bin string
	// env are the flags of GOFLAGS (one flag per word).
	env []GoArg
	// cmd are the arguments after `go test` up to -args (for a wrapper, its words after
	// argv[0]), flags and package patterns.
	cmd []GoArg
	// tail are the words after -args (nil when there is none).
	tail []string
	// dashdash is set when a "--" hands the rest of the words to something smates does
	// not parse.
	dashdash bool
}

// goCommandOf reads the go test configuration of rc's suite and the arguments passed
// through (rc.ExtraArgs).
func goCommandOf(rc *RunContext) *goCommand {
	g := &goCommand{bin: "go"}
	g.env, _ = ParseGoTestArgs(strings.Fields(Getenv(suiteEnv(rc.Suite), "GOFLAGS")), true)
	g.argv, g.err = rc.CommandLine(nil, nil)
	if g.err != nil || len(g.argv) == 0 {
		g.argv = nil
		return g
	}
	if isGoBin(g.argv[0]) {
		g.bin = g.argv[0]
		if !filepath.IsAbs(g.bin) && strings.ContainsAny(g.bin, `/\`) && rc.Repo != nil {
			g.bin = filepath.Join(rc.Dir(), g.bin) // also for go commands run elsewhere
		}
	}
	rest := g.argv[1:]
	if g.goTest = isGoBin(g.argv[0]) && len(g.argv) >= 2 && g.argv[1] == "test"; g.goTest {
		rest = g.argv[2:]
	}
	g.dashdash = slices.Contains(rest, "--")
	if g.goTest {
		// go test hands "--" and every word after it to the test binary
		// (cmdflag.ErrFlagTerminator): they are not its flags.
		rest, _ = cutTerminator(rest)
	}
	g.cmd, g.tail = ParseGoTestArgs(rest, false)
	return g
}

// cutTerminator splits go test arguments at a "--" that comes before -args: the words
// before it are go test's flags and packages, the words from it on (the "--"
// included) are passed to the test binary as they are.
func cutTerminator(words []string) (flags, rest []string) {
	for i, w := range words {
		switch w {
		case "-args", "--args":
			return words, nil
		case "--":
			return words[:i], words[i:]
		}
	}
	return words, nil
}

// isGoBin reports whether argv0 runs the go command.
func isGoBin(argv0 string) bool {
	return strings.TrimSuffix(filepath.Base(filepath.ToSlash(argv0)), ".exe") == "go"
}

// all is every flag and package pattern go test gets, in the order it reads them
// (GOFLAGS first: the command line wins).
func (g *goCommand) all() []GoArg { return slices.Concat(g.env, g.cmd) }

// visible reports whether smates sees every build flag go test gets: the command is go
// test itself (not a wrapper, whose flags reach go test unseen) and no "--" hands the
// rest of the words to something smates does not parse. When it cannot tell, a build
// constraint excluding a file under the flags it does see proves nothing.
func (g *goCommand) visible() bool { return g.goTest && !g.dashdash }

// last returns the last flag named name (without dashes: -exec and --exec are "exec").
func (g *goCommand) last(name string) (GoArg, bool) {
	all := g.all()
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Name == name {
			return all[i], true
		}
	}
	return GoArg{}, false
}

// onCommandLine reports whether a flag named name is on the command line (not only in
// GOFLAGS).
func (g *goCommand) onCommandLine(name string) bool {
	return slices.ContainsFunc(g.cmd, func(a GoArg) bool { return a.Name == name })
}

// Build flags that change which files go test compiles, forwarded to go list. Others
// (test flags, -exec, -coverpkg, -count) are not go list's and are left out.
var (
	goListValueFlags = map[string]bool{"tags": true, "mod": true, "modfile": true, "overlay": true}
	goListBoolFlags  = map[string]bool{"race": true, "msan": true, "asan": true}
)

// listFlags returns the build flags (of GOFLAGS, then the command: later ones win, as
// with go test) that go list needs to see the files go test builds. A "--" ends the
// command's flags: what follows goes to something else.
func (g *goCommand) listFlags() []string {
	var out []string
	add := func(args []GoArg) {
		for _, a := range args {
			switch {
			case a.Name == "" && a.Words[0] == "--":
				return
			case goListValueFlags[a.Name] && a.HasValue:
				out = append(out, "-"+a.Name+"="+a.Value)
			case goListBoolFlags[a.Name]:
				out = append(out, a.Words[0])
			}
		}
	}
	add(g.env)
	add(g.cmd)
	return out
}

// coverPkgs returns the package patterns of the -coverpkg flags (nil when there is
// none).
func (g *goCommand) coverPkgs() []string {
	var pats []string
	for _, a := range g.all() {
		if a.Name != "coverpkg" || !a.HasValue {
			continue
		}
		for _, p := range strings.Split(a.Value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				pats = append(pats, p)
			}
		}
	}
	return pats
}

// goflags is GOFLAGS without the flags drop reports (the words of the others, in their
// order).
func (g *goCommand) goflags(drop func(GoArg) bool) []string {
	var kept []string
	for _, a := range g.env {
		if !drop(a) {
			kept = append(kept, a.Words...)
		}
	}
	return kept
}

// userExec returns the -exec program go test would run the test binaries with (the
// last -exec of GOFLAGS and the command line; "" for none).
func (g *goCommand) userExec() string {
	if a, ok := g.last("exec"); ok {
		return a.Value
	}
	return ""
}

// withoutExec removes the -exec flags (every spelling: -exec=X, --exec=X, -exec X,
// --exec X) from the go test arguments of argv, a command line of the suite (with the
// package arguments of one invocation), so that the -exec smates sets in GOFLAGS
// applies: go test takes the command line's over GOFLAGS'. The words after -args are
// the test binary's and stay. Only a go test command line is changed.
func (g *goCommand) withoutExec(argv []string) []string {
	if !g.goTest || len(argv) < 2 || !isGoBin(argv[0]) || argv[1] != "test" {
		return argv
	}
	flags, passed := cutTerminator(argv[2:])
	args, tail := ParseGoTestArgs(flags, false)
	out := slices.Clone(argv[:2])
	n := 2
	for _, a := range args {
		n += len(a.Words)
		if a.Name != "exec" {
			out = append(out, a.Words...)
		}
	}
	if tail != nil {
		out = append(out, argv[n]) // -args
		out = append(out, tail...)
	}
	return append(out, passed...) // "--" and the test binary's words, as they are
}
