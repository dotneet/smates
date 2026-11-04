// Package static builds an import graph over JS/TS, Python and Go sources without
// invoking any toolchain. It is a safety net under the runtime traces: it sees imports
// that a run did not execute, imports added since the last run, and works with no state.
package static

import (
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/dotneet/smates/internal/static/golang"
	"github.com/dotneet/smates/internal/static/host"
	"github.com/dotneet/smates/internal/static/js"
	"github.com/dotneet/smates/internal/static/python"
)

// Options configure resolution.
type Options struct {
	// PythonRoots are extra repo-relative import roots for Python.
	PythonRoots []string
}

// Graph resolves direct dependencies lazily and memoizes them.
type Graph struct {
	root  string
	files map[string]bool // all files in the working tree (repo-relative)
	opts  Options

	mu     sync.Mutex
	direct map[string][]string
	// runtime holds, for files whose analyzer is a host.TypeAnalyzer and that import
	// something only for its types, their direct dependencies left once type-only
	// imports are erased (absent: direct).
	runtime map[string][]string
	langs   []host.Analyzer
}

// New creates a graph over the given set of repo files.
func New(root string, files []string, opts Options) *Graph {
	g := &Graph{root: root, files: make(map[string]bool, len(files)), opts: opts, direct: map[string][]string{}, runtime: map[string][]string{}}
	for _, f := range files {
		g.files[f] = true
	}
	g.langs = []host.Analyzer{
		js.New(g, js.Options{}),
		python.New(g, python.Options{Roots: opts.PythonRoots}),
		golang.New(g, golang.Options{}),
	}
	return g
}

// Root returns the repository root.
func (g *Graph) Root() string { return g.root }

// Has reports whether a repo-relative file exists.
func (g *Graph) Has(p string) bool { return g.files[p] }

// Files returns all repo files (unsorted).
func (g *Graph) Files() map[string]bool { return g.files }

// maxRead bounds what is read for analysis; sources larger than this are not scanned.
const maxRead = 8 << 20

// Read returns a regular file's content (nil for anything else or anything too large:
// a symlink to /dev/zero or a huge generated file must not stall selection).
func (g *Graph) Read(p string) []byte {
	fi, err := os.Stat(filepath.Join(g.root, filepath.FromSlash(p)))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxRead {
		return nil
	}
	b, _ := os.ReadFile(filepath.Join(g.root, filepath.FromSlash(p)))
	return b
}

// Direct returns the direct repo-relative dependencies of a file.
func (g *Graph) Direct(p string) []string {
	d, _ := g.deps(p)
	return d
}

// RuntimeDirect returns the direct repo-relative dependencies of a file that remain
// once its type-only imports are erased (TypeScript's import type, see
// host.TypeAnalyzer): what loading the file loads.
func (g *Graph) RuntimeDirect(p string) []string {
	_, r := g.deps(p)
	return r
}

// deps returns Direct(p) and RuntimeDirect(p), resolving them once.
func (g *Graph) deps(p string) (direct, runtime []string) {
	g.mu.Lock()
	d, ok := g.direct[p]
	r, typed := g.runtime[p]
	g.mu.Unlock()
	if ok {
		if !typed {
			r = d
		}
		return d, r
	}
	typed = false
	for _, l := range g.langs {
		if !l.Handles(p) {
			continue
		}
		if ta, ok := l.(host.TypeAnalyzer); ok {
			d, r = ta.SplitDeps(p, g.Read(p))
			typed = true
		} else {
			d = l.Deps(p, g.Read(p))
		}
		break
	}
	d = dedupe(d, p)
	r = dedupe(r, p)
	typed = typed && len(r) != len(d)
	if !typed {
		r = d
	}
	g.mu.Lock()
	g.direct[p] = d
	if typed {
		g.runtime[p] = r
	}
	g.mu.Unlock()
	return d, r
}

// Closure returns the transitive dependencies of the start files, including them.
func (g *Graph) Closure(start ...string) []string {
	return g.closure(g.Direct, start)
}

// RuntimeClosure returns the transitive dependencies of the start files through the
// imports that remain once type-only imports are erased (see RuntimeDirect),
// including the start files: what running them loads, for a runner that strips
// types without checking them.
func (g *Graph) RuntimeClosure(start ...string) []string {
	return g.closure(g.RuntimeDirect, start)
}

func (g *Graph) closure(direct func(string) []string, start []string) []string {
	seen := map[string]bool{}
	stack := append([]string{}, start...)
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		for _, d := range direct(p) {
			if !seen[d] {
				stack = append(stack, d)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Warm resolves direct dependencies of the files in parallel (closure computation
// afterwards is then memory-only).
func (g *Graph) Warm(paths []string) {
	var wg sync.WaitGroup
	ch := make(chan string)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range ch {
				g.Direct(p)
			}
		}()
	}
	for _, p := range paths {
		ch <- p
	}
	close(ch)
	wg.Wait()
}

func dedupe(in []string, self string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{self: true}
	var out []string
	for _, p := range in {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
