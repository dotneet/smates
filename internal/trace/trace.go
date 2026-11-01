// Package trace collects what test processes actually used (executed code, files read,
// directories listed) and attributes it to test units.
//
// Language shims write JSON-lines streams into a trace directory (protocol documented in
// shims/node.cjs and shims/py/smates_trace.py); V8 coverage files and Go test logs are
// read from the same directory. Collect parses everything into processes; Attribute maps
// processes and their contexts to units.
package trace

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
)

//go:embed shims
var shimFS embed.FS

// ExtractShims writes the embedded shims into dir and returns their locations.
func ExtractShims(dir string) (*Shims, error) {
	err := fs.WalkDir(shimFS, "shims", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "shims")))
		if p == "shims/node.cjs" {
			dst = NodeShim(dir)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
		}
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := shimFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		return nil, err
	}
	return &Shims{Dir: dir, Node: NodeShim(dir), Python: filepath.Join(dir, "py")}, nil
}

// NodeShim is where the Node shim is extracted in a shim directory. Inside a
// node_modules directory: V8 coverage lists the preloaded shim like any other script,
// and coverage reports (node --test --experimental-test-coverage, c8, vitest, jest)
// leave out files below node_modules, so the shim never shows up in the user's report.
func NodeShim(dir string) string { return filepath.Join(dir, "node_modules", "smates", "node.cjs") }

// Shims are the extracted shim locations.
type Shims struct {
	Dir    string
	Node   string // --require / --preload module
	Python string // directory to prepend to PYTHONPATH
}

// V8Dir is where NODE_V8_COVERAGE output goes inside a trace directory.
func V8Dir(traceDir string) string { return filepath.Join(traceDir, "v8") }

// NodeEnv returns environment variables that trace Node and Bun processes. Bun ignores
// NODE_OPTIONS --require: it preloads the shim through BUN_OPTIONS (a process that also
// preloads it with bun test --preload loads it once).
func NodeEnv(traceDir, repoRoot string, sh *Shims, base []string) []string {
	opts := getenv(base, "NODE_OPTIONS")
	req := "--require=" + quoteNodeOption(sh.Node)
	if !strings.Contains(opts, sh.Node) {
		opts = strings.TrimSpace(req + " " + opts)
	}
	bunOpts := getenv(base, "BUN_OPTIONS")
	if !strings.Contains(bunOpts, sh.Node) {
		bunOpts = strings.TrimSpace(bunOpts + " " + bunPreloadOption(sh.Node))
	}
	return []string{
		"SMATES_TRACE_DIR=" + traceDir,
		"SMATES_REPO_ROOT=" + repoRoot,
		"NODE_V8_COVERAGE=" + V8Dir(traceDir),
		"NODE_OPTIONS=" + opts,
		"BUN_OPTIONS=" + bunOpts,
	}
}

