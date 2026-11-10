package runner

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/pathutil"
)

// GradleSettings is what smates reads of a build's settings file: its projects.
type GradleSettings struct {
	// Projects are the root project (path ":", directory ".") and the projects the
	// settings include with string literals (and their parents: ":a:b" includes ":a"),
	// with their directories relative to the build root: their path below it ("a/b"),
	// or where a project(':x').projectDir = file('dir') with literals moves them. A
	// project includeFlat names, or one moved outside the build, has a directory
	// starting with "../".
	Projects []GradleProject
	// OK is false when the settings include or move a project by anything else (a
	// computed name or directory): Projects is then incomplete.
	OK bool
}

// GradleProject is a project of a Gradle build.
type GradleProject struct{ Path, Dir string }

var (
	includeWord     = regexp.MustCompile(`\binclude\b`)
	includeFlatWord = regexp.MustCompile(`\bincludeFlat\b`)
	projectDirWord  = regexp.MustCompile(`\bprojectDir\b`)
	projectDirAssig = regexp.MustCompile(`\bproject\s*\(\s*['"](:?[\w.:-]+)['"]\s*\)\s*\.\s*projectDir\s*=\s*(?:file\s*\(|new\s+File\s*\(\s*(?:settingsDir|rootDir)\s*,)\s*['"]([^'"$]+)['"]\s*\)`)
)

// ReadGradleSettings reads the settings of the build rooted at build (repo-relative).
// A build without a settings file has the root project only.
func ReadGradleSettings(repoRoot, build string) GradleSettings {
	var src string
	for _, s := range gradleSettings {
		if data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(build), s)); err == nil {
			src += StripGroovyComments(string(data)) + "\n"
		}
	}
	// Replay the statements in order, as Gradle does: include creates each project
	// (and its missing parents) in its parent's directory as it is at that point, and a
	// projectDir assignment moves only that project, not the children included before.
	type stmt struct {
		pos   int
		flat  bool
		paths []string // include, includeFlat
		dir   string   // projectDir: paths[0] moves to dir
	}
	var stmts []stmt
	ok := true
	for _, w := range []*regexp.Regexp{includeWord, includeFlatWord} {
		for _, loc := range w.FindAllStringIndex(src, -1) {
			lits, good := includeArgs(src[loc[1]:])
			if !good {
				ok = false
				continue
			}
			stmts = append(stmts, stmt{pos: loc[0], flat: w == includeFlatWord, paths: lits})
		}
	}
	for _, m := range projectDirAssig.FindAllStringSubmatchIndex(src, -1) {
		stmts = append(stmts, stmt{pos: m[0], paths: []string{src[m[2]:m[3]]}, dir: path.Clean(filepath.ToSlash(src[m[4]:m[5]]))})
	}
	if len(projectDirWord.FindAllStringIndex(src, -1)) > len(projectDirAssig.FindAllStringIndex(src, -1)) {
		ok = false
	}
	sort.Slice(stmts, func(a, b int) bool { return stmts[a].pos < stmts[b].pos })
	dirs := map[string]string{":": "."}
	for _, st := range stmts {
		for _, l := range st.paths {
			p := ":" + strings.TrimPrefix(l, ":")
			switch {
			case st.dir != "":
				dirs[p] = st.dir
			case st.flat:
				if _, done := dirs[p]; !done && strings.Count(p, ":") == 1 {
					dirs[p] = "../" + p[1:] // a sibling of the build root
				}
			default:
				for i := 1; i <= len(p); i++ {
					if i < len(p) && p[i] != ':' {
						continue
					}
					sub := p[:i]
					if _, done := dirs[sub]; done {
						continue
					}
					parent := ":"
					if k := strings.LastIndexByte(sub, ':'); k > 0 {
						parent = sub[:k]
					}
					dirs[sub] = pathutil.Join(dirs[parent], sub[strings.LastIndexByte(sub, ':')+1:])
				}
			}
		}
	}
	st := GradleSettings{Projects: []GradleProject{{Path: ":", Dir: "."}}, OK: ok}
	var paths []string
	for p := range dirs {
		if p != ":" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		st.Projects = append(st.Projects, GradleProject{Path: p, Dir: dirs[p]})
	}
	return st
}

// includeArgs reads the arguments of an include call: string literals separated by
// commas, in parentheses or not (Groovy's `include 'a', 'b'` may continue on the next
// line after a comma). ok is false for anything else (a variable, an interpolated
// string, a spread list).
func includeArgs(s string) ([]string, bool) {
	i := 0
	space := func(newlines bool) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || newlines && s[i] == '\n') {
			i++
		}
	}
	space(false)
	paren := i < len(s) && s[i] == '('
	if paren {
		i++
	}
	var out []string
	for {
		space(true)
		if paren && len(out) > 0 && i < len(s) && s[i] == ')' {
			return out, true // a trailing comma (Kotlin)
		}
		if i >= len(s) || s[i] != '\'' && s[i] != '"' {
			return nil, false
		}
		q := s[i]
		end := strings.IndexByte(s[i+1:], q)
		if end < 0 {
			return nil, false
		}
		lit := s[i+1 : i+1+end]
		if strings.ContainsAny(lit, "$\n") || lit == "" {
			return nil, false
		}
		out = append(out, lit)
		i += end + 2
		space(paren)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if paren {
			return out, i < len(s) && s[i] == ')'
		}
		return out, i >= len(s) || s[i] == '\n' || s[i] == ';' || s[i] == '}'
	}
}
