package trace

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// readers only read the paths named on their command line (recursively for -r tools).
var readers = map[string]bool{
	"cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true, "rg": true,
	"sed": true, "awk": true, "sort": true, "uniq": true, "wc": true, "cut": true, "tr": true,
	"diff": true, "cmp": true, "jq": true, "yq": true, "ls": true, "find": true, "test": true,
	"[": true, "[[": true, "echo": true, "printf": true, "true": true, "false": true, "md5sum": true,
	"sha1sum": true, "sha256sum": true, "shasum": true, "base64": true, "stat": true, "file": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true, "pwd": true,
	"uname": true, "hostname": true, "whoami": true, "id": true, "nproc": true, "arch": true,
	"getconf": true, "sw_vers": true, "sysctl": true, "vm_stat": true, "locale": true, "tty": true,
	"printenv": true, "sleep": true,
}

// nameOnly commands never open files, whatever their arguments. That includes the
// system information commands libraries run on their own: platform.processor() runs
// `uname -p` in every pytest-xdist worker, tinypool (vitest) `sysctl -n
// hw.physicalcpu_max` in every worker on macOS, for example. tr only reads its standard
// input.
var nameOnly = map[string]bool{
	"basename": true, "dirname": true, "pwd": true, "echo": true, "printf": true, "true": true, "false": true,
	"uname": true, "hostname": true, "whoami": true, "id": true, "nproc": true, "arch": true,
	"getconf": true, "sw_vers": true, "tr": true, "sysctl": true, "vm_stat": true, "locale": true, "tty": true,
	"printenv": true, "sleep": true,
}

// scriptFirst readers take a script or pattern as their first operand, unless an option
// gives one (grep -e, sed -f, awk -f ...).
var scriptFirst = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "sed": true, "awk": true, "jq": true, "yq": true,
}

// readerOpts lists, per reader, the options whose arguments are separate words, one
// letter per argument: 's' an inline script or pattern (the first operand is then a
// file), 'f' a script or pattern file, 'p' another file the command reads, 'd' a
// directory it reads files from, 'l' a file listing more files to read (NUL-separated),
// 'x' a program it runs, '%' a format (an argument only when it contains "%": BSD stat -f
// FORMAT, GNU stat -f takes none), 'o' anything else (a number, a separator, a name, an
// output file).
var readerOpts = map[string]map[string]string{
	"grep": grepOpts, "egrep": grepOpts, "fgrep": grepOpts,
	"rg": {
		"-e": "s", "--regexp": "s", "-f": "f", "--file": "f", "-g": "o", "--glob": "o", "--iglob": "o",
		"-t": "o", "--type": "o", "-T": "o", "--type-not": "o", "-m": "o", "--max-count": "o",
		"-A": "o", "-B": "o", "-C": "o", "--context": "o", "-r": "o", "--replace": "o", "-j": "o",
		"--threads": "o", "-M": "o", "--max-columns": "o", "-E": "o", "--encoding": "o",
		"--ignore-file": "p", "-d": "o", "--max-depth": "o", "--max-filesize": "o", "--sort": "o",
		"--sortr": "o", "--type-add": "o", "--colors": "o", "--path-separator": "o", "--pre": "x",
		"--pre-glob": "o",
	},
	"sed":    {"-e": "s", "--expression": "s", "-f": "f", "--file": "f"},
	"awk":    {"-f": "f", "-v": "o", "-F": "o"},
	"jq":     {"-f": "f", "--from-file": "f", "--arg": "oo", "--argjson": "oo", "--slurpfile": "op", "--rawfile": "op", "--indent": "o", "-L": "d"},
	"yq":     {"-f": "o", "--from-file": "f", "-o": "o", "--output-format": "o", "-p": "o", "--input-format": "o", "-I": "o", "--indent": "o"},
	"head":   {"-n": "o", "-c": "o"},
	"tail":   {"-n": "o", "-c": "o"},
	"sort":   {"-k": "o", "--key": "o", "-t": "o", "--field-separator": "o", "-o": "o", "--output": "o", "-S": "o", "-T": "o", "--files0-from": "l"},
	"wc":     {"--files0-from": "l"},
	"cut":    {"-d": "o", "-f": "o", "-c": "o", "-b": "o"},
	"uniq":   {"-f": "o", "-s": "o"},
	"diff":   {"-U": "o", "-C": "o", "-x": "o", "-X": "p", "-I": "o", "--ignore-matching-lines": "o"},
	"cmp":    {"-i": "o", "-n": "o"},
	"stat":   {"-c": "o", "-f": "%", "-t": "%"},
	"shasum": {"-a": "o"},
	"base64": {"-i": "p", "-o": "o"},
	"file":   {"-m": "p"},
}

