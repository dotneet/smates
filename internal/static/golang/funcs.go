package golang

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"hash"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DeclsHash hashes the declarations of a Go source file: the file with the bodies of its
// functions and methods left out. Two versions of a file with the same hash differ only
// inside function bodies, so a test that executed none of those bodies cannot tell them
// apart (constants, variables and their initializers, types, signatures, the method set,
// imports and directives such as //go:embed are all part of the hash; comments that are
// not directives are not, except in a file using cgo, whose preamble is a comment).
//
// With tests set (a _test.go file), the hash covers what can change a test that reaches
// none of the file's declarations (see TestFile): the imports, the types and the
// signatures of their methods (method sets), and the declarations that run for every
// test (Decl.Init; the bodies of functions among them are left out: tests depend on
// those through their Decl keys). Test functions, other functions, constants and
// variables are left out entirely: the tests that reach them depend on their Decl
// keys, and a new one changes nothing until something mentions it.
//
// ok is false when the file does not parse; callers then fall back to the content.
func DeclsHash(src []byte, tests bool) (hash string, ok bool) {
	fset := token.NewFileSet()
	mode := parser.ParseComments
	if !tests {
		mode |= parser.SkipObjectResolution
	}
	f, err := parser.ParseFile(fset, "", src, mode)
	if err != nil || f == nil {
		return "", false
	}
	var kept []byte
	if tests {
		kept, ok = testShape(fset, f, src)
	} else {
		kept, ok = withoutBodies(fset, f, src)
	}
	if !ok {
		return "", false
	}
	h := sha1.New()
	hashTokens(h, kept, usesCgo(f))
	return "decls:" + hex.EncodeToString(h.Sum(nil)), true
}

// withoutBodies is src with the body of every function and method replaced by {}.
func withoutBodies(fset *token.FileSet, f *ast.File, src []byte) ([]byte, bool) {
	var kept []byte
	last := 0
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		start, end := offset(fset, fd.Body.Lbrace), offset(fset, fd.Body.Rbrace)
		if start < last || end >= len(src) || end < start {
			return nil, false
		}
		kept = append(kept, src[last:start]...)
		kept = append(kept, "{}"...)
		last = end + 1
	}
	return append(kept, src[last:]...), true
}

// testShape is what DeclsHash hashes of a test file: the file up to its first
// declaration that is not an import (package clause, build constraints), imports and
// types, the signatures of methods and of functions that run for every test, and the
// variables and constants among those.
func testShape(fset *token.FileSet, f *ast.File, src []byte) ([]byte, bool) {
	imports := fileImports(f)
	var kept []byte
	add := func(from, to token.Pos) bool {
		start, end := offset(fset, from), offset(fset, to)
		if start < 0 || end > len(src) || end < start {
			return false
		}
		kept = append(append(kept, src[start:end]...), '\n')
		return true
	}
	header := len(src)
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); !ok || gd.Tok != token.IMPORT {
			header = offset(fset, declStart(d))
			break
		}
	}
	if header < 0 || header > len(src) {
		return nil, false
	}
	kept = append(kept, src[:header]...)
	for _, d := range f.Decls {
		if offset(fset, d.Pos()) < header {
			continue
		}
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && !runsForEveryTest(d) {
				continue
			}
			to := d.End()
			if d.Body != nil {
				to = d.Body.Lbrace
			}
			if !add(declStart(d), to) {
				return nil, false
			}
		case *ast.GenDecl:
			switch d.Tok {
			case token.IMPORT, token.TYPE:
				if !add(declStart(d), d.End()) {
					return nil, false
				}
			default:
				for _, s := range d.Specs {
					if vs, ok := s.(*ast.ValueSpec); ok && specRunsForEveryTest(d.Tok, vs, imports) {
						kept = append(kept, d.Tok.String()...)
						kept = append(kept, ' ')
						if !add(vs.Pos(), vs.End()) {
							return nil, false
						}
					}
				}
			}
		}
	}
	return kept, true
}

