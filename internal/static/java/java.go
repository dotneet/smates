// Package java resolves direct dependencies of Java source files.
//
// It reads sources, not class files: class files reflect the last build, while the
// static graph must reflect the working tree as it is now. A small lexer (see scan.go)
// skips comments and literals, so names inside them are not references. A file depends
// on the files declaring the top-level types it may refer to:
//
//  1. single-type and static imports (import a.b.C; import a.b.C.Inner;
//     import static a.b.C.m;): the file of the longest prefix that is a top-level type;
//  2. on-demand imports (import a.b.*;): every identifier of the file that names a
//     top-level type of package a.b (import a.b.C.*; and import static a.b.C.*; name
//     the file of C);
//  3. its own package, whose types need no import: every identifier that names a
//     top-level type of the package, in any source root (main and test sources share
//     packages, and so may several projects of a build);
//  4. qualified names in code (a.b.C.m()): the longest prefix that is a top-level type;
//  5. the package-info.java files of its package.
//
// Without type information, identifiers are matched by name: a variable named like a
// class of the package makes the class a dependency. The analyzer is a safety net under
// the runtime traces, so wherever it cannot decide it over-approximates, never the
// other way round. It sees what a trace cannot: compile-time constants javac inlines
// into the classes using them, annotations and types used only in signatures, and
// references added since the last recording.
package java

import (
	"strings"
	"sync"

	"github.com/dotneet/smates/internal/static/host"
)

// Analyzer implements host.Analyzer for Java.
type Analyzer struct {
	h    host.Host
	opts Options

	once sync.Once
	idx  *index
}

// Options configure the analyzer. There are none yet.
type Options struct{}

// New creates an analyzer bound to a repository.
func New(h host.Host, opts Options) *Analyzer { return &Analyzer{h: h, opts: opts} }

func isJava(p string) bool { return strings.HasSuffix(p, ".java") }

// Handles implements host.Analyzer.
func (a *Analyzer) Handles(p string) bool { return isJava(p) }

// Deps implements host.Analyzer. It returns the repo-relative Java files that p
// directly depends on (see the package documentation).
func (a *Analyzer) Deps(p string, src []byte) []string {
	if !a.Handles(p) || src == nil {
		return nil
	}
	a.once.Do(func() { a.idx = buildIndex(a.h) })
	idx := a.idx
	u := scan(src)

	var out []string
	add := func(fs []string) { out = append(out, fs...) }
	add(idx.pkgInfo[u.pkg])
	ownTypes := idx.packages[u.pkg]
	for id := range u.idents {
		add(ownTypes[id])
	}
	for _, im := range u.imports {
		switch {
		case !im.onDemand:
			add(idx.typeFiles(im.name))
		case im.static:
			// import static a.b.C.*: the members of C.
			add(idx.typeFiles(im.name))
		default:
			// import a.b.*: types of package a.b, or the nested types of a type a.b.
			add(idx.typeFiles(im.name))
			if byName := idx.packages[im.name]; byName != nil {
				for id := range u.idents {
					add(byName[id])
				}
			}
		}
	}
	for _, name := range u.names {
		add(idx.typeFiles(name))
	}
	return out
}
