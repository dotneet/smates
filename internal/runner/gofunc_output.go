package runner

// Per-function outcomes read from go test output (plain or -json).

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// goTestLineRe matches the result line of a top-level test (subtests are indented).
var goTestLineRe = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+) \(`)

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
		if m[1] == "FAIL" {
			st = state.Fail
			if g.failOut == nil {
				g.failOut = map[string][]byte{}
			}
			g.cur, g.curOut = m[2], []byte(l+"\n")
		} else {
			g.cur = ""
		}
		g.pending[m[2]] = st
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
	switch ev.Action {
	case "output":
		if len(p.fails[ev.Test]) < trace.MaxMessage {
			p.fails[ev.Test] += ev.Output
		}
	case "pass", "skip":
		p.tests[ev.Test] = state.Pass
		delete(p.fails, ev.Test)
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
