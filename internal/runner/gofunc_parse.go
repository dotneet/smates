package runner

// Parsing of Go files for granularity: func, memoized by content hash.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/static/golang"
)

// parseMemo memoizes what a parse function returns for a file by its content hash.
// Selection looks each file up many times (a test file once per function of its
// package, a covered file once per function that executed it): a file whose size and
// modification time are unchanged, and not so recent that a rewrite could keep them,
// is not read again.
type parseMemo[T any] struct {
	parse  func([]byte) T
	byHash sync.Map // content hash -> T
	stats  sync.Map // absolute path -> statHash
}

// read returns parse of the file at abs; err is set when it cannot be read.
func (m *parseMemo[T]) read(abs string) (T, error) {
	var zero T
	fi, err := os.Stat(abs)
	if err != nil {
		return zero, err
	}
	st := fileStat{fi.Size(), fi.ModTime()}
	if v, ok := m.stats.Load(abs); ok && v.(statHash).stat == st && time.Since(st.mod) > 2*time.Second {
		if p, ok := m.byHash.Load(v.(statHash).hash); ok {
			return p.(T), nil
		}
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return zero, err
	}
	key := gitx.HashBytes(src)
	m.stats.Store(abs, statHash{st, key})
	if p, ok := m.byHash.Load(key); ok {
		return p.(T), nil
	}
	p := m.parse(src)
	m.byHash.Store(key, p)
	return p, nil
}

type fileStat struct {
	size int64
	mod  time.Time
}

type statHash struct {
	stat fileStat
	hash string
}

type parsedTestFile struct {
	tf  *golang.TestFile
	err error
}

var testFiles = &parseMemo[parsedTestFile]{parse: func(src []byte) parsedTestFile {
	tf, err := golang.ParseTestFile(src)
	return parsedTestFile{tf, err}
}}

// readTestFile parses a repo-relative _test.go file.
func readTestFile(root, rel string) (*golang.TestFile, error) {
	if root == "" {
		return nil, fmt.Errorf("repository root unknown")
	}
	p, err := testFiles.read(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	return p.tf, p.err
}

type parsedSpans struct {
	spans []golang.Span
	ok    bool
}

var spanFiles = &parseMemo[parsedSpans]{parse: func(src []byte) parsedSpans {
	spans, ok := golang.Spans(src)
	return parsedSpans{spans, ok}
}}

// readSpans returns where the declarations of a Go file are (see golang.Spans); ok is
// false when the file cannot be read or mapped.
func readSpans(abs string) ([]golang.Span, bool) {
	p, err := spanFiles.read(abs)
	return p.spans, err == nil && p.ok
}
