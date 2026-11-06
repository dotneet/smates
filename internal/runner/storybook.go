package runner

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/dotneet/smates/internal/static/js"
)

// Storybook's Vitest plugin (storybookTest from @storybook/addon-vitest) turns every
// story into a test: it reads the stories globs of <configDir>/main.* and adds them to
// the include of the project it is a plugin of, so the Vitest configuration never
// names the story files. smates reads them from main.* itself. Stories run in a
// browser, which no trace sees: their dependencies are their static import closure,
// the configuration directory (main.*, preview.*, preview-head.html, mocks), the
// closure of preview.* (decorators, global styles) and the static directories.

// storybookModuleRe matches an import of Storybook's Vitest plugin.
var storybookModuleRe = regexp.MustCompile(`["']@storybook/(?:addon-vitest|experimental-addon-test)/vitest-plugin["']`)

// storybookImportRe matches a named import of the plugin, to find a local alias
// (import { storybookTest as sb } from ...).
var storybookImportRe = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*["']@storybook/(?:addon-vitest|experimental-addon-test)/vitest-plugin["']`)

// storyFallback are the story files of a Storybook project whose stories smates
// cannot read, relative to the project root.
const storyFallback = "**/*.stories.{js,jsx,mjs,ts,tsx}"

// storyDefaultFiles is Storybook's files pattern of a stories directory given
// without one (a string naming a directory, or a specifier without files).
const storyDefaultFiles = "**/*.@(mdx|stories.@(js|jsx|mjs|ts|tsx))"

// storybookNames returns the local names Storybook's Vitest plugin may be called by
// in configuration source, and whether the source imports it.
func storybookNames(src []byte) (names []string, imported bool) {
	names = []string{"storybookTest"}
	for _, m := range storybookImportRe.FindAllSubmatch(src, -1) {
		for _, spec := range strings.Split(string(m[1]), ",") {
			if orig, local, ok := strings.Cut(strings.TrimSpace(spec), " as "); ok && strings.TrimSpace(orig) == "storybookTest" {
				names = append(names, strings.TrimSpace(local))
			}
		}
	}
	return names, storybookModuleRe.Match(src)
}

// storybookDir is the configuration directory of one use of Storybook's Vitest plugin
// (repo-relative), and why the stories' dependencies may not all be known there
// (see Matcher.Unsure; "" when they are).
type storybookDir struct {
	dir, unsure string
}

// storybookDirs finds the calls of Storybook's Vitest plugin in configuration source
// (calls, a part of it such as an inline project's plugins, searched for names; see
// storybookNames) and returns the configuration directory of each, repo-relative:
// the configDir option, else .storybook. Storybook resolves a literal (and its
// default) against its working directory, which is cwd, the directory vitest runs in
// (the suite root), and path.join / path.resolve of the configuration file's
// directory (see js.PathValue) against dir, the directory of file. found reports
// whether the plugin is used; err says why the directory of some use cannot be told
// (a computed configDir, or an import with no call smates sees).
func storybookDirs(file, dir, cwd string, calls []byte, names []string, imported bool) (dirs []storybookDir, found bool, err error) {
	for _, name := range names {
		for _, arg := range js.CallArgs(calls, name) {
			found = true
			d, e := storybookConfigDir(file, dir, cwd, arg)
			if e != nil {
				if err == nil {
					err = fmt.Errorf("%s: %v", file, e)
				}
				continue
			}
			dirs = append(dirs, d)
		}
	}
	if imported && !found {
		return nil, true, fmt.Errorf("%s imports Storybook's Vitest plugin but calls it in a way smates cannot read", file)
	}
	return dirs, found, err
}

// storybookConfigDir evaluates the options of one storybookTest call of file (in
// directory dir; see storybookDirs for cwd). A directory resolved against cwd while
// file is elsewhere is unsure: a wrapper may start vitest in another directory, and
// the configuration's author may have meant the one next to the file.
func storybookConfigDir(file, dir, cwd string, arg any) (storybookDir, error) {
	var v any
	switch a := arg.(type) {
	case nil:
	case *js.Object:
		cd, known := a.Get("configDir")
		if !known {
			return storybookDir{}, fmt.Errorf("Storybook's configDir may come from a ...spread")
		}
		v = cd
	default:
		return storybookDir{}, fmt.Errorf("Storybook's plugin options are not a literal (%s)", short(arg))
	}
	fromCwd := func(p string) (storybookDir, bool) {
		d, ok := repoRel(cwd, p)
		if !ok {
			return storybookDir{}, false
		}
		out := storybookDir{dir: d}
		if cwd != dir && !path.IsAbs(p) {
			out.unsure = fmt.Sprintf("%s: Storybook resolves configDir %q against the directory vitest runs in (%s), not %s's", file, p, cwd, file)
		}
		return out, true
	}
	switch x := v.(type) {
	case nil:
		if d, ok := fromCwd(".storybook"); ok {
			return d, nil
		}
	case string:
		if d, ok := fromCwd(x); ok {
			return d, nil
		}
	case js.Unknown:
		if p, fromFile, ok := js.PathValue(string(x)); ok {
			if !fromFile {
				if d, ok := fromCwd(p); ok {
					return d, nil
				}
			} else if d, ok := repoRel(dir, p); ok {
				return storybookDir{dir: d}, nil
			}
		}
	}
	return storybookDir{}, fmt.Errorf("Storybook's configDir is not a path smates can read (%s)", short(v))
}

// storybookMain is what main.* of a Storybook configuration directory says.
type storybookMain struct {
	file string // "" when there is none
	src  []byte
	obj  *js.Object
	err  error // why main.* cannot be read
}

// storybookConfigExts are the extensions Storybook looks main.* and preview.* up by.
var storybookConfigExts = []string{"ts", "js", "mjs", "cjs", "mts", "cts", "tsx", "jsx"}

// readStorybookMain reads <dir>/main.* through read (nil: missing).
func readStorybookMain(dir string, read func(string) []byte) storybookMain {
	for _, e := range storybookConfigExts {
		f := path.Join(dir, "main."+e)
		src := read(f)
		if src == nil {
			continue
		}
		v, ok := js.ConfigObject(src)
		o, isObj := v.(*js.Object)
		switch {
		case !ok:
			return storybookMain{file: f, err: fmt.Errorf("%s has no `export default` or `module.exports =`", f)}
		case !isObj:
			return storybookMain{file: f, err: fmt.Errorf("%s does not export an object literal (%s)", f, short(v))}
		}
		return storybookMain{file: f, src: src, obj: o}
	}
	return storybookMain{err: fmt.Errorf("no main.* in Storybook's configuration directory %s", dir)}
}

// storybookMatchers returns the test files of the Storybook projects whose
// configuration (file, a project rooted at root) is src, or the part of it in calls
// (nil: all of it):
// one matcher per stories entry of each configuration directory's main.*. A project
// whose stories cannot be read gets the conventional story files under root
// (storyFallback), and is run whole whenever one of them is selected; err says why.
func storybookMatchers(file, root, cwd string, src, calls []byte, read func(string) []byte) ([]Matcher, error) {
	names, imported := storybookNames(src)
	if calls == nil {
		calls = src
	} else {
		imported = false // the import may serve another part of the file
	}
	dirs, found, err := storybookDirs(file, path.Dir(file), cwd, calls, names, imported)
	if !found {
		return nil, nil
	}
	var ms []Matcher
	if err != nil {
		ms = append(ms, Matcher{Base: root, Tests: []string{storyFallback}, Browser: true, Whole: true, Unsure: err.Error()})
	}
	for _, d := range dirs {
		dm, e := storybookDirMatchers(d.dir, root, read)
		for i := range dm {
			if dm[i].Unsure == "" {
				dm[i].Unsure = d.unsure
			}
		}
		ms = append(ms, dm...)
		if e != nil && err == nil {
			err = e
		}
	}
	if err != nil {
		err = &storiesError{err}
	}
	return ms, err
}

// storiesError is why the stories of a Storybook project cannot be read.
type storiesError struct{ err error }

func (e *storiesError) Error() string {
	return fmt.Sprintf("%v: using %s for its stories, and running the whole suite whenever one of them is selected", e.err, storyFallback)
}

func (e *storiesError) Unwrap() error { return e.err }

// browserEnabled reports whether a project's test options may run its tests in a
// browser: test.browser is set and not turned off (enabled: false), or unknown (src,
// the configuration's source, then decides, see mentionsBrowser).
func browserEnabled(test *js.Object, src []byte) bool {
	v, known := test.Get("browser")
	if !known {
		return mentionsBrowser(src)
	}
	switch b := v.(type) {
	case nil, bool:
		return b == true
	case *js.Object:
		e, known := b.Get("enabled")
		return !known || e != false
	}
	return true
}

// pluginText returns the source of the calls in a plugins value (a list of plugin
// calls, which are not literals), to search for Storybook's plugin.
func pluginText(v any) []byte {
	var b strings.Builder
	switch x := v.(type) {
	case js.Unknown:
		b.WriteString(string(x))
	case []any:
		for _, e := range x {
			if u, ok := e.(js.Unknown); ok {
				b.WriteString(strings.TrimPrefix(string(u), "..."))
				b.WriteString(";\n")
			}
		}
	}
	return []byte(b.String())
}

// mainLoadsRe matches what makes main.* load files for the stories outside the
// configuration directory that smates cannot follow: Vite settings of its own
// (viteFinal, which may add plugins or aliases that replace modules), and preview
// annotations of addons or presets.
var mainLoadsRe = regexp.MustCompile(`\b(?:viteFinal|webpackFinal|previewAnnotations|managerEntries)\b`)

// relativeRe matches a string literal naming a relative path.
var relativeRe = regexp.MustCompile(`["'` + "`" + `]\.\.?/`)

// storybookMainUnsure says why main.* may make the stories depend on files smates
// does not know of ("" when it cannot): see mainLoadsRe, and addons or a framework
// given by relative path (a local preset, whose own settings smates does not read).
func storybookMainUnsure(main storybookMain) string {
	if m := mainLoadsRe.Find(main.src); m != nil {
		return fmt.Sprintf("%s sets %s, which may load files for the stories that smates cannot follow", main.file, m)
	}
	for _, key := range []string{"addons", "framework"} {
		v, known := main.obj.Get(key)
		if !known {
			return fmt.Sprintf("%s: its %s may come from a ...spread", main.file, key)
		}
		if mentionsRelative(v) || relativeRe.MatchString(unknownText(v)) {
			return fmt.Sprintf("%s names a local addon or preset in %s, whose settings smates does not read", main.file, key)
		}
	}
	return ""
}

// unknownText returns the source of the expressions in a value (its non-literal parts).
func unknownText(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case js.Unknown:
			b.WriteString(string(x))
			b.WriteByte('\n')
		case []any:
			for _, e := range x {
				walk(e)
			}
		case *js.Object:
			for _, e := range x.Fields {
				walk(e)
			}
		}
	}
	walk(v)
	return b.String()
}

