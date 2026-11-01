package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/selector"
)

// Limits applied to tool output so a single call cannot flood the context window.
const (
	maxToolOutput     = 12000   // characters of tool output returned to the model
	maxListFiles      = 500     // list_files results
	maxGrepMatches    = 200     // grep matches
	defaultGrepResult = 100     // grep matches when max_results is not given
	maxGrepFileSize   = 1 << 20 // files larger than this are not searched
	maxGrepLine       = 300     // characters of a matching line
	defaultReadLines  = 300     // read_file lines when max_lines is not given
	maxReadLines      = 2000    // read_file upper bound
	maxReadLine       = 1000    // characters of one line returned by read_file
	maxTestsForFile   = 200     // tests_for_file results
	maxTestDeps       = 300     // test_deps results
	maxSearchTests    = 200     // search_tests results
)

// Tool names.
const (
	toolListFiles    = "list_files"
	toolGrep         = "grep"
	toolReadFile     = "read_file"
	toolGitDiff      = "git_diff"
	toolTestsForFile = "tests_for_file"
	toolTestDeps     = "test_deps"
	toolSearchTests  = "search_tests"
	toolSubmit       = "submit_selection"
)

// toolsJSON is the tool list sent with every request. It is a constant so the
// request prefix stays byte-identical across runs and prompt caching applies.
const toolsJSON = `[
{"name":"list_files","description":"List repository files (tracked, plus untracked files that are not git-ignored) whose repo-relative path matches a doublestar glob, e.g. \"**/*.sh\" or \"ci/**\". A pattern without \"/\" also matches file names at any depth. At most 500 results.","strict":true,
 "input_schema":{"type":"object","additionalProperties":false,"required":["glob"],"properties":{
  "glob":{"type":"string","description":"Doublestar glob over repo-relative paths."}}}},
{"name":"grep","description":"Search text files (up to 1 MB, binary files skipped) for a Go RE2 regular expression. Returns \"path:line: text\" lines, at most 200 matches. Use it to find where an orphan file is referenced (by name or path) or where identifiers it defines are used.","strict":true,
 "input_schema":{"type":"object","additionalProperties":false,"required":["pattern"],"properties":{
  "pattern":{"type":"string","description":"Go RE2 regular expression, e.g. \"seed\\.sql\" or \"(?i)docker\"."},
  "glob":{"type":"string","description":"Optional doublestar glob restricting the files searched."},
  "max_results":{"type":"integer","description":"Optional maximum number of matches (default 100, at most 200)."}}}},
{"name":"read_file","description":"Read a repository text file with line numbers. Defaults to the first 300 lines; use start_line and max_lines to page.","strict":true,
 "input_schema":{"type":"object","additionalProperties":false,"required":["path"],"properties":{
  "path":{"type":"string","description":"Repo-relative path."},
  "start_line":{"type":"integer","description":"Optional 1-based first line (default 1)."},
  "max_lines":{"type":"integer","description":"Optional number of lines (default 300, at most 2000)."}}}},
{"name":"git_diff","description":"Unified diff of a changed file between the state smates last recorded (or the base revision) and the working tree. Deleted files show their old content as removed lines.","strict":true,
 "input_schema":{"type":"object","additionalProperties":false,"required":["path"],"properties":{
  "path":{"type":"string","description":"Repo-relative path."}}}},
{"name":"tests_for_file","description":"List tests whose known dependencies (runtime-traced and static) include a file, with their suite and whether they are already selected. Use it on the files that reference an orphan to reach the tests behind them. At most 200 results.","strict":true,
 "input_schema":{"type":"object","additionalProperties":false,"required":["path"],"properties":{
  "path":{"type":"string","description":"Repo-relative file path, or a directory ending in \"/\"."}}}},
{"name":"test_deps","description":"List the known dependencies of one test (at most 300).","strict":true,
 "input_schema":{"type":"object","additionalProperties":false,"required":["suite","test"],"properties":{
  "suite":{"type":"string","description":"Suite name."},
  "test":{"type":"string","description":"Test ID."}}}},
{"name":"search_tests","description":"Find test IDs containing a substring (case-insensitive), with their suite and selection status. At most 200 results; an empty query lists the first tests.","strict":true,
 "input_schema":{"type":"object","additionalProperties":false,"required":["query"],"properties":{
  "query":{"type":"string","description":"Substring of the test ID, e.g. a directory or file name."}}}},
{"name":"submit_selection","description":"Submit the final decision. Call it exactly once, when you are done; it ends the session.","strict":true,
 "input_schema":{"type":"object","additionalProperties":false,"required":["decision","tests","no_impact","summary"],"properties":{
  "decision":{"type":"string","enum":["none","tests","run_all"],"description":"none: no test can be affected; tests: add the listed tests; run_all: run every test."},
  "tests":{"type":"array","description":"Tests to add (empty unless decision is tests).","items":{"type":"object","additionalProperties":false,"required":["suite","test","why"],"properties":{
   "suite":{"type":"string","description":"Suite name."},
   "test":{"type":"string","description":"Exact test ID, or \"*\" for every test of the suite."},
   "why":{"type":"string","description":"Short concrete evidence linking the change to this test."}}}},
  "no_impact":{"type":"array","items":{"type":"string"},"description":"Orphan paths judged unable to affect any test."},
  "summary":{"type":"string","description":"One or two sentences for the CI log."}}}}
]`

