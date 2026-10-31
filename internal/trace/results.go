package trace

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
)

// Results reads per-test-file results written by runner reporters
// (results-*.jsonl in the trace directory), keyed by repo-relative file.
func Results(traceDir, repoRoot string) map[string]Result {
	out := map[string]Result{}
	rel := relCache(repoRoot)
	files, _ := filepath.Glob(filepath.Join(traceDir, "results-*.jsonl"))
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev event
			if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.K != "result" {
				continue
			}
			r, ok := rel(ev.ID)
			if !ok {
				continue
			}
			addResult(out, r, ev.Status, ev.Ms, ev.Msg)
		}
		f.Close()
	}
	return out
}

func addResult(m map[string]Result, file, status string, ms int64, msg string) {
	prev, seen := m[file]
	if seen && prev.Status == "fail" {
		status = "fail"
	}
	m[file] = Result{Status: status, Ms: prev.Ms + ms, Message: JoinMessage(prev.Message, msg)}
}

// JUnit reads a JUnit XML report and returns per-file results. Files are taken from the
// "file" attribute of test cases or suites, falling back to the suite name and the
// test case classname when they name an existing file under baseDir.
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
			st, msg := "pass", ""
			for _, pr := range append(c.Failures, c.Errors...) {
				st = "fail"
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
