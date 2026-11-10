package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	if want := []string{cart, env, check, greeter}; !reflect.DeepEqual(units, want) {
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
		want := map[string]string{cart: state.Pass, greeter: state.Pass, check: state.Pass, env: state.Unknown}
		if got := statuses(res); res.ExitCode != 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("exit %d, statuses %v, want %v\n%s", res.ExitCode, got, want, out)
		}
		if !res.Observations[cart].NoTrace || res.Observations[env].NoTrace {
			t.Error("a pass is NoTrace, an assumption is not")
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
