package runner

// Recording of gradle suites (docs/DESIGN.md section 2, "Gradle").
//
// While recording, the init script runs one JVM per test class (forkEvery = 1, unless
// isolate: false) and starts the JDK Flight Recorder in each, with settings
// (shims/gradle/smates.jfc) that keep the classes the JVM loaded, the files it read, the
// processes it started and its command line. It also writes the facts of each Test task
// smates needs to map paths. After the build, one JVM runs the collector
// (shims/gradle/Collect.java, in source-file mode, on the task's own Java) over the
// recordings.
//
// Attribution: the listener says which classes ran in which executor ("Gradle Test
// Executor N"), the recording's sun.java.command names the executor of its JVM. A
// unit's dependencies are those of every JVM that ran one of its classes. What a JVM
// did before its first test (Gradle's worker, JUnit's discovery) belongs to it too;
// Gradle's jars and the JDK lie outside the repository and drop out. A JVM that ran no
// class of a unit (an abstract class, a nested class run through its outer one) is
// dropped. A unit that ran in a JVM without a readable recording is NoTrace: its pass
// is recorded as unknown.
//
// Mapping to repository keys (gradleMapper):
//   - a loaded class maps to every Java source declaring its top-level class (lambda
//     and hidden classes, nested classes: the enclosing top-level class); a class no
//     source declares (a library, generated code) adds nothing;
//   - a read below a source set's resources output directory maps to the file of the
//     same relative path in the source set's resource directories (all of them when
//     none has it: a generated resource); a read of a project's jar to that project's
//     main resource trees (dir/**); reads below class output directories are dropped
//     (class loads stand for them); other reads in the repository are kept as they are,
//     relative ones resolved against the task's working directory;
//   - a read file adds the listing of its directory: JFR has no event for listings, and
//     a test that read a file next to others may be scanning that directory;
//   - a started process is analyzed from its command line (trace.CommandLineDeps); a
//     Node, Bun or Python child that traced itself is attributed to the JVM that started
//     it (or its traced ancestor), otherwise to every unit of the run.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/pathutil"
	"github.com/dotneet/smates/internal/static/java"
	"github.com/dotneet/smates/internal/trace"
)

// gradleFacts is what the init script writes about a Test task (facts-<task>.json).
type gradleFacts struct {
	Task        string          `json:"task"`
	WorkingDir  string          `json:"workingDir"`
	Java        string          `json:"java"`
	JavaVersion int             `json:"javaVersion"`
	JFR         bool            `json:"jfr"`
	Projects    []gradleProject `json:"projects"`
	// Error says why the projects could not be described: paths cannot be mapped.
	Error string `json:"error"`
}

type gradleProject struct {
	Path       string            `json:"path"`
	Dir        string            `json:"dir"`
	SourceSets []gradleSourceSet `json:"sourceSets"`
	Jar        string            `json:"jar"`
}

type gradleSourceSet struct {
	Name         string   `json:"name"`
	Resources    []string `json:"resources"`
	ResourcesOut string   `json:"resourcesOut"`
	Classes      []string `json:"classes"`
}

// jvmRecording is what the collector read from one JVM's recording.
type jvmRecording struct {
	Pid      int
	Executor string
	Classes  []string
	Reads    []string
	Procs    []jvmProc
	Err      string
}

type jvmProc struct {
	Pid          int
	Dir, Command string
}

// gradleRecordEnv returns the environment that makes the init script record into dir,
// and the tracing environment of Node, Bun and Python children.
func gradleRecordEnv(rc *RunContext, dir string) []string {
	trc := trace.PytestEnv(rc.TraceDir, rc.Repo.Root, rc.Shims(), suiteEnv(rc.Suite))
	var names []string
	for _, kv := range trc {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	env := append([]string{
		"SMATES_GRADLE_RECORD=" + dir,
		"SMATES_GRADLE_JFC=" + filepath.Join(rc.ShimDir, "gradle", "smates.jfc"),
		"SMATES_GRADLE_ENV=" + strings.Join(names, ","),
	}, trc...)
	if !config.Bool(rc.Suite.Isolate, true) {
		env = append(env, "SMATES_GRADLE_ISOLATE=false")
	}
	return env
}

func readGradleFacts(dir string) ([]gradleFacts, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "facts-*.json"))
	sort.Strings(files)
	var out []gradleFacts
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var fa gradleFacts
		if err := json.Unmarshal(b, &fa); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		out = append(out, fa)
	}
	return out, nil
}