// mentionsRelative reports whether a value holds a string that is a relative path.
func mentionsRelative(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.HasPrefix(x, "./") || strings.HasPrefix(x, "../")
	case []any:
		for _, e := range x {
			if mentionsRelative(e) {
				return true
			}
		}
	case *js.Object:
		for _, e := range x.Fields {
			if mentionsRelative(e) {
				return true
			}
		}
	}
	return false
}

// storybookDirMatchers returns the story matchers of the Storybook configuration
// directory dir, with the dependencies every story has: the directory itself, the
// static directories and the import closures of preview.* and main.* (what main.*
// imports, a mock module it aliases in, runs for every story).
func storybookDirMatchers(dir, root string, read func(string) []byte) ([]Matcher, error) {
	deps := []string{dir + "/**"}
	var starts []string
	for _, e := range storybookConfigExts {
		if f := path.Join(dir, "preview."+e); read(f) != nil {
			starts = append(starts, f)
		}
	}
	fallback := func(why error) ([]Matcher, error) {
		return []Matcher{{Base: root, Tests: []string{storyFallback}, Browser: true, Whole: true, Unsure: why.Error(),
			Deps: deps, Starts: starts}}, why
	}
	main := readStorybookMain(dir, read)
	if main.file != "" {
		starts = append(starts, main.file)
	}
	if main.err != nil {
		return fallback(main.err)
	}
	unsure := storybookMainUnsure(main)
	staticDirs, err := storybookStaticDirs(main, dir)
	if err != nil && unsure == "" {
		unsure = err.Error()
	}
	for _, d := range staticDirs {
		deps = append(deps, d+"/**")
	}
	stories, known := main.obj.Get("stories")
	list, isList := stories.([]any)
	if s, isStr := stories.(string); isStr {
		list, isList = []any{s}, true
	}
	if !known || !isList {
		return fallback(errNotLiteral(main.file, "stories", stories))
	}
	var ms []Matcher
	var warn error
	for _, e := range list {
		m, err := storyMatcher(main.file, dir, e)
		if err != nil {
			if warn == nil {
				warn = err
			}
			fm, _ := fallback(err)
			ms = append(ms, fm...)
			continue
		}
		m.Browser, m.Deps, m.Starts, m.Unsure = true, deps, starts, unsure
		ms = append(ms, m)
	}
	return ms, warn
}

