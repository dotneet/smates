package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/static/js"
)

var jsConfigExts = []string{"js", "ts", "mjs", "cjs", "mts", "cts"}

// jsConfig is a runner configuration file read without running it.
type jsConfig struct {
	file string // repo-relative path (package.json for its "jest" key)
	obj  *js.Object
}

// loadJSConfig reads the object a config file exports: JSON files are decoded, JS/TS
// modules evaluated statically (see js.ConfigObject). A config that is not a literal
// object yields an error explaining why.
func loadJSConfig(repo *gitx.Repo, file string) (*js.Object, error) {
	data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(file)))
	if err != nil {
		return nil, err
	}
	var v any
	if strings.HasSuffix(file, ".json") {
		if err := js.ParseJSONC(data, &v); err != nil {
			return nil, fmt.Errorf("%s: %v", file, err)
		}
		v = js.FromJSON(v)
	} else {
		var ok bool
		if v, ok = js.ConfigObject(data); !ok {
			return nil, fmt.Errorf("%s has no `export default` or `module.exports =`", file)
		}
	}
	if a, isArr := v.([]any); isArr {
		// A workspace file exports an array of projects.
		return &js.Object{Fields: map[string]any{"projects": a}}, nil
	}
	o, isObj := v.(*js.Object)
	if !isObj {
		return nil, fmt.Errorf("%s does not export an object literal (%s)", file, short(v))
	}
	return o, nil
}

// short describes a value that is not a literal.
func short(v any) string {
	s := fmt.Sprint(v)
	if u, ok := v.(js.Unknown); ok {
		s = string(u)
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 40 {
		s = s[:37] + "..."
	}
	return s
}

// errNotLiteral is the reason a config value cannot be read.
func errNotLiteral(file, key string, v any) error {
	if v == nil {
		return fmt.Errorf("%s: %s may come from a ...spread", file, key)
	}
	return fmt.Errorf("%s: %s is not a literal (%s)", file, key, short(v))
}

// get returns obj[key] (nil if absent) and an error when it may be set but is unknown.
// A key "test.include" is looked up as "include" (obj being the test options) and
// named in full in the error.
func get(file string, obj *js.Object, key string) (any, error) {
	name := key
	if k, ok := strings.CutPrefix(key, "test."); ok {
		key = k
	}
	v, known := obj.Get(key)
	if !known {
		return nil, errNotLiteral(file, name, nil)
	}
	if _, ok := v.(js.Unknown); ok {
		return nil, errNotLiteral(file, name, v)
	}
	return v, nil
}

// stringList returns a string or array of strings; spread elements are passed to expand
// (nil: not allowed).
func stringList(file, key string, v any, expand func(string) ([]string, bool)) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{x}, nil
	case []any:
		var out []string
		for _, e := range x {
			switch y := e.(type) {
			case string:
				out = append(out, y)
			case js.Unknown:
				if expand != nil && strings.HasPrefix(string(y), "...") {
					if ex, ok := expand(strings.TrimPrefix(string(y), "...")); ok {
						out = append(out, ex...)
						continue
					}
				}
				return nil, errNotLiteral(file, key, y)
			default:
				return nil, errNotLiteral(file, key, y)
			}
		}
		return out, nil
	}
	return nil, errNotLiteral(file, key, v)
}

// commandFlag returns the value of a --name/-short flag of the suite command
// ("--name=v", "--name v"), and whether the flag is present.
func commandFlag(s *Suite, names ...string) (string, bool) {
	words, err := SplitWords(s.Command)
	if err != nil {
		return "", false
	}
	for i, w := range words {
		for _, n := range names {
			if w == n {
				if i+1 < len(words) {
					return words[i+1], true
				}
				return "", true
			}
			if strings.HasPrefix(w, n+"=") {
				return w[len(n)+1:], true
			}
		}
	}
	return "", false
}

