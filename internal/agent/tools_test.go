package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/selector"
	"github.com/dotneet/smates/internal/testutil"
)

func TestToolDefinitions(t *testing.T) {
	var tools []struct {
		Name        string          `json:"name"`
		Strict      bool            `json:"strict"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(toolDefs, &tools); err != nil {
		t.Fatal(err)
	}
	want := []string{toolListFiles, toolGrep, toolReadFile, toolGitDiff, toolTestsForFile, toolTestDeps, toolSearchTests, toolSubmit}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(tools), len(want))
	}
	for i, tool := range tools {
		if tool.Name != want[i] {
			t.Errorf("tool %d = %s, want %s", i, tool.Name, want[i])
		}
		if !tool.Strict {
			t.Errorf("%s: strict not set", tool.Name)
		}
		// Every object schema must forbid additional properties.
		var walk func(v any)
		walk = func(v any) {
			switch v := v.(type) {
			case map[string]any:
				if v["type"] == "object" && v["additionalProperties"] != false {
					t.Errorf("%s: object schema without additionalProperties:false: %v", tool.Name, v)
				}
				for _, c := range v {
					walk(c)
				}
			case []any:
				for _, c := range v {
					walk(c)
				}
			}
		}
		var schema any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		walk(schema)
	}
}

func runTool(t *testing.T, tb *toolbox, name string, input any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return tb.run(context.Background(), name, raw)
}

func TestTools(t *testing.T) {
	f := newFixture(t)
	tb := newToolbox(f.request(testConfig()))

	tests := []struct {
		name     string
		tool     string
		input    any
		want     []string // substrings that must appear
		wantNot  []string // substrings that must not appear
		wantErr  string   // substring of the tool error, if any
		maxBytes int
	}{
		{name: "list glob", tool: toolListFiles, input: map[string]any{"glob": "scripts/**"},
			want: []string{"scripts/seed.sh", "scripts/ci/run-tests.sh"}, wantNot: []string{"scripts/gone.sh"}},
		{name: "list base name pattern matches at any depth", tool: toolListFiles, input: map[string]any{"glob": "*.sh"},
			want: []string{"scripts/seed.sh", "scripts/ci/run-tests.sh"}, wantNot: []string{".ts"}},
		{name: "list includes untracked, excludes ignored", tool: toolListFiles, input: map[string]any{"glob": "**"},
			want: []string{"notes/new.txt", ".gitignore"}, wantNot: []string{".env\n", ".git/"}},
		{name: "list no match", tool: toolListFiles, input: map[string]any{"glob": "**/*.rs"}, want: []string{"no files match"}},
		{name: "list invalid glob", tool: toolListFiles, input: map[string]any{"glob": "[abc"}, wantErr: "invalid glob"},
		{name: "list empty glob", tool: toolListFiles, input: map[string]any{"glob": ""}, wantErr: "glob is required"},

		{name: "grep finds references", tool: toolGrep, input: map[string]any{"pattern": `seed\.sh`},
			want: []string{"src/helpers/db.ts:2: export const seed"}},
		{name: "grep glob filter", tool: toolGrep, input: map[string]any{"pattern": "seed", "glob": "fixtures/**"},
			want: []string{"no matches"}},
		{name: "grep skips binary and ignored files", tool: toolGrep, input: map[string]any{"pattern": "x|do-not-leak"},
			wantNot: []string{"assets/blob.bin", "do-not-leak"}},
		{name: "grep max results", tool: toolGrep, input: map[string]any{"pattern": "^line ", "max_results": 5},
			want: []string{"src/helpers/long.txt:5: line 5", "stopped after 5 matches"}, wantNot: []string{"line 6\n"}},
		{name: "grep bad regexp", tool: toolGrep, input: map[string]any{"pattern": "("}, wantErr: "invalid regular expression"},

		{name: "read default window", tool: toolReadFile, input: map[string]any{"path": "src/helpers/long.txt"},
			want: []string{"     1\tline 1\n", "   300\tline 300\n", "continue with start_line 301"}, wantNot: []string{"line 301\n"}},
		{name: "read paging", tool: toolReadFile, input: map[string]any{"path": "src/helpers/long.txt", "start_line": 399, "max_lines": 10},
			want: []string{"   399\tline 399", "   400\tline 400"}, wantNot: []string{"continue with"}},
		{name: "read past end", tool: toolReadFile, input: map[string]any{"path": "src/helpers/long.txt", "start_line": 1000},
			want: []string{"has only 400 lines"}},
		{name: "read untracked", tool: toolReadFile, input: map[string]any{"path": "./notes/new.txt"}, want: []string{"brand new"}},
		{name: "read absolute path inside repo", tool: toolReadFile, input: map[string]any{"path": f.repo.Root + "/src/lib/util.ts"},
			want: []string{"export const x"}},
		{name: "read binary", tool: toolReadFile, input: map[string]any{"path": "assets/blob.bin"}, want: []string{"binary file"}},
		{name: "read ignored file refused", tool: toolReadFile, input: map[string]any{"path": ".env"}, wantErr: "git-ignored"},
		{name: "read .git refused", tool: toolReadFile, input: map[string]any{"path": ".git/config"}, wantErr: "not readable"},
		{name: "read traversal refused", tool: toolReadFile, input: map[string]any{"path": "../../etc/passwd"}, wantErr: "outside the repository"},
		{name: "read symlink escaping refused", tool: toolReadFile, input: map[string]any{"path": "link-out"}, wantErr: "points outside"},
		{name: "read deleted orphan", tool: toolReadFile, input: map[string]any{"path": "scripts/gone.sh"}, wantErr: "use git_diff"},
		{name: "read directory", tool: toolReadFile, input: map[string]any{"path": "src"}, wantErr: "not a repository file"},

		{name: "diff against recorded blob", tool: toolGitDiff, input: map[string]any{"path": "scripts/seed.sh"},
			want: []string{"--- a/scripts/seed.sh (recorded blob", "+++ b/scripts/seed.sh (working tree)", "+set -e", "+psql < fixtures/extra.sql", "@@"}},
		{name: "diff deleted file shows old content", tool: toolGitDiff, input: map[string]any{"path": "scripts/gone.sh"},
			want: []string{"+++ /dev/null", "-echo bye"}},
		{name: "diff untracked file", tool: toolGitDiff, input: map[string]any{"path": "notes/new.txt"},
			want: []string{"--- /dev/null (no previous version)", "+brand new"}},
		{name: "diff unchanged non-orphan uses HEAD", tool: toolGitDiff, input: map[string]any{"path": "src/lib/util.ts"},
			want: []string{"no content change against HEAD"}},
		{name: "diff binary", tool: toolGitDiff, input: map[string]any{"path": "assets/blob.bin"}, want: []string{"no content change"}},
		{name: "diff missing", tool: toolGitDiff, input: map[string]any{"path": "nope.txt"}, wantErr: "exists neither"},
		{name: "diff ignored refused", tool: toolGitDiff, input: map[string]any{"path": ".env"}, wantErr: "git-ignored"},

		{name: "tests for file", tool: toolTestsForFile, input: map[string]any{"path": "src/helpers/db.ts"},
			want: []string{"web\tsrc/db.test.ts\tnot selected"}, wantNot: []string{"other"}},
		{name: "tests for file under directory dep", tool: toolTestsForFile, input: map[string]any{"path": "src/lib/util.ts"},
			want: []string{"web\tsrc/other.test.ts\tselected (changed)"}},
		{name: "tests for directory", tool: toolTestsForFile, input: map[string]any{"path": "svc/pkg/"},
			want: []string{"go\tsvc/pkg/a\t", "go\tsvc/pkg/b\t"}},
		{name: "tests for unknown file", tool: toolTestsForFile, input: map[string]any{"path": "scripts/seed.sh"},
			want: []string{"no test is known to depend on scripts/seed.sh"}},

		{name: "test deps", tool: toolTestDeps, input: map[string]any{"suite": "web", "test": "src/db.test.ts"},
			want: []string{"web src/db.test.ts: 2 dependencies", "src/helpers/db.ts"}},
		{name: "test deps suffix match", tool: toolTestDeps, input: map[string]any{"suite": "go", "test": "pkg/a"},
			want: []string{"go svc/pkg/a: 1 dependencies"}},
		{name: "test deps unknown", tool: toolTestDeps, input: map[string]any{"suite": "web", "test": "nope.test.ts"},
			wantErr: "search_tests"},
		{name: "test deps whole suite refused", tool: toolTestDeps, input: map[string]any{"suite": "web", "test": "*"},
			wantErr: "single test"},

		{name: "search tests", tool: toolSearchTests, input: map[string]any{"query": "DB"},
			want: []string{"web\tsrc/db.test.ts\tnot selected"}, wantNot: []string{"other"}},
		{name: "search tests empty query lists all", tool: toolSearchTests, input: map[string]any{"query": ""},
			want: []string{"py\ttests/test_api.py", "go\tsvc/pkg/b"}},
		{name: "search tests no match", tool: toolSearchTests, input: map[string]any{"query": "zzz"}, want: []string{"no test ID contains zzz"}},

		{name: "unknown tool", tool: "rm_rf", input: map[string]any{}, wantErr: "unknown tool"},
		{name: "bad input", tool: toolGrep, input: map[string]any{"pattern": 42}, wantErr: "invalid input"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runTool(t, tb, tt.tool, tt.input)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q (out %q)", err, tt.wantErr, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("output contains %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestToolErrorsAreToolErrors(t *testing.T) {
	f := newFixture(t)
	tb := newToolbox(f.request(testConfig()))
	_, err := runTool(t, tb, toolReadFile, map[string]any{"path": "../x"})
	var te *toolError
	if !errors.As(err, &te) {
		t.Fatalf("want *toolError, got %T", err)
	}
}

func TestToolOutputTruncated(t *testing.T) {
	f := newFixture(t)
	testutil.Write(t, f.repo.Root, "big/huge.txt", strings.Repeat(strings.Repeat("x", 990)+"\n", 40))
	tb := newToolbox(f.request(testConfig()))
	out, err := runTool(t, tb, toolReadFile, map[string]any{"path": "big/huge.txt", "max_lines": 40})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxToolOutput+200 || !strings.Contains(out, "[output truncated:") {
		t.Fatalf("output not truncated (%d bytes)", len(out))
	}
}

func TestDiffUsesBaseWhenBlobMissing(t *testing.T) {
	f := newFixture(t)
	req := f.request(testConfig())
	req.Orphans = []Orphan{{Path: "scripts/seed.sh", Status: "modified", OldBlob: strings.Repeat("0", 40), Base: "HEAD"}}
	tb := newToolbox(req)
	out, err := runTool(t, tb, toolGitDiff, map[string]any{"path": "scripts/seed.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(HEAD)") || !strings.Contains(out, "+set -e") {
		t.Fatalf("unexpected diff:\n%s", out)
	}
}

func TestTruncateRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	got := truncate(s, 5)
	if !strings.HasPrefix(got, "éé\n[output truncated: 16 more bytes") {
		t.Fatalf("truncate = %q", got)
	}
	if truncate("short", 10) != "short" {
		t.Fatal("short strings must be unchanged")
	}
}

// A Go file of a package matches the Go test functions that depend on the package's
// shape, as depkey.Covers has it: a file holding only an init function or the methods
// of a type they reach is no declaration they name.
func TestTestsForFileGoPackage(t *testing.T) {
	sel := &selector.Result{Suites: []*selector.Suite{{
		Name:  "go",
		Tests: []*selector.Test{{ID: "pkg#TestA"}, {ID: "other#TestB"}},
		Deps: map[string][]string{
			"pkg#TestA":   {depkey.Shape("pkg"), depkey.Decl("pkg/a_test.go", "TestA")},
			"other#TestB": {depkey.Shape("other")},
		},
	}}}
	tb := &toolbox{root: t.TempDir(), sel: sel}
	for p, want := range map[string]bool{"pkg/setup.go": true, "pkg/data.txt": false, "pkg/": true, "other/x.go": false} {
		out, err := tb.testsForFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out, "pkg#TestA"); got != want {
			t.Errorf("%s: %q", p, out)
		}
	}
}
