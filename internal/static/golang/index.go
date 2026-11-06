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
	vendored map[string]bool     // directories containing a vendor/modules.txt
	// modPins and workPins map a module or workspace directory to the other go.mod and
	// go.sum files whose content pins the dependency versions of its builds (see
	// workPins).
	modPins  map[string][]string
	workPins map[string][]string
}

// vendorDirs returns the vendor directories `go build` may read for code in dir: the
// one of the enclosing module and the one of the enclosing workspace (go work vendor),
// each only when it has a vendor/modules.txt. Since Go 1.14 such a directory turns on
// -mod=vendor by default; older go directives and -mod flags are not evaluated, so
// both are returned whenever they exist.
func (ix *index) vendorDirs(dir string) []string {
	var out []string
	if m, ok := enclosing(dir, ix.modDirs); ok && ix.vendored[m] {
		out = append(out, joinRel(m, "vendor"))
	}
	if w, ok := enclosing(dir, ix.workDirs); ok && ix.vendored[w] {
		if v := joinRel(w, "vendor"); len(out) == 0 || out[0] != v {
			out = append(out, v)
		}
	}
	return out
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
		vendored: map[string]bool{},
		modPins:  map[string][]string{},
		workPins: map[string][]string{},
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
		case base == "modules.txt" && path.Base(dir) == "vendor":
			ix.vendored[dirOf(dir)] = true
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
	modReplaces := map[string][]string{} // module directory -> its local replace targets
	for _, f := range mods {
		dir := dirOf(f)
		mf := parseModFile(a.h.Read(f))
		if mf.module != "" {
			ix.addModule(mf.module, dir)
		}
		modReplaces[dir] = a.addReplaces(ix, dir, mf.replaces)
		ix.modPins[dir] = goModFiles(modReplaces[dir])
	}
	for _, f := range works {
		dir := dirOf(f)
		wf := parseModFile(a.h.Read(f))
		reps := a.addReplaces(ix, dir, wf.replaces)
		ix.workPins[dir] = a.workPins(ix, dir, wf, reps, modReplaces)
	}
	return ix
}

// Which go.mod and go.sum files pin the dependency versions of a build.
//
// Minimal version selection (MVS) reads the requirements of the main modules and of
// every module in the build list, so a require bump in a go.mod whose packages a file
// never imports can still change the version of a third-party module that file's
// package links. Besides its own module's go.mod and go.sum (added by Deps), a Go file
// therefore depends on:
//
//   - outside a workspace, the go.mod of each local directory its module's go.mod
//     replaces a module with (the replacement's requirements take part in MVS);
//   - in a go.work workspace, where MVS picks one version per module across all used
//     modules, the go.mod and go.sum of every module the go.work uses, and the go.mod of
//     each local replace target of the go.work or of those modules (their replace
//     directives all apply).
//
// The other used modules' go.sum files count although a checksum by itself does not
// change what is compiled: in workspace mode the go command verifies every module
// against the go.sum files of all used modules plus go.work.sum, so a line removed or
// altered in module B's go.sum can fail module C's build ("missing go.sum entry",
// "checksum mismatch"), and a content key cannot tell such an edit from a harmless
// one. A go.sum edit nearly always comes with a go.mod edit, which selects the same
// tests anyway. A replace target's own go.sum is never read and is left out.
//
// A go.work whose directives cannot be parsed, or that uses no module, pins every
// go.mod and go.sum below its directory: which modules it means to use is unknown.

// workPins returns the pinning files of the go.work workspace in dir: wf is its parsed
// content, reps its local replace targets, modReplaces those of each module.
func (a *Analyzer) workPins(ix *index, dir string, wf modFile, reps []string, modReplaces map[string][]string) []string {
	var used []string
	if wf.malformed || len(wf.uses) == 0 {
		for m := range ix.modDirs {
			if under(m, dir) {
				used = append(used, m)
			}
		}
		sort.Strings(used)
	} else {
		for _, u := range wf.uses {
			if m, ok := a.localDir(dir, u, false); ok {
				used = append(used, m)
			}
		}
	}
	var out []string
	for _, m := range used {
		out = append(out, joinRel(m, "go.mod"), joinRel(m, "go.sum"))
		reps = append(reps, modReplaces[m]...)
	}
	return append(out, goModFiles(reps)...)
}