// toolDefs is toolsJSON compacted once.
var toolDefs = func() json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(toolsJSON)); err != nil {
		panic("agent: invalid tool definitions: " + err.Error())
	}
	return buf.Bytes()
}()

// toolbox implements the read-only tools against one repository and selection.
// Its methods are safe for concurrent use.
type toolbox struct {
	root    string
	sel     *selector.Result
	orphans map[string]Orphan
	base    string // fallback revision for files that are not orphans

	filesOnce sync.Once
	files     []string
	fileSet   map[string]bool
	filesErr  error
}

func newToolbox(req *Request) *toolbox {
	tb := &toolbox{root: req.Repo.Root, sel: req.Selection, orphans: map[string]Orphan{}}
	for _, o := range req.Orphans {
		tb.orphans[o.Path] = o
		if tb.base == "" && o.Base != "" {
			tb.base = o.Base
		}
	}
	return tb
}

// toolError is a failure reported back to the model as an is_error tool result.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

func toolErrorf(format string, args ...any) error {
	return &toolError{fmt.Sprintf(format, args...)}
}

// run executes one tool call. Errors become is_error tool results.
func (tb *toolbox) run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	var out string
	var err error
	switch name {
	case toolListFiles:
		var in struct{ Glob string }
		if err = decodeInput(input, &in); err == nil {
			out, err = tb.listFiles(in.Glob)
		}
	case toolGrep:
		var in struct {
			Pattern    string
			Glob       string
			MaxResults int `json:"max_results"`
		}
		if err = decodeInput(input, &in); err == nil {
			out, err = tb.grep(ctx, in.Pattern, in.Glob, in.MaxResults)
		}
	case toolReadFile:
		var in struct {
			Path      string
			StartLine int `json:"start_line"`
			MaxLines  int `json:"max_lines"`
		}
		if err = decodeInput(input, &in); err == nil {
			out, err = tb.readFile(in.Path, in.StartLine, in.MaxLines)
		}
	case toolGitDiff:
		var in struct{ Path string }
		if err = decodeInput(input, &in); err == nil {
			out, err = tb.gitDiff(ctx, in.Path)
		}
	case toolTestsForFile:
		var in struct{ Path string }
		if err = decodeInput(input, &in); err == nil {
			out, err = tb.testsForFile(in.Path)
		}
	case toolTestDeps:
		var in struct{ Suite, Test string }
		if err = decodeInput(input, &in); err == nil {
			out, err = tb.testDeps(in.Suite, in.Test)
		}
	case toolSearchTests:
		var in struct{ Query string }
		if err = decodeInput(input, &in); err == nil {
			out = tb.searchTests(in.Query)
		}
	default:
		err = toolErrorf("unknown tool %q", name)
	}
	if err != nil {
		return "", err
	}
	return truncate(out, maxToolOutput), nil
}

func decodeInput(input json.RawMessage, v any) error {
	if len(input) == 0 {
		input = []byte("{}")
	}
	if err := json.Unmarshal(input, v); err != nil {
		return toolErrorf("invalid input: %v", err)
	}
	return nil
}

// truncate shortens s to at most max bytes (on a rune boundary) and says so.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[output truncated: %d more bytes omitted; narrow the query]", len(s)-cut)
}

