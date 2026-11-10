package java

import (
	"reflect"
	"sort"
	"testing"
)

func TestDescribe(t *testing.T) {
	src := `
package com.example.shop;

import com.example.base.BaseTest;
import org.junit.jupiter.api.*;

// class Commented extends Nothing {}
@DisplayName("cart")
public abstract class CartTest<T extends Comparable<T>> extends BaseTest<Map<String, T>> implements Runnable {
    @Test void adds() { String s = "class Fake extends X"; }
    @org.junit.jupiter.api.Nested class Inner extends Other { @Test void inner() {} }
}
final class Helper extends com.example.util.Base {}
@SuppressWarnings({"a"}) class Plain {}
interface Port extends Runnable {}
@interface Slow {}
record Point(int x) {}
enum Color { RED }
`
	in := Describe([]byte(src))
	if in.Package != "com.example.shop" {
		t.Errorf("package = %q", in.Package)
	}
	want := []Decl{
		{Name: "CartTest", Kind: "class", Abstract: true, Extends: "BaseTest"},
		{Name: "Helper", Kind: "class", Extends: "com.example.util.Base"},
		{Name: "Plain", Kind: "class"},
		{Name: "Port", Kind: "interface"},
		{Name: "Slow", Kind: "@interface"},
		{Name: "Point", Kind: "record"},
		{Name: "Color", Kind: "enum"},
	}
	if !reflect.DeepEqual(in.Decls, want) {
		t.Errorf("decls:\n got %+v\nwant %+v", in.Decls, want)
	}
	var anns []string
	for a := range in.Annotations {
		anns = append(anns, a)
	}
	sort.Strings(anns)
	if want := []string{"DisplayName", "Nested", "SuppressWarnings", "Test"}; !reflect.DeepEqual(anns, want) {
		t.Errorf("annotations = %v, want %v", anns, want)
	}
}

func TestDescribeAbstractModifier(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"abstract class A {}", true},
		{"public abstract class A {}", true},
		{"@Tag(\"x\") public abstract class A {}", true},
		{"class B {} class A {}", false},
		{"abstract class B {} class A {}", false}, // the modifier belongs to B
		{"import x.y; class A {}", false},
		{"@abstract class A {}", false},
		// An annotation with an array argument between the modifier and the class hides
		// it: the class counts as concrete (it may then be a test unit, never fewer).
		{"abstract @A({1}) class A {}", false},
	} {
		in := Describe([]byte(tc.src))
		last := in.Decls[len(in.Decls)-1]
		if last.Name != "A" || last.Abstract != tc.want {
			t.Errorf("%q: %+v, want abstract=%v", tc.src, last, tc.want)
		}
	}
}

func TestResolve(t *testing.T) {
	in := Describe([]byte(`
package p;
import a.b.Base;
import c.d.*;
import static e.f.G.*;
import x.Outer;
class T {}
`))
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"Base", []string{"a.b.Base", "p.Base", "c.d.Base", "java.lang.Base"}},
		{"Other", []string{"p.Other", "c.d.Other", "java.lang.Other"}},
		{"Outer.Inner", []string{"x.Outer.Inner", "Outer.Inner", "p.Outer.Inner", "c.d.Outer.Inner", "java.lang.Outer.Inner"}},
		{"q.r.S", []string{"q.r.S", "p.q.r.S", "c.d.q.r.S", "java.lang.q.r.S"}},
	} {
		if got := in.Resolve(tc.name); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Resolve(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !in.Imported("Base", "a.") || in.Imported("Base", "java.") || in.Imported("Other", "") {
		t.Error("Imported")
	}
}