// DeclHashes hashes each top-level declaration of a Go source file by its key (see
// DeclKeys): full, a function or method with its signature and body, a variable,
// constant or type with the whole declaration it is part of (a parenthesized group:
// the values of constants depend on their position in it); sig, the same without the
// bodies of functions and methods. Both include the file's imports, which decide what
// the names a declaration mentions refer to. Declarations sharing a key (init
// functions, blank identifiers) hash together. Comments do not count except
// directives and, in examples, the output comment (and every comment of a file using
// cgo). ok is false when the file does not parse.
func DeclHashes(src []byte) (full, sig map[string]string, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil || f == nil {
		return nil, nil, false
	}
	cgo := usesCgo(f)
	var imports []byte
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			start, end := offset(fset, gd.Pos()), offset(fset, gd.End())
			if start < 0 || end > len(src) || end < start {
				return nil, nil, false
			}
			imports = append(append(imports, src[start:end]...), '\n')
		}
	}
	fullH, sigH := map[string]hash.Hash{}, map[string]hash.Hash{}
	get := func(m map[string]hash.Hash, k string) hash.Hash {
		h := m[k]
		if h == nil {
			h = sha1.New()
			hashTokens(h, imports, false)
			m[k] = h
		}
		return h
	}
	for _, d := range f.Decls {
		keys := DeclKeys(d)
		if len(keys) == 0 {
			continue
		}
		start, end := offset(fset, declStart(d)), offset(fset, d.End())
		if start < 0 || end > len(src) || end < start {
			return nil, nil, false
		}
		sigEnd := end
		fd, _ := d.(*ast.FuncDecl)
		if fd != nil && fd.Body != nil {
			sigEnd = offset(fset, fd.Body.Lbrace)
		}
		comments := cgo || fd != nil && fd.Recv == nil && hasTestPrefix(fd.Name.Name, "Example")
		for _, k := range keys {
			h := get(fullH, k)
			hashTokens(h, src[start:end], comments)
			h.Write([]byte{1})
			h = get(sigH, k)
			hashTokens(h, src[start:sigEnd], cgo)
			h.Write([]byte{1})
		}
	}
	full, sig = make(map[string]string, len(fullH)), make(map[string]string, len(sigH))
	for k, h := range fullH {
		full[k] = "decl:" + hex.EncodeToString(h.Sum(nil))
	}
	for k, h := range sigH {
		sig[k] = "sig:" + hex.EncodeToString(h.Sum(nil))
	}
	return full, sig, true
}

