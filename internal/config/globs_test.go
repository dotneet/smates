package config

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// globFieldCase places one value in one glob list of a config and reads it back.
type globFieldCase struct {
	name string
	doc  func(v string) string
	get  func(c *Config) string
	// kind of the list, which decides the expected outcome of each variant.
	kind globKind
}

func suiteDoc(body string) string {
	return "suites:\n  - name: api\n    runner: go\n  - name: web\n    runner: vitest\n    root: web\n" + body
}

func globFieldCases() []globFieldCase {
	q := strconv.Quote
	web := func(c *Config) *Suite { return &c.Suites[1] }
	return []globFieldCase{
		{"ignore", func(v string) string { return "ignore: [" + q(v) + "]\n" }, func(c *Config) string { return c.Ignore[0] }, repoGlob},
		{"inputs", func(v string) string { return "inputs: [" + q(v) + "]\n" }, func(c *Config) string { return c.Inputs[0] }, repoGlob},
		{"env_ignore", func(v string) string { return "env_ignore: [" + q(v) + "]\n" }, func(c *Config) string { return c.EnvIgnore[0] }, envPattern},
		{"env_track", func(v string) string { return "env_track: [" + q(v) + "]\n" }, func(c *Config) string { return c.EnvTrack[0] }, envPattern},
		{"suite tests", func(v string) string { return suiteDoc("    tests: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).Tests[0] }, rootGlob},
		{"suite exclude", func(v string) string { return suiteDoc("    exclude: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).Exclude[0] }, rootGlob},
		{"suite inputs", func(v string) string { return suiteDoc("    inputs: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).Inputs[0] }, inputGlob},
		{"suite inputs repo-relative", func(v string) string { return suiteDoc("    inputs: [" + q("/"+v) + "]\n") }, func(c *Config) string { return strings.TrimPrefix(web(c).Inputs[0], "/") }, repoGlob},
		{"suite python_roots", func(v string) string { return suiteDoc("    python_roots: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).PythonRoots[0] }, rootPath},
		{"suite env_ignore", func(v string) string { return suiteDoc("    env_ignore: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).EnvIgnore[0] }, envPattern},
		{"suite env_track", func(v string) string { return suiteDoc("    env_track: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).EnvTrack[0] }, envPattern},
		{"suite volatile", func(v string) string { return suiteDoc("    volatile: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).Volatile[0] }, rootGlob},
		{"suite untraced", func(v string) string { return suiteDoc("    untraced: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).Untraced[0] }, rootGlob},
		{"suite serves", func(v string) string { return suiteDoc("    serves: [" + q(v) + "]\n") }, func(c *Config) string { return web(c).Serves[0] }, repoGlob},
		{"rule tests", func(v string) string { return suiteDoc("    rules: [{tests: [" + q(v) + "], deps: [m]}]\n") }, func(c *Config) string { return web(c).Rules[0].Tests[0] }, rootGlob},
		{"rule deps", func(v string) string { return suiteDoc("    rules: [{tests: [a], deps: [" + q(v) + "]}]\n") }, func(c *Config) string { return web(c).Rules[0].Deps[0] }, repoGlob},
		{"rule reaches", func(v string) string { return suiteDoc("    rules: [{reaches: [" + q(v) + "], deps: [m]}]\n") }, func(c *Config) string { return web(c).Rules[0].Reaches[0] }, repoGlob},
		{"rule reaches variable", func(v string) string { return suiteDoc("    rules: [{reaches: [" + q("$"+v) + "], deps: [m]}]\n") }, func(c *Config) string { return web(c).Rules[0].Reaches[0][1:] }, envPattern},
		{"requires tests", func(v string) string { return suiteDoc("    requires: [{env: A, tests: [" + q(v) + "]}]\n") }, func(c *Config) string { return web(c).Requires[0].Tests[0] }, rootGlob},
		{"depends_on paths", func(v string) string { return suiteDoc("    depends_on: [{paths: [" + q(v) + "]}]\n") }, func(c *Config) string { return web(c).DependsOn[0].Paths[0] }, repoGlob},
		{"depends_on tests", func(v string) string { return suiteDoc("    depends_on: [{suite: api, tests: [" + q(v) + "]}]\n") }, func(c *Config) string { return web(c).DependsOn[0].Tests[0] }, rootGlob},
	}
}

// TestGlobFields: every glob list of the config is normalized and checked the same
// way. A glob that starts with "./" or is invalid used to be kept as written in some
// lists (the top-level inputs among them), where it matched nothing: a changed lockfile
// declared as "./pnpm-lock.yaml" selected no test.
func TestGlobFields(t *testing.T) {
	type variant struct {
		name, value string
		// want: the normalized value per kind; wantErr: the error per kind ("" none).
		want    map[globKind]string
		wantErr map[globKind]string
	}
	all := func(v string) map[globKind]string {
		return map[globKind]string{repoGlob: v, rootGlob: v, inputGlob: v, rootPath: v, envPattern: v}
	}
	variants := []variant{
		{"plain", "lib/**", all("lib/**"), nil},
		// Variable names have no "./": the pattern is kept as written.
		{"dot slash", "./lib/**",
			map[globKind]string{repoGlob: "lib/**", rootGlob: "lib/**", inputGlob: "lib/**", rootPath: "lib/**", envPattern: "./lib/**"}, nil},
		{"repeated dot slash", "././pnpm-lock.yaml",
			map[globKind]string{repoGlob: "pnpm-lock.yaml", rootGlob: "pnpm-lock.yaml", inputGlob: "pnpm-lock.yaml", rootPath: "pnpm-lock.yaml", envPattern: "././pnpm-lock.yaml"}, nil},
		{"invalid glob", "lib/[",
			map[globKind]string{rootPath: "lib/["},
			map[globKind]string{repoGlob: "bad glob", rootGlob: "bad glob", inputGlob: "bad glob", envPattern: "bad environment variable pattern"}},
		{"empty", "",
			map[globKind]string{rootPath: "."},
			map[globKind]string{repoGlob: "bad glob", rootGlob: "bad glob", inputGlob: "bad glob", envPattern: "bad environment variable pattern"}},
		{"dot slash only", "./",
			map[globKind]string{rootPath: ".", envPattern: "./"},
			map[globKind]string{repoGlob: "bad glob", rootGlob: "bad glob", inputGlob: "bad glob"}},
		{"windows separator", `lib\core.py`,
			map[globKind]string{envPattern: `lib\core.py`},
			map[globKind]string{repoGlob: "path separator", rootGlob: "path separator", inputGlob: "path separator", rootPath: "path separator"}},
		{"trailing backslash", `lib\`,
			nil,
			map[globKind]string{repoGlob: "path separator", rootGlob: "path separator", inputGlob: "path separator", rootPath: "path separator", envPattern: "bad environment variable pattern"}},
		{"parent", "../shared/**",
			map[globKind]string{rootGlob: "../shared/**", rootPath: "../shared/**", envPattern: "../shared/**"},
			map[globKind]string{repoGlob: "relative to the repository root", inputGlob: "outside the suite root"}},
	}
	for _, f := range globFieldCases() {
		for _, v := range variants {
			t.Run(f.name+"/"+v.name, func(t *testing.T) {
				c, err := Parse([]byte(f.doc(v.value)))
				wantErr := v.wantErr[f.kind]
				switch {
				case wantErr != "":
					if err == nil || !strings.Contains(err.Error(), wantErr) {
						t.Fatalf("Parse(%q): err %v, want %q", v.value, err, wantErr)
					}
				case err != nil:
					t.Fatalf("Parse(%q): %v", v.value, err)
				default:
					if got, want := f.get(c), v.want[f.kind]; got != want {
						t.Fatalf("Parse(%q) = %q, want %q", v.value, got, want)
					}
				}
			})
		}
		// An escaped metacharacter, the only way to name a directory with brackets in
		// its name (`app/[id]`): kept as written by every glob list (and by path.Match
		// patterns); python_roots holds paths, where a backslash is a separator.
		t.Run(f.name+"/escape", func(t *testing.T) {
			const glob = `app/\[id\]/**`
			c, err := Parse([]byte(f.doc(glob)))
			switch {
			case f.kind == rootPath:
				if err == nil || !strings.Contains(err.Error(), "path separator") {
					t.Fatalf("err %v, want a path separator error", err)
				}
			case err != nil:
				t.Fatal(err)
			case f.get(c) != glob:
				t.Fatalf("got %q, want %q", f.get(c), glob)
			}
		})
		// A backslash before a wildcard: a Windows separator (`src\**`) everywhere but
		// in the top-level ignore and inputs, which `smates orphans` fills with globs
		// that escape every metacharacter of a file name.
		t.Run(f.name+"/escaped wildcard", func(t *testing.T) {
			const glob = `src\**`
			c, err := Parse([]byte(f.doc(glob)))
			switch {
			case f.kind == envPattern || f.name == "ignore" || f.name == "inputs":
				if err != nil {
					t.Fatal(err)
				}
				if f.kind != envPattern && f.get(c) != glob {
					t.Fatalf("got %q, want %q", f.get(c), glob)
				}
			case err == nil || !strings.Contains(err.Error(), "path separator"):
				t.Fatalf("err %v, want a path separator error", err)
			}
		})
	}
}

// TestGlobFieldsListed: every list of strings of Config and Suite (and of their
// entries) is in globFields or listed here as not a glob, so that a list added later
// cannot skip the checks.
func TestGlobFieldsListed(t *testing.T) {
	notGlobs := map[string]bool{"full_run_branches": true}
	c, err := Parse([]byte(suiteDoc("    rules: [{tests: [a], reaches: [b], deps: [c]}]\n    requires: [{env: A}]\n    depends_on: [api]\n")))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range c.globFields() {
		have[f.where] = true
	}
	check := func(typ reflect.Type, prefix string) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			key, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if f.Type != reflect.TypeOf([]string(nil)) || key == "-" || notGlobs[key] {
				continue
			}
			if !have[prefix+key] {
				t.Errorf("%s%s is not in globFields", prefix, key)
			}
		}
	}
	check(reflect.TypeOf(Config{}), "")
	check(reflect.TypeOf(Suite{}), `suite "web": `)
	check(reflect.TypeOf(Rule{}), `suite "web": rules[0]: `)
	check(reflect.TypeOf(Requirement{}), `suite "web": requires[0]: `)
	check(reflect.TypeOf(Dependency{}), `suite "web": depends_on[0]: `)
}

// The error for a suite input outside the suite root suggests the repo-relative form,
// and that form is kept with its "/".
func TestSuiteInputRepoRelative(t *testing.T) {
	c, err := Parse([]byte(suiteDoc("    inputs: [\"/./pnpm-lock.yaml\", \"./vite.config.ts\"]\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Suites[1].Inputs; got[0] != "/pnpm-lock.yaml" || got[1] != "vite.config.ts" {
		t.Errorf("inputs = %q", got)
	}
}
