package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
	"github.com/dotneet/smates/internal/trace"
)

// TestGradleRun runs the fixture build (testdata/gradle) with Gradle: selected classes
// only, per-class outcomes, a failure, a compile error. It needs gradle and a JDK on
// PATH, and JUnit from Maven Central (or Gradle's cache).
func TestGradleRun(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Gradle")
	}
	for _, tool := range []string{"gradle", "java"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	files := map[string]string{}
	src := filepath.Join("testdata", "gradle")
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(src, p)
		files[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	r := testutil.NewGitRepo(t, files)
	repo, err := gitx.Open(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	if _, err := trace.ExtractShims(shims); err != nil {
		t.Fatal(err)
	}
	s, err := NewSuite(repo, config.Suite{Name: "app", Runner: "gradle", Root: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Command != "gradle :app:test" {
		t.Errorf("command = %q", s.Command)
	}
	var all []string
	for f := range files {
		all = append(all, f)
	}
	units := s.Adapter.Units(s, s.TestFiles(all))
	const dir = "app/src/test/java/com/example/app/"
	cart, greeter, check, env := dir+"CartTest.java", dir+"GreeterTest.java", dir+"GreeterCheck.java", dir+"EnvironmentTest.java"
	catalog, migrations, plugins, shell := dir+"CatalogTest.java", dir+"MigrationsTest.java", dir+"PluginsTest.java", dir+"ShellTest.java"
	if want := []string{cart, catalog, env, check, greeter, migrations, plugins, shell}; !reflect.DeepEqual(units, want) {
		t.Fatalf("units = %v, want %v", units, want)
	}
	run := func(t *testing.T, units []string, all bool) (*RunResult, string) {
		t.Helper()
		var out strings.Builder
		rc := &RunContext{
			Repo: repo, Suite: s, Units: units, All: all, Record: true,
			TraceDir: t.TempDir(), ShimDir: shims, Stdout: &out, Stderr: &out,
			Known: func(string) []string { return nil },
		}
		res, err := s.Adapter.Run(context.Background(), rc)
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out.String())
		}
		return res, out.String()
	}
	statuses := func(res *RunResult) map[string]string {
		m := map[string]string{}
		for u, o := range res.Observations {
			m[u] = o.Status
		}
		return m
	}

	t.Run("all", func(t *testing.T) {
		res, out := run(t, units, true)
		want := map[string]string{cart: state.Pass, greeter: state.Pass, check: state.Pass, env: state.Unknown,
			catalog: state.Pass, migrations: state.Pass, plugins: state.Pass, shell: state.Pass}
		if got := statuses(res); res.ExitCode != 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("exit %d, statuses %v, want %v\n%s", res.ExitCode, got, want, out)
		}
		const main = "app/src/main/java/com/example/app/"
		// What each class was recorded to use, and what it must not depend on: one JVM
		// per class keeps them apart.
		for _, c := range []struct {
			unit      string
			want, not []string
		}{
			{cart, []string{main + "Cart.java", "lib/src/main/java/com/example/lib/Money.java", "lib/src/main/resources/**"}, []string{main + "Greeter.java"}},
			// The inlined constant's class is never loaded: the static closure has it.
			{greeter, []string{main + "Greeter.java"}, []string{dir + "support/Names.java", main + "Cart.java"}},
			{check, []string{main + "Greeter.java", dir + "support/BaseCheck.java"}, nil},
			{catalog, []string{main + "Catalog.java", "app/src/main/resources/catalog.txt", "app/src/main/resources/"}, nil},
			{plugins, []string{main + "plugins/Upper.java"}, nil},
			{migrations, []string{"app/data/migrations/001.sql", "app/data/migrations/002.sql", "app/data/migrations/"}, []string{"app/src/main/resources/catalog.txt"}},
			{shell, []string{"app/data/motd.txt"}, nil},
		} {
			o := res.Observations[c.unit]
			if o.NoTrace {
				t.Errorf("%s: not traced", c.unit)
			}
			for _, f := range c.want {
				if !o.Files[f] {
					t.Errorf("%s: %s not recorded; recorded %v", c.unit, f, keys(o.Files))
				}
			}
			for _, f := range c.not {
				if o.Files[f] {
					t.Errorf("%s: %s recorded", c.unit, f)
				}
			}
			for f := range o.Files {
				if strings.Contains(f, "/build/") {
					t.Errorf("%s: build output %s recorded", c.unit, f)
				}
			}
		}
		if len(res.Warnings) > 0 {
			t.Errorf("warnings: %q", res.Warnings)
		}
	})

	t.Run("selected", func(t *testing.T) {
		res, out := run(t, []string{greeter}, false)
		if got := statuses(res); res.ExitCode != 0 || !reflect.DeepEqual(got, map[string]string{greeter: state.Pass}) {
			t.Fatalf("exit %d, statuses %v\n%s", res.ExitCode, got, out)
		}
	})

	t.Run("failure", func(t *testing.T) {
		orig := files["app/src/main/java/com/example/app/Cart.java"]
		r.Write("app/src/main/java/com/example/app/Cart.java", strings.Replace(orig, "units += n;", "units += n + 1;", 1))
		defer r.Write("app/src/main/java/com/example/app/Cart.java", orig)
		res, out := run(t, []string{cart, greeter}, false)
		want := map[string]string{cart: state.Fail, greeter: state.Pass}
		if got := statuses(res); res.ExitCode == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("exit %d, statuses %v, want %v\n%s", res.ExitCode, got, want, out)
		}
		if f := res.Observations[cart].Failure; !strings.Contains(f, "total()") {
			t.Errorf("failure = %q", f)
		}
	})

	t.Run("compile error", func(t *testing.T) {
		orig := files["app/src/main/java/com/example/app/Greeter.java"]
		r.Write("app/src/main/java/com/example/app/Greeter.java", orig+"\nbroken")
		defer r.Write("app/src/main/java/com/example/app/Greeter.java", orig)
		res, out := run(t, []string{greeter}, false)
		o := res.Observations[greeter]
		if res.ExitCode == 0 || o == nil || o.Status != state.Fail || !o.Inferred {
			t.Fatalf("exit %d, %+v\n%s", res.ExitCode, o, out)
		}
	})
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
