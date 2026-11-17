package config

import (
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// globKind says what the entries of a configured list are relative to and how they are
// checked.
type globKind int

const (
	// repoGlob: a glob relative to the repository root (no leading "/" or "..").
	repoGlob globKind = iota
	// rootGlob: a glob relative to the suite root.
	rootGlob
	// inputGlob: a glob relative to the suite root, or repo-relative with a leading "/"
	// (Suite.Inputs).
	inputGlob
	// rootPath: a directory relative to the suite root, not a glob (python_roots).
	rootPath
	// envPattern: a path.Match pattern over environment variable names.
	envPattern
	// reachGlob: a repoGlob, or a "$NAME" envPattern (Rule.Reaches).
	reachGlob
)

// globField is one list of globs (or paths, or patterns) of the configuration.
type globField struct {
	where string
	// list aliases the configuration's slice: normalizeGlobs rewrites its entries in place.
	list []string
	kind globKind
	// escapes are the characters a backslash may escape in the list's globs (see
	// checkGlob).
	escapes string
}

// globFields lists every glob, path and pattern list of the configuration, so that no
// list escapes the checks: a glob that is invalid or starts with "./" matches nothing,
// and a file it was meant to declare as a dependency would select no test.
func (c *Config) globFields() []globField {
	out := []globField{
		// `smates orphans` suggests ignore and inputs globs with every metacharacter of
		// a file name escaped.
		{where: "ignore", list: c.Ignore, kind: repoGlob, escapes: globMeta},
		{where: "inputs", list: c.Inputs, kind: repoGlob, escapes: globMeta},
		{where: "env_ignore", list: c.EnvIgnore, kind: envPattern},
		{where: "env_track", list: c.EnvTrack, kind: envPattern},
	}
	for i := range c.Suites {
		out = append(out, c.Suites[i].globFields()...)
	}
	return out
}

func (s *Suite) globFields() []globField {
	at := fmt.Sprintf("suite %q", s.Name)
	out := []globField{
		{where: at + ": tests", list: s.Tests, kind: rootGlob},
		{where: at + ": exclude", list: s.Exclude, kind: rootGlob},
		{where: at + ": inputs", list: s.Inputs, kind: inputGlob},
		{where: at + ": python_roots", list: s.PythonRoots, kind: rootPath},
		{where: at + ": env_ignore", list: s.EnvIgnore, kind: envPattern},
		{where: at + ": env_track", list: s.EnvTrack, kind: envPattern},
		{where: at + ": volatile", list: s.Volatile, kind: rootGlob},
		{where: at + ": untraced", list: s.Untraced, kind: rootGlob},
		{where: at + ": serves", list: s.Serves, kind: repoGlob},
	}
	for i, r := range s.Rules {
		ri := fmt.Sprintf("%s: rules[%d]", at, i)
		out = append(out,
			globField{where: ri + ": tests", list: r.Tests, kind: rootGlob},
			globField{where: ri + ": reaches", list: r.Reaches, kind: reachGlob},
			globField{where: ri + ": deps", list: r.Deps, kind: repoGlob})
	}
	for i, r := range s.Requires {
		out = append(out, globField{where: fmt.Sprintf("%s: requires[%d]: tests", at, i), list: r.Tests, kind: rootGlob})
	}
	for i, d := range s.DependsOn {
		di := fmt.Sprintf("%s: depends_on[%d]", at, i)
		out = append(out,
			globField{where: di + ": paths", list: d.Paths, kind: repoGlob},
			globField{where: di + ": tests", list: d.Tests, kind: rootGlob})
	}
	return out
}

// normalizeGlobs strips a leading "./" from every glob and path of the configuration
// and checks them.
func (c *Config) normalizeGlobs() error {
	for _, f := range c.globFields() {
		for i, g := range f.list {
			escapes := f.escapes
			if escapes == "" {
				escapes = bracketMeta
			}
			n, err := normalizeGlob(g, f.kind, escapes)
			if err != nil {
				return fmt.Errorf("%s: %w", f.where, err)
			}
			f.list[i] = n
		}
	}
	return nil
}

// normalizeGlob returns g without a leading "./", or why it is not a valid entry of a
// list of kind k.
func normalizeGlob(g string, k globKind, escapes string) (string, error) {
	switch k {
	case envPattern:
		if _, err := path.Match(g, ""); err != nil || g == "" {
			return "", fmt.Errorf("bad environment variable pattern %q", g)
		}
		return g, nil
	case reachGlob:
		if name, ok := strings.CutPrefix(g, "$"); ok {
			if _, err := normalizeGlob(name, envPattern, ""); err != nil {
				return "", err
			}
			return g, nil
		}
		return normalizeGlob(g, repoGlob, escapes)
	case inputGlob:
		if repo, ok := strings.CutPrefix(g, "/"); ok {
			n, err := normalizeGlob(repo, repoGlob, escapes)
			return "/" + n, err
		}
		n := trimDotSlash(g)
		if n == ".." || strings.HasPrefix(n, "../") {
			return "", fmt.Errorf("%q is outside the suite root: write it repo-relative with a leading / (\"/shared/**\")", g)
		}
		return checkGlob(n, escapes)
	case rootPath:
		n := trimDotSlash(g)
		if n == "" {
			n = "."
		}
		if strings.Contains(n, "\\") {
			return "", fmt.Errorf("%q: use / as the path separator", g)
		}
		return n, nil
	case repoGlob:
		n := trimDotSlash(g)
		if strings.HasPrefix(n, "/") || n == ".." || strings.HasPrefix(n, "../") {
			return "", fmt.Errorf("%q must be relative to the repository root", g)
		}
		return checkGlob(n, escapes)
	}
	return checkGlob(trimDotSlash(g), escapes)
}

// trimDotSlash removes the leading "./" (repeated) from g.
func trimDotSlash(g string) string {
	for strings.HasPrefix(g, "./") {
		g = g[2:]
	}
	return g
}

const (
	// globMeta are all the characters a backslash may escape in a glob.
	globMeta = `*?[]{}\`
	// bracketMeta are the characters any glob list may escape: a directory named with
	// brackets or braces (`app/[id]`, `app/\[id\]/**`) can be matched no other way,
	// while `\*` and `\?` more likely are a Windows path separator before a wildcard
	// (`src\**`), which would match nothing.
	bracketMeta = `[]{}`
)

// checkGlob checks a glob relative to some directory. A backslash escapes the next
// character in a glob, so one written as a Windows path separator would match nothing:
// it is an error, unless it escapes one of the characters escapes lists.
func checkGlob(g string, escapes string) (string, error) {
	for i := 0; i < len(g); i++ {
		if g[i] != '\\' {
			continue
		}
		if i+1 == len(g) || !strings.ContainsRune(escapes, rune(g[i+1])) {
			return "", fmt.Errorf("%q: use / as the path separator (a backslash escapes the next character in a glob)", g)
		}
		i++
	}
	if g == "" || !doublestar.ValidatePattern(g) {
		return "", fmt.Errorf("bad glob %q", g)
	}
	return g, nil
}
