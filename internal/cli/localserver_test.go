package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/testutil"
)

// A local URL counts as a server the tests need only in a file a suite runs, on a port a
// server can use, when the file does not start or intercept that server itself.
func TestScanSourcesLocalServers(t *testing.T) {
	for name, c := range map[string]struct {
		file, content string
		tested        bool
		want          string // reported URL, "" for none
	}{
		"plain call":               {"t/a.test.js", "fetch('http://localhost:3000/x')\n", true, "http://localhost:3000"},
		"websocket":                {"t/a.test.js", "new WebSocket('ws://localhost:4000')\n", true, "ws://localhost:4000"},
		"lowest port kept":         {"t/a.test.js", "fetch('http://localhost:1024/')\n", true, "http://localhost:1024"},
		"privileged port":          {"t/a.test.js", "fetch('http://127.0.0.1:9/')\n", true, ""},
		"invalid port":             {"t/a.test.js", "fetch('http://127.0.0.1:65536/')\n", true, ""},
		"file no suite runs":       {"scripts/probe.py", "requests.get('http://localhost:3000')\n", false, ""},
		"go httptest":              {"t/a_test.go", "srv := httptest.NewServer(h)\nhttp.Get(\"http://127.0.0.1:3000\")\n", true, ""},
		"go listener":              {"t/a_test.go", "ln, _ := net.Listen(\"tcp\", \"127.0.0.1:0\")\nurl := \"http://127.0.0.1:3000\"\n", true, ""},
		"python http.server":       {"t/test_a.py", "server = ThreadingHTTPServer((\"127.0.0.1\", 0), H)\nURL = 'http://127.0.0.1:3000'\n", true, ""},
		"python socketserver":      {"t/test_a.py", "with socketserver.TCPServer(('', 0), H) as s:\n  u = 'http://localhost:3000'\n", true, ""},
		"python responses mock":    {"t/test_a.py", "responses.add(responses.GET, 'http://localhost:3000/x')\n", true, ""},
		"python requests_mock":     {"t/test_a.py", "def test(requests_mock):\n  requests_mock.get('http://localhost:3000/x')\n", true, ""},
		"node createServer":        {"t/a.test.mjs", "const s = createServer(h).listen(0)\nconst u = 'http://127.0.0.1:3000'\n", true, ""},
		"node nock":                {"t/a.test.js", "nock('http://localhost:3000').get('/x').reply(200)\n", true, ""},
		"msw":                      {"t/a.test.ts", "const server = setupServer(http.get('http://localhost:3000/x', h))\n", true, ""},
		"express app listening":    {"t/a.test.js", "app.listen(3000)\nfetch('http://localhost:3000')\n", true, ""},
		"no port is a placeholder": {"t/a.test.js", "fetch('http://localhost/x')\n", true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			testutil.Write(t, root, c.file, c.content)
			sc := scanSources(root, []string{c.file}, func(string) bool { return c.tested })
			got := ""
			for _, l := range sc.Local {
				if l.Kind == "" {
					got = l.URL
				}
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A known service is reported wherever the code connects to it; a port a file left out
// still counts for the next file that calls it.
func TestScanSourcesLocalServersAcrossFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"app/db.go":         "dsn := \"postgres://localhost:5432/app\"\n",
		"t/a_test.go":       "srv := httptest.NewServer(h)\n_ = \"http://localhost:3000\"\n",
		"t/b_test.go":       "http.Get(\"http://localhost:3000/b\")\n",
		"tools/x/x_test.go": "http.Get(\"http://localhost:4000/b\")\n",
	}
	testutil.WriteTree(t, root, files)
	names := []string{"app/db.go", "t/a_test.go", "t/b_test.go", "tools/x/x_test.go"}
	suites := []config.Suite{{Name: "go", Runner: "go", Root: "t"}}
	sc := scanSources(root, names, func(f string) bool { return inSuite(suites, f) })
	var got []string
	for _, l := range sc.Local {
		got = append(got, l.File+" "+l.URL)
	}
	sort.Strings(got)
	want := "app/db.go postgres://localhost:5432,t/b_test.go http://localhost:3000"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s", got, want)
	}
}

// A suite whose tests start the server they call is no end-to-end suite.
func TestServerSuiteHintOwnServer(t *testing.T) {
	for name, c := range map[string]struct {
		content string
		want    bool
	}{
		"calls a local server":  {"def test_a():\n    requests.get('http://localhost:8000/x')\n", true},
		"starts its own server": {"server = ThreadingHTTPServer(('127.0.0.1', 0), H)\ndef test_a():\n    requests.get('http://127.0.0.1:%d' % port)\n", false},
		"mocks the server":      {"def test_a(requests_mock):\n    requests_mock.get('http://localhost:8000/x')\n", false},
		"no port":               {"BASE = 'http://localhost/api'\n", true},
		"unreachable port":      {"BASE = 'http://127.0.0.1:9/'\n", false},
		"invalid port":          {"BASE = 'http://127.0.0.1:65536/'\n", false},
		"websocket only":        {"WS = 'ws://localhost:8000/'\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			g := testutil.NewGitRepo(t, map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n", "tests/test_a.py": c.content})
			repo, err := gitx.Open(g.Dir)
			if err != nil {
				t.Fatal(err)
			}
			suites, err := runner.Resolve(repo, []config.Suite{{Name: "py", Runner: "pytest", Root: "."}})
			if err != nil {
				t.Fatal(err)
			}
			if got := serverSuiteHint(repo.Root, suites[0], []string{"pyproject.toml", "tests/test_a.py"}) != ""; got != c.want {
				t.Errorf("hint %v, want %v", got, c.want)
			}
		})
	}
}

// suiteOf names the suite holding a file, and never another suite for a file none holds.
func TestSuiteOf(t *testing.T) {
	two := []config.Suite{{Name: "root", Root: "."}, {Name: "web", Root: "web"}}
	nested := []config.Suite{{Name: "api", Root: "api"}, {Name: "web", Root: "web"}}
	for name, c := range map[string]struct {
		suites []config.Suite
		file   string
		want   string
	}{
		"deepest root":             {two, "web/src/a.ts", "web"},
		"root suite":               {two, "lib/a.py", "root"},
		"no suite, one suite":      {nested[:1], "tools/a.py", "api"},
		"no suite, several suites": {nested, "tools/a.py", "<suite>"},
		"prefix is not a parent":   {nested, "webapp/a.ts", "<suite>"},
		"the suite root itself":    {nested, "web", "web"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := suiteOf(c.suites, c.file); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