// fileList returns the repository's tracked and untracked, non-ignored files that
// exist in the working tree, sorted. Ignored files (secrets, build output) are never
// exposed to the model.
func (tb *toolbox) fileList() ([]string, map[string]bool, error) {
	tb.filesOnce.Do(func() {
		out, err := gitOutput(context.Background(), tb.root, "-c", "core.quotepath=off", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		if err != nil {
			tb.filesErr = err
			return
		}
		tb.fileSet = map[string]bool{}
		for _, p := range strings.Split(string(out), "\x00") {
			if p == "" || tb.fileSet[p] {
				continue
			}
			if _, err := os.Lstat(filepath.Join(tb.root, filepath.FromSlash(p))); err != nil {
				continue // deleted in the working tree
			}
			tb.fileSet[p] = true
			tb.files = append(tb.files, p)
		}
		sort.Strings(tb.files)
	})
	return tb.files, tb.fileSet, tb.filesErr
}

// globMatcher compiles a doublestar glob; patterns without "/" also match base names.
func globMatcher(glob string) (func(string) bool, error) {
	glob = strings.TrimPrefix(strings.TrimSpace(glob), "./")
	if glob == "" {
		return func(string) bool { return true }, nil
	}
	if !doublestar.ValidatePattern(glob) {
		return nil, toolErrorf("invalid glob %q", glob)
	}
	baseOnly := !strings.Contains(glob, "/")
	return func(p string) bool {
		if ok, _ := doublestar.Match(glob, p); ok {
			return true
		}
		if baseOnly {
			ok, _ := doublestar.Match(glob, path.Base(p))
			return ok
		}
		return false
	}, nil
}

func (tb *toolbox) listFiles(glob string) (string, error) {
	if strings.TrimSpace(glob) == "" {
		return "", toolErrorf("glob is required")
	}
	match, err := globMatcher(glob)
	if err != nil {
		return "", err
	}
	files, _, err := tb.fileList()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	for _, f := range files {
		if !match(f) {
			continue
		}
		n++
		if n <= maxListFiles {
			b.WriteString(f)
			b.WriteByte('\n')
		}
	}
	switch {
	case n == 0:
		return "no files match " + glob, nil
	case n > maxListFiles:
		fmt.Fprintf(&b, "[%d more files not shown; use a narrower glob]\n", n-maxListFiles)
	}
	return b.String(), nil
}

func (tb *toolbox) grep(ctx context.Context, pattern, glob string, maxResults int) (string, error) {
	if pattern == "" {
		return "", toolErrorf("pattern is required")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", toolErrorf("invalid regular expression: %v", err)
	}
	match, err := globMatcher(glob)
	if err != nil {
		return "", err
	}
	if maxResults <= 0 {
		maxResults = defaultGrepResult
	}
	maxResults = min(maxResults, maxGrepMatches)
	files, _, err := tb.fileList()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	for _, f := range files {
		if !match(f) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		data, ok := tb.readText(f, maxGrepFileSize)
		if !ok {
			continue
		}
		line := 0
		for len(data) > 0 {
			line++
			var l []byte
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				l, data = data[:i], data[i+1:]
			} else {
				l, data = data, nil
			}
			if !re.Match(l) {
				continue
			}
			n++
			if n > maxResults {
				fmt.Fprintf(&b, "[stopped after %d matches; refine the pattern or glob]\n", maxResults)
				return b.String(), nil
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", f, line, clip(strings.TrimRight(string(l), "\r"), maxGrepLine))
		}
	}
	if n == 0 {
		return "no matches", nil
	}
	return b.String(), nil
}

// readText reads a regular, non-binary file of at most limit bytes.
func (tb *toolbox) readText(rel string, limit int64) ([]byte, bool) {
	abs := filepath.Join(tb.root, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > limit {
		return nil, false
	}
	data, err := os.ReadFile(abs)
	if err != nil || isBinary(data) {
		return nil, false
	}
	return data, true
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// cleanRel normalizes a model-supplied path to a clean repo-relative slash path,
// rejecting anything that escapes the repository or points into .git.
func (tb *toolbox) cleanRel(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", toolErrorf("path is required")
	}
	if filepath.IsAbs(p) || path.IsAbs(filepath.ToSlash(p)) {
		rel, ok := pathutil.Rel(tb.root, p)
		if !ok {
			return "", toolErrorf("%s is outside the repository", p)
		}
		p = rel
	}
	c := path.Clean(filepath.ToSlash(p))
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", toolErrorf("%s is outside the repository", p)
	}
	if c == ".git" || strings.HasPrefix(c, ".git/") {
		return "", toolErrorf("%s is not readable", p)
	}
	return c, nil
}

func (tb *toolbox) readFile(p string, start, maxLines int) (string, error) {
	rel, err := tb.cleanRel(p)
	if err != nil {
		return "", err
	}
	_, set, err := tb.fileList()
	if err != nil {
		return "", err
	}
	if !set[rel] {
		if _, ok := tb.orphans[rel]; ok {
			return "", toolErrorf("%s does not exist in the working tree (deleted); use git_diff to see its old content", rel)
		}
		return "", toolErrorf("%s is not a repository file (missing, a directory, or git-ignored); use list_files", rel)
	}
	abs := filepath.Join(tb.root, filepath.FromSlash(rel))
	// A tracked symlink must not lead outside the repository.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", toolErrorf("cannot read %s: %v", rel, err)
	}
	root := tb.root
	if rr, err := filepath.EvalSymlinks(root); err == nil {
		root = rr
	}
	target, err := filepath.Rel(root, real)
	if err != nil || target == ".." || strings.HasPrefix(target, ".."+string(filepath.Separator)) {
		return "", toolErrorf("%s points outside the repository", rel)
	}
	// ...nor to an ignored file (.env) or into .git.
	target = filepath.ToSlash(target)
	if first, _, _ := strings.Cut(target, "/"); strings.EqualFold(first, ".git") || !set[target] {
		return "", toolErrorf("%s points to a file that is not readable (ignored or internal)", rel)
	}
	f, err := os.Open(real)
	if err != nil {
		return "", toolErrorf("cannot read %s: %v", rel, err)
	}
	defer f.Close()

	if start < 1 {
		start = 1
	}
	if maxLines <= 0 {
		maxLines = defaultReadLines
	}
	maxLines = min(maxLines, maxReadLines)

	r := bufio.NewReader(f)
	head, _ := r.Peek(8000)
	if bytes.IndexByte(head, 0) >= 0 {
		fi, _ := f.Stat()
		return fmt.Sprintf("%s is a binary file (%d bytes)", rel, fi.Size()), nil
	}
	var b strings.Builder
	line, shown := 0, 0
	more := false
	for {
		l, err := r.ReadString('\n')
		if l == "" && err != nil {
			if !errors.Is(err, io.EOF) {
				return "", toolErrorf("cannot read %s: %v", rel, err)
			}
			break
		}
		line++
		if line < start {
			continue
		}
		if shown == maxLines {
			more = true
			break
		}
		shown++
		fmt.Fprintf(&b, "%6d\t%s\n", line, clip(strings.TrimRight(l, "\r\n"), maxReadLine))
	}
	switch {
	case line == 0:
		return rel + " is empty", nil
	case shown == 0:
		return fmt.Sprintf("%s has only %d lines", rel, line), nil
	case more:
		fmt.Fprintf(&b, "[more lines follow; continue with start_line %d]\n", start+shown)
	}
	return b.String(), nil
}

