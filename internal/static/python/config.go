package python

import (
	"path"
	"regexp"
	"strings"
)

// Minimal, dependency-free readers for the few settings of pyproject.toml and the
// INI-style pytest configs (pytest.ini, tox.ini, setup.cfg) that put directories on
// sys.path. They are line oriented and lenient: anything they do not understand is
// skipped.

// pyprojectRoots returns the import roots, relative to the config file's directory,
// that a pyproject.toml declares.
func pyprojectRoots(doc string) []string {
	var out []string
	for _, sec := range []string{"tool.pytest.ini_options", "tool.pytest"} {
		out = append(out, tomlStrings(doc, sec, "pythonpath")...)
	}
	// setuptools: [tool.setuptools.packages.find] where = ["src"], and
	// [tool.setuptools.package-dir] "" = "src" (or pkg = "src/pkg") or its inline form
	// [tool.setuptools] package-dir = {"" = "src"}.
	out = append(out, tomlStrings(doc, "tool.setuptools.packages.find", "where")...)
	pairs := tomlSectionPairs(doc, "tool.setuptools.package-dir")
	for _, v := range tomlRaw(doc, "tool.setuptools", "package-dir") {
		pairs = append(pairs, inlineTablePairs(v)...)
	}
	for _, kv := range pairs {
		if r, ok := mappedRoot(strings.ReplaceAll(kv[0], ".", "/"), kv[1]); ok {
			out = append(out, r)
		}
	}
	// poetry: packages = [{ include = "pkg", from = "src" }].
	for _, v := range tomlRaw(doc, "tool.poetry", "packages") {
		out = append(out, inlineKeyStrings(v, "from")...)
	}
	// pdm-backend: [tool.pdm.build] package-dir = "src" (older: [tool.pdm]).
	for _, sec := range []string{"tool.pdm.build", "tool.pdm"} {
		out = append(out, tomlStrings(doc, sec, "package-dir")...)
	}
	// hatch, build-wide or for the wheel: packages = ["src/pkg"] makes "src" a root;
	// sources = ["src"] strips the prefix "src", and sources = {"src/a" = "a"} (or a
	// [...sources] table) rewrites a prefix.
	for _, sec := range []string{"tool.hatch.build.targets.wheel", "tool.hatch.build"} {
		for _, p := range tomlStrings(doc, sec, "packages") {
			out = append(out, path.Dir(p))
		}
		hatch := tomlSectionPairs(doc, sec+".sources")
		for _, v := range tomlRaw(doc, sec, "sources") {
			if strings.HasPrefix(v, "{") {
				hatch = append(hatch, inlineTablePairs(v)...)
			} else {
				out = append(out, tomlStringLits(v)...)
			}
		}
		for _, kv := range hatch {
			if r, ok := mappedRoot(kv[1], kv[0]); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// setupCfgRoots returns the import roots a setup.cfg declares for setuptools:
// [options] package_dir (lines "= lib" or "pkg = src/pkg") and
// [options.packages.find] where.
func setupCfgRoots(doc string) []string {
	var out []string
	for _, line := range iniLines(doc, "options", "package_dir") {
		if pkg, dir, ok := strings.Cut(line, "="); ok {
			if r, ok := mappedRoot(strings.ReplaceAll(strings.TrimSpace(pkg), ".", "/"), dir); ok {
				out = append(out, r)
			}
		}
	}
	return append(out, iniValues(doc, "options.packages.find", "where")...)
}

// setupPyPackageDir matches the package_dir argument of setup() in a setup.py, and
// setupPyPair one entry of it (” : 'lib').
var (
	setupPyPackageDir = regexp.MustCompile(`\bpackage_dir\s*=\s*\{([^}]*)\}`)
	setupPyPair       = regexp.MustCompile(`(?:'([^']*)'|"([^"]*)")\s*:\s*(?:'([^']*)'|"([^"]*)")`)
)

// setupPyRoots returns the import roots the package_dir={...} literal of a setup.py
// declares.
func setupPyRoots(doc string) []string {
	var out []string
	for _, m := range setupPyPackageDir.FindAllStringSubmatch(doc, -1) {
		for _, p := range setupPyPair.FindAllStringSubmatch(m[1], -1) {
			if r, ok := mappedRoot(strings.ReplaceAll(p[1]+p[2], ".", "/"), p[3]+p[4]); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// mappedRoot returns the import root that a mapping of the import path mod ("a/b" for
// package a.b; "" for the top level) to the directory dir makes: dir itself for "",
// and the part of dir before mod when dir ends with it (src for a/b at src/a/b). A
// mapping that renames (pkg at lib) has no root: such imports are found by the
// fallback of resolver.module.
func mappedRoot(mod, dir string) (string, bool) {
	mod = strings.Trim(strings.TrimSpace(mod), "/")
	dir = strings.TrimSuffix(strings.TrimSpace(dir), "/")
	switch {
	case mod == "" && dir == "":
		return ".", true
	case mod == "":
		return dir, true
	case dir == mod:
		return ".", true
	case strings.HasSuffix(dir, "/"+mod):
		return strings.TrimSuffix(dir, "/"+mod), true
	}
	return "", false
}

// pyprojectTestGlobs returns the pytest python_files patterns of a pyproject.toml
// (an array of patterns or a whitespace separated string).
func pyprojectTestGlobs(doc string) []string {
	var out []string
	for _, sec := range []string{"tool.pytest.ini_options", "tool.pytest"} {
		for _, s := range tomlStrings(doc, sec, "python_files") {
			out = append(out, strings.Fields(s)...)
		}
	}
	return out
}

// iniRoots returns the pytest pythonpath entries of an INI-style config file.
func iniRoots(doc, base string) []string {
	return iniValues(doc, iniSection(base), "pythonpath")
}

// iniTestGlobs returns the pytest python_files patterns of an INI-style config file.
func iniTestGlobs(doc, base string) []string {
	return iniValues(doc, iniSection(base), "python_files")
}

// iniSection is the section of an INI-style config file that holds pytest settings.
func iniSection(base string) string {
	if base == "setup.cfg" {
		return "tool:pytest"
	}
	return "pytest"
}

// nextLine returns the line starting at doc[i] (without its newline) and the index of
// the following line.
func nextLine(doc string, i int) (string, int) {
	if k := strings.IndexByte(doc[i:], '\n'); k >= 0 {
		return strings.TrimSuffix(doc[i:i+k], "\r"), i + k + 1
	}
	return doc[i:], len(doc)
}

// tomlHeader normalizes a table header line such as `[ tool."pytest" ] # x` to
// "tool.pytest".
func tomlHeader(line string) string {
	if k := strings.IndexByte(line, '#'); k >= 0 {
		line = line[:k]
	}
	line = strings.Trim(strings.TrimSpace(line), "[]")
	return strings.NewReplacer(" ", "", "\t", "", `"`, "", "'", "").Replace(line)
}

// tomlRaw returns the raw value text of every assignment to key in table section.
// Multi-line arrays are joined.
func tomlRaw(doc, section, key string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(doc); {
		var line string
		line, i = nextLine(doc, i)
		t := strings.TrimSpace(line)
		if t == "" || t[0] == '#' {
			continue
		}
		if t[0] == '[' {
			cur = tomlHeader(t)
			continue
		}
		if cur != section {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok || strings.Trim(strings.TrimSpace(k), `"'`) != key {
			continue
		}
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "[") || strings.HasPrefix(v, "{") {
			for !tomlBalanced(v) && i < len(doc) {
				line, i = nextLine(doc, i)
				v += "\n" + line
			}
		}
		out = append(out, v)
	}
	return out
}

// tomlStrings returns the string literals assigned to key in table section (a single
// string or an array of strings).
func tomlStrings(doc, section, key string) []string {
	var out []string
	for _, v := range tomlRaw(doc, section, key) {
		out = append(out, tomlStringLits(v)...)
	}
	return out
}

// tomlBalanced reports whether every bracket/brace opened in v is closed, ignoring
// strings and comments.
func tomlBalanced(v string) bool {
	depth := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '"', '\'':
			_, i = tomlString(v, i)
		case '#':
			for i < len(v) && v[i] != '\n' {
				i++
			}
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return depth <= 0
}

// tomlString reads the basic or literal string whose quote is at v[i] and returns its
// raw content and the index of its closing quote.
func tomlString(v string, i int) (string, int) {
	q := v[i]
	for j := i + 1; j < len(v); j++ {
		switch v[j] {
		case '\\':
			if q == '"' {
				j++
			}
		case q:
			return v[i+1 : j], j
		case '\n':
			return v[i+1 : j], j
		}
	}
	return v[i+1:], len(v)
}

// tomlStringLits returns every string literal in a TOML value, skipping comments.
func tomlStringLits(v string) []string {
	var out []string
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '"', '\'':
			var s string
			s, i = tomlString(v, i)
			out = append(out, s)
		case '#':
			for i < len(v) && v[i] != '\n' {
				i++
			}
		}
	}
	return out
}

// inlineKeyStrings returns the string values of `key = "..."` pairs inside a TOML
// value (typically inline tables in an array).
func inlineKeyStrings(v, key string) []string {
	var out []string
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '"' || c == '\'':
			_, i = tomlString(v, i)
		case c == '#':
			for i < len(v) && v[i] != '\n' {
				i++
			}
		case strings.HasPrefix(v[i:], key) && (i == 0 || !isIdentChar(v[i-1]) && v[i-1] != '-'):
			j := i + len(key)
			for j < len(v) && (v[j] == ' ' || v[j] == '\t') {
				j++
			}
			if j >= len(v) || v[j] != '=' {
				continue
			}
			j++
			for j < len(v) && (v[j] == ' ' || v[j] == '\t') {
				j++
			}
			if j < len(v) && (v[j] == '"' || v[j] == '\'') {
				var s string
				s, i = tomlString(v, j)
				out = append(out, s)
			}
		}
	}
	return out
}

// inlineTablePairs returns the keys (quoted or bare) with string values of a TOML
// inline table such as {"" = "lib", pkg = "src/pkg"}.
func inlineTablePairs(v string) [][2]string {
	var out [][2]string
	k, haveKey, afterEq := "", false, false
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '#':
			for i < len(v) && v[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			var s string
			s, i = tomlString(v, i)
			switch {
			case afterEq:
				out = append(out, [2]string{k, s})
				haveKey, afterEq = false, false
			case !haveKey:
				k, haveKey = s, true
			}
		case c == '=':
			afterEq = haveKey
		case c == ',' || c == '{' || c == '}':
			haveKey, afterEq = false, false
		case isIdentChar(c) || c == '-':
			j := i
			for j < len(v) && (isIdentChar(v[j]) || v[j] == '-' || v[j] == '.') {
				j++
			}
			switch {
			case afterEq: // a non-string value
				haveKey, afterEq = false, false
			case !haveKey:
				k, haveKey = v[i:j], true
			}
			i = j - 1
		}
	}
	return out
}

// tomlSectionPairs returns the keys with a string value of table section, such as
// "" = "src" and pkg = "src/pkg" under [tool.setuptools.package-dir].
func tomlSectionPairs(doc, section string) [][2]string {
	var out [][2]string
	cur := ""
	for i := 0; i < len(doc); {
		var line string
		line, i = nextLine(doc, i)
		t := strings.TrimSpace(line)
		if t == "" || t[0] == '#' {
			continue
		}
		if t[0] == '[' {
			cur = tomlHeader(t)
			continue
		}
		if cur != section {
			continue
		}
		k, rest := "", t
		if t[0] == '"' || t[0] == '\'' {
			var end int
			k, end = tomlString(t, 0)
			rest = t[min(end+1, len(t)):]
		} else if kk, _, ok := strings.Cut(t, "="); ok {
			k = strings.TrimSpace(kk)
			rest = t[len(kk):]
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		if lits := tomlStringLits(rest[1:]); len(lits) > 0 {
			out = append(out, [2]string{k, lits[0]})
		}
	}
	return out
}

// iniValues returns the whitespace separated values of key in an INI section,
// including indented continuation lines.
func iniValues(doc, section, key string) []string {
	var out []string
	for _, line := range iniLines(doc, section, key) {
		out = append(out, strings.Fields(line)...)
	}
	return out
}

// iniLines returns the value of key in an INI section line by line: the rest of the
// key's line, then its indented continuation lines, without comments.
func iniLines(doc, section, key string) []string {
	var out []string
	cur := ""
	collecting := false
	for i := 0; i < len(doc); {
		var line string
		line, i = nextLine(doc, i)
		t := strings.TrimSpace(line)
		if collecting {
			if t == "" {
				continue
			}
			if line[0] == ' ' || line[0] == '\t' {
				if t[0] != '#' && t[0] != ';' {
					out = append(out, strings.TrimSpace(iniStripComment(t)))
				}
				continue
			}
			collecting = false
		}
		if t == "" || t[0] == '#' || t[0] == ';' {
			continue
		}
		if t[0] == '[' {
			if k := strings.IndexByte(t, ']'); k > 0 {
				cur = strings.TrimSpace(t[1:k])
			}
			continue
		}
		if cur != section {
			continue
		}
		k := strings.IndexAny(t, "=:")
		if k < 0 || strings.TrimSpace(t[:k]) != key {
			continue
		}
		if v := strings.TrimSpace(iniStripComment(t[k+1:])); v != "" {
			out = append(out, v)
		}
		collecting = true
	}
	return out
}

// iniStripComment removes an inline " #" or " ;" comment.
func iniStripComment(s string) string {
	for _, m := range []string{" #", "\t#", " ;", "\t;"} {
		if k := strings.Index(s, m); k >= 0 {
			s = s[:k]
		}
	}
	return s
}
