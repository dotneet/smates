package golang

import (
	"bufio"
	"bytes"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// compiledExt lists the non-Go extensions that `go build` compiles or links as part of
// a package (cgo, assembly, SWIG, Fortran, prebuilt objects).
var compiledExt = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".m": true,
	".h": true, ".hh": true, ".hpp": true, ".hxx": true,
	".s": true, ".S": true, ".sx": true,
	".f": true, ".F": true, ".for": true, ".f90": true,
	".swig": true, ".swigcxx": true, ".syso": true,
}

// pkgDir lists the files of one directory that take part in a package build.
type pkgDir struct {
	src   []string // non-test .go files plus other compiled sources, sorted
	tests []string // _test.go files, sorted
	hasGo bool     // the directory holds at least one non-test .go file (importable)
}

// index is the per-repository view of Go packages and modules, built once.
type index struct {
	dirs     map[string]*pkgDir
	modules  map[string][]string // module path -> repo-relative module directories
	modDirs  map[string]bool     // directories containing a go.mod
	workDirs map[string]bool     // directories containing a go.work
}

// pkgFiles returns the compiled files of the package in dir, or nil if dir holds none.
func (ix *index) pkgFiles(dir string) []string {
	if d := ix.dirs[dir]; d != nil && d.hasGo {
		return d.src
	}
	return nil
}

func (a *Analyzer) index() *index {
	a.idxOnce.Do(func() { a.idx = a.buildIndex() })
	return a.idx
}

func (a *Analyzer) buildIndex() *index {
	ix := &index{
		dirs:     map[string]*pkgDir{},
		modules:  map[string][]string{},
		modDirs:  map[string]bool{},
		workDirs: map[string]bool{},
	}
	var mods, works []string
	for f := range a.h.Files() {
		if excluded(f) {
			continue
		}
		dir, base := dirOf(f), f[strings.LastIndexByte(f, '/')+1:]
		switch {
		case base == "go.mod":
			// A go.mod inside vendor/ is a vendored copy, not a module of this repo.
			if !hasElem(dir, "vendor") {
				ix.modDirs[dir] = true
				mods = append(mods, f)
			}
			continue
		case base == "go.work":
			ix.workDirs[dir] = true
			works = append(works, f)
			continue
		case strings.HasPrefix(base, "_"), strings.HasPrefix(base, "."):
			continue // ignored by the go command
		}
		ext := path.Ext(base)
		if ext != ".go" && !compiledExt[ext] {
			continue
		}
		d := ix.dirs[dir]
		if d == nil {
			d = &pkgDir{}
			ix.dirs[dir] = d
		}
		switch {
		case strings.HasSuffix(base, "_test.go"):
			d.tests = append(d.tests, f)
		case ext == ".go":
			d.src = append(d.src, f)
			d.hasGo = true
		default:
			d.src = append(d.src, f)
		}
	}
	for _, d := range ix.dirs {
		sort.Strings(d.src)
		sort.Strings(d.tests)
	}

	// Sorted so that module directory lists are deterministic.
	sort.Strings(mods)
	sort.Strings(works)
	for _, f := range mods {
		dir := dirOf(f)
		mf := parseModFile(a.h.Read(f))
		if mf.module != "" {
			ix.addModule(mf.module, dir)
		}
		a.addReplaces(ix, dir, mf.replaces)
	}
	for _, f := range works {
		a.addReplaces(ix, dirOf(f), parseModFile(a.h.Read(f)).replaces)
	}
	return ix
}

func (ix *index) addModule(modPath, dir string) {
	for _, d := range ix.modules[modPath] {
		if d == dir {
			return
		}
	}
	ix.modules[modPath] = append(ix.modules[modPath], dir)
}

// addReplaces maps replaced module paths to local directories inside the repository.
func (a *Analyzer) addReplaces(ix *index, dir string, reps []replace) {
	for _, r := range reps {
		var target string
		switch {
		case strings.HasPrefix(r.new, "./"), strings.HasPrefix(r.new, "../"):
			target = path.Join(dir, r.new)
		case filepath.IsAbs(r.new) || strings.HasPrefix(r.new, "/"):
			rel, err := filepath.Rel(a.h.Root(), filepath.FromSlash(r.new))
			if err != nil {
				continue
			}
			target = filepath.ToSlash(rel)
		default:
			continue // module-path replacement: external
		}
		if t, ok := cleanRel(target); ok && !excluded(t) {
			ix.addModule(r.old, t)
		}
	}
}

// modFile is the subset of a go.mod / go.work file the analyzer needs.
type modFile struct {
	module   string
	replaces []replace
}

// replace is one replace directive: old module path => new module path or directory.
type replace struct{ old, new string }

// parseModFile extracts the module path and replace directives from go.mod or go.work
// content. It tolerates malformed input, skipping lines it does not understand.
func parseModFile(data []byte) modFile {
	var mf modFile
	var block string // verb of the enclosing "verb (" block, if any
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		fields := modFields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if block != "" {
			if fields[0] == ")" {
				block = ""
				continue
			}
			fields = append([]string{block}, fields...)
		} else if len(fields) == 2 && fields[1] == "(" {
			block = fields[0]
			continue
		}
		switch fields[0] {
		case "module":
			if len(fields) >= 2 {
				mf.module = fields[1]
			}
		case "replace":
			if r, ok := parseReplace(fields[1:]); ok {
				mf.replaces = append(mf.replaces, r)
			}
		}
	}
	return mf
}

// parseReplace parses "old [version] => new [version]".
func parseReplace(f []string) (replace, bool) {
	for i, s := range f {
		if s == "=>" && i >= 1 && i+1 < len(f) {
			return replace{old: f[0], new: f[i+1]}, true
		}
	}
	return replace{}, false
}

// modFields splits a go.mod line into tokens, dropping // comments and unquoting
// quoted tokens.
func modFields(line string) []string {
	var out []string
	for {
		line = strings.TrimLeft(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "//") {
			return out
		}
		switch line[0] {
		case '"', '`':
			end := quotedEnd(line)
			if end < 0 {
				return out
			}
			s, err := strconv.Unquote(line[:end])
			if err != nil {
				return out
			}
			out = append(out, s)
			line = line[end:]
		case '(', ')':
			out = append(out, line[:1])
			line = line[1:]
		default:
			i := strings.IndexAny(line, " \t\r()")
			if j := strings.Index(line, "//"); j >= 0 && (i < 0 || j < i) {
				i = j
			}
			if i < 0 {
				i = len(line)
			}
			out = append(out, line[:i])
			line = line[i:]
		}
	}
}

// quotedEnd returns the index just past the closing quote of the Go string literal at
// the start of s, or -1 if it is unterminated.
func quotedEnd(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && q == '"':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return -1
}
