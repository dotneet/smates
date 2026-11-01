package golang

import (
	"bufio"
	"bytes"
	"strings"
)

// cgoHeaders returns the headers a cgo preamble includes from directories outside the
// package: `#include "common.h"` found through `#cgo CFLAGS: -I${SRCDIR}/../include`.
// Headers of the package directory itself are already package files. Only the
// file's own #cgo directives are read (cgo combines those of every file of the
// package, but include directories are practically always declared next to the
// includes that need them), and only ${SRCDIR}-relative directories can be mapped
// into the repository.
func (a *Analyzer) cgoHeaders(dir string, src []byte) []string {
	if !bytes.Contains(src, []byte("${SRCDIR}")) || !bytes.Contains(src, []byte("#include")) {
		return nil
	}
	var incDirs, includes []string
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "//"))
		switch {
		case strings.HasPrefix(line, "#cgo"):
			incDirs = append(incDirs, cgoIncludeDirs(dir, line[len("#cgo"):])...)
		case strings.HasPrefix(line, "#include"):
			rest := strings.TrimSpace(line[len("#include"):])
			if len(rest) > 1 && rest[0] == '"' {
				if k := strings.IndexByte(rest[1:], '"'); k > 0 {
					includes = append(includes, rest[1:1+k])
				}
			}
		}
	}
	var out []string
	for _, d := range incDirs {
		for _, inc := range includes {
			if f, ok := cleanRel(joinRel(d, inc)); ok && !excluded(f) && a.h.Has(f) {
				out = append(out, f)
			}
		}
	}
	return out
}

// cgoIncludeDirs returns the repo-relative -I${SRCDIR}/... directories of a #cgo
// directive (the text after "#cgo") of a file in dir.
func cgoIncludeDirs(dir, directive string) []string {
	head, args, ok := strings.Cut(directive, ":")
	if !ok {
		return nil
	}
	if f := strings.Fields(head); len(f) == 0 ||
		!(f[len(f)-1] == "CFLAGS" || f[len(f)-1] == "CPPFLAGS" || f[len(f)-1] == "CXXFLAGS") {
		return nil
	}
	var out []string
	fields := strings.Fields(args)
	for i := 0; i < len(fields); i++ {
		v := strings.Trim(fields[i], `"'`)
		switch {
		case v == "-I" && i+1 < len(fields):
			i++
			v = strings.Trim(fields[i], `"'`)
		case strings.HasPrefix(v, "-I"):
			v = v[2:]
		default:
			continue
		}
		rel, ok := strings.CutPrefix(v, "${SRCDIR}")
		if !ok || rel != "" && rel[0] != '/' {
			continue
		}
		if d, ok := cleanRel(joinRel(dir, "."+rel)); ok {
			out = append(out, d)
		}
	}
	return out
}
