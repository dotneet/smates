package runner

// Per-function outcomes read from go test output (plain or -json).

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// goTestLineRe matches the result line of a top-level test (subtests are indented).
var goTestLineRe = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+) \(`)

// verboseFlags returns the -test.* flags of a test binary with -test.v=true added
// unless they already make it print the result line of every test (-test.v true, or
// test2json, whose framing funcOutcome reads too): without it a skipped test prints
// nothing a passing one does not.
func verboseFlags(flags []string) []string {
	verbose := false
	for _, f := range flags {
		name, val, has := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if name != "test.v" {
			continue
		}
		on, err := strconv.ParseBool(val)
		verbose = !has || val == "test2json" || err == nil && on // the last one wins
	}
	if verbose {
		return flags
	}
	return append(slices.Clone(flags), "-test.v=true")
}

// funcOutcome reads the verbose output (verboseFlags) of a test binary that ran the
// top-level test function name alone and exited 0: state.Pass when it reported the
// function passing; state.Skip when the function skipped itself (in any iteration of
// -test.count) or never started (a TestMain that exits before m.Run); state.Unknown
// when the output does not say. Lines are matched anywhere, after test2json's framing
// byte is dropped: output a test printed without a final newline precedes its result
// on the same line. Text a test prints that looks like a skip can only make it run
// again.
func funcOutcome(out []byte, name string) string {
	run, pass, skip := false, false, false
	start := "=== RUN   " + name
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimRight(strings.ReplaceAll(l, test2jsonMark, ""), "\r")
		if i := strings.Index(l, start); i >= 0 && strings.TrimSpace(l[i+len(start):]) == "" {
			run = true
		}
		skip = skip || strings.Contains(l, "--- SKIP: "+name+" (")
		pass = pass || strings.Contains(l, "--- PASS: "+name+" (")
	}
	switch {
	case !run || skip:
		return state.Skip
	case pass:
		return state.Pass
	}
	return state.Unknown
}

// test2jsonMark is the byte -test.v=test2json puts before the lines test2json reads.
const test2jsonMark = "\x16"

// worseOutcome is the outcome of a test reported more than once (-count=N): a failure
// in any iteration, else a skip in any (not proven), else a pass.
func worseOutcome(prev, cur string) string {
	rank := map[string]int{state.Pass: 1, state.Skip: 2, state.Fail: 3}
	if rank[prev] > rank[cur] {
		return prev
	}
	return cur
}

// goOutcomes reads per-function outcomes from go test output: the result lines go test
// prints for failing tests (and, with -v, for every test), the per-package summary
// lines, or the events of go test -json.
type goOutcomes struct {
	mu      sync.Mutex
	buf     []byte
	pending map[string]string    // test -> status, for the package printing now
	failOut map[string][]byte    // test -> its output when it failed, for the package printing now
	pkgs    map[string]*goPkgRes // import path -> outcomes
	cur     string               // the test whose output is being printed (plain output)
	curOut  []byte
}

type goPkgRes struct {
	status string // state.Pass / state.Fail ("" = not seen)
	build  bool   // it did not build
	tests  map[string]string
	fails  map[string]string // test -> failure output
}

func (g *goOutcomes) pkg(ip string) *goPkgRes {
	if g.pkgs == nil {
		g.pkgs = map[string]*goPkgRes{}
	}
	p := g.pkgs[ip]
	if p == nil {
		p = &goPkgRes{tests: map[string]string{}, fails: map[string]string{}}
		g.pkgs[ip] = p
	}
	return p
}

func (g *goOutcomes) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.buf = append(g.buf, p...)
	for {
		i := bytes.IndexByte(g.buf, '\n')
		if i < 0 {
			break
		}
		g.line(string(bytes.TrimRight(g.buf[:i], "\r")))
		g.buf = g.buf[i+1:]
	}
	return len(p), nil
}

type goTestEvent struct {
	Action      string
	Package     string
	Test        string
	Output      string
	FailedBuild string
}

func (g *goOutcomes) line(l string) {
	if strings.HasPrefix(l, "{") {
		var ev goTestEvent
		if json.Unmarshal([]byte(l), &ev) == nil && ev.Action != "" {
			g.event(ev)
			return
		}
	}
	if m := buildFailedRe.FindStringSubmatch(l); m != nil {
		p := g.pkg(m[1])
		p.status, p.build = state.Fail, true
		g.pending, g.failOut = nil, nil
		return
	}
	if m := goTestLineRe.FindStringSubmatch(l); m != nil {
		if g.pending == nil {
			g.pending = map[string]string{}
		}
		st := state.Pass
		if m[1] == "SKIP" {
			st = state.Skip
		}
		if m[1] == "FAIL" {
			st = state.Fail
			if g.failOut == nil {
				g.failOut = map[string][]byte{}
			}
			g.cur, g.curOut = m[2], []byte(l+"\n")
		} else {
			g.cur = ""
		}
		// With -count=N a test reports once per iteration.
		g.pending[m[2]] = worseOutcome(g.pending[m[2]], st)
		return
	}
	if m := goSummaryRe.FindStringSubmatch(l); m != nil {
		p := g.pkg(m[2])
		g.flushCur()
		switch m[1] {
		case "FAIL":
			p.status = state.Fail
		default:
			p.status = state.Pass
		}
		for t, st := range g.pending {
			p.tests[t] = st
		}
		for t, out := range g.failOut {
			p.fails[t] = string(out)
		}
		g.pending, g.failOut, g.cur = nil, nil, ""
		return
	}
	if g.cur != "" {
		if strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "panic:") || strings.HasPrefix(l, "goroutine ") {
			if len(g.curOut) < trace.MaxMessage {
				g.curOut = append(g.curOut, l+"\n"...)
			}
			return
		}
		if l != "FAIL" {
			g.flushCur()
		}
	}
}

func (g *goOutcomes) flushCur() {
	if g.cur != "" && g.failOut != nil {
		g.failOut[g.cur] = g.curOut
	}
	g.cur, g.curOut = "", nil
}

func (g *goOutcomes) event(ev goTestEvent) {
	if ev.Package == "" {
		return
	}
	p := g.pkg(ev.Package)
	if ev.Test == "" {
		switch ev.Action {
		case "pass", "skip":
			p.status = state.Pass
		case "fail":
			p.status = state.Fail
			p.build = p.build || ev.FailedBuild != ""
		}
		return
	}
	if strings.Contains(ev.Test, "/") {
		return
	}
	// With -count=N a test reports once per iteration (worseOutcome); one that failed
	// keeps the output of that iteration. (A package-level skip, above, is a package
	// with no test files: a pass.)
	failed := p.tests[ev.Test] == state.Fail
	switch ev.Action {
	case "output":
		if !failed && len(p.fails[ev.Test]) < trace.MaxMessage {
			p.fails[ev.Test] += ev.Output
		}
	case "pass", "skip":
		if !failed {
			st := state.Pass
			if ev.Action == "skip" {
				st = state.Skip
			}
			p.tests[ev.Test] = worseOutcome(p.tests[ev.Test], st)
			delete(p.fails, ev.Test)
		}
	case "fail":
		p.tests[ev.Test] = state.Fail
	}
}

// status returns the outcome of test name in package ip: reported for the test
// itself, or derived from the package (its passing summary covers every test; a failed
// build fails them all; otherwise it is unknown and inferred). ran is false when the
// package was not seen at all.
func (g *goOutcomes) status(ip, name string) (st string, inferred, ran bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pkgs[ip]
	if p == nil || ip == "" {
		return "", false, false
	}
	if s := p.tests[name]; s != "" {
		return s, false, true
	}
	switch {
	case p.build:
		return state.Fail, false, true
	case p.status == state.Pass:
		return state.Pass, false, true
	case p.status == state.Fail:
		return state.Unknown, true, true
	}
	return "", false, false
}

// failure returns the output of a failed test (bounded).
func (g *goOutcomes) failure(ip, name string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pkgs[ip]
	if p == nil || p.tests[name] != state.Fail {
		return ""
	}
	return lastMessage(p.fails[name])
}

var _ io.Writer = (*goOutcomes)(nil)
