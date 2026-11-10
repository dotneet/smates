package java

import "strings"

// Decl is a top-level type declaration of a Java file.
type Decl struct {
	Name string
	// Kind is "class", "interface", "enum", "record" or "@interface".
	Kind string
	// Abstract is set for a class declared abstract. A modifier written after an
	// annotation with an array argument (`@A({x}) abstract class`) is not seen: the
	// class then counts as concrete.
	Abstract bool
	// Extends is a class's superclass as written (a simple or qualified name, type
	// arguments dropped); "" without an extends clause.
	Extends string
}

// Info is what Describe finds in one Java source file.
type Info struct {
	Package string
	Decls   []Decl
	// Annotations holds the simple names of every annotation the file uses (the last
	// segment of a qualified one), wherever it appears.
	Annotations map[string]bool
	imports     []imp
}

// Describe reads the package, the top-level type declarations and the annotations of
// a Java source file. It needs no other file: resolving a name it holds is Resolve's.
func Describe(src []byte) *Info {
	toks := lex(string(src))
	u := scan(src)
	info := &Info{Package: u.pkg, imports: u.imports, Annotations: map[string]bool{}}
	depth := 0
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tokPunct {
			switch t.op {
			case '{':
				depth++
			case '}':
				if depth > 0 {
					depth--
				}
			case '@':
				if i+1 < len(toks) && toks[i+1].kind == tokIdent && toks[i+1].text != "interface" {
					name, next, _ := dotted(toks, i+1)
					info.Annotations[name[strings.LastIndexByte(name, '.')+1:]] = true
					i = next - 1
				}
			}
			continue
		}
		if depth != 0 || after(toks, i, '.') {
			continue
		}
		kind := t.text
		if kind == "interface" && after(toks, i, '@') {
			kind = "@interface"
		} else if !typeKeywords[kind] {
			continue
		}
		name, ok := typeName(toks, i)
		if !ok {
			continue
		}
		d := Decl{Name: name, Kind: kind, Abstract: modifier(toks, i, "abstract")}
		if kind == "class" {
			d.Extends = superclass(toks, i+2)
		}
		info.Decls = append(info.Decls, d)
	}
	return info
}

// modifier reports whether the modifiers before the type keyword at toks[i] include
// mod. They start after the previous declaration, statement or block.
func modifier(toks []token, i int, mod string) bool {
	for j := i - 1; j >= 0; j-- {
		t := toks[j]
		if t.kind == tokPunct && (t.op == ';' || t.op == '{' || t.op == '}') {
			return false
		}
		if t.kind == tokIdent && t.text == mod && !after(toks, j, '.') && !after(toks, j, '@') {
			return true
		}
	}
	return false
}

// superclass returns the name after "extends" in a class header whose type
// parameters, if any, start at toks[i].
func superclass(toks []token, i int) string {
	if i < len(toks) && toks[i].kind == tokPunct && toks[i].op == '<' {
		for n := 0; i < len(toks); i++ {
			if toks[i].kind == tokPunct && toks[i].op == '<' {
				n++
			} else if toks[i].kind == tokPunct && toks[i].op == '>' {
				if n--; n == 0 {
					i++
					break
				}
			}
		}
	}
	if i < len(toks) && toks[i].kind == tokIdent && toks[i].text == "extends" {
		if name, _, ok := dotted(toks, i+1); ok {
			return name
		}
	}
	return ""
}

// Resolve returns the qualified names a type name used in the file may refer to, most
// likely first: a qualified name as written (it may also be a nested type of an
// imported one: Outer.Inner), the single-type import of a simple name, the file's own
// package, each on-demand import, and java.lang.
func (in *Info) Resolve(name string) []string {
	first, rest, nested := strings.Cut(name, ".")
	var out []string
	for _, im := range in.imports {
		if !im.static && !im.onDemand && (im.name == first || strings.HasSuffix(im.name, "."+first)) {
			out = append(out, im.name+suffix(rest, nested))
		}
	}
	if nested {
		out = append(out, name)
	}
	out = append(out, qualify(in.Package, name))
	for _, im := range in.imports {
		if im.onDemand && !im.static {
			out = append(out, im.name+"."+name)
		}
	}
	return append(out, "java.lang."+name)
}

func suffix(rest string, ok bool) string {
	if !ok {
		return ""
	}
	return "." + rest
}

// Imported reports whether a simple name is brought in by a single-type import whose
// package starts with prefix (e.g. "java.").
func (in *Info) Imported(name, prefix string) bool {
	for _, im := range in.imports {
		if !im.static && !im.onDemand && strings.HasSuffix(im.name, "."+name) && strings.HasPrefix(im.name, prefix) {
			return true
		}
	}
	return false
}
