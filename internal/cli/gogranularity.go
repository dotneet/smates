package cli

// The hint to select a Go suite's tests per function: when one package holds most of
// the suite's test functions, nearly every change reaches it and package units save
// little (see docs/guides/go-test-functions.md).

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/pathutil"
)

const (
	// goFuncMinFuncs: the package must hold at least this many test functions...
	goFuncMinFuncs = 40
	// goFuncMinShare: ...and at least this share of the suite's.
	goFuncMinShare = 0.5
)

// goTestFuncRe matches a top-level test, fuzz test or example declaration.
var goTestFuncRe = regexp.MustCompile(`(?m)^func (Test|Fuzz|Example)([^a-z(][\w]*)?\(`)

// goConcentration says that package Dir holds Funcs of the Total test functions of a
// Go suite with package granularity.
type goConcentration struct {
	Suite, Dir   string
	Funcs, Total int
}

// goFuncHint reports the package of Go suite s holding most of its test functions, when
// it holds enough of them that per-function selection pays off. files are the
// repository's files.
func goFuncHint(root string, s config.Suite, files []string) (goConcentration, bool) {
	if s.Runner != "go" || s.Granularity == config.GranularityFunc || s.Granularity == "function" {
		return goConcentration{}, false
	}
	perDir := map[string]int{}
	total := 0
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") || !pathutil.Under(f, s.Root) || strings.Contains("/"+f, "/vendor/") || strings.Contains("/"+f, "/testdata/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		n := 0
		for _, m := range goTestFuncRe.FindAllSubmatch(data, -1) {
			if string(m[1])+string(m[2]) != "TestMain" {
				n++
			}
		}
		perDir[path.Dir(f)] += n
		total += n
	}
	best := goConcentration{Suite: s.Name, Total: total}
	for _, d := range slices.Sorted(maps.Keys(perDir)) {
		if perDir[d] > best.Funcs {
			best.Dir, best.Funcs = d, perDir[d]
		}
	}
	if best.Funcs < goFuncMinFuncs || float64(best.Funcs) < goFuncMinShare*float64(total) {
		return goConcentration{}, false
	}
	return best, true
}

func (g goConcentration) reason() string {
	return fmt.Sprintf("package %s holds %d of the %d test functions of suite %s: nearly every change reaches it, so selecting whole packages saves little", g.Dir, g.Funcs, g.Total, g.Suite)
}

const goFuncAction = "select its test functions one by one with granularity: func (recording then runs each function in a process of its own, record_jobs: N runs several at once when the tests share no state)"

// goFuncConfigHints adds the commented granularity line to the hints of each Go suite
// goFuncHint picks.
func goFuncConfigHints(root string, suites []config.Suite, files []string, hints map[string][]string) {
	for _, s := range suites {
		if g, ok := goFuncHint(root, s, files); ok {
			hints[s.Name] = append(hints[s.Name], "# "+g.reason()+"; to "+goFuncAction+", uncomment:", "# granularity: func")
		}
	}
}

// goFuncReview is the init review item for goFuncHint.
func goFuncReview(root string, suites []config.Suite, files []string) []reviewItem {
	var out []reviewItem
	for _, s := range suites {
		g, ok := goFuncHint(root, s, files)
		if !ok {
			continue
		}
		out = append(out, reviewItem{
			ID: "go-granularity-" + slug(s.Name), Kind: "go-granularity",
			Reason:   g.reason(),
			Evidence: []string{fmt.Sprintf("%s: %d test functions", g.Dir, g.Funcs)},
			Action: reviewAction{
				Description: goFuncAction,
				Config:      &configFix{Key: "suites." + s.Name + ".granularity", Value: config.GranularityFunc},
				Snippet:     "granularity: func\n", SnippetFile: "smates.yml",
			},
		})
	}
	return out
}
