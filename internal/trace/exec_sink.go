package trace

import (
	"path/filepath"
	"strings"

	"github.com/dotneet/smates/internal/depkey"
)

// The sink: every key command-line analysis records goes through the methods below,
// the single home of the policy for paths outside the repository, for its root (also
// reached through a symlink) and for the root's ancestors.

// add records that the file p is read (nothing outside the repository).
func (a *execAnalysis) add(p string) {
	if r, ok := a.rel(p); ok {
		a.out = append(a.out, r)
	}
}

// opaque records that a known program running in dir may read anything below it: the
// tree of dir in the repository, the whole repository when dir is its root or an
// ancestor of it. A program running elsewhere (in a temp directory, say) only reaches
// the repository through the paths its command line names, recorded separately.
func (a *execAnalysis) opaque(dir string) {
	if k := treeKey(dir, a.root, a.rel, false); k != "" {
		a.out = append(a.out, k)
	}
}

// unresolved records that a command run in cwd uses something the analysis does not
// know (an unset variable, a command's output, a script it could not read, commands
// read from standard input, a cut command line): like an opaque program it may read
// anything below cwd, and outside the repository the unknown value may still name a
// path in it, so the whole repository is recorded.
func (a *execAnalysis) unresolved(cwd string) {
	a.out = append(a.out, treeKey(cwd, a.root, a.rel, true))
}

// treeKey is the key of everything below dir: its tree in the repository, the whole
// repository when dir is the root or an ancestor of it (also through a symlink), and
// outside the repository the whole repository when unknown is set ("" otherwise).
func treeKey(dir, root string, rel func(string) (string, bool), unknown bool) string {
	if r, ok := rel(dir); ok {
		return depkey.Tree(r)
	}
	if unknown || covers(dir, root) {
		return depkey.Tree(".")
	}
	return ""
}

// covers reports whether dir is root or one of its ancestors, also through symlinks.
func covers(dir, root string) bool {
	dir = filepath.Clean(dir)
	if contains(dir, root) {
		return true
	}
	r, err := filepath.EvalSymlinks(dir)
	return err == nil && contains(r, root)
}

// contains reports whether the absolute path p is dir or below it.
func contains(dir, p string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	r, err := filepath.Rel(dir, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// absoluteTrees records, for a command with too many words to follow (seg, run in cwd),
// the tree of the literal directory each absolute word starts with: opaque(cwd) does
// not cover them when the command runs outside the repository. A word that does not
// start with a literal absolute path and uses a variable not in vars (nil: none is
// known) or a command's output may name anything (unresolved).
func (a *execAnalysis) absoluteTrees(seg []string, cwd string, vars map[string][]string) {
	for _, w := range seg {
		prefix := w
		if i := strings.IndexAny(w, "{$*?["); i >= 0 {
			prefix = w[:i]
		}
		prefix = literal(prefix)
		if !filepath.IsAbs(prefix) {
			if unknownVars(w, vars) {
				a.unresolved(cwd)
			}
			continue
		}
		if !strings.HasSuffix(prefix, "/") {
			prefix = filepath.Dir(prefix)
		}
		a.opaque(filepath.Clean(prefix))
	}
}

// unknownVars reports whether the word w uses a variable that is not in vars, or an
// expansion the analysis does not follow (${X:-y}, a command's output).
func unknownVars(w string, vars map[string][]string) bool {
	rest := varRef.ReplaceAllStringFunc(w, func(ref string) string {
		m := varRef.FindStringSubmatch(ref)
		if _, ok := vars[m[1]+m[2]]; ok {
			return ""
		}
		return ref
	})
	return strings.Contains(rest, "$")
}

// listing records that the entries of dir are read.
func (a *execAnalysis) listing(dir string) {
	if r, ok := a.rel(dir); ok {
		a.out = append(a.out, depkey.Listing(r))
	} else if isRoot(dir, a.root) {
		a.out = append(a.out, depkey.Listing("."))
	}
}

// existence records that a command depends on whether p exists.
func (a *execAnalysis) existence(p string) {
	if r, ok := a.rel(p); ok {
		a.out = append(a.out, depkey.Existence(r))
	}
}

// glob records what the matches of a glob pattern (word, relative to cwd) depend on:
// the entries of the directory holding the pattern, or every file below the directory
// before the first pattern component when a directory component is a pattern too.
func (a *execAnalysis) glob(word, cwd string) {
	if !strings.ContainsAny(word, "*?[") {
		return // no pattern (quoted pattern characters are literal, see splitShell)
	}
	dir := filepath.Dir(filepath.Clean(word))
	tree := false
	for strings.ContainsAny(dir, "*?[") {
		dir, tree = filepath.Dir(dir), true
	}
	dir = literal(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	if tree {
		a.opaque(dir)
	} else {
		a.listing(dir)
	}
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// expand resolves a command-line word relative to cwd, expanding glob patterns.
func expand(word, cwd string) []string {
	pattern := strings.ContainsAny(word, "*?[")
	word = literal(word)
	if word == "" {
		return nil
	}
	p := word
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if pattern {
		m, _ := filepath.Glob(p)
		return m
	}
	return []string{p}
}
