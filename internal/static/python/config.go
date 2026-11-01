package python

import (
	"path"
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
	// [tool.setuptools.package-dir] "" = "src" or its inline form
	// [tool.setuptools] package-dir = {"" = "src"}.
	out = append(out, tomlStrings(doc, "tool.setuptools.packages.find", "where")...)
	out = append(out, tomlStrings(doc, "tool.setuptools.package-dir", "")...)
	for _, v := range tomlRaw(doc, "tool.setuptools", "package-dir") {
		out = append(out, inlineTableValues(v, "")...)
	}
	// poetry: packages = [{ include = "pkg", from = "src" }].
	for _, v := range tomlRaw(doc, "tool.poetry", "packages") {
		out = append(out, inlineKeyStrings(v, "from")...)
	}
	// hatch: packages = ["src/pkg"] makes "src" a root.
	for _, p := range tomlStrings(doc, "tool.hatch.build.targets.wheel", "packages") {
		out = append(out, path.Dir(p))
	}
	return out
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

// inlineTableValues returns the string values assigned to key (quoted or bare) in a
// TOML inline table such as {"" = "lib", pkg = "src/pkg"}.
func inlineTableValues(v, key string) []string {
	var out []string
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
				if k == key {
					out = append(out, s)
				}
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
			for j < len(v) && (isIdentChar(v[j]) || v[j] == '-') {
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

// iniValues returns the whitespace separated values of key in an INI section,
// including indented continuation lines.
func iniValues(doc, section, key string) []string {
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
					out = append(out, strings.Fields(iniStripComment(t))...)
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
		out = append(out, strings.Fields(iniStripComment(t[k+1:]))...)
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