// PackageParts returns what a Go file contributes to its package beyond the
// declarations other code names (see DeclHashes): shape, a hash of what can change
// every test of the package's binary, and methods, by receiver type, the signature of
// each method the file declares (a type's method set decides which interfaces it
// satisfies). The shape covers the package clause, build constraints, blank imports
// (their packages' init runs), init functions and TestMain (their presence: their
// bodies count where they run), package-level variables whose initializer calls code
// (coverage does not see initializers), and declarations shadowing a predeclared name;
// a file using cgo is its whole content. An ordinary function, type, constant or
// variable is not part of it: adding one changes nothing until something names it.
// shape is "" for a file with none of these (its package clause and build constraints
// then change nothing either). ok is false when the file does not parse.
func PackageParts(src []byte) (shape string, methods map[string][]string, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil || f == nil {
		return "", nil, false
	}
	imports := fileImports(f)
	h := sha1.New()
	if usesCgo(f) {
		hashTokens(h, src, true)
		return "shape:" + hex.EncodeToString(h.Sum(nil)), nil, true
	}
	significant := false // the file contributes something beyond its package clause
	add := func(from, to token.Pos) bool {
		start, end := offset(fset, from), offset(fset, to)
		if start < 0 || end > len(src) || end < start {
			return false
		}
		significant = true
		hashTokens(h, src[start:end], false)
		h.Write([]byte{1})
		return true
	}
	for _, cg := range f.Comments {
		if cg.Pos() < f.Package {
			for _, c := range cg.List {
				if directive(c.Text) {
					h.Write([]byte(c.Text + "\n"))
				}
			}
		}
	}
	h.Write([]byte("package " + f.Name.Name + "\n"))
	for _, imp := range f.Imports {
		if imp.Name != nil && imp.Name.Name == "_" {
			h.Write([]byte("import _ " + imp.Path.Value + "\n"))
			significant = true
		}
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			to := d.End()
			if d.Body != nil {
				to = d.Body.Lbrace
			}
			if r := recvType(d); r != "" {
				start, end := offset(fset, d.Pos()), offset(fset, to)
				if start < 0 || end > len(src) || end < start {
					return "", nil, false
				}
				sh := sha1.New()
				hashTokens(sh, src[start:end], false)
				if methods == nil {
					methods = map[string][]string{}
				}
				methods[r] = append(methods[r], d.Name.Name+":"+hex.EncodeToString(sh.Sum(nil)))
				continue
			}
			if runsForEveryTest(d) && !add(d.Pos(), to) {
				return "", nil, false
			}
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.ValueSpec:
					if specRunsForEveryTest(d.Tok, sp, imports) {
						h.Write([]byte(d.Tok.String() + " "))
						if !add(sp.Pos(), sp.End()) {
							return "", nil, false
						}
					}
				case *ast.TypeSpec:
					if types.Universe.Lookup(sp.Name.Name) != nil {
						h.Write([]byte("type "))
						if !add(sp.Pos(), sp.End()) {
							return "", nil, false
						}
					}
				}
			}
		}
	}
	if !significant {
		return "", methods, true
	}
	return "shape:" + hex.EncodeToString(h.Sum(nil)), methods, true
}

// DeclKeys returns the keys of a top-level declaration: the name of a function, "T.M"
// for method M of type T (whatever its receiver's form: *T, T[K]), the names a
// variable, constant or type declaration declares. Imports have none.
func DeclKeys(d ast.Decl) []string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return []string{funcKey(d)}
	case *ast.GenDecl:
		var keys []string
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.ValueSpec:
				for _, n := range s.Names {
					keys = append(keys, n.Name)
				}
			case *ast.TypeSpec:
				keys = append(keys, s.Name.Name)
			}
		}
		return keys
	}
	return nil
}

func funcKey(fd *ast.FuncDecl) string {
	if r := recvType(fd); r != "" {
		return r + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// recvType is the base type name of a method's receiver, "" for a function.
func recvType(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	e := fd.Recv.List[0].Type
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		default:
			return "?"
		}
	}
}

// Span is where a top-level declaration of a Go file is: the body of a function or
// method (Func), or a whole variable, constant or type declaration. Lines and columns
// are 1-based, columns in bytes, as in coverage profiles.
type Span struct {
	Key               string
	Func              bool
	FromLine, FromCol int
	ToLine, ToCol     int
}

// Contains reports whether position line:col is inside the span.
func (s Span) Contains(line, col int) bool {
	if line < s.FromLine || line == s.FromLine && col < s.FromCol {
		return false
	}
	return line < s.ToLine || line == s.ToLine && col <= s.ToCol
}

// Spans lists where the declarations of a Go source file are, to map the blocks of a
// coverage profile to the declarations they belong to. ok is false when the file does
// not parse or has //line directives (profiles then use other positions).
func Spans(src []byte) (spans []Span, ok bool) {
	if bytes.Contains(src, []byte("//line ")) || bytes.Contains(src, []byte("/*line ")) {
		return nil, false
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil || f == nil {
		return nil, false
	}
	span := func(key string, fn bool, from, to token.Pos) {
		a, b := fset.Position(from), fset.Position(to)
		spans = append(spans, Span{Key: key, Func: fn, FromLine: a.Line, FromCol: a.Column, ToLine: b.Line, ToCol: b.Column})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				span(funcKey(d), true, d.Body.Lbrace, d.Body.Rbrace)
			}
		case *ast.GenDecl:
			if keys := DeclKeys(d); len(keys) > 0 {
				span(keys[0], false, d.Pos(), d.End())
			}
		}
	}
	return spans, true
}

