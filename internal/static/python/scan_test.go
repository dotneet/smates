package python

import (
	"reflect"
	"strings"
	"testing"
)

// refString renders a modRef compactly: dots for the level, the name, then
// "[a,b]" for from-imported names, "*" for a prefix reference, "!" for an
// every-prefix reference and "@pkg" for an anchor package.
func refString(m modRef) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(".", m.level))
	b.WriteString(m.name)
	if m.names != nil {
		b.WriteString("[" + strings.Join(m.names, ",") + "]")
	}
	if m.prefix {
		b.WriteString("*")
	}
	if m.every {
		b.WriteString("!")
	}
	if m.pkg != "" {
		b.WriteString("@" + m.pkg)
	}
	return b.String()
}

func scanRefs(src string) []string {
	var out []string
	for _, r := range scan([]byte(src)).refs {
		out = append(out, refString(r))
	}
	return out
}

func TestScanImports(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"plain", "import os\n", []string{"os"}},
		{"dotted alias list", "import a.b.c as d, e\n", []string{"a.b.c", "e"}},
		{"spaces in dotted name", "import a . b\n", []string{"a.b"}},
		{"from", "from pkg.sub import name\n", []string{"pkg.sub[name]"}},
		{"from alias", "from x import a as b, c\n", []string{"x[a,c]"}},
		{"from star", "from x import *\n", []string{"x"}},
		{"relative dot", "from . import x\n", []string{".[x]"}},
		{"relative dotdot", "from .. import y\n", []string{"..[y]"}},
		{"relative module", "from .mod import z\n", []string{".mod[z]"}},
		{"relative ellipsis", "from ...pkg.m import (a)\n", []string{"...pkg.m[a]"}},
		{
			"parenthesized multi-line",
			"from pkg import (\n    a,  # first\n    b as c,\n\n    d,\n)\nimport after\n",
			[]string{"pkg[a,b,d]", "after"},
		},
		{"backslash continuation", "from pkg \\\n    import a, \\\n    b\n", []string{"pkg[a,b]"}},
		{"crlf continuation", "import a, \\\r\n  b\r\nimport c\r\n", []string{"a", "b", "c"}},
		{"semicolons", "x = 1; import a; from b import c\n", []string{"a", "b[c]"}},
		{
			"nested in def/try/if",
			"def f():\n    import inner\n\ntry:\n    import fast\nexcept ImportError:\n    import slow\nif TYPE_CHECKING:\n    from typing_mod import T\n",
			[]string{"inner", "fast", "slow", "typing_mod[T]"},
		},
		{"compound one-liners", "try: import a\nexcept: import b\nif x: from c import d\n", []string{"a", "b", "c[d]"}},
		{"comment", "# import nope\nimport yes  # import nope2\n", []string{"yes"}},
		{
			"docstring",
			"\"\"\"Module docs.\n\nimport nope\nfrom nope import x\n\"\"\"\nimport yes\n",
			[]string{"yes"},
		},
		{
			"single-quoted triple and raw",
			"x = r'''\nimport nope\n'''\ny = '\\'import nope'\nimport yes\n",
			[]string{"yes"},
		},
		{"string with escaped quote", "s = \"a\\\"import nope\"\nimport yes\n", []string{"yes"}},
		{"bytes prefix", "s = rb\"import nope\"\nimport yes\n", []string{"yes"}},
		{"yield from / raise from", "def g():\n    yield from gen()\n    raise E from err\n", nil},
		{"from as identifier prefix", "from_x = 1\nimportant = 2\n", nil},
		{"import in expression is not a statement", "x = foo.import_\n", nil},
		{"f-string nested same quotes", "s = f\"{d[\"k\"]} import nope\"\nimport yes\n", []string{"yes"}},
		{"f-string braces and format spec", "s = f\"{{import nope}} {x:{w}} {y!r:>10}\"\nimport yes\n", []string{"yes"}},
		{"f-string named escape", "s = f\"\\N{DASH} {x}\"\nimport yes\n", []string{"yes"}},
		{
			"triple f-string with multi-line field",
			"s = f\"\"\"\n{\n  x  # comment }\n}\nimport nope\n\"\"\"\nimport yes\n",
			[]string{"yes"},
		},
		{"unterminated string is line-local", "s = 'oops\nimport yes\n", []string{"yes"}},
		{"unbalanced bracket recovery", "x = foo(\nimport yes\nfrom a import b\n", []string{"yes", "a[b]"}},
		{"bom", "\xef\xbb\xbfimport yes\n", []string{"yes"}},
		{"future", "from __future__ import annotations\n", []string{"__future__[annotations]"}},
		{"no trailing newline", "import a", []string{"a"}},
		{"incomplete from", "from pkg\nfrom . \nfrom import x\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scanRefs(tt.src); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("refs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScanDynamic(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"import_module", "importlib.import_module(\"a.b\")\n", []string{"a.b"}},
		{"bare import_module", "from importlib import import_module\nm = import_module('x.y')\n", []string{"importlib[import_module]", "x.y"}},
		{"__import__", "m = __import__(\"a\")\n", []string{"a"}},
		{"importorskip", "np = pytest.importorskip(\"numpy\")\n", []string{"numpy"}},
		{"f-string prefix", "importlib.import_module(f\"pkg.plugins.{name}\")\n", []string{"pkg.plugins.*"}},
		{"f-string partial name", "import_module(f\"pkg.mod_{v}\")\n", []string{"pkg.mod_*"}},
		{"concatenation", "import_module(\"pkg.plugins.\" + name)\n", []string{"pkg.plugins.*"}},
		{"percent format", "import_module(\"pkg.%s\" % name)\n", []string{"pkg.*"}},
		{"str.format", "import_module(\"pkg.{}.impl\".format(name))\n", []string{"pkg.*"}},
		{"relative anchored", "import_module(\".x\", package=\"a.b\")\n", []string{".x@a.b"}},
		{"relative positional anchor", "import_module(\"..x\", \"a.b\")\n", []string{"..x@a.b"}},
		{"relative unanchored", "import_module(\".x\", __package__)\n", []string{".x"}},
		{"relative __import__ rejected", "__import__(\".x\")\n", nil},
		{"fully dynamic rejected", "import_module(name)\nimport_module(f\"{name}\")\n", nil},
		{"invalid literal rejected", "import_module(\"not a module\")\n", nil},
		{"pytest_plugins list", "pytest_plugins = [\n    \"fixtures.db\",\n    \"fixtures.web\",\n]\n", []string{"fixtures.db", "fixtures.web"}},
		{"pytest_plugins string", "pytest_plugins = \"fixtures.db\"\n", []string{"fixtures.db"}},
		{"pytest_plugins tuple", "pytest_plugins = \"a\", \"b\"\n", []string{"a", "b"}},
		{"patch target", "@mock.patch(\"pkg.mod.func\")\ndef t(): pass\n", []string{"pkg.mod.func!"}},
		{"monkeypatch setattr", "monkeypatch.setattr(\"pkg.mod.X\", 1)\n", []string{"pkg.mod.X!"}},
		{"patch.object ignored", "mock.patch.object(obj, \"attr\")\nsetattr(obj, \"a.b\", 1)\n", nil},
		{"string mention ignored", "x = \"importlib.import_module('a')\"\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scanRefs(tt.src); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("refs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScanAll(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"list", "__all__ = [\"a\", 'b']\n", []string{"a", "b"}},
		{"annotated", "__all__: list[str] = [\"a\"]\n", []string{"a"}},
		{"augmented", "__all__ += (\"c\",)\n", []string{"c"}},
		{"extend", "__all__.extend([\"d\"])\n__all__.append(\"e\")\n", []string{"d", "e"}},
		{"concatenation stops", "__all__ = [\"a\"] + other.__all__\n", []string{"a"}},
		{"comparison ignored", "if __all__ == [\"x\"]: pass\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scan([]byte(tt.src)).all; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("all = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScanEmpty(t *testing.T) {
	if got := scan(nil); got.refs != nil || got.all != nil {
		t.Errorf("scan(nil) = %+v", got)
	}
}

func FuzzScan(f *testing.F) {
	for _, s := range []string{
		"import a\n", "f'{x:{y}}'", "f\"{'", "'''", "from . import (", "\\", "f'{",
		"importlib.import_module(f\"a.{b}\")", "rb'\\", "f\"\\N{", "__all__: =",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		scan([]byte(src)) // must not panic or hang
	})
}