// findUp returns the first repo-relative file among names (in order) found in dir or
// its ancestors up to the repository root; stop, when set, ends the search in the
// first directory where it returns true.
func findUp(repo *gitx.Repo, dir string, names []string, stop func(string) bool) string {
	for d := dir; ; d = path.Dir(d) {
		for _, n := range names {
			p := pathutil.Join(d, n)
			if fi, err := os.Stat(filepath.Join(repo.Root, filepath.FromSlash(p))); err == nil && !fi.IsDir() {
				return p
			}
		}
		if stop != nil && stop(d) {
			return ""
		}
		if d == "." || d == "/" || d == "" {
			return ""
		}
	}
}

func repoExists(repo *gitx.Repo, p string) bool {
	_, err := os.Stat(filepath.Join(repo.Root, filepath.FromSlash(p)))
	return err == nil
}

// ---- jest ----

// Discover reads testMatch, testRegex, roots, rootDir and testPathIgnorePatterns from
// the jest configuration jest would use: --config of the suite command, else the
// nearest jest.config.* or package.json (its "jest" key) from the suite directory up.
func (jest) Discover(repo *gitx.Repo, s *Suite) Discovery {
	file := ""
	var obj *js.Object
	var err error
	if c, ok := commandFlag(s, "--config", "-c"); ok {
		if strings.HasPrefix(strings.TrimSpace(c), "{") {
			obj, err = js.DecodeJSONObject([]byte(c))
			file = "--config"
		} else if f, ok := repoRel(s.Root, c); ok && c != "" {
			file = f
			obj, err = loadJSConfig(repo, f)
		} else {
			err = fmt.Errorf("the suite command's --config %q cannot be read", c)
		}
	} else {
		var names []string
		for _, e := range append(jsConfigExts, "json") {
			names = append(names, "jest.config."+e)
		}
		file = findUp(repo, s.Root, names, func(d string) bool { return repoExists(repo, pathutil.Join(d, "package.json")) })
		if file == "" {
			// Jest stops at the nearest package.json; its "jest" key, if any, configures it.
			for d := s.Root; ; d = path.Dir(d) {
				pj := pathutil.Join(d, "package.json")
				if repoExists(repo, pj) {
					obj, err = packageJSONJest(repo, pj)
					file = pj
					break
				}
				if d == "." {
					break
				}
			}
			if obj == nil && err == nil {
				return Discovery{}
			}
		} else {
			obj, err = loadJSConfig(repo, file)
		}
	}
	if err == nil {
		var d Discovery
		if d, err = jestDiscovery(file, obj, s); err == nil {
			return d
		}
	}
	return Discovery{Warning: fmt.Sprintf("%v: using the default test patterns (set tests: in smates.yml if they differ)", err)}
}