// collectJFR runs the collector with java over the recordings in dir.
func collectJFR(ctx context.Context, java, shimDir, dir string) ([]*jvmRecording, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, java, filepath.Join(shimDir, "gradle", "Collect.java"), dir)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("reading the flight recordings: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseCollected(stdout.Bytes()), nil
}

var (
	executorName = regexp.MustCompile(`'(Gradle Test Executor \d+)'`)
	jfrPid       = regexp.MustCompile(`^jvm-(\d+)(?:-[\d_]+)?\.jfr$`)
)

// parseCollected reads the collector's output (see Collect.java).
func parseCollected(out []byte) []*jvmRecording {
	var recs []*jvmRecording
	var cur *jvmRecording
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		for i := range fields {
			fields[i] = unescapeCollected(fields[i])
		}
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "jvm" {
			cur = &jvmRecording{}
			if m := jfrPid.FindStringSubmatch(fields[1]); m != nil {
				cur.Pid, _ = strconv.Atoi(m[1])
			}
			recs = append(recs, cur)
			continue
		}
		if cur == nil {
			continue
		}
		switch fields[0] {
		case "command":
			if m := executorName.FindStringSubmatch(fields[1]); m != nil {
				cur.Executor = m[1]
			}
		case "class":
			cur.Classes = append(cur.Classes, fields[1])
		case "read":
			cur.Reads = append(cur.Reads, fields[1])
		case "proc":
			if len(fields) >= 4 {
				pid, _ := strconv.Atoi(fields[1])
				cur.Procs = append(cur.Procs, jvmProc{Pid: pid, Dir: fields[2], Command: fields[3]})
			}
		case "error":
			cur.Err = fields[1]
		}
	}
	return recs
}

