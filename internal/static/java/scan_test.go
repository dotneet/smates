package java

import (
	"reflect"
	"sort"
	"testing"
)

func TestScanDeclarations(t *testing.T) {
	src := `
@SuppressWarnings("x")
package com.example.a;

import java.util.List;
import static org.junit.jupiter.api.Assertions.*;
import com.example.b.*;

@Anno(value = Foo.class, list = {Bar.class})
public final class Main<T extends List<?>> implements Runnable {
    class Inner {}
    enum Nested { A }
    public void run() { record = 1; Object o = Main.class; }
}
interface Port {}
enum Color { RED }
@interface Marker {}
record Point(int x, int y) {}
record Box<T>(T value) {}
sealed interface Shape permits Circle {}
non-sealed class Circle implements Shape {}
`
	u := scan([]byte(src))
	if u.pkg != "com.example.a" {
		t.Errorf("pkg = %q", u.pkg)
	}
	wantImports := []imp{
		{name: "java.util.List"},
		{name: "org.junit.jupiter.api.Assertions", static: true, onDemand: true},
		{name: "com.example.b", onDemand: true},
	}
	if !reflect.DeepEqual(u.imports, wantImports) {
		t.Errorf("imports = %+v, want %+v", u.imports, wantImports)
	}
	wantTypes := []string{"Main", "Port", "Color", "Marker", "Point", "Box", "Shape", "Circle"}
	if !reflect.DeepEqual(u.types, wantTypes) {
		t.Errorf("types = %q, want %q", u.types, wantTypes)
	}
	for _, id := range []string{"Foo", "Bar", "Runnable", "List", "Inner", "Main"} {
		if !u.idents[id] {
			t.Errorf("idents lacks %q", id)
		}
	}
	// Identifiers of the package and import declarations are not references.
	for _, id := range []string{"jupiter", "Assertions"} {
		if u.idents[id] {
			t.Errorf("idents has %q from an import", id)
		}
	}
}

func TestScanNames(t *testing.T) {
	u := scan([]byte(`package p;
class A {
    Object a = com.example.b.Store.open();
    java.util.function.Supplier<String> s = com.example.c.Util::name;
    int n = 1.5e-3 > 0x1Fp+2 ? 1_000 : 0;
    int m = 0xFE+Config.MASK + 0xCAFE-Limits.MAX + 1e+Other.X;
}`))
	got := append([]string{}, u.names...)
	sort.Strings(got)
	want := []string{
		"Config.MASK", "Limits.MAX", "Other.X",
		"com.example.b.Store.open", "com.example.c.Util", "java.util.function.Supplier",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
}

func TestScanCROnlyLineEndings(t *testing.T) {
	// Old Mac line endings: a line comment or an unterminated literal ends at the CR.
	u := scan([]byte("package com.example.a;\r// note\rimport com.example.b.Store;\rclass A { String s = \"x\r; Store st; }\r"))
	if u.pkg != "com.example.a" || len(u.imports) != 1 || !u.idents["Store"] || !u.idents["st"] {
		t.Errorf("scan with CR line endings = pkg %q, imports %+v, idents %v", u.pkg, u.imports, u.idents)
	}
}

func TestUnescapeUnicode(t *testing.T) {
	cases := map[string]string{
		`a\u0022b`:   `a"b`,
		`a\uu0041b`:  `aAb`,
		`a\\u0041b`:  `a\\u0041b`, // an escaped backslash does not start an escape
		`a\\\u0041b`: `a\\Ab`,
		`a\u00zz`:    `a\u00zz`,
		`\u`:         `\u`,
	}
	for in, want := range cases {
		if got := unescapeUnicode(in); got != want {
			t.Errorf("unescapeUnicode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLexUnterminated(t *testing.T) {
	// Unterminated comments and literals end the input or the line without panicking.
	for _, src := range []string{"/* x", `"abc`, `'a`, `"""x`, "1e", "class \xff A", `"a\`} {
		lex(src)
	}
	toks := lex("String s = \"abc\nGreeter g;")
	found := false
	for _, tk := range toks {
		if tk.kind == tokIdent && tk.text == "Greeter" {
			found = true
		}
	}
	if !found {
		t.Errorf("an unterminated string swallowed the next line: %+v", toks)
	}
}

func FuzzScan(f *testing.F) {
	f.Add([]byte("package a; import b.C; class D { String s = \"\"\"\n x \"\"\"; }"))
	f.Add([]byte("\\u0022 /* */ 'x' 0x1p-3"))
	f.Fuzz(func(t *testing.T, src []byte) {
		scan(src)
	})
}