// packageJSONJest returns the "jest" key of a package.json (nil if absent).
func packageJSONJest(repo *gitx.Repo, pj string) (*js.Object, error) {
	data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(pj)))
	if err != nil {
		return nil, err
	}
	var p struct {
		Jest any `json:"jest"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %v", pj, err)
	}
	switch j := js.FromJSON(p.Jest).(type) {
	case nil:
		return nil, nil
	case *js.Object:
		return j, nil
	default:
		return nil, fmt.Errorf(`%s: "jest" is not an object`, pj)
	}
}

func jestDiscovery(file string, obj *js.Object, s *Suite) (Discovery, error) {
	if obj == nil {
		return Discovery{}, nil
	}
	label := file
	if path.Base(file) == "package.json" {
		label = file + ` "jest"`
	}
	dir := path.Dir(file)
	if file == "--config" {
		dir = s.Root
	}
	vals := map[string]any{}
	for _, k := range []string{"rootDir", "roots", "testMatch", "testRegex", "testPathIgnorePatterns", "projects"} {
		v, err := get(label, obj, k)
		if err != nil {
			return Discovery{}, err
		}
		if v != nil {
			vals[k] = v
		}
	}
	if p, ok := vals["projects"].([]any); ok && len(p) > 0 || vals["projects"] != nil && !ok {
		return Discovery{}, fmt.Errorf("%s: jest projects are not read", label)
	}
	if len(vals) == 0 {
		return Discovery{}, nil
	}
	m := Matcher{Base: dir}
	var keys []string
	if v, ok := vals["rootDir"]; ok {
		r, isStr := v.(string)
		base, inRepo := repoRel(dir, r)
		if !isStr || !inRepo {
			return Discovery{}, fmt.Errorf("%s: rootDir %v is outside the repository", label, v)
		}
		m.Base = base
		keys = append(keys, "rootDir")
	}
	rootDir := func(p string) (string, bool) {
		p = strings.ReplaceAll(p, "<rootDir>", "")
		p = strings.TrimPrefix(strings.TrimPrefix(p, "/"), "./")
		if p == "" {
			p = "."
		}
		return p, !strings.HasPrefix(path.Clean(p), "..")
	}
	if v, ok := vals["roots"]; ok {
		roots, err := stringList(label, "roots", v, nil)
		if err != nil {
			return Discovery{}, err
		}
		for _, r := range roots {
			if strings.HasPrefix(r, "/") {
				return Discovery{}, fmt.Errorf("%s: root %q is absolute", label, r)
			}
			rr, ok := rootDir(r)
			if !ok {
				return Discovery{}, fmt.Errorf("%s: root %q is outside rootDir", label, r)
			}
			m.Roots = append(m.Roots, path.Clean(rr))
		}
		keys = append(keys, "roots")
	}
	var exclude []string
	if v, ok := vals["testRegex"]; ok {
		var res []*regexp.Regexp
		list, isList := v.([]any)
		if !isList {
			list = []any{v}
		}
		for _, e := range list {
			src, flags := "", ""
			switch x := e.(type) {
			case string:
				src = x
			case js.Regex:
				src, flags = x.Source, x.Flags
			default:
				return Discovery{}, errNotLiteral(label, "testRegex", e)
			}
			re, ok := jsRegex(src, flags)
			if !ok {
				return Discovery{}, fmt.Errorf("%s: testRegex %q uses syntax Go's regexp lacks", label, src)
			}
			res = append(res, re)
		}
		if len(res) > 0 {
			m.TestRegex = res
			exclude = jsOutputExclude
			keys = append(keys, "testRegex")
		}
	}
	if v, ok := vals["testMatch"]; ok && len(m.TestRegex) == 0 {
		globs, err := stringList(label, "testMatch", v, nil)
		if err != nil {
			return Discovery{}, err
		}
		for _, g := range globs {
			neg := strings.HasPrefix(g, "!")
			g = strings.TrimPrefix(g, "!")
			switch {
			case strings.HasPrefix(g, "<rootDir>"):
				g = strings.TrimPrefix(strings.TrimPrefix(g, "<rootDir>"), "/")
			case strings.HasPrefix(g, "/"):
				return Discovery{}, fmt.Errorf("%s: testMatch %q is an absolute path", label, g)
			}
			conv, ok := micromatchToGlob(strings.TrimPrefix(g, "./"))
			if !ok {
				return Discovery{}, fmt.Errorf("%s: testMatch %q uses glob syntax smates cannot convert", label, g)
			}
			if neg {
				m.Exclude = append(m.Exclude, conv)
			} else {
				m.Tests = append(m.Tests, conv)
			}
		}
		exclude = jsOutputExclude
		keys = append(keys, "testMatch")
	}
	if len(m.TestRegex) == 0 && len(m.Tests) == 0 {
		// Jest's default testMatch.
		m.Tests = []string{"**/__tests__/**/*." + jsTestExts, "**/*.{test,spec}." + jsTestExts}
	}
	if v, ok := vals["testPathIgnorePatterns"]; ok {
		pats, err := stringList(label, "testPathIgnorePatterns", v, nil)
		if err != nil {
			return Discovery{}, err
		}
		for _, p := range pats {
			re, ok := jsRegex(strings.ReplaceAll(p, "<rootDir>", ""), "")
			if !ok {
				return Discovery{}, fmt.Errorf("%s: testPathIgnorePatterns %q uses syntax Go's regexp lacks", label, p)
			}
			m.IgnoreRegex = append(m.IgnoreRegex, re)
		}
		keys = append(keys, "testPathIgnorePatterns")
	}
	return Discovery{Matchers: []Matcher{m}, Source: label + " " + strings.Join(keys, ", "), Exclude: exclude}, nil
}

// ---- vitest ----

// Vitest's defaults (configDefaults.include / configDefaults.exclude; the exclude list
// of Vitest 2, a superset of later versions').
var (
	vitestInclude = []string{"**/*.{test,spec}." + jsTestExts}
	vitestExclude = []string{"**/node_modules/**", "**/dist/**", "**/cypress/**", "**/.{idea,git,cache,output,temp}/**",
		"**/{karma,rollup,webpack,vite,vitest,jest,ava,babel,nyc,cypress,tsup,build,eslint,prettier}.config.*"}
)

// vitestSpread expands spreads of vitest's exported defaults in include/exclude arrays.
func vitestSpread(expr string) ([]string, bool) {
	switch expr {
	case "configDefaults.include", "defaultInclude":
		return vitestInclude, true
	case "configDefaults.exclude", "defaultExclude":
		return vitestExclude, true
	}
	return nil, false
}

func vitestConfigNames() []string {
	var names []string
	for _, b := range []string{"vitest.config.", "vite.config."} {
		for _, e := range jsConfigExts {
			names = append(names, b+e)
		}
	}
	return names
}

func vitestWorkspaceNames() []string {
	var names []string
	for _, e := range append(jsConfigExts, "json") {
		names = append(names, "vitest.workspace."+e)
	}
	return names
}

// Discover reads test.include, test.exclude, test.dir, root and projects (test.projects,
// test.workspace or a vitest.workspace file) from the vitest configuration: --config
// of the suite command, else the nearest vitest.config.* / vite.config.* from the suite
// directory up. Projects are united into one suite: each contributes its own include
// and exclude patterns below its root.
func (vitest) Discover(repo *gitx.Repo, s *Suite) Discovery {
	d, err := vitestDiscover(repo, s)
	if err != nil {
		// With matchers, the error concerns one project, which keeps the defaults.
		scope := ""
		if len(d.Matchers) > 0 {
			scope = " for that project"
		}
		d.Warning = fmt.Sprintf("%v: using the default test patterns%s (set tests: in smates.yml if they differ)", err, scope)
	}
	return d
}

func vitestDiscover(repo *gitx.Repo, s *Suite) (Discovery, error) {
	for _, f := range []string{"--root", "-r", "--dir", "--workspace", "--project"} {
		if _, ok := commandFlag(s, f); ok {
			return Discovery{}, fmt.Errorf("the suite command sets %s", f)
		}
	}
	file := ""
	if c, ok := commandFlag(s, "--config", "-c"); ok {
		f, inRepo := repoRel(s.Root, c)
		if !inRepo || c == "" {
			return Discovery{}, fmt.Errorf("the suite command's --config %q cannot be read", c)
		}
		file = f
	} else {
		file = findUp(repo, s.Root, vitestConfigNames(), nil)
	}
	var obj *js.Object
	if file != "" {
		var err error
		if obj, err = loadJSConfig(repo, file); err != nil {
			return Discovery{}, err
		}
	}
	// The project root: `root` resolves against the working directory (the suite root).
	root := s.Root
	var keys []string
	test := &js.Object{}
	if obj != nil {
		v, err := get(file, obj, "root")
		if err != nil {
			return Discovery{}, err
		}
		if r, ok := v.(string); ok {
			if root, ok = repoRel(s.Root, r); !ok {
				return Discovery{}, fmt.Errorf("%s: root %q is outside the repository", file, r)
			}
			keys = append(keys, "root")
		} else if v != nil {
			return Discovery{}, errNotLiteral(file, "root", v)
		}
		tv, err := get(file, obj, "test")
		if err != nil {
			return Discovery{}, err
		}
		if tv != nil {
			if test, _ = tv.(*js.Object); test == nil {
				return Discovery{}, errNotLiteral(file, "test", tv)
			}
		}
	}
	// Projects: test.projects (Vitest 3.2+), test.workspace (3.0) or a workspace file.
	var projects []any
	projFile, projKey := file, ""
	for _, k := range []string{"projects", "workspace"} {
		v, err := get(file, test, "test."+k)
		if err != nil {
			return Discovery{}, err
		}
		if v == nil {
			continue
		}
		if ws, ok := v.(string); ok && k == "workspace" {
			// A path to a workspace file.
			f, inRepo := repoRel(root, ws)
			if !inRepo {
				return Discovery{}, fmt.Errorf("%s: test.workspace %q is outside the repository", file, ws)
			}
			wo, err := loadJSConfig(repo, f)
			if err != nil {
				return Discovery{}, err
			}
			v, _ = wo.Get("projects")
			projFile, projKey = f, ""
		} else {
			projKey = "test." + k
		}
		list, ok := v.([]any)
		if !ok {
			return Discovery{}, errNotLiteral(file, "test."+k, v)
		}
		projects = list
		break
	}
	if projects == nil {
		wsDir := root
		if file != "" {
			wsDir = path.Dir(file)
		}
		if wf := findUp(repo, wsDir, vitestWorkspaceNames(), func(string) bool { return true }); wf != "" {
			wo, err := loadJSConfig(repo, wf)
			if err != nil {
				return Discovery{}, err
			}
			v, _ := wo.Get("projects")
			list, ok := v.([]any)
			if !ok {
				return Discovery{}, errNotLiteral(wf, "the project list", v)
			}
			projects, projFile, projKey = list, wf, ""
		}
	}
	if projects != nil {
		return vitestProjects(repo, root, projFile, projKey, test, projects)
	}
	m, k, err := vitestMatcher(file, root, test)
	if err != nil {
		return Discovery{}, err
	}
	keys = append(keys, k...)
	if len(keys) == 0 {
		return Discovery{}, nil
	}
	return Discovery{Matchers: []Matcher{m}, Source: file + " " + strings.Join(keys, ", ")}, nil
}

// vitestMatcher builds the matcher of one project from its test options; keys lists
// the options that were set.
func vitestMatcher(file, root string, test *js.Object) (Matcher, []string, error) {
	m := Matcher{Base: root}
	var keys []string
	for _, k := range []string{"root", "dir"} {
		v, err := get(file, test, "test."+k)
		if err != nil {
			return m, nil, err
		}
		if v == nil {
			continue
		}
		r, ok := v.(string)
		if !ok {
			return m, nil, errNotLiteral(file, "test."+k, v)
		}
		if m.Base, ok = repoRel(m.Base, r); !ok {
			return m, nil, fmt.Errorf("%s: test.%s %q is outside the repository", file, k, r)
		}
		keys = append(keys, "test."+k)
	}
	conv := func(key string) ([]string, error) {
		v, err := get(file, test, "test."+key)
		if err != nil {
			return nil, err
		}
		globs, err := stringList(file, "test."+key, v, vitestSpread)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, g := range globs {
			c, ok := micromatchToGlob(strings.TrimPrefix(g, "./"))
			if !ok || strings.HasPrefix(g, "/") {
				return nil, fmt.Errorf("%s: test.%s %q uses glob syntax smates cannot convert", file, key, g)
			}
			out = append(out, c)
		}
		if v != nil {
			keys = append(keys, "test."+key)
		}
		return out, nil
	}
	var err error
	if m.Tests, err = conv("include"); err != nil {
		return m, nil, err
	}
	if m.Exclude, err = conv("exclude"); err != nil {
		return m, nil, err
	}
	if len(m.Tests) == 0 {
		m.Tests = vitestInclude
	}
	return m, keys, nil
}

// vitestProjects unites the projects of a workspace: strings are globs of project
// directories or config files (relative to root), objects inline project configs.
func vitestProjects(repo *gitx.Repo, root, file, key string, rootTest *js.Object, projects []any) (Discovery, error) {
	label := file
	if key != "" {
		label += " " + key
	}
	var ms []Matcher
	var warn error
	add := func(m Matcher, err error) {
		if err != nil && warn == nil {
			warn = err
		}
		ms = append(ms, m)
	}
	base := path.Dir(file)
	if key != "" {
		base = root
	}
	fsys := os.DirFS(repo.Root)
	for _, p := range projects {
		switch x := p.(type) {
		case string:
			if strings.HasPrefix(x, "!") {
				continue // negated globs exclude matches of the others; rarely used
			}
			pat, ok := repoRel(base, x)
			if !ok {
				return Discovery{}, fmt.Errorf("%s: project %q is outside the repository", label, x)
			}
			matches, err := doublestar.Glob(fsys, pat)
			if err != nil {
				return Discovery{}, fmt.Errorf("%s: project %q: %v", label, x, err)
			}
			sort.Strings(matches)
			for _, mt := range matches {
				if pathutil.IsVendored(mt) {
					continue
				}
				fi, err := os.Stat(filepath.Join(repo.Root, filepath.FromSlash(mt)))
				if err != nil {
					continue
				}
				cfg := ""
				if fi.IsDir() {
					cfg = findUp(repo, mt, vitestConfigNames(), func(string) bool { return true })
				} else {
					cfg = mt
				}
				if cfg == "" {
					ms = append(ms, Matcher{Base: mt, Tests: vitestInclude})
					continue
				}
				add(vitestProjectFile(repo, cfg))
			}
		case *js.Object:
			pr := root
			t := &js.Object{}
			if tv, err := get(label, x, "test"); err != nil {
				return Discovery{}, err
			} else if tv != nil {
				if t, _ = tv.(*js.Object); t == nil {
					return Discovery{}, errNotLiteral(label, "a project's test", tv)
				}
			}
			if ext, _ := x.Get("extends"); ext == true {
				// The project inherits the root config's test options.
				merged := &js.Object{Fields: map[string]any{}, Spread: rootTest.Spread || t.Spread}
				for k, v := range rootTest.Fields {
					if k != "projects" && k != "workspace" {
						merged.Fields[k] = v
					}
				}
				for k, v := range t.Fields {
					merged.Fields[k] = v
				}
				t = merged
			} else if ext != nil {
				return Discovery{}, fmt.Errorf("%s: a project extends another config file", label)
			}
			if r, err := get(label, x, "root"); err != nil {
				return Discovery{}, err
			} else if rs, ok := r.(string); ok {
				if pr, ok = repoRel(root, rs); !ok {
					return Discovery{}, fmt.Errorf("%s: project root %q is outside the repository", label, rs)
				}
			}
			m, _, err := vitestMatcher(label, pr, t)
			if err != nil {
				return Discovery{}, err
			}
			ms = append(ms, m)
		default:
			return Discovery{}, errNotLiteral(label, "a project", p)
		}
	}
	if len(ms) == 0 {
		return Discovery{}, fmt.Errorf("%s: no project found", label)
	}
	return Discovery{Matchers: ms, Source: fmt.Sprintf("%s (%d projects)", label, len(ms))}, warn
}

// vitestProjectFile reads the test options of a project config file; its root is the
// file's directory unless it sets one. An unreadable file yields the defaults for its
// directory and the reason.
func vitestProjectFile(repo *gitx.Repo, cfg string) (Matcher, error) {
	dir := path.Dir(cfg)
	def := Matcher{Base: dir, Tests: vitestInclude}
	obj, err := loadJSConfig(repo, cfg)
	if err != nil {
		return def, err
	}
	root := dir
	if v, err := get(cfg, obj, "root"); err != nil {
		return def, err
	} else if r, ok := v.(string); ok {
		if root, ok = repoRel(dir, r); !ok {
			return def, fmt.Errorf("%s: root %q is outside the repository", cfg, r)
		}
	}
	t := &js.Object{}
	if tv, err := get(cfg, obj, "test"); err != nil {
		return def, err
	} else if tv != nil {
		if t, _ = tv.(*js.Object); t == nil {
			return def, errNotLiteral(cfg, "test", tv)
		}
	}
	m, _, err := vitestMatcher(cfg, root, t)
	if err != nil {
		return def, err
	}
	return m, nil
}
