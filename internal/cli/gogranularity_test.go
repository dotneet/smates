package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/testutil"
)

// goTests is a test file declaring n functions named prefix0, prefix1, ...
func goTests(prefix string, n int) string {
	var b strings.Builder
	b.WriteString("package p\n\nimport \"testing\"\n\n")
	for i := range n {
		fmt.Fprintf(&b, "func %s%d(t *testing.T) {}\n", prefix, i)
	}
	return b.String()
}

func TestGoFuncHint(t *testing.T) {
	go1 := config.Suite{Name: "api", Runner: "go", Root: "."}
	for name, c := range map[string]struct {
		suite config.Suite
		files map[string]string
		want  string // "dir funcs/total", "" for no hint
	}{
		"one package holds most":  {go1, map[string]string{"big/a_test.go": goTests("TestA", 30), "big/b_test.go": goTests("TestB", 20), "small/s_test.go": goTests("TestS", 10)}, "big 50/60"},
		"spread over packages":    {go1, map[string]string{"a/a_test.go": goTests("TestA", 45), "b/b_test.go": goTests("TestB", 45), "c/c_test.go": goTests("TestC", 45)}, ""},
		"too few functions":       {go1, map[string]string{"big/a_test.go": goTests("TestA", 39)}, ""},
		"TestMain is no test":     {go1, map[string]string{"big/a_test.go": goTests("TestA", 39) + "func TestMain(m *testing.M) {}\n"}, ""},
		"fuzz tests and examples": {go1, map[string]string{"big/a_test.go": goTests("TestA", 20) + goTests("FuzzF", 10) + goTests("Example_e", 10)}, "big 40/40"},
		"helpers are not tests":   {go1, map[string]string{"big/a_test.go": goTests("TestA", 39) + goTests("Testify", 5) + goTests("testA", 5)}, ""},
		"already per function":    {config.Suite{Name: "api", Runner: "go", Root: ".", Granularity: config.GranularityFunc}, map[string]string{"big/a_test.go": goTests("TestA", 50)}, ""},
		"not a go suite":          {config.Suite{Name: "py", Runner: "pytest", Root: "."}, map[string]string{"big/a_test.go": goTests("TestA", 50)}, ""},
		"outside the suite root":  {config.Suite{Name: "api", Runner: "go", Root: "svc"}, map[string]string{"big/a_test.go": goTests("TestA", 50), "svc/x_test.go": goTests("TestX", 5)}, ""},
		"vendor and testdata":     {go1, map[string]string{"vendor/m/a_test.go": goTests("TestA", 50), "p/testdata/a_test.go": goTests("TestA", 50)}, ""},
		"nested root":             {config.Suite{Name: "api", Runner: "go", Root: "svc"}, map[string]string{"svc/core/a_test.go": goTests("TestA", 40), "svc/x_test.go": goTests("TestX", 5)}, "svc/core 40/45"},
		"not a test file":         {go1, map[string]string{"big/a.go": goTests("TestA", 50)}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			testutil.WriteTree(t, root, c.files)
			var files []string
			for f := range c.files {
				files = append(files, f)
			}
			g, ok := goFuncHint(root, c.suite, files)
			got := ""
			if ok {
				got = fmt.Sprintf("%s %d/%d", g.Dir, g.Funcs, g.Total)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// init comments the setting in smates.yml and lists it for review.
func TestGoFuncHintInit(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"big/a_test.go": goTests("TestA", 45)}
	testutil.WriteTree(t, root, files)
	suites := []config.Suite{{Name: "api", Runner: "go", Root: "."}}
	hints := map[string][]string{}
	goFuncConfigHints(root, suites, []string{"big/a_test.go"}, hints)
	doc := renderConfig(suites, hints)
	if !strings.Contains(doc, "    # granularity: func\n") || !strings.Contains(doc, "package big holds 45 of the 45 test functions") {
		t.Errorf("config:\n%s", doc)
	}
	items := goFuncReview(root, suites, []string{"big/a_test.go"})
	if len(items) != 1 || items[0].Kind != "go-granularity" || items[0].Action.Config.Key != "suites.api.granularity" {
		t.Errorf("review %+v", items)
	}
}