// gitDiff diffs a file's old content (the orphan's recorded blob when it is present
// locally, else the file at the base revision or HEAD) against the working tree.
func (tb *toolbox) gitDiff(ctx context.Context, p string) (string, error) {
	rel, err := tb.cleanRel(p)
	if err != nil {
		return "", err
	}
	old, oldDesc := tb.oldContent(ctx, rel)
	cur, curExists, err := tb.workingContent(rel)
	if err != nil {
		return "", err
	}
	if old == nil && !curExists {
		return "", toolErrorf("%s exists neither in the old state nor in the working tree", rel)
	}
	if old != nil && curExists && bytes.Equal(old, cur) {
		return fmt.Sprintf("%s: no content change against %s", rel, oldDesc), nil
	}
	if old != nil && isBinary(old) || curExists && isBinary(cur) {
		return fmt.Sprintf("%s: binary file changed (%d -> %d bytes)", rel, len(old), len(cur)), nil
	}
	body, err := unifiedDiff(ctx, old, cur)
	if err != nil {
		return "", err
	}
	from, to := "a/"+rel, "b/"+rel
	if old == nil {
		from = "/dev/null"
	}
	if !curExists {
		to = "/dev/null"
	}
	return fmt.Sprintf("--- %s (%s)\n+++ %s (working tree)\n%s", from, oldDesc, to, body), nil
}

// oldContent returns the previous content of rel, or nil when there is none.
func (tb *toolbox) oldContent(ctx context.Context, rel string) ([]byte, string) {
	o, isOrphan := tb.orphans[rel]
	if isOrphan && o.OldBlob != "" {
		if data, err := gitOutput(ctx, tb.root, "cat-file", "blob", o.OldBlob); err == nil {
			return data, "recorded blob " + short(o.OldBlob)
		}
	}
	base := tb.base
	if isOrphan && o.Base != "" {
		base = o.Base
	}
	revs := []string{"HEAD"}
	if base != "" {
		revs = []string{base}
	}
	for _, rev := range revs {
		if data, err := gitOutput(ctx, tb.root, "cat-file", "blob", rev+":"+rel); err == nil {
			return data, rev
		}
	}
	return nil, "no previous version"
}