// TestFile is what per-function selection needs to know about a _test.go file.
type TestFile struct {
	// Package is the package clause (pkg or pkg_test).
	Package string
	// Tests are the top-level test functions go test runs with -run: TestXxx(*testing.T),
	// FuzzXxx(*testing.F) (their seed corpus) and ExampleXxx(), in source order.
	Tests []string
	// Silent are the examples among Tests that go test compiles but never runs: those
	// without an output comment (go/doc's rule, which go test applies).
	Silent []string
	// Decls are the file's top-level declarations by key (see DeclKeys).
	Decls map[string]*Decl
}

// Decl is a top-level declaration of a test file and the names it mentions.
type Decl struct {
	// Refs are the identifiers it mentions that may name a package-level declaration:
	// not those declared inside it (parameters, locals, fields), nor the name selected
	// by a selector (x.Name), which is a field, a method or a name of another package.
	Refs []string
	// Qualified are the names selected on an identifier declared nowhere in the file
	// (pkg.Name): in an external test package (pkg_test), the names it may use of the
	// package under test, its test files included.
	Qualified []string
	// BodyRefs and BodyQualified are those of a function's or method's body (Refs and
	// Qualified are those of its signature): what it names when it runs.
	BodyRefs, BodyQualified []string
	// Recv is the type of a method's receiver.
	Recv string
	// Type is set for a type declaration (its methods come with it).
	Type bool
	// Init is set when it runs for every test of the binary: TestMain, an init
	// function, a package-level variable whose initializer calls a function, or a
	// declaration shadowing a predeclared identifier (it changes what every file of the
	// package means).
	Init bool
}

// ParseTestFile analyzes a _test.go file (or any Go file: Tests and Silent are then
// usually empty).
func ParseTestFile(src []byte) (*TestFile, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	imports := fileImports(f)
	tf := &TestFile{Package: f.Name.Name, Decls: map[string]*Decl{}}
	decl := func(key string) *Decl {
		d := tf.Decls[key]
		if d == nil {
			d = &Decl{}
			tf.Decls[key] = d
		}
		if types.Universe.Lookup(key) != nil {
			d.Init = true
		}
		return d
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			key := funcKey(d)
			dd := decl(key)
			dd.Recv = recvType(d)
			if dd.Recv == "" && runsForEveryTest(d) {
				dd.Init = true
			}
			if dd.Recv == "" && isTestFunc(d) {
				tf.Tests = append(tf.Tests, key)
			}
			// Not the name: a method's is not declared in the file scope.
			r, q := refs(f, d.Recv, d.Type)
			dd.Refs, dd.Qualified = append(dd.Refs, r...), append(dd.Qualified, q...)
			r, q = refs(f, d.Body)
			dd.BodyRefs, dd.BodyQualified = append(dd.BodyRefs, r...), append(dd.BodyQualified, q...)
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			// Every key of a group gets the refs of the whole group, as DeclHashes hashes it.
			group := &Decl{}
			for _, s := range d.Specs {
				var nodes []ast.Node
				switch s := s.(type) {
				case *ast.ValueSpec:
					nodes = append(nodes, s.Type)
					for _, v := range s.Values {
						nodes = append(nodes, v)
					}
				case *ast.TypeSpec:
					nodes = append(nodes, s.TypeParams, s.Type)
				}
				r, q := refs(f, nodes...)
				group.Refs, group.Qualified = append(group.Refs, r...), append(group.Qualified, q...)
			}
			for _, s := range d.Specs {
				var dd *Decl
				switch s := s.(type) {
				case *ast.ValueSpec:
					init := specRunsForEveryTest(d.Tok, s, imports)
					for _, n := range s.Names {
						dd = decl(n.Name)
						dd.Init = dd.Init || init
						dd.Refs, dd.Qualified = append(dd.Refs, group.Refs...), append(dd.Qualified, group.Qualified...)
					}
				case *ast.TypeSpec:
					dd = decl(s.Name.Name)
					dd.Type = true
					dd.Refs, dd.Qualified = append(dd.Refs, group.Refs...), append(dd.Qualified, group.Qualified...)
				}
			}
		}
	}
	for _, e := range doc.Examples(f) {
		if name := "Example" + e.Name; e.Output == "" && !e.EmptyOutput && containsString(tf.Tests, name) {
			tf.Silent = append(tf.Silent, name)
		}
	}
	for _, d := range tf.Decls {
		d.Refs, d.Qualified = sortedUnique(d.Refs), sortedUnique(d.Qualified)
		d.BodyRefs, d.BodyQualified = sortedUnique(d.BodyRefs), sortedUnique(d.BodyQualified)
	}
	return tf, nil
}

