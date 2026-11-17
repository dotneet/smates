package trace

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"

	"github.com/dotneet/smates/internal/state"
)

// ReporterLoaded reports whether a runner loaded smates' reporter during a run (the
// reporter leaves reporter-loaded-* in the trace directory when it starts).
func ReporterLoaded(traceDir string) bool {
	m, _ := filepath.Glob(filepath.Join(traceDir, "reporter-loaded-*"))
	return len(m) > 0
}

// Results reads per-test-file results written by runner reporters
// (results-*.jsonl in the trace directory), keyed by repo-relative file.
//
// A results file that lost a line (its reporter could not write one: {"k":"lost"}, or a
// line cut short or overlong) records no pass: the line lost may have been a failure,
// or the "unknown" a reporter writes for a file it cannot vouch for after all. Its
// passes are unknown; failures and unknowns stand.
func Results(traceDir, repoRoot string) map[string]Result {
	out := map[string]Result{}
	rel := relCache(repoRoot)
	files, _ := filepath.Glob(filepath.Join(traceDir, "results-*.jsonl"))
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		got := map[string]Result{}
		lost := false
		skipped, err := eachLine(f, maxLine, func(line []byte) {
			var ev event
			if json.Unmarshal(line, &ev) != nil {
				lost = lost || len(bytes.TrimSpace(line)) > 0
				return
			}
			switch ev.K {
			case "lost":
				lost = true
			case "result":
				if r, ok := rel(ev.ID); ok {
					addResult(got, r, ev.Status, ev.Ms, ev.Msg)
				}
			}
		})
		f.Close()
		lost = lost || skipped > 0 || err != nil
		for r, res := range got {
			if lost && res.Status == state.Pass {
				res.Status = state.Unknown
			}
			addResult(out, r, res.Status, res.Ms, res.Message)
		}
	}
	return out
}

// addResult adds a result for file; a file reported several times (by several workspace
// projects, or test cases of a JUnit report) keeps the worst status.
func addResult(m map[string]Result, file, status string, ms int64, msg string) {
	prev, seen := m[file]
	if seen {
		status = worse(prev.Status, status)
	}
	m[file] = Result{Status: status, Ms: prev.Ms + ms, Message: JoinMessage(prev.Message, msg)}
}

// JUnit reads a JUnit XML report and returns per-file results. Files are taken from the
// "file" attribute of test cases or suites, falling back to the suite name and the
// test case classname when they name an existing file under baseDir.
//
// A skipped test case (<skipped/>) makes its file unknown: a report cannot tell a test
// that skipped itself at run time (an assumption, pytest.skip(), a skip condition) from
// a disabled one, and a test that skipped itself proved nothing (as Gradle's assumption
// skips; see runner.gradleObservations).
func JUnit(path, baseDir, repoRoot string) (map[string]Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	type problem struct {
		Message string `xml:"message,attr"`
		Text    string `xml:",chardata"`
	}
	type testcase struct {
		Name      string    `xml:"name,attr"`
		File      string    `xml:"file,attr"`
		Classname string    `xml:"classname,attr"`
		Time      float64   `xml:"time,attr"`
		Failures  []problem `xml:"failure"`
		Errors    []problem `xml:"error"`
		Skipped   []problem `xml:"skipped"`
	}
	type suite struct {
		Name   string     `xml:"name,attr"`
		File   string     `xml:"file,attr"`
		Cases  []testcase `xml:"testcase"`
		Suites []suite    `xml:"testsuite"`
	}
	var root struct {
		Suites  []suite    `xml:"testsuite"`
		Cases   []testcase `xml:"testcase"`
		XMLName xml.Name
	}
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	rel := relCache(repoRoot)
	out := map[string]Result{}
	resolve := func(cands ...string) (string, bool) {
		for _, c := range cands {
			if c == "" {
				continue
			}
			p := c
			if !filepath.IsAbs(p) {
				p = filepath.Join(baseDir, p)
			}
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return rel(p)
			}
		}
		return "", false
	}
	var walk func(s suite)
	walk = func(s suite) {
		for _, c := range s.Cases {
			f, ok := resolve(c.File, s.File, s.Name, strings.ReplaceAll(c.Classname, " ", ""), c.Classname)
			if !ok {
				continue
			}
			st, msg := state.Pass, ""
			if len(c.Skipped) > 0 {
				st = state.Unknown
			}
			for _, pr := range append(c.Failures, c.Errors...) {
				st = state.Fail
				text := strings.TrimSpace(pr.Text)
				if text == "" || !strings.Contains(text, pr.Message) {
					text = strings.TrimSpace(pr.Message + "\n" + text)
				}
				msg = JoinMessage(msg, c.Name+": "+text)
			}
			addResult(out, f, st, int64(c.Time*1000), msg)
		}
		for _, sub := range s.Suites {
			if sub.File == "" {
				sub.File = s.File
			}
			walk(sub)
		}
	}
	for _, s := range root.Suites {
		walk(s)
	}
	if root.XMLName.Local == "testsuite" {
		var s suite
		if xml.Unmarshal(data, &s) == nil {
			walk(s)
		}
	}
	return out, nil
}
