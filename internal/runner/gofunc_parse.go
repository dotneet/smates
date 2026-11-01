package runner

// Parsing of Go test files for granularity: func, memoized by content hash.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/static/golang"
)

// testFileCache memoizes parsed test files by content hash.
var testFileCache sync.Map

type parsedTestFile struct {
	tf  *golang.TestFile
	err error
}

// readTestFile parses a repo-relative _test.go file.
func readTestFile(root, rel string) (*golang.TestFile, error) {
	if root == "" {
		return nil, fmt.Errorf("repository root unknown")
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	// Selection looks each test file up once per function of its package: a file whose
	// size and modification time are unchanged, and not so recent that a rewrite could
	// keep them, is not read again.
	st := fileStat{fi.Size(), fi.ModTime()}
	if v, ok := testFileStats.Load(abs); ok && v.(statHash).stat == st && time.Since(st.mod) > 2*time.Second {
		if p, ok := testFileCache.Load(v.(statHash).hash); ok {
			return p.(parsedTestFile).tf, p.(parsedTestFile).err
		}
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	key := gitx.HashBytes(src)
	testFileStats.Store(abs, statHash{st, key})
	if v, ok := testFileCache.Load(key); ok {
		p := v.(parsedTestFile)
		return p.tf, p.err
	}
	tf, err := golang.ParseTestFile(src)
	testFileCache.Store(key, parsedTestFile{tf, err})
	return tf, err
}

type fileStat struct {
	size int64
	mod  time.Time
}

type statHash struct {
	stat fileStat
	hash string
}

// testFileStats maps a test file's absolute path to its last seen stat and hash.
var testFileStats sync.Map