// refs returns the names nodes mention that may be package-level declarations (see
// Decl.Refs) and the names they select on an identifier the file does not declare
// (Decl.Qualified), using the parser's resolution: an identifier is one when the file
// does not resolve it (another file's declaration, a predeclared or imported name) or
// resolves it to one of its own top-level declarations.
func refs(f *ast.File, nodes ...ast.Node) (names, qualified []string) {
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && id.Obj == nil {
				qualified = append(qualified, n.Sel.Name)
			}
			ast.Inspect(n.X, visit)
			return false
		case *ast.Ident:
			if n.Name != "_" && (n.Obj == nil || f.Scope.Lookup(n.Name) == n.Obj) {
				names = append(names, n.Name)
			}
		}
		return true
	}
	for _, n := range nodes {
		if n != nil && !isNilNode(n) {
			ast.Inspect(n, visit)
		}
	}
	return names, qualified
}

// isNilNode reports whether n is a typed nil (an absent optional node of a spec).
func isNilNode(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.FieldList:
		return n == nil
	case *ast.BlockStmt:
		return n == nil
	case *ast.FuncType:
		return n == nil
	}
	return false
}

// runsForEveryTest reports whether a function runs for every test of the binary
// (TestMain, init) or shadows a predeclared identifier.
func runsForEveryTest(fd *ast.FuncDecl) bool {
	return fd.Recv == nil && (fd.Name.Name == "init" || fd.Name.Name == "TestMain" || types.Universe.Lookup(fd.Name.Name) != nil)
}

// specRunsForEveryTest reports whether a package-level variable or constant specification
// runs code for every test of the binary (its initializer calls a function) or shadows
// a predeclared identifier. imports maps the file's import names to their paths.
func specRunsForEveryTest(tok token.Token, s *ast.ValueSpec, imports map[string]string) bool {
	for _, n := range s.Names {
		if types.Universe.Lookup(n.Name) != nil {
			return true
		}
	}
	if tok != token.VAR {
		return false
	}
	for _, v := range s.Values {
		if callsAtInit(v, imports) {
			return true
		}
	}
	return false
}

// callsAtInit reports whether evaluating e calls a function. Function literals are not
// evaluated; conversions and builtins that run no code of the program are not calls
// (see conversion), nor are the standard library functions that cannot panic or
// have side effects (see pureCall).
func callsAtInit(e ast.Expr, imports map[string]string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if !conversion(n) && !pureCall(n, imports) {
				found = true
			}
		}
		return !found
	})
	return found
}

// pureCalls are standard library functions that only return a value made from their
// arguments, with no side effect and no panic.
var pureCalls = map[string]bool{"errors.New": true, "fmt.Errorf": true, "time.FixedZone": true}