// goModFiles returns the go.mod path of each directory.
func goModFiles(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, joinRel(d, "go.mod"))
	}
	return out
}

// under reports whether the repo-relative path p is dir or below it.
func under(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

func (ix *index) addModule(modPath, dir string) {
	for _, d := range ix.modules[modPath] {
		if d == dir {
			return
		}
	}
	ix.modules[modPath] = append(ix.modules[modPath], dir)
}

// addReplaces maps replaced module paths to local directories inside the repository
// and returns those directories.
func (a *Analyzer) addReplaces(ix *index, dir string, reps []replace) []string {
	var out []string
	for _, r := range reps {
		if t, ok := a.localDir(dir, r.new, true); ok {
			ix.addModule(r.old, t)
			out = append(out, t)
		}
	}
	return out
}

// localDir resolves a directory p named by the go.mod or go.work in dir to a
// repo-relative directory, reporting false when it is outside the repository. A
// replace target (replace: true) is a directory only when it is . or .., starts with
// ./ or ../, or is absolute, anything else being a module path; a use path is always a
// directory, relative to dir unless absolute. Backslashes (a file written on Windows)
// count as separators.
func (a *Analyzer) localDir(dir, p string, replace bool) (string, bool) {
	p = strings.ReplaceAll(p, `\`, "/")
	var target string
	switch {
	case filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
		rel, err := filepath.Rel(a.h.Root(), filepath.FromSlash(p))
		if err != nil {
			return "", false
		}
		target = filepath.ToSlash(rel)
	case !replace, p == ".", p == "..", strings.HasPrefix(p, "./"), strings.HasPrefix(p, "../"):
		target = path.Join(dir, p)
	default:
		return "", false // module-path replacement: external
	}
	t, ok := cleanRel(target)
	if !ok || excluded(t) {
		return "", false
	}
	return t, true
}

// modFile is the subset of a go.mod / go.work file the analyzer needs.
type modFile struct {
	module   string
	replaces []replace
	uses     []string // go.work use directives: module directories as written
	// malformed is set when a line could not be parsed: an unterminated quote or
	// block, a stray ")", or a module, replace or use directive of the wrong shape.
	malformed bool
}

// replace is one replace directive: old module path => new module path or directory.
type replace struct{ old, new string }

// parseModFile extracts the module path and the replace and use directives from
// go.mod or go.work content. It tolerates malformed input, skipping (and noting, in
// malformed) lines it does not understand.
func parseModFile(data []byte) modFile {
	var mf modFile
	var block string // verb of the enclosing "verb (" block, if any
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		fields, ok := modFields(sc.Text())
		if !ok {
			mf.malformed = true
		}
		if len(fields) == 0 {
			continue
		}
		if block != "" {
			if fields[0] == ")" {
				if len(fields) > 1 {
					mf.malformed = true
				}
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
			} else {
				mf.malformed = true
			}
		case "replace":
			if r, ok := parseReplace(fields[1:]); ok {
				mf.replaces = append(mf.replaces, r)
			} else {
				mf.malformed = true
			}
		case "use":
			// One directory per directive: "use ( ./a )" on one line is not valid.
			if len(fields) == 2 && fields[1] != "(" && fields[1] != ")" {
				mf.uses = append(mf.uses, fields[1])
			} else {
				mf.malformed = true
			}
		case "(", ")":
			mf.malformed = true
		}
	}
	if block != "" || sc.Err() != nil {
		mf.malformed = true
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
// quoted tokens. ok is false when a quoted token is unterminated or invalid; the
// tokens before it are returned.
func modFields(line string) (out []string, ok bool) {
	for {
		line = strings.TrimLeft(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "//") {
			return out, true
		}
		switch line[0] {
		case '"', '`':
			end := quotedEnd(line)
			if end < 0 {
				return out, false
			}
			s, err := strconv.Unquote(line[:end])
			if err != nil {
				return out, false
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