var grepOpts = map[string]string{
	"-e": "s", "--regexp": "s", "-f": "f", "--file": "f", "-m": "o", "--max-count": "o",
	"-A": "o", "-B": "o", "-C": "o", "--context": "o", "-d": "o", "--directories": "o", "-D": "o", "--devices": "o",
	"--include": "o", "--exclude": "o", "--exclude-dir": "o", "--exclude-from": "p", "--label": "o",
	"--binary-files": "o", "--group-separator": "o",
}

// testFileOps are the test/[ operators whose operand is a path.
var testFileOps = map[string]bool{
	"-e": true, "-f": true, "-d": true, "-r": true, "-w": true, "-x": true, "-s": true, "-L": true, "-h": true,
	"-b": true, "-c": true, "-p": true, "-S": true, "-g": true, "-u": true, "-k": true, "-O": true, "-G": true,
	"-N": true, "-nt": true, "-ot": true, "-ef": true,
}

// awkIO matches awk programs that read or write files or run commands themselves.
var awkIO = regexp.MustCompile(`\bgetline\b|\bsystem[ \t]*\(|\bprintf?\b[^;}\n]*[>|]`)

// command analyzes one simple command (argv) run in cwd. shell says whether its words
// come from shell code (a shell expanded their globs) or from an argv.
func (a *execAnalysis) command(seg []string, cwd string, depth int, shell bool) {
	seg = trimAssignments(seg)
	for len(seg) > 0 && keywords[seg[0]] {
		seg = seg[1:]
	}
	if len(seg) == 0 {
		return
	}
	// "${PYTHON:-python}" names the program through a variable with a default: judge
	// the program by the default (the child traces itself if it is a runtime).
	if m := varDefault.FindStringSubmatch(seg[0]); m != nil {
		seg = append([]string{m[1]}, seg[1:]...)
	}
	prog := filepath.Base(seg[0])
	if strings.Contains(seg[0], "$") {
		a.unresolved() // an unknown program
		return
	}
	// A shell (running a -c string, a script file or stdin), or a script started
	// directly: analyze the shell code.
	if shells[prog] {
		a.shellInvocation(seg[1:], cwd, depth, shell)
		return
	}
	if isScriptFile(seg[0], cwd) {
		a.script(seg[0], cwd, depth, positional(seg[1:], shell))
		return
	}
	// A native launcher executes its command itself: analyze that command, which traces
	// itself only when it runs on Node or Python.
	if cmd, dir, ok := launched(seg); ok {
		if strings.Contains(dir, "$") {
			a.unresolved()
			return
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		if len(cmd) > 0 {
			switch {
			case strings.HasSuffix(cmd[0], ".py"):
				cmd = append([]string{"python"}, cmd...) // uv run script.py
			case strings.Contains(cmd[0], "/"):
				// A program named by path: a Node or Python script runs on its runtime
				// (and is read); anything else (a shell script, a binary) is analyzed
				// as itself.
				p := cmd[0]
				if !filepath.IsAbs(p) {
					p = filepath.Join(dir, p)
				}
				if rt := scriptRuntime(p, false); rt != "" {
					cmd = append([]string{rt}, cmd...)
				}
			default:
				if rt := a.consoleScript(cmd[0], dir); rt != "" {
					// The runtime runs the script: its arguments are the tool's own
					// (uv run black -S src is no python -S).
					cmd = append([]string{rt}, cmd...)
				}
			}
			a.command(cmd, dir, depth, shell)
		}
		return
	}
	traced := tracedPrograms[prog] || strings.HasPrefix(prog, "python3.")
	// A runtime traces itself only when its shim loads: the environment must carry it,
	// and Python must not be told to ignore PYTHONPATH or to skip site (-I, -E, -S).
	if traced && ((a.rt != nil && !a.rt[runtimeOf(prog)]) || (runtimeOf(prog) == "python" && skipsSite(seg[1:]))) {
		traced = false
	}
	// A program named by its path (./build, ../bin/tool, /repo/bin/tool) is read to run
	// it, wherever the command runs; one that is missing may appear. A bare name is looked
	// up in PATH.
	if !traced && strings.Contains(seg[0], "/") {
		p := literal(seg[0])
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			a.add(p)
		} else if err != nil && !within(p, cwd) {
			a.existence(p) // below cwd, the tree of the opaque command covers it
		}
	}
	grepLike := prog == "grep" || prog == "egrep" || prog == "fgrep"
	recursive := prog == "rg" || prog == "find"
	for i, w := range seg[1:] {
		cluster := strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--")
		switch {
		case prog == "ls":
			// ls -r reverses the order; only -R recurses.
			recursive = recursive || w == "--recursive" || (cluster && strings.Contains(w, "R"))
		case w == "-r" || w == "-R" || w == "--recursive" || (grepLike && cluster && strings.ContainsAny(w, "rR")):
			recursive = true
		case grepLike && (w == "-drecurse" || w == "--directories=recurse" ||
			((w == "-d" || w == "--directories") && i+2 < len(seg) && seg[i+2] == "recurse")):
			recursive = true // grep -d recurse is grep -r
		}
	}
	a.redirects(seg, cwd)
	args := operands(prog, seg[1:])
	// --files0-from lists the files to read, NUL-separated.
	unresolved := false
	for _, l := range args.lists {
		if l == "-" || strings.Contains(l, "$") {
			unresolved = true // names read from stdin, or an unresolved list
			continue
		}
		for _, m := range expand(l, cwd) {
			a.add(m)
			data, _ := os.ReadFile(m)
			for n, name := range strings.Split(string(data), "\x00") {
				if name != "" && n < 10000 {
					// Names in the list are literal: no glob, no variable.
					args.files = append(args.files, quoteMarks.Replace(strings.ReplaceAll(name, "$", literalDollar)))
				}
			}
		}
	}
	// find -exec runs a command for the files found: {} stands for them, below the
	// paths searched (recorded as trees). -execdir runs it in the directory of each.
	for _, x := range args.execs {
		roots := args.roots
		if len(roots) == 0 {
			roots = []string{"."}
		}
		switch sub := replacePlaceholder(x.cmd, roots); {
		case x.dir:
			for _, r := range roots {
				for _, d := range expand(r, cwd) {
					a.command(replacePlaceholder(x.cmd, []string{"."}), d, depth, shell)
				}
			}
		case len(sub) > max(maxWords, len(x.cmd)):
			a.opaque(cwd)
			a.absoluteTrees(x.cmd, cwd, nil)
		default:
			a.command(sub, cwd, depth, shell)
		}
	}
	// Without a path, ls lists and find, rg and grep -r search the working directory.
	if len(args.files) == 0 && !traced {
		switch {
		case prog == "ls" && !recursive:
			a.listing(cwd)
		case prog == "ls" || prog == "find" || prog == "rg" || (grepLike && recursive):
			a.opaque(cwd)
		}
	}
	// A script that reads files or runs commands itself (awk's getline and system(),
	// yq's load, rg --pre) makes the reader opaque; so does an unresolved awk or sed
	// script.
	scripted := args.runs
	switch prog {
	case "awk":
		scripted = scripted || awkIO.MatchString(literal(args.script))
		for _, f := range args.scriptFiles {
			for _, m := range expand(f, cwd) {
				if data, err := os.ReadFile(m); err == nil && awkIO.Match(data) {
					scripted = true
				}
			}
		}
	case "sed":
		// The r and R commands read a file; e and s///e run commands.
		scripts := []string{literal(args.script)}
		for _, f := range args.scriptFiles {
			for _, m := range expand(f, cwd) {
				if data, err := os.ReadFile(m); err == nil {
					scripts = append(scripts, string(data))
				}
			}
		}
		for _, s := range scripts {
			reads, runs := sedScript(s)
			scripted = scripted || runs
			for _, r := range reads {
				if strings.Contains(r, "$") {
					unresolved = true
					continue
				}
				if !filepath.IsAbs(r) {
					r = filepath.Join(cwd, r)
				}
				a.add(r)
			}
		}
	case "yq":
		scripted = strings.Contains(args.script, "load")
	}
	// Unresolved words (a variable, a command's output) may name any path.
	unresolved = unresolved || ((prog == "awk" || prog == "sed") && strings.Contains(args.script, "$"))
	for _, w := range args.dirs {
		if strings.Contains(w, "$") {
			unresolved = true
			continue
		}
		for _, m := range expand(w, cwd) {
			a.opaque(m)
		}
	}
	for _, w := range args.files {
		if strings.Contains(w, "$") {
			unresolved = true
			continue
		}
		// The shell expands an unquoted glob into the files it matches: the command
		// line changes with the directory. Programs that only print names are left out
		// (echo * output is not a read).
		if shell && !nameOnly[prog] {
			a.glob(w, cwd)
		}
		for _, m := range expand(w, cwd) {
			fi, err := os.Stat(m)
			switch {
			case err != nil:
				// A path that does not exist (yet): the command behaves differently once
				// it does. Paths below the directory of an opaque command are covered by
				// its tree.
				if !traced && !nameOnly[prog] && (readers[prog] || !within(m, cwd)) {
					a.existence(m)
				}
			case fi.IsDir() && traced:
				// A runtime given a directory runs code that traces itself.
			case fi.IsDir() && (recursive || !readers[prog] || prog == "diff"):
				// diff of two directories compares the files they have in common.
				a.opaque(m)
			case fi.IsDir():
				a.listing(m)
			default:
				a.add(m)
			}
		}
	}
	// A file named in an option value or a response file is read (a directory may be);
	// one that is missing may appear, when the value is a path.
	for _, w := range args.values {
		if traced {
			continue
		}
		if strings.Contains(w, "$") {
			unresolved = true
			continue
		}
		for _, m := range expand(w, cwd) {
			fi, err := os.Stat(m)
			switch {
			case err != nil:
				if strings.Contains(w, "/") && !within(m, cwd) {
					a.existence(m)
				}
			case fi.IsDir():
				a.opaque(m)
			default:
				a.add(m)
			}
		}
	}
	switch {
	case traced:
		// The child traces itself.
	case nameOnly[prog]:
		// Only manipulates names, never opens files.
	case readers[prog] && !unresolved && !scripted:
		// Reads only what it names.
	case unresolved:
		a.unresolved()
	default:
		a.opaque(cwd)
	}
}