func (tb *toolbox) workingContent(rel string) ([]byte, bool, error) {
	abs := filepath.Join(tb.root, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, toolErrorf("cannot read %s: %v", rel, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(abs)
		if err != nil {
			return nil, false, toolErrorf("cannot read %s: %v", rel, err)
		}
		return []byte(target), true, nil
	}
	if !fi.Mode().IsRegular() {
		return nil, false, toolErrorf("%s is not a regular file", rel)
	}
	if _, set, err := tb.fileList(); err == nil && !set[rel] {
		return nil, false, toolErrorf("%s is git-ignored", rel)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, false, toolErrorf("cannot read %s: %v", rel, err)
	}
	return data, true, nil
}

// unifiedDiff runs `git diff --no-index` over two temporary files and returns the
// hunks (everything from the first "@@").
func unifiedDiff(ctx context.Context, old, cur []byte) (string, error) {
	dir, err := os.MkdirTemp("", "smates-diff-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(a, old, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(b, cur, 0o600); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "-c", "core.quotepath=off", "diff", "--no-index", "--no-color", "--no-ext-diff", "-U3", "--", a, b)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) {
		return "", fmt.Errorf("git diff --no-index: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	out := stdout.String()
	if i := strings.Index(out, "\n@@"); i >= 0 {
		return out[i+1:], nil
	}
	if strings.HasPrefix(out, "@@") {
		return out, nil
	}
	return "(no textual difference)\n", nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func selectedMark(t *selector.Test) string {
	if t.Selected {
		return "selected (" + t.Reason + ")"
	}
	return "not selected"
}

func (tb *toolbox) testsForFile(p string) (string, error) {
	dir := strings.HasSuffix(strings.TrimSpace(p), "/")
	rel, err := tb.cleanRel(p)
	if err != nil {
		return "", err
	}
	if dir {
		rel += "/"
	}
	matches := func(dep string) bool {
		switch {
		case dep == rel, dep == depkey.Decls(rel):
			return true
		case dir:
			return strings.HasPrefix(dep, rel)
		case depkey.KindOf(dep) == depkey.KindListing: // a directory dependency covers its files
			return strings.HasPrefix(rel, dep)
		}
		return false
	}
	var b strings.Builder
	n := 0
	for _, s := range tb.sel.Suites {
		for _, t := range s.Tests {
			hit := false
			for _, d := range s.Deps[t.ID] {
				if matches(d) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			n++
			if n <= maxTestsForFile {
				fmt.Fprintf(&b, "%s\t%s\t%s\n", s.Name, t.ID, selectedMark(t))
			}
		}
	}
	switch {
	case n == 0:
		return "no test is known to depend on " + rel, nil
	case n > maxTestsForFile:
		fmt.Fprintf(&b, "[%d more tests not shown]\n", n-maxTestsForFile)
	}
	return b.String(), nil
}

func (tb *toolbox) testDeps(suite, test string) (string, error) {
	inv := newInventory(tb.sel)
	sn, id, err := inv.resolve(suite, test)
	if err != nil {
		return "", toolErrorf("%v; use search_tests to find test IDs", err)
	}
	if id == "" {
		return "", toolErrorf("test_deps needs a single test, not a whole suite")
	}
	deps := tb.sel.SuiteByName(sn).Deps[id]
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d dependencies\n", sn, id, len(deps))
	for i, d := range deps {
		if i == maxTestDeps {
			fmt.Fprintf(&b, "[%d more not shown]\n", len(deps)-maxTestDeps)
			break
		}
		b.WriteString(d)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func (tb *toolbox) searchTests(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	var b strings.Builder
	n := 0
	for _, s := range tb.sel.Suites {
		for _, t := range s.Tests {
			if q != "" && !strings.Contains(strings.ToLower(t.ID), q) {
				continue
			}
			n++
			if n <= maxSearchTests {
				fmt.Fprintf(&b, "%s\t%s\t%s\n", s.Name, t.ID, selectedMark(t))
			}
		}
	}
	switch {
	case n == 0:
		return "no test ID contains " + q
	case n > maxSearchTests:
		fmt.Fprintf(&b, "[%d more tests not shown; refine the query]\n", n-maxSearchTests)
	}
	return b.String()
}