func unescapeCollected(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// gradleMapper maps what a JVM loaded and read to repository dependency keys.
type gradleMapper struct {
	root string
	rel  func(string) (string, bool)
	// types maps a qualified top-level Java type to the repository files declaring it.
	types map[string][]string
	// classes are the repo-relative class output directories; resources the source
	// sets' resource directories by their repo-relative output directory; jars the
	// projects' main resource trees by their repo-relative jar.
	classes   []classDir
	resources map[string][]string
	jars      map[string][]string
	exists    func(string) bool
	// generated caches whether a loaded class without a tracked source has a class file
	// in a class output directory: the tree of its project, else "".
	generated map[string]string
}

// classDir is a repo-relative class output directory and the dependency key of the
// tree of the project it belongs to.
type classDir struct{ dir, project string }

func newGradleMapper(root string, javaFiles []string, read func(string) []byte, facts []gradleFacts) *gradleMapper {
	m := &gradleMapper{root: root, rel: trace.RelFunc(root), types: map[string][]string{}, resources: map[string][]string{}, jars: map[string][]string{},
		exists: func(p string) bool { return exists(filepath.Join(root, filepath.FromSlash(p))) }}
	for _, f := range javaFiles {
		src := read(f)
		if src == nil {
			continue
		}
		in := java.Describe(src)
		for _, d := range in.Decls {
			q := qualifyJava(in.Package, d.Name)
			m.types[q] = append(m.types[q], f)
		}
	}
	seen := map[string]bool{}
	for _, fa := range facts {
		for _, p := range fa.Projects {
			if seen[p.Path+"\x00"+p.Dir] {
				continue
			}
			seen[p.Path+"\x00"+p.Dir] = true
			var mainRes []string
			for _, ss := range p.SourceSets {
				var srcs []string
				for _, r := range ss.Resources {
					if rr, ok := m.rel(r); ok {
						srcs = append(srcs, rr)
					}
				}
				if out, ok := m.rel(ss.ResourcesOut); ok && ss.ResourcesOut != "" {
					m.resources[out] = append(m.resources[out], srcs...)
				}
				for _, c := range ss.Classes {
					if cr, ok := m.rel(c); ok {
						cd := classDir{dir: cr}
						if pd, ok := m.rel(p.Dir); ok && p.Dir != "" {
							cd.project = depkey.Tree(pd)
						}
						m.classes = append(m.classes, cd)
					}
				}
				if ss.Name == "main" {
					mainRes = srcs
				}
			}
			if jar, ok := m.rel(p.Jar); ok && p.Jar != "" {
				for _, r := range mainRes {
					m.jars[jar] = append(m.jars[jar], depkey.Tree(r))
				}
			}
		}
	}
	return m
}

// class returns the sources declaring the top-level class of a loaded class. A class no
// tracked source declares whose class file is in a class output directory of the build
// (generated code, another JVM language) counts as the tree of its project: what it was
// made from is unknown.
func (m *gradleMapper) class(name string) []string {
	if i := strings.IndexAny(name, "$/"); i >= 0 {
		name = name[:i]
	}
	if fs := m.types[name]; len(fs) > 0 {
		return fs
	}
	if platformClass(name) {
		return nil
	}
	if m.generated == nil {
		m.generated = map[string]string{}
	}
	tree, ok := m.generated[name]
	if !ok {
		file := strings.ReplaceAll(name, ".", "/") + ".class"
		for _, c := range m.classes {
			if c.project != "" && m.exists(pathutil.Join(c.dir, file)) {
				tree = c.project
				break
			}
		}
		m.generated[name] = tree
	}
	if tree == "" {
		return nil
	}
	return []string{tree}
}

// platformClass reports whether a class belongs to the JDK, Gradle or JUnit, which no
// class output directory of the build holds.
func platformClass(name string) bool {
	for _, p := range []string{"java.", "javax.", "jdk.", "sun.", "com.sun.", "org.gradle.", "org.junit.", "worker.org.gradle."} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// read returns the keys of a file a JVM read (relative paths against wd).
func (m *gradleMapper) read(p, wd string) []string {
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(wd, p)
	}
	r, ok := m.rel(p)
	if !ok || r == "." {
		return nil
	}
	if keys, ok := m.jars[r]; ok {
		return keys
	}
	// Resources first: a build may put them in a class output directory.
	for out, srcs := range m.resources {
		if !pathutil.Under(r, out) || r == out {
			continue
		}
		sub := pathutil.RelTo(r, out)
		var keys []string
		for _, s := range srcs {
			if f := pathutil.Join(s, sub); m.exists(f) {
				keys = append(keys, f, depkey.Listing(path.Dir(f)))
			}
		}
		if len(keys) == 0 {
			// A resource no source directory holds was generated: what it was made
			// from is unknown, so every resource of the source set counts.
			for _, s := range srcs {
				keys = append(keys, depkey.Tree(s))
			}
		}
		return keys
	}
	for _, c := range m.classes {
		if !pathutil.Under(r, c.dir) || r == c.dir {
			continue
		}
		if strings.HasSuffix(r, ".class") {
			// The class load stands for it (its source, else its project).
			return m.class(strings.ReplaceAll(strings.TrimSuffix(pathutil.RelTo(r, c.dir), ".class"), "/", "."))
		}
		// Another file a tool wrote there (an annotation processor's
		// META-INF/services): made from something in its project.
		if c.project != "" {
			return []string{c.project}
		}
		return nil
	}
	return []string{r, depkey.Listing(path.Dir(r))}
}

// gradleRecorded adds what the recordings say to the observations of the units: the
// dependencies of every JVM that ran a class of the unit. A unit that ran in a JVM
// whose recording is missing or unreadable, or that ran in no known JVM, stays NoTrace.
// procs are the Node, Bun and Python processes that traced themselves.
func gradleRecorded(obs map[string]*Observation, events []gradleEvent, classes map[string]string, recs []*jvmRecording, facts []gradleFacts, m *gradleMapper, procs []*trace.Proc) {
	wd := map[string]string{}
	for _, fa := range facts {
		wd[fa.Task] = fa.WorkingDir
	}
	unitOf := func(class string) string {
		if i := strings.IndexByte(class, '$'); i >= 0 {
			class = class[:i]
		}
		return classes[class]
	}
	// The units and the task of each executor.
	byExec := map[string]map[string]bool{}
	taskOf := map[string]string{}
	ranIn := map[string]map[string]bool{} // unit -> executors
	for _, ev := range events {
		u := unitOf(ev.Class)
		if u == "" || ev.Executor == "" {
			continue
		}
		if byExec[ev.Executor] == nil {
			byExec[ev.Executor] = map[string]bool{}
		}
		byExec[ev.Executor][u] = true
		taskOf[ev.Executor] = ev.Task
		if ranIn[u] == nil {
			ranIn[u] = map[string]bool{}
		}
		ranIn[u][ev.Executor] = true
	}
	// Files per executor.
	files := map[string]map[string]bool{}
	// child pid -> executors: pids are reused, and the JVMs run one after the other, so
	// a pid may stand for children of several of them.
	childOf := map[int][]string{}
	for _, r := range recs {
		if r.Executor == "" || byExec[r.Executor] == nil {
			continue // a JVM that ran no class of a unit
		}
		if r.Err != "" {
			files[r.Executor] = nil // unreadable: the units stay untraced
			continue
		}
		fs := map[string]bool{}
		for _, c := range r.Classes {
			for _, f := range m.class(c) {
				fs[f] = true
			}
		}
		dir := wd[taskOf[r.Executor]]
		for _, p := range r.Reads {
			for _, k := range m.read(p, dir) {
				fs[k] = true
			}
		}
		for _, p := range r.Procs {
			if !slices.Contains(childOf[p.Pid], r.Executor) {
				childOf[p.Pid] = append(childOf[p.Pid], r.Executor)
			}
			cwd := p.Dir
			if cwd == "" {
				cwd = dir // a child started without a directory runs in the JVM's
			}
			for _, k := range trace.CommandLineDeps(p.Command, cwd, m.root) {
				fs[k] = true
			}
		}
		if _, bad := files[r.Executor]; !bad {
			files[r.Executor] = fs
		}
	}
	// Children that traced themselves: by parent process, or through traced ancestors.
	byKey := map[string]*trace.Proc{}
	for _, p := range procs {
		byKey[p.Key()] = p
	}
	// owner returns the executors a child may belong to: every one whose JVM started a
	// process with its pid (or its parent's), when a pid was reused.
	var owner func(p *trace.Proc, depth int) []string
	owner = func(p *trace.Proc, depth int) []string {
		if e, ok := childOf[p.Pid]; ok {
			return e
		}
		if e, ok := childOf[p.Ppid]; ok {
			return e
		}
		if a := byKey[p.Anc]; a != nil && a != p && depth < 64 {
			return owner(a, depth+1)
		}
		return nil
	}
	global := map[string]bool{}
	for _, p := range procs {
		execs := owner(p, 0)
		for f := range p.Files() {
			if len(execs) == 0 {
				global[f] = true
			}
			for _, e := range execs {
				if files[e] != nil {
					files[e][f] = true
				}
			}
		}
	}
	for u, o := range obs {
		execs := ranIn[u]
		traced := len(execs) > 0
		for e := range execs {
			fs, ok := files[e]
			if !ok || fs == nil {
				traced = false
				continue
			}
			for f := range fs {
				o.AddFile(f)
			}
		}
		if !traced {
			continue
		}
		for f := range global {
			o.AddFile(f)
		}
		o.NoTrace = false
	}
}

// gitJavaFiles lists the repository's Java sources (tracked and untracked, not ignored).
func gitJavaFiles(rc *RunContext) []string {
	out, err := rc.Repo.Git("ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.java")
	if err != nil {
		return nil
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, filepath.ToSlash(f))
		}
	}
	sort.Strings(files)
	return slices.Compact(files)
}
