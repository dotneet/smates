package runner

import (
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// A test function recorded in a process of its own passes only if its verbose output
// says it ran and passed; a skip, a process that never started it, or output that does
// not say make it unproven.
func TestFuncOutcome(t *testing.T) {
	const tm = "\x16" // test2json framing
	for _, c := range []struct {
		name, out, want string
	}{
		{"pass", "=== RUN   TestA\n--- PASS: TestA (0.00s)\nPASS\n", state.Pass},
		{"top-level skip", "=== RUN   TestA\n    a_test.go:5: no database\n--- SKIP: TestA (0.00s)\nPASS\n", state.Skip},
		// A subtest that skips under a passing parent: the function passed.
		{"subtest skip", "=== RUN   TestA\n=== RUN   TestA/sub\n    a_test.go:9: later\n    --- SKIP: TestA/sub (0.00s)\n--- PASS: TestA (0.00s)\nPASS\n", state.Pass},
		// -count=2: a skip in any iteration is not a proof.
		{"skip then pass", "=== RUN   TestA\n--- SKIP: TestA (0.00s)\n=== RUN   TestA\n--- PASS: TestA (0.00s)\nPASS\n", state.Skip},
		{"pass then skip", "=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestA\n--- SKIP: TestA (0.00s)\nPASS\n", state.Skip},
		// A TestMain that exits 0 before m.Run: nothing ran.
		{"TestMain exits early", "no database: skipping the package\n", state.Skip},
		{"no output", "", state.Skip},
		{"-run matched nothing", "testing: warning: no tests to run\nPASS\n", state.Skip},
		// Another function with the name as a prefix, or a subtest, is not this one.
		{"prefix name", "=== RUN   TestAB\n--- PASS: TestAB (0.00s)\nPASS\n", state.Skip},
		{"only a subtest ran", "=== RUN   TestA/sub\n    --- PASS: TestA/sub (0.00s)\nPASS\n", state.Skip},
		// It started but its result line is missing: the output does not say.
		{"no result line", "=== RUN   TestA\nPASS\n", state.Unknown},
		// Output without a final newline puts the result mid-line.
		{"result mid-line", "=== RUN   TestA\nprogress...--- PASS: TestA (0.01s)\nPASS\n", state.Pass},
		{"skip mid-line", "=== RUN   TestA\nprogress...--- SKIP: TestA (0.01s)\nPASS\n", state.Skip},
		{"CRLF", "=== RUN   TestA\r\n--- PASS: TestA (0.00s)\r\nPASS\r\n", state.Pass},
		// -test.v=test2json (a user's flag) frames the lines.
		{"test2json pass", tm + "=== RUN   TestA\n" + tm + "--- PASS: TestA (0.00s)\n" + tm + "PASS\n", state.Pass},
		{"test2json skip", tm + "=== RUN   TestA\n" + tm + "--- SKIP: TestA (0.00s)\n" + tm + "PASS\n", state.Skip},
		// Examples and fuzz targets report like tests.
		{"example", "=== RUN   ExampleA\n--- PASS: ExampleA (0.00s)\nPASS\n", state.Pass},
	} {
		name := "TestA"
		if c.name == "example" {
			name = "ExampleA"
		}
		if got := funcOutcome([]byte(c.out), name); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Recording adds -test.v=true unless the flags already print every result line, and
// never replaces a user's test2json setting.
func TestVerboseFlags(t *testing.T) {
	for _, c := range []struct {
		in, want []string
	}{
		{nil, []string{"-test.v=true"}},
		{[]string{"-test.timeout=10m0s"}, []string{"-test.timeout=10m0s", "-test.v=true"}},
		{[]string{"-test.v"}, []string{"-test.v"}},
		{[]string{"-test.v=true"}, []string{"-test.v=true"}},
		{[]string{"-test.v=1"}, []string{"-test.v=1"}},
		{[]string{"-test.v=test2json"}, []string{"-test.v=test2json"}},
		{[]string{"-test.v=false"}, []string{"-test.v=false", "-test.v=true"}},
		{[]string{"-test.v", "-test.v=false"}, []string{"-test.v", "-test.v=false", "-test.v=true"}},
		{[]string{"--test.v"}, []string{"--test.v"}},
		{[]string{"-test.vet=off"}, []string{"-test.vet=off", "-test.v=true"}},
	} {
		in := slices.Clone(c.in)
		if got := verboseFlags(in); !slices.Equal(got, c.want) {
			t.Errorf("verboseFlags(%q) = %q, want %q", c.in, got, c.want)
		}
		if !slices.Equal(in, c.in) {
			t.Errorf("verboseFlags(%q) changed its argument", c.in)
		}
	}
}

// go test output: a top-level skip is not proven, in any -count iteration; a subtest
// skip and a package with no tests to run stay passes.
func TestGoOutcomesSkip(t *testing.T) {
	plain := &goOutcomes{}
	feed(plain, "=== RUN   TestS\n--- SKIP: TestS (0.00s)\n=== RUN   TestS\n--- PASS: TestS (0.00s)\n"+
		"=== RUN   TestP\n--- PASS: TestP (0.00s)\n=== RUN   TestP\n--- SKIP: TestP (0.00s)\n"+
		"=== RUN   TestF\n--- SKIP: TestF (0.00s)\n=== RUN   TestF\n--- FAIL: TestF (0.00s)\n"+
		"=== RUN   TestSub\n    --- SKIP: TestSub/a (0.00s)\n--- PASS: TestSub (0.00s)\nFAIL\nFAIL\tm/a\t0.01s\n"+
		"testing: warning: no tests to run\nPASS\nok  \tm/none\t0.01s [no tests to run]\n")
	js := &goOutcomes{}
	feed(js, `{"Action":"skip","Package":"m/a","Test":"TestS"}
{"Action":"pass","Package":"m/a","Test":"TestS"}
{"Action":"pass","Package":"m/a","Test":"TestP"}
{"Action":"skip","Package":"m/a","Test":"TestP"}
{"Action":"skip","Package":"m/a","Test":"TestF"}
{"Action":"fail","Package":"m/a","Test":"TestF"}
{"Action":"skip","Package":"m/a","Test":"TestSub/a"}
{"Action":"pass","Package":"m/a","Test":"TestSub"}
{"Action":"fail","Package":"m/a"}
{"Action":"skip","Package":"m/none"}
`)
	for name, g := range map[string]*goOutcomes{"plain": plain, "json": js} {
		for _, c := range []struct{ ip, test, want string }{
			{"m/a", "TestS", state.Skip},
			{"m/a", "TestP", state.Skip},
			{"m/a", "TestF", state.Fail},
			{"m/a", "TestSub", state.Pass},
			{"m/none", "TestN", state.Pass},
		} {
			if st, _, _ := g.status(c.ip, c.test); st != c.want {
				t.Errorf("%s: %s %s: %q, want %q", name, c.ip, c.test, st, c.want)
			}
		}
	}
}