// pureCall reports whether c calls a function in pureCalls, through the import of its
// package, or compiles a constant regular expression that compiles
// (regexp.MustCompile("..."): it cannot panic). Its arguments are checked on their own.
func pureCall(c *ast.CallExpr, imports map[string]string) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Obj != nil {
		return false
	}
	fn := imports[id.Name] + "." + sel.Sel.Name
	if pureCalls[fn] {
		return true
	}
	if fn != "regexp.MustCompile" && fn != "regexp.MustCompilePOSIX" || len(c.Args) != 1 {
		return false
	}
	lit, ok := c.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	pattern, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	if fn == "regexp.MustCompilePOSIX" {
		_, err = regexp.CompilePOSIX(pattern)
	} else {
		_, err = regexp.Compile(pattern)
	}
	return err == nil
}

// fileImports maps the names a file imports packages as to their paths (the last path
// element when unnamed: the package name of the standard library packages pureCall
// knows). Blank and dot imports are left out.
func fileImports(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != "_" && name != "." {
			m[name] = p
		}
	}
	return m
}

// pureBuiltins are the predeclared functions that run no code of the program.
var pureBuiltins = map[string]bool{"append": true, "cap": true, "complex": true, "imag": true, "len": true,
	"make": true, "max": true, "min": true, "new": true, "real": true}

// conversion reports whether a call is known, without type information, to run no code
// of the program: a conversion to a type literal ([]byte(s)), to a predeclared type
// (string(b)) or to a type declared in the same file, a pointer conversion of nil
// ((*T)(nil)), or a pure builtin (len, make, ...). Anything else may be a function
// call, and counts as one. (A package-level declaration of another file shadowing a
// predeclared name is not seen here; in a test file it runs for every test itself, see
// Decl.Init.)
func conversion(c *ast.CallExpr) bool {
	switch fun := ast.Unparen(c.Fun).(type) {
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	case *ast.StarExpr:
		// (*f)(nil) could call through a pointer to a function, but not usefully.
		if len(c.Args) != 1 {
			return false
		}
		id, ok := ast.Unparen(c.Args[0]).(*ast.Ident)
		return ok && id.Name == "nil" && id.Obj == nil
	case *ast.Ident:
		if fun.Obj != nil {
			return fun.Obj.Kind == ast.Typ
		}
		switch types.Universe.Lookup(fun.Name).(type) {
		case *types.TypeName:
			return true
		case *types.Builtin:
			return pureBuiltins[fun.Name]
		}
	}
	return false
}

// declStart is where a declaration starts, its doc comment included (directives such as
// //go:embed are part of it).
func declStart(d ast.Decl) token.Pos {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Doc != nil {
			return d.Doc.Pos()
		}
	case *ast.GenDecl:
		if d.Doc != nil {
			return d.Doc.Pos()
		}
	}
	return d.Pos()
}

func offset(fset *token.FileSet, p token.Pos) int {
	if !p.IsValid() {
		return -1
	}
	return fset.Position(p).Offset
}

// usesCgo reports whether a file imports "C": its preamble is a comment.
func usesCgo(f *ast.File) bool {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == "C" {
			return true
		}
	}
	return false
}

// directive reports whether a comment can change what a file compiles to.
func directive(c string) bool {
	for _, p := range []string{"//go:", "//line ", "/*line ", "//export ", "//extern ", "// +build", "//+build"} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// hashTokens writes the tokens of src to h: blank lines and indentation do not count,
// nor comments other than directives unless comments is set.
func hashTokens(h hash.Hash, src []byte, comments bool) {
	var sc scanner.Scanner
	tf := token.NewFileSet().AddFile("", -1, len(src))
	sc.Init(tf, src, nil, scanner.ScanComments)
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT && !comments && !directive(lit) {
			continue
		}
		h.Write([]byte(tok.String()))
		h.Write([]byte{0})
		h.Write([]byte(lit))
		h.Write([]byte{0})
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedUnique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	sort.Strings(s)
	out := s[:1]
	for _, x := range s[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
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
