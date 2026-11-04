package java

import (
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/dotneet/smates/internal/static/host"
)

// index is the repository-wide view of Java types the analyzer builds once, on first
// use, by scanning every .java file of the working tree. It is read-only afterwards.
type index struct {
	// types maps a top-level type's qualified name to the files declaring it. Several
	// projects (or a main and a test source root) may declare the same name: every
	// candidate is a dependency.
	types map[string][]string
	// packages maps a package name to its top-level types by simple name, and those to
	// the files declaring them.
	packages map[string]map[string][]string
	// pkgInfo maps a package name to its package-info.java files, whose annotations
	// apply to every type of the package.
	pkgInfo map[string][]string
}

// infoFile declares annotations of its package, not a type.
const infoFile = "package-info.java"

func buildIndex(h host.Host) *index {
	var files []string
	for f := range h.Files() {
		if isJava(f) {
			files = append(files, f)
		}
	}
	sort.Strings(files)

	// Scanning reads every Java file of the repository: spread it over the CPUs.
	units := make([]*unit, len(files))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if src := h.Read(files[i]); src != nil {
					units[i] = scan(src)
				}
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()

	idx := &index{types: map[string][]string{}, packages: map[string]map[string][]string{}, pkgInfo: map[string][]string{}}
	for i, f := range files {
		u := units[i]
		if u == nil {
			continue
		}
		if path.Base(f) == infoFile {
			idx.pkgInfo[u.pkg] = append(idx.pkgInfo[u.pkg], f)
			continue
		}
		byName := idx.packages[u.pkg]
		if byName == nil {
			byName = map[string][]string{}
			idx.packages[u.pkg] = byName
		}
		for _, t := range u.types {
			byName[t] = append(byName[t], f)
			idx.types[qualify(u.pkg, t)] = append(idx.types[qualify(u.pkg, t)], f)
		}
	}
	return idx
}

// qualify joins a package name and a simple type name.
func qualify(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// typeFiles returns the files declaring the longest prefix of a qualified name that is
// a top-level type: a.b.C for a.b.C, a.b.C.Inner and a.b.C.member alike. Prefixes of
// one identifier only match types of the unnamed package.
func (idx *index) typeFiles(name string) []string {
	for {
		if fs, ok := idx.types[name]; ok {
			return fs
		}
		k := strings.LastIndexByte(name, '.')
		if k < 0 {
			return nil
		}
		name = name[:k]
	}
}
