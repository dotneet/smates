package golang

import (
	"crypto/sha1"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DeclsHash hashes the declarations of a Go source file: the file with the bodies of its
// functions and methods left out. Two versions of a file with the same hash differ only
// inside function bodies, so a test that executed none of those bodies cannot tell them
// apart (constants, variables and their initializers, types, signatures, the method set,
// imports and directives such as //go:embed are all part of the hash). With tests set
// (a _test.go file), the test functions go test runs (see TestFile.Tests) are left out
// entirely: their own units depend on the file's content. ok is false when the file
// does not parse; callers then fall back to the content.
func DeclsHash(src []byte, tests bool) (hash string, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil || f == nil {
		return "", false
	}
	var kept []byte
	last := 0
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		start, end := fset.Position(fd.Body.Lbrace).Offset, fset.Position(fd.Body.Rbrace).Offset
		body := true
		if tests && fd.Recv == nil && isTestFunc(fd) {
			start, body = fset.Position(fd.Pos()).Offset, false
			if fd.Doc != nil {
				start = fset.Position(fd.Doc.Pos()).Offset
			}
		}
		if start < last || end >= len(src) || end < start {
			return "", false
		}
		kept = append(kept, src[last:start]...)
		if body {
			kept = append(kept, "{}"...)
		} else {
			kept = append(kept, '\n')
		}
		last = end + 1
	}
	kept = append(kept, src[last:]...)
	// Tokens, not bytes: blank lines and indentation do not count.
	h := sha1.New()
	var sc scanner.Scanner
	tf := token.NewFileSet().AddFile("", -1, len(kept))
	sc.Init(tf, kept, nil, scanner.ScanComments)
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		h.Write([]byte(tok.String()))
		h.Write([]byte{0})
		h.Write([]byte(lit))
		h.Write([]byte{0})
	}
	return "decls:" + hex.EncodeToString(h.Sum(nil)), true
}

// TestFile is what per-function selection needs to know about a _test.go file.
type TestFile struct {
	// Package is the package clause (pkg or pkg_test).
	Package string
	// Tests are the top-level test functions go test runs with -run: TestXxx(*testing.T),
	// FuzzXxx(*testing.F) (their seed corpus) and ExampleXxx(), in source order.
	Tests []string
	// Decls are the names the file declares at the top level (functions, methods,
	// variables, constants, types).
	Decls []string
	// Uses holds every identifier the file mentions.
	Uses map[string]bool
	// Global is set when the file runs code for every test of the package: TestMain,
	// init functions, or package-level variables initialized by a function call.
	Global bool
}

// ParseTestFile analyzes a _test.go file.
func ParseTestFile(src []byte) (*TestFile, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	tf := &TestFile{Package: f.Name.Name, Uses: map[string]bool{}}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			tf.Uses[id.Name] = true
		}
		return true
	})
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			tf.Decls = append(tf.Decls, name)
			if d.Recv != nil {
				continue
			}
			if name == "init" || name == "TestMain" {
				tf.Global = true
				continue
			}
			if isTestFunc(d) {
				tf.Tests = append(tf.Tests, name)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.ValueSpec:
					for _, n := range s.Names {
						tf.Decls = append(tf.Decls, n.Name)
					}
					for _, v := range s.Values {
						if callsAtInit(v) {
							tf.Global = true
						}
					}
				case *ast.TypeSpec:
					tf.Decls = append(tf.Decls, s.Name.Name)
				}
			}
		}
	}
	return tf, nil
}

// callsAtInit reports whether evaluating e calls a function (conversions cannot be told
// apart without type information and count too). Function literals are not evaluated.
func callsAtInit(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			found = true
		}
		return !found
	})
	return found
}

// isTestFunc reports whether go test runs fd with -run: TestXxx(t *testing.T),
// FuzzXxx(f *testing.F), ExampleXxx(). Benchmarks need -bench and are left out.
func isTestFunc(fd *ast.FuncDecl) bool {
	name := fd.Name.Name
	if fd.Type.TypeParams != nil && len(fd.Type.TypeParams.List) > 0 {
		return false
	}
	params, results := fd.Type.Params, fd.Type.Results
	nparams := 0
	if params != nil {
		for _, p := range params.List {
			nparams += max(1, len(p.Names))
		}
	}
	if results != nil && len(results.List) > 0 {
		return false
	}
	switch {
	case hasTestPrefix(name, "Example"):
		return nparams == 0
	case hasTestPrefix(name, "Test"):
		return nparams == 1 && isPtrTo(params.List[0].Type, "T")
	case hasTestPrefix(name, "Fuzz"):
		return nparams == 1 && isPtrTo(params.List[0].Type, "F")
	}
	return false
}

// hasTestPrefix is go test's rule: the name is prefix, or prefix followed by a
// character that is not a lower-case letter.
func hasTestPrefix(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// isPtrTo reports whether e is *testing.<name> (or *<name>, with a dot import).
func isPtrTo(e ast.Expr, name string) bool {
	st, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch x := st.X.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == name
	case *ast.Ident:
		return x.Name == name
	}
	return false
}
