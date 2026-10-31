package golang

import (
	"bufio"
	"bytes"
	"path"
	"sort"
	"strconv"
	"strings"
)

// embedPatterns returns the patterns of every //go:embed directive in src. Lines are
// scanned textually, so a directive inside a raw string literal is also picked up
// (harmless over-approximation).
func embedPatterns(src []byte) []string {
	const directive = "//go:embed"
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimLeft(sc.Text(), " \t")
		if !strings.HasPrefix(line, directive) {
			continue
		}
		rest := line[len(directive):]
		if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			continue // e.g. //go:embedded
		}
		out = append(out, parseEmbedArgs(rest)...)
	}
	return out
}

// parseEmbedArgs splits the argument list of a //go:embed directive: patterns are
// separated by spaces or tabs and may be written as Go string literals. Parsing stops
// at the first malformed argument, keeping the patterns before it.
func parseEmbedArgs(args string) []string {
	var out []string
	for {
		args = strings.TrimLeft(args, " \t\r")
		if args == "" {
			return out
		}
		var pat string
		switch args[0] {
		case '"', '`':
			end := quotedEnd(args)
			if end < 0 {
				return out
			}
			s, err := strconv.Unquote(args[:end])
			if err != nil {
				return out
			}
			pat, args = s, args[end:]
		default:
			i := strings.IndexAny(args, " \t\r")
			if i < 0 {
				i = len(args)
			}
			pat, args = args[:i], args[i:]
		}
		if pat != "" {
			out = append(out, pat)
		}
	}
}

// expandEmbed returns the repo files a //go:embed pattern in dir selects. As in the go
// command, a pattern is matched with path.Match against paths relative to dir; a match
// on a directory selects every file below it, except those with a path element below
// the matched directory starting with '.' or '_', unless the pattern has the "all:"
// prefix.
func (a *Analyzer) expandEmbed(dir, pattern string) []string {
	all := strings.HasPrefix(pattern, "all:")
	pattern = strings.TrimPrefix(pattern, "all:")
	if !validEmbedPattern(pattern) {
		return nil
	}
	elems := strings.Count(pattern, "/") + 1
	prefix := dir + "/"
	if dir == "" {
		prefix = ""
	}

	// Narrow the scan to the deepest directory named by the pattern's literal leading
	// elements. A pattern without metacharacters names exactly one file or directory.
	var out []string
	lit := pattern
	if i := strings.IndexAny(pattern, `*?[\`); i >= 0 {
		lit = pattern[:i]
		if j := strings.LastIndexByte(lit, '/'); j >= 0 {
			lit = lit[:j]
		} else {
			lit = ""
		}
	} else if f := joinRel(dir, pattern); a.h.Has(f) {
		out = append(out, f)
	}
	scanPrefix := joinRel(dir, lit) + "/"
	if scanPrefix == "/" {
		scanPrefix = ""
	}

	tree := a.sortedTree()
	for i := sort.SearchStrings(tree, scanPrefix); i < len(tree) && strings.HasPrefix(tree[i], scanPrefix); i++ {
		f := tree[i]
		rel := f[len(prefix):]
		head, rest := splitElems(rel, elems)
		if ok, _ := path.Match(pattern, head); !ok {
			continue
		}
		if rest != "" && !all && hasHiddenElem(rest) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// sortedTree returns all non-excluded repository files in sorted order.
func (a *Analyzer) sortedTree() []string {
	a.treeOnce.Do(func() {
		files := a.h.Files()
		t := make([]string, 0, len(files))
		for f := range files {
			if !excluded(f) {
				t = append(t, f)
			}
		}
		sort.Strings(t)
		a.tree = t
	})
	return a.tree
}

// validEmbedPattern rejects patterns the go command refuses: absolute paths, empty,
// "." or ".." elements, and malformed match syntax.
func validEmbedPattern(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for _, e := range strings.Split(p, "/") {
		if e == "" || e == "." || e == ".." {
			return false
		}
	}
	_, err := path.Match(p, "")
	return err == nil
}

// splitElems splits p after its first n slash-separated elements.
func splitElems(p string, n int) (head, rest string) {
	i := 0
	for ; n > 0; n-- {
		j := strings.IndexByte(p[i:], '/')
		if j < 0 {
			return p, ""
		}
		i += j + 1
	}
	return p[:i-1], p[i:]
}

// hasHiddenElem reports whether any element of p starts with '.' or '_'.
func hasHiddenElem(p string) bool {
	for _, e := range strings.Split(p, "/") {
		if strings.HasPrefix(e, ".") || strings.HasPrefix(e, "_") {
			return true
		}
	}
	return false
}
