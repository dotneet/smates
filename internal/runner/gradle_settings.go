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
	paths, ok := gradleIncludes(src, includeWord)
	flat, flatOK := gradleIncludes(src, includeFlatWord)
	ok = ok && flatOK
	moved := map[string]string{}
	for _, m := range projectDirAssig.FindAllStringSubmatch(src, -1) {
		moved[":"+strings.TrimPrefix(m[1], ":")] = path.Clean(filepath.ToSlash(m[2]))
	}
	if len(projectDirWord.FindAllStringIndex(src, -1)) > len(projectDirAssig.FindAllStringIndex(src, -1)) {
		ok = false
	}
	for _, p := range flat {
		if _, set := moved[p]; !set && strings.Count(p, ":") == 1 {
			moved[p] = "../" + p[1:] // includeFlat: a sibling of the build root
		}
	}
	var dirOf func(p string) string
	dirOf = func(p string) string {
		if d, ok := moved[p]; ok {
			return d
		}
		i := strings.LastIndexByte(p, ':')
		if i <= 0 {
			return p[1:]
		}
		return pathutil.Join(dirOf(p[:i]), p[i+1:])
	}
	st := GradleSettings{Projects: []GradleProject{{Path: ":", Dir: "."}}, OK: ok}
	all := append(paths, flat...)
	sort.Strings(all)
	for i, p := range all {
		if i > 0 && all[i-1] == p {
			continue
		}
		st.Projects = append(st.Projects, GradleProject{Path: p, Dir: dirOf(p)})
	}
	return st
}

// gradleIncludes returns the project paths the include (or includeFlat) calls of settings (comments
// stripped) name, with their parents (":a:b" includes ":a"), sorted. ok is false when
// an include names a project by anything but string literals.
func gradleIncludes(src string, word *regexp.Regexp) ([]string, bool) {
	set := map[string]bool{}
	ok := true
	for _, loc := range word.FindAllStringIndex(src, -1) {
		lits, good := includeArgs(src[loc[1]:])
		if !good {
			ok = false
			continue
		}
		for _, l := range lits {
			p := ":" + strings.TrimPrefix(l, ":")
			for ; p != ""; p = p[:strings.LastIndexByte(p, ':')] {
				set[p] = true
			}
		}
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, ok
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