// cmdArgs is what the arguments of a command name.
type cmdArgs struct {
	files       []string   // operands and file arguments of options (not redirection targets)
	dirs        []string   // directories it reads files from (jq -L)
	lists       []string   // files listing, NUL-separated, more files to read (--files0-from)
	script      string     // the inline script or pattern of a scriptFirst reader (-e ones too)
	scriptFiles []string   // the files a scriptFirst reader reads its script from
	runs        bool       // an option runs another program (rg --pre)
	values      []string   // option values (--input=x, -Dk=x) and response files (@x) of other programs
	roots       []string   // the paths find searches
	execs       []findExec // the commands find runs
}

// findExec is a command find runs: -exec / -ok (dir: -execdir / -okdir).
type findExec struct {
	cmd []string
	dir bool
}

// replacePlaceholder replaces find's {} in the words of cmd by each of paths.
func replacePlaceholder(cmd, paths []string) []string {
	var out []string
	for _, w := range cmd {
		if !strings.Contains(w, "{}") {
			out = append(out, w)
			continue
		}
		for _, p := range paths {
			out = append(out, strings.ReplaceAll(w, "{}", p))
		}
	}
	return out
}

// operands parses the arguments of a command.
func operands(prog string, args []string) (c cmdArgs) {
	switch prog {
	case "find":
		// find [-H|-L|-P] path... expression: the expression names no file to read
		// except the reference files of -newer and -samefile, the list of -files0-from
		// and what the commands of -exec and -ok read.
		paths := true
		for i := 0; i < len(args); i++ {
			w := args[i]
			switch {
			case w == "<" || w == ">" || w == ">>" || w == "<<" || w == "<<<":
				paths = false
			case paths && (w == "-H" || w == "-L" || w == "-P"):
			case w == "-exec" || w == "-execdir" || w == "-ok" || w == "-okdir":
				paths = false
				// The command ends at ";" or, after {}, at "+".
				j := i + 1
				for j < len(args) && args[j] != ";" && (args[j] != "+" || args[j-1] != "{}") {
					j++
				}
				if j > i+1 {
					c.execs = append(c.execs, findExec{cmd: args[i+1 : j], dir: strings.HasSuffix(w, "dir")})
				}
				i = j
			case strings.HasPrefix(w, "-") || w == "!" || w == "(":
				paths = false
				switch {
				case i+1 >= len(args):
				case strings.HasPrefix(w, "-newer") || w == "-anewer" || w == "-cnewer" || w == "-samefile":
					c.files = append(c.files, args[i+1])
				case w == "-files0-from":
					c.lists = append(c.lists, args[i+1])
				}
			case paths:
				c.files = append(c.files, w)
				c.roots = append(c.roots, w)
			}
		}
		return c
	case "test", "[", "[[":
		// Only the operands of file operators are paths: [ -f x ], [ a -nt b ].
		for i, w := range args {
			if testFileOps[w] && i+1 < len(args) && args[i+1] != "]" && args[i+1] != "]]" {
				c.files = append(c.files, args[i+1])
			}
			if (w == "-nt" || w == "-ot" || w == "-ef") && i > 0 {
				c.files = append(c.files, args[i-1])
			}
		}
		return c
	}
	opts := readerOpts[prog]
	needScript := scriptFirst[prog]
	// The options of a program smates does not know may name files in their values
	// (--input=data.txt, -Dconfig=cfg/app.ini), its operands response files (@args.txt).
	other := !readers[prog]
	// take consumes the arguments of an option (kinds, see readerOpts); the first one is
	// inline when the option word carries it (-n5, --file=x).
	var i int
	take := func(kinds, inline string, hasInline bool) {
		for k, kind := range kinds {
			arg := inline
			if k > 0 || !hasInline {
				if i+1 >= len(args) || (kind == '%' && !strings.Contains(args[i+1], "%")) {
					return
				}
				i++
				arg = args[i]
			}
			switch kind {
			case 's':
				needScript = false
				if c.script != "" {
					c.script += "\n"
				}
				c.script += arg
			case 'l':
				c.lists = append(c.lists, arg)
			case 'f':
				needScript = false
				c.files = append(c.files, arg)
				c.scriptFiles = append(c.scriptFiles, arg)
			case 'p':
				c.files = append(c.files, arg)
			case 'd':
				c.dirs = append(c.dirs, arg)
			case 'x':
				c.runs = true
			}
		}
	}
	end := false
	for i = 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w == "<" || w == ">" || w == ">>" || w == "<<" || w == "<<<":
			i++ // standard input (recorded by the caller), an output, a here-document
			continue
		case end || w == "-" || !strings.HasPrefix(w, "-"):
		case w == "--":
			end = true
			continue
		case strings.HasPrefix(w, "--"):
			if prog == "rg" && (w == "--files" || w == "--type-list") {
				needScript = false
			}
			name, val, ok := strings.Cut(w, "=")
			take(opts[name], val, ok)
			if other && ok && opts[name] == "" {
				c.values = append(c.values, val)
			}
			continue
		default:
			if _, val, ok := strings.Cut(w, "="); other && ok {
				c.values = append(c.values, val)
			}
			// A cluster of short options; the first one taking an argument takes the rest
			// of the word (-n5) or the next word.
			for j := 1; j < len(w); j++ {
				if kinds := opts["-"+w[j:j+1]]; kinds != "" {
					take(kinds, w[j+1:], j+1 < len(w))
					break
				}
			}
			continue
		}
		switch {
		case w == "" || w == "-":
			// BSD sed -i '', standard input
		case needScript && prog == "yq" && (w == "eval" || w == "e" || w == "eval-all" || w == "ea"):
		case needScript:
			needScript = false
			c.script = w
		case prog == "awk" && assign.MatchString(w):
			// var=value operands set awk variables
		case other && len(w) > 1 && w[0] == '@':
			c.values = append(c.values, w[1:])
		default:
			c.files = append(c.files, w)
		}
	}
	return c
}