// bunPreloadOption is the BUN_OPTIONS argument preloading p. Bun splits BUN_OPTIONS at
// whitespace without escapes, but takes a whole argument in quotes.
func bunPreloadOption(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"--preload=` + p + `"`
	}
	return "--preload=" + p
}

func quoteNodeOption(p string) string {
	if strings.ContainsAny(p, " \t\"'\\") {
		return strconv.Quote(p)
	}
	return p
}

// PythonEnv returns environment variables that trace Python processes. pytest loads the
// attribution plugin through PYTEST_PLUGINS.
func PythonEnv(traceDir, repoRoot string, sh *Shims, base []string) []string {
	pp := sh.Python
	if cur := getenv(base, "PYTHONPATH"); cur != "" {
		pp += string(os.PathListSeparator) + cur
	}
	plugins := "smates_pytest"
	if cur := getenv(base, "PYTEST_PLUGINS"); cur != "" {
		plugins = cur + "," + plugins
	}
	return []string{
		"SMATES_TRACE_DIR=" + traceDir,
		"SMATES_REPO_ROOT=" + repoRoot,
		"PYTHONPATH=" + pp,
		"PYTEST_PLUGINS=" + plugins,
	}
}

// PytestResultsEnv returns environment variables that load the pytest plugin without
// tracing: it only writes per-file results into traceDir (see Results).
func PytestResultsEnv(traceDir string, sh *Shims, base []string) []string {
	// With an empty SMATES_REPO_ROOT the shims trace nothing.
	return PythonEnv(traceDir, "", sh, base)
}

func getenv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], key+"=") {
			return env[i][len(key)+1:]
		}
	}
	return os.Getenv(key)
}

// Proc is one traced process (or Node worker thread).
type Proc struct {
	Pid, Ppid, Tid int
	// Anc is the key ("pid-tid") of the nearest traced ancestor, inherited through the
	// environment; it links processes started through untraced shells.
	Anc        string
	Lang       string
	Argv       []string
	Cwd        string
	Start, End int64 // unix ms (End 0 if unknown)
	Exit       *int
	// Code are executed source files, Reads data files read, Dirs listed directories;
	// all repo-relative. Dirs are stored with a trailing "/".
	Code, Reads map[string]bool
	// Ctx maps attribution contexts ("test:<repo-rel file>", "collect:...",
	// "fixture:...", "" for none) to the repo-relative paths used within them.
	Ctx map[string]map[string]bool
	// CtxSpans lists context switches in order (for attributing child processes).
	CtxSpans []Span
	// Uses maps a test file to the extra contexts (fixtures) it used.
	Uses map[string][]string
	// ReadsBy maps a repo source file to the dependency keys it read (or imported) when
	// it ran: module-level and cached reads happen once per process, so they are given
	// to every test depending on the reading file.
	ReadsBy map[string]map[string]bool
	// Results maps a test file (repo-relative) to its status and duration.
	Results map[string]Result
	// Coverage reports whether code coverage was collected for this process.
	Coverage bool
	// SpawnKey is the process ("pid-tid") that exported SpawnCtx, its attribution context
	// when it started this process (or a traced ancestor of it), through the environment
	// (SMATES_TRACE_CTX="<key>:<context>").
	SpawnKey, SpawnCtx string
	// SpawnFiles maps contexts of SpawnKey to the keys this process recorded while
	// SpawnKey was in them: SpawnCtx first, then those a forked child followed ("pctx").
	SpawnFiles map[string]map[string]bool
}

// Span is a context switch.
type Span struct {
	At int64
	ID string
}

// Result is a per-test-file outcome reported by a runner plugin.
type Result struct {
	Status string
	Ms     int64
	// Message is the failure output reported for the file (bounded, may be empty).
	Message string
}

// MaxMessage bounds the failure message kept per test file.
const MaxMessage = 4000

// JoinMessage appends a failure message to another, keeping the result within
// MaxMessage bytes.
func JoinMessage(a, b string) string {
	b = strings.TrimSpace(b)
	switch {
	case b == "" || len(a) >= MaxMessage:
		return a
	case a != "":
		b = a + "\n\n" + b
	}
	if len(b) > MaxMessage {
		cut := MaxMessage
		for cut > 0 && !utf8.RuneStart(b[cut]) {
			cut--
		}
		b = b[:cut] + "\n[…]"
	}
	return b
}

// Key identifies a process or thread.
func (p *Proc) Key() string { return fmt.Sprintf("%d-%d", p.Pid, p.Tid) }

// GlobalContext marks activity that affects every test of a run (global setup).
const GlobalContext = "global:"

// HasContexts reports whether a runner plugin attributed activity inside this process
// to individual tests.
func (p *Proc) HasContexts() bool {
	for _, s := range p.CtxSpans {
		if s.ID != "" && s.ID != GlobalContext {
			return true
		}
	}
	return false
}

func (p *Proc) add(ctx, kind, rel string) string {
	switch kind {
	case "code":
		p.Code[rel] = true
	case "dir":
		rel = depkey.Listing(rel)
		p.Reads[rel] = true
	case "probe":
		rel = depkey.Existence(rel)
		p.Reads[rel] = true
	case "key": // already a dependency key
		p.Reads[rel] = true
	case "tree":
		rel = depkey.Tree(rel)
		p.Reads[rel] = true
	case "env": // rel is the variable's name
		rel = depkey.Env(rel)
		p.Reads[rel] = true
	default:
		p.Reads[rel] = true
	}
	m := p.Ctx[ctx]
	if m == nil {
		m = map[string]bool{}
		p.Ctx[ctx] = m
	}
	m[rel] = true
	return rel
}

func newProc(pid, tid int) *Proc {
	return &Proc{Pid: pid, Tid: tid, Code: map[string]bool{}, Reads: map[string]bool{}, Ctx: map[string]map[string]bool{}, Uses: map[string][]string{}, Results: map[string]Result{}, ReadsBy: map[string]map[string]bool{}}
}

type event struct {
	K      string   `json:"k"`
	P      string   `json:"p"`
	ID     string   `json:"id"`
	T      int64    `json:"t"`
	Pid    int      `json:"pid"`
	Ppid   int      `json:"ppid"`
	Anc    string   `json:"anc"`
	By     string   `json:"by"`
	Tid    int      `json:"tid"`
	Argv   []string `json:"argv"`
	Cwd    string   `json:"cwd"`
	Lang   string   `json:"lang"`
	Code   *int     `json:"code"`
	Args   []string `json:"args"`
	Shell  bool     `json:"shell"`
	Ctx    []string `json:"ctx"`
	Status string   `json:"status"`
	Ms     int64    `json:"ms"`
	Msg    string   `json:"msg"`
	Pctx   string   `json:"pctx"`
}

var (
	procFileRe = regexp.MustCompile(`^proc-(\d+)-(\d+)\.jsonl$`)
	v8FileRe   = regexp.MustCompile(`^coverage-(\d+)-(\d+)-(\d+)\.json$`)
)

// Collect reads a trace directory. repoRoot must be absolute with symlinks resolved.
func Collect(traceDir, repoRoot string) ([]*Proc, error) {
	procs := map[string]*Proc{}
	get := func(pid, tid int) *Proc {
		k := fmt.Sprintf("%d-%d", pid, tid)
		p := procs[k]
		if p == nil {
			p = newProc(pid, tid)
			procs[k] = p
		}
		return p
	}
	rel := relCache(repoRoot)

	entries, err := os.ReadDir(traceDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		m := procFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		tid, _ := strconv.Atoi(m[2])
		if err := readStream(filepath.Join(traceDir, e.Name()), get(pid, tid), rel, repoRoot); err != nil {
			return nil, err
		}
	}

	for _, dir := range v8Dirs(traceDir) {
		v8, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range v8 {
			m := v8FileRe.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			pid, _ := strconv.Atoi(m[1])
			ts, _ := strconv.ParseInt(m[2], 10, 64)
			tid, _ := strconv.Atoi(m[3])
			p := get(pid, tid)
			p.Coverage = true
			if ts > p.End {
				p.End = ts
			}
			urls, err := v8URLs(filepath.Join(dir, e.Name()))
			if err != nil {
				continue // a truncated file from a killed process: skip it
			}
			for _, u := range urls {
				if r, ok := rel(u); ok {
					p.add("", "code", r)
				}
			}
		}
	}

	out := make([]*Proc, 0, len(procs))
	for _, p := range procs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pid != out[j].Pid {
			return out[i].Pid < out[j].Pid
		}
		return out[i].Tid < out[j].Tid
	})
	return out, nil
}

// RelFunc returns a memoized converter from absolute paths to repo-relative ones that
// rejects vendored paths.
func RelFunc(root string) func(string) (string, bool) { return relCache(root) }

func relCache(root string) func(string) (string, bool) {
	type res struct {
		r  string
		ok bool
	}
	cache := map[string]res{}
	return func(p string) (string, bool) {
		if v, ok := cache[p]; ok {
			return v.r, v.ok
		}
		r, ok := pathutil.Rel(root, p)
		if ok && pathutil.IsVendored(r) {
			ok = false
		}
		cache[p] = res{r, ok}
		return r, ok
	}
}

func readStream(file string, p *Proc, rel func(string) (string, bool), root string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	ctx, spawn := "", ""
	add := func(kind, r string) string {
		key := p.add(ctx, kind, r)
		if p.SpawnKey != "" {
			if p.SpawnFiles == nil {
				p.SpawnFiles = map[string]map[string]bool{}
			}
			if p.SpawnFiles[spawn] == nil {
				p.SpawnFiles[spawn] = map[string]bool{}
			}
			p.SpawnFiles[spawn][key] = true
		}
		return key
	}
	linkCache := map[string][]string{}
	links := func(path string) []string {
		l, ok := linkCache[path]
		if !ok {
			l = pathutil.Links(root, path)
			linkCache[path] = l
		}
		return l
	}
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue // partial line from a killed process
		}
		switch ev.K {
		case "start":
			p.Ppid, p.Anc, p.Argv, p.Cwd, p.Lang, p.Start = ev.Ppid, ev.Anc, ev.Argv, ev.Cwd, ev.Lang, ev.T
			if key, c, ok := strings.Cut(ev.Pctx, ":"); ok && key != "" && p.SpawnKey == "" {
				p.SpawnKey, p.SpawnCtx = key, relCtx(c, rel)
				spawn = p.SpawnCtx
			}
		case "pctx": // a forked child follows its parent into another context
			if key, c, ok := strings.Cut(ev.ID, ":"); ok && key == p.SpawnKey {
				spawn = relCtx(c, rel)
			}
		case "read", "code", "dir", "probe", "tree":
			var keys []string
			if r, ok := rel(ev.P); ok {
				keys = append(keys, add(ev.K, r))
				// The symlinks the path went through are read too: retargeting one
				// changes what the test sees.
				for _, l := range links(ev.P) {
					keys = append(keys, add("read", l))
				}
			} else if (ev.K == "dir" || ev.K == "tree") && isRoot(ev.P, root) {
				keys = append(keys, add(ev.K, "."))
			}
			if by, ok := rel(ev.By); ok && len(keys) > 0 && ev.By != "" {
				m := p.ReadsBy[by]
				if m == nil {
					m = map[string]bool{}
					p.ReadsBy[by] = m
				}
				for _, key := range keys {
					m[key] = true
				}
			}
		case "env":
			if ev.P == "" {
				continue
			}
			key := add("env", ev.P)
			if by, ok := rel(ev.By); ok && ev.By != "" {
				m := p.ReadsBy[by]
				if m == nil {
					m = map[string]bool{}
					p.ReadsBy[by] = m
				}
				m[key] = true
			}
		case "exec":
			cwd := ev.Cwd
			if cwd == "" {
				cwd = p.Cwd
			}
			for _, k := range execDeps(ev.Args, ev.Shell, cwd, root, rel) {
				add("key", k)
			}
		case "ctx":
			ctx = relCtx(ev.ID, rel)
			p.CtxSpans = append(p.CtxSpans, Span{At: ev.T, ID: ctx})
		case "uses":
			if r, ok := rel(ev.ID); ok {
				for _, c := range ev.Ctx {
					p.Uses[r] = append(p.Uses[r], relCtx(c, rel))
				}
			}
		case "result":
			if r, ok := rel(ev.ID); ok {
				addResult(p.Results, r, ev.Status, ev.Ms, ev.Msg)
			}
		case "exit":
			if ev.Code != nil {
				c := *ev.Code
				p.Exit = &c
			}
			if ev.T > p.End {
				p.End = ev.T
			}
		}
	}
	return sc.Err()
}

// relCtx converts "kind:/abs/path" contexts to "kind:repo/rel"; other forms are kept.
func relCtx(id string, rel func(string) (string, bool)) string {
	i := strings.IndexByte(id, ':')
	if i < 0 {
		return id
	}
	kind, p := id[:i], id[i+1:]
	if kind == "test" || kind == "collect" {
		if r, ok := rel(p); ok {
			return kind + ":" + r
		}
	}
	return id
}

var urlKey = []byte(`"url":"`)

// v8URLs extracts script URLs from a V8 coverage JSON file without decoding the
// (large) function range data.
func v8URLs(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	// A file cut short anywhere (even right after a range object) is incomplete: the
	// scripts after the cut would be missing.
	if !json.Valid(data) {
		return nil, fmt.Errorf("truncated coverage file")
	}
	var urls []string
	for {
		i := bytes.Index(data, urlKey)
		if i < 0 {
			break
		}
		data = data[i+len(urlKey):]
		j := 0
		for j < len(data) && data[j] != '"' {
			if data[j] == '\\' {
				j++
			}
			j++
		}
		if j > len(data) {
			break
		}
		raw := data[:j]
		data = data[j:]
		s := string(raw)
		if bytes.IndexByte(raw, '\\') >= 0 {
			if u, err := strconv.Unquote(`"` + s + `"`); err == nil {
				s = u
			}
		}
		if strings.HasPrefix(s, "file://") || strings.HasPrefix(s, "/") || (len(s) > 2 && s[1] == ':') {
			urls = append(urls, s)
		}
	}
	return urls, nil
}

// isRoot reports whether p names the repository root itself (also through a symlink).
func isRoot(p, root string) bool {
	p = filepath.Clean(p)
	if p == root {
		return true
	}
	r, err := filepath.EvalSymlinks(p)
	return err == nil && r == root
}
