package js

import (
	"reflect"
	"testing"
)

// TestScanSourceSlash covers the regex-or-division decision for '/'. A wrong guess must
// never swallow (or expose) a backtick, which would invert the template state and hide
// every reference up to the next template literal.
func TestScanSourceSlash(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		// Divisions that used to be lexed as regexes.
		{"jsx self-closing after expression",
			"const C = () => <><Avatar src={u.a} /> <Link href={`/users/${u.id}`}>P</Link></>;\nimport('./Lazy')\nvi.mock('./api')",
			[]string{"m:./Lazy", "m:./api"}},
		{"jsx self-closing on one line",
			"const C = () => <><Image source={require('./a.png')} /><Image source={require('./b.png')} /></>",
			[]string{"m:./a.png", "m:./b.png"}},
		{"jsx self-closing after spread", "<Foo {...props}/>{`x`}\nrequire('./yes')", []string{"m:./yes"}},
		{"jsx closing tag then template", "const C = () => <p>Hi</p><a href={`/x`}>a</a>;\nrequire('./yes')",
			[]string{"m:./yes"}},
		{"jsx text division between expressions", "const C = () => <span>{page} / {`${total}`} / {n}</span>;\nimport('./yes')",
			[]string{"m:./yes"}},
		{"jsx text division with require", "<span>{a} / {require('./b')}</span>", []string{"m:./b"}},
		{"non-null assertion", "const half = width! / 2; const url = `${base}/x`;\nimport('./later')",
			[]string{"m:./later"}},
		{"non-null assertion after call and index", "const a = f()! / 2 + `/`; const b = xs[i]! / 2 + `/`; const c = this.w! / 2 + `/`;\nimport('./yes')",
			[]string{"m:./yes"}},
		{"postfix increment", "const h = i++ / 2; const u = `${a}/b`;\nimport('./yes')", []string{"m:./yes"}},
		{"postfix decrement", "const h = i-- / 2; const u = `${a}/b`;\nimport('./yes')", []string{"m:./yes"}},
		{"object literal then division", "const r = {a: 1}.a / 2 + `/`;\nrequire('./yes')", []string{"m:./yes"}},

		// Genuine regexes must still be recognised, so that the literals after them are.
		{"regex of backtick after assignment", "x = /`/; import './yes'; const t = `a`", []string{"m:./yes"}},
		{"regex of backtick as argument", "s.replace(/`/g, '\\\\`'); import('./yes'); const t = `a`", []string{"m:./yes"}},
		{"regex of gt as argument", `s.replace(/>/g, "&gt;"); require("./yes")`, []string{"m:./yes"}},
		{"regex after if head", "if (a) /re/.test(s) && require('./yes')", []string{"m:./yes"}},
		{"regex after while head", "while (x) /a/.exec(s); require('./yes')", []string{"m:./yes"}},
		{"regex after block", "function f() {}\n/foo/.test(x) && require('./yes')", []string{"m:./yes"}},
		{"regex after block with quote", "function f() {}\n/'/.test(x)\nrequire('./yes')", []string{"m:./yes"}},
		{"regex with escaped slash", "function f() { return /a\\/b/.test(s) ? require('./yes') : 0 }", []string{"m:./yes"}},
		{"regex with quote after return", `function f() { return /'/.test(s) ? "a" : require('./yes') }`, []string{"m:./yes"}},
		{"regex after minified return not", "function f(s){return!/'/.test(s)&&require('./yes')}", []string{"m:./yes"}},
		{"regex after arrow", "const f = () => /x/.test(s); import './yes'", []string{"m:./yes"}},
		{"regex with slash and backtick in class", "const re = /[/'`]/; require('./yes'); const t = `x`", []string{"m:./yes"}},
		{"regex after binary plus following increment", "n = i+++/`/.source.length; require('./yes'); t = `x`", []string{"m:./yes"}},
		{"regex after prefix not", "const ok = !/`/.test(s); require('./yes'); t = `x`", []string{"m:./yes"}},
		{"regex after keyword", "if (x instanceof /`/.constructor) require('./yes'); t = `x`", []string{"m:./yes"}},
		{"regex in template substitution", "const s = `${/`/.source}`; require('./yes')", []string{"m:./yes"}},

		// UTF-8 byte order mark.
		{"bom", "\xef\xbb\xbfimport './setup'", []string{"m:./setup"}},
		{"bom and hashbang", "\xef\xbb\xbf#!/usr/bin/env node\nrequire('./cli')", []string{"m:./cli"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := refStrings(scanSource([]byte(tt.src)))
			want := tt.want
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("scanSource(%q)\n got: %q\nwant: %q", tt.src, got, want)
			}
		})
	}
}