var varDefault = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:?-([^}$]+)\}$`)

// sedScript parses a sed script: it returns the files its r and R commands read, and
// whether it runs commands (e, the e flag of s) or could not be parsed.
func sedScript(s string) (reads []string, runs bool) {
	i := 0
	skip := func(set string) {
		for i < len(s) && strings.IndexByte(set, s[i]) >= 0 {
			i++
		}
	}
	// toEOL returns the rest of the line (a file name, a command, a label).
	toEOL := func() string {
		start := i
		for i < len(s) && s[i] != '\n' {
			i++
		}
		return s[start:i]
	}
	// delimited skips a regular expression or replacement ending at delim; brackets
	// protect it in regular expressions.
	delimited := func(delim byte, regex bool) bool {
		for ; i < len(s); i++ {
			switch {
			case s[i] == '\\':
				i++
			case regex && s[i] == '[':
				j := i + 1
				if j < len(s) && s[j] == '^' {
					j++
				}
				if j < len(s) && s[j] == ']' {
					j++
				}
				for j < len(s) && s[j] != ']' {
					j++
				}
				i = j
			case s[i] == delim:
				i++
				return true
			}
		}
		return false
	}
	address := func() bool {
		switch {
		case i >= len(s):
			return false
		case s[i] >= '0' && s[i] <= '9':
			skip("0123456789")
			if i < len(s) && s[i] == '~' {
				i++
				skip("0123456789")
			}
		case s[i] == '$':
			i++
		case s[i] == '/':
			i++
			if !delimited('/', true) {
				return false
			}
			skip("IM")
		case s[i] == '\\' && i+1 < len(s):
			d := s[i+1]
			i += 2
			if !delimited(d, true) {
				return false
			}
			skip("IM")
		}
		return true
	}
	for {
		skip(" \t\n;")
		if i >= len(s) {
			return reads, runs
		}
		if !address() {
			return reads, true
		}
		skip(" \t")
		if i < len(s) && s[i] == ',' {
			i++
			skip(" \t")
			if i < len(s) && (s[i] == '+' || s[i] == '~') {
				i++
				skip("0123456789")
			} else if !address() {
				return reads, true
			}
		}
		skip(" \t!")
		if i >= len(s) {
			return reads, true
		}
		cmd := s[i]
		i++
		switch cmd {
		case '{', '}', '=', 'd', 'D', 'g', 'G', 'h', 'H', 'n', 'N', 'p', 'P', 'x', 'z', 'F':
		case '#':
			toEOL()
		case ':', 'b', 't', 'T', 'v':
			for i < len(s) && s[i] != '\n' && s[i] != ';' {
				i++
			}
		case 'a', 'i', 'c':
			// Text to the end of the line; a line ending in a backslash continues it.
			for {
				line := toEOL()
				if !strings.HasSuffix(line, "\\") || i >= len(s) {
					break
				}
				i++
			}
		case 'r', 'R', 'w', 'W':
			skip(" \t")
			name := toEOL()
			if (cmd == 'r' || cmd == 'R') && name != "" {
				reads = append(reads, name)
			}
		case 'e':
			toEOL()
			runs = true
		case 'l', 'L', 'q', 'Q':
			skip(" \t")
			skip("0123456789")
		case 's', 'y':
			if i >= len(s) || s[i] == '\\' || s[i] == '\n' {
				return reads, true
			}
			d := s[i]
			i++
			if !delimited(d, cmd == 's') || !delimited(d, false) {
				return reads, true
			}
			for cmd == 's' && i < len(s) && strings.IndexByte("gpiImMe0123456789w", s[i]) >= 0 {
				if s[i] == 'e' {
					runs = true
				}
				if s[i] == 'w' {
					toEOL() // writes a file
					break
				}
				i++
			}
		default:
			return reads, true
		}
	}
}