// storybookStaticDirs returns the staticDirs of main.* (repo-relative): strings and
// {from, to} objects relative to the configuration directory.
func storybookStaticDirs(main storybookMain, dir string) ([]string, error) {
	v, known := main.obj.Get("staticDirs")
	if !known {
		return nil, errNotLiteral(main.file, "staticDirs", nil)
	}
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errNotLiteral(main.file, "staticDirs", v)
	}
	var out []string
	for _, e := range list {
		from := e
		if o, isObj := e.(*js.Object); isObj {
			from, _ = o.Get("from")
		}
		s, isStr := from.(string)
		if !isStr {
			return out, errNotLiteral(main.file, "staticDirs", e)
		}
		d, inRepo := repoRel(dir, s)
		if !inRepo {
			return out, fmt.Errorf("%s: static directory %q is outside the repository", main.file, s)
		}
		out = append(out, d)
	}
	return out, nil
}

// storyMatcher converts one stories entry of main.* (a glob relative to the
// configuration directory dir, a directory, or a {directory, files} specifier) into a
// matcher of the story files. MDX docs are not tests.
func storyMatcher(file, dir string, entry any) (Matcher, error) {
	var glob string
	switch e := entry.(type) {
	case string:
		glob = e
		last := path.Base(e)
		if !strings.ContainsAny(e, "*?[]{}()") && !strings.Contains(last, ".") {
			glob = strings.TrimSuffix(e, "/") + "/" + storyDefaultFiles // a directory
		}
	case *js.Object:
		d, dk := e.Get("directory")
		f, fk := e.Get("files")
		ds, dIsStr := d.(string)
		if !dk || !fk || !dIsStr {
			return Matcher{}, errNotLiteral(file, "a stories specifier", entry)
		}
		fs := storyDefaultFiles
		if f != nil {
			s, isStr := f.(string)
			if !isStr {
				return Matcher{}, errNotLiteral(file, "a stories specifier's files", f)
			}
			fs = s
		}
		glob = strings.TrimSuffix(ds, "/") + "/" + strings.TrimPrefix(fs, "./")
	default:
		return Matcher{}, errNotLiteral(file, "a stories entry", entry)
	}
	if strings.HasPrefix(glob, "!") || strings.HasPrefix(glob, "/") {
		return Matcher{}, fmt.Errorf("%s: stories entry %q cannot be read", file, glob)
	}
	full, inRepo := repoRel(dir, glob)
	if !inRepo {
		return Matcher{}, fmt.Errorf("%s: stories entry %q is outside the repository", file, glob)
	}
	// The directory before the first segment with glob syntax is the base.
	segs := strings.Split(full, "/")
	i := 0
	for i < len(segs)-1 && !strings.ContainsAny(segs[i], "*?[]{}()!@+") {
		i++
	}
	base, rest := strings.Join(segs[:i], "/"), strings.Join(segs[i:], "/")
	if base == "" {
		base = "."
	}
	conv, ok := micromatchToGlob(rest)
	if !ok {
		return Matcher{}, fmt.Errorf("%s: stories entry %q uses glob syntax smates cannot convert", file, glob)
	}
	return Matcher{Base: base, Tests: []string{conv}, Exclude: []string{"**/*.mdx"}}, nil
}
