package trace

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Quoted "$", "*", "?", "[", "{", "}" and blanks in the words the parser produces are
// marked so they are not mistaken for an unresolved variable, a glob pattern, a brace
// expansion or a field separator; literal restores them.
const (
	literalDollar = "\x01"
	literalStar   = "\x03"
	literalQuery  = "\x04"
	literalSet    = "\x05"
	literalOpen   = "\x06"
	literalClose  = "\x07"
	literalSpace  = "\x10"
	literalTab    = "\x11"
	literalNL     = "\x12"
)

var (
	quoteMarks = strings.NewReplacer("*", literalStar, "?", literalQuery, "[", literalSet, "{", literalOpen, "}", literalClose,
		" ", literalSpace, "\t", literalTab, "\n", literalNL)
	unquoteMarks = strings.NewReplacer(literalDollar, "$", literalStar, "*", literalQuery, "?", literalSet, "[", literalOpen, "{",
		literalClose, "}", literalSpace, " ", literalTab, "\t", literalNL, "\n")
	// globChars makes quoted glob characters and blanks active again (a variable used
	// unquoted is split into fields and its globs expand); braces stay marked: the shell
	// expands braces before variables.
	globChars = strings.NewReplacer(literalStar, "*", literalQuery, "?", literalSet, "[",
		literalSpace, " ", literalTab, "\t", literalNL, "\n") // keeps literalDollar
)

func literal(word string) string { return unquoteMarks.Replace(word) }

// A substitution ($(...), `...`, $((...)), <(...)) stands in a word as its id in the
// analysis' table between substOpen and substClose; it is evaluated where the command
// runs, in the state of the shell there (see substitute).
const (
	substOpen  = '\x0e'
	substClose = '\x0f'
)

func placeholder(id int) string {
	return string(rune(substOpen)) + strconv.Itoa(id) + string(rune(substClose))
}

// placeholders returns the ids of the substitutions in word, in order.
func placeholders(word string) []int {
	var ids []int
	for {
		i := strings.IndexByte(word, substOpen)
		if i < 0 {
			return ids
		}
		j := strings.IndexByte(word[i:], substClose)
		if j < 0 {
			return ids
		}
		if id, err := strconv.Atoi(word[i+1 : i+j]); err == nil {
			ids = append(ids, id)
		}
		word = word[i+j+1:]
	}
}

// The syntax tree of shell code. Words are as the lexer produced them: quoted characters
// marked (see literal), substitutions replaced by placeholders, redirection operators
// ("<", ">", ">>", "<<", "<<<") separate words followed by their target (a
// here-document's delimiter).
type (
	// shList is a sequence of and-or lists (separated by ";", "&" or newlines).
	shList struct{ items []*shAndOr }
	// shAndOr is pipelines joined by "&&" and "||" (ops[i] joins pipes[i] and pipes[i+1]);
	// bg says it runs asynchronously (ended by "&").
	shAndOr struct {
		pipes []*shPipeline
		ops   []string
		bg    bool
	}
	shPipeline struct {
		bang bool
		cmds []shCmd
	}
	// shCmd is one of *shSimple, *shGroup, *shIf, *shLoop, *shFor, *shCase, *shArith and
	// *shFuncDef.
	shCmd interface{}

	// shWords are words; extra lists the substitutions of the bodies of their
	// here-documents (run, their output is not used).
	shWords struct {
		words []string
		extra []int
	}
	shSimple struct{ shWords }
	// shGroup is { list } or, with subshell, ( list ).
	shGroup struct {
		body     *shList
		subshell bool
		redir    shWords
	}
	shIf struct {
		conds, bodies []*shList
		els           *shList // nil: no else
		redir         shWords
	}
	// shLoop is a while (until) loop.
	shLoop struct {
		until      bool
		cond, body *shList
		redir      shWords
	}
	// shFor is a for (select) loop over words (nil: "$@") or, with arith, a for (( ))
	// loop.
	shFor struct {
		name  string
		words *shWords
		arith *shArith
		body  *shList
		redir shWords
	}
	shCase struct {
		word  shWords
		items []*shCaseItem
		redir shWords
	}
	shCaseItem struct {
		patterns shWords
		body     *shList
		next     string // ";;", ";&" or ";;&"
		catchAll bool   // a "*" pattern: some item always matches
	}
	// shArith is an arithmetic expression: a (( )) command, the head of a for (( ))
	// loop or a $(( )) expansion. subs are the substitutions in it.
	shArith struct {
		text  string
		subs  []int
		redir shWords
	}
	shFuncDef struct {
		name string
		body shCmd
	}
)

// shSubst is a substitution: kind '$' for $(...), '`' for backticks, '<' for a process
// substitution, '(' for an arithmetic expansion.
type shSubst struct {
	kind  byte
	body  *shList
	arith *shArith
}

type tokKind int

const (
	tEOF tokKind = iota
	tWord
	tOp    // ; ;; ;& ;;& & && | || |& ( ) and newlines
	tRedir // a redirection operator ("" for one duplicating a descriptor)
	tArith // (( expression ))
)

type shTok struct {
	kind  tokKind
	text  string
	plain bool     // a word without quotes or expansions: it may be a reserved word
	hd    *heredoc // "<<": the here-document (text is its delimiter word)
	arith *shArith
}

type heredoc struct {
	delim  string
	tabs   bool // <<-: leading tabs are stripped
	quoted bool // the body is not expanded
	owner  *shWords
}

// reserved are the reserved words ending a list.
var reserved = map[string]bool{"then": true, "elif": true, "else": true, "fi": true, "do": true, "done": true, "esac": true, "}": true}

// shParser lexes and parses shell code in one pass: comments, quotes, escapes,
// here-documents and (nested) substitutions are handled where they occur. It never
// fails: what it cannot parse sets bad, and the analysis then assumes the code may read
// anything.
type shParser struct {
	a        *execAnalysis
	src      string
	pos      int
	tok      *shTok // the token peeked
	bad      bool
	backtick bool
	heredocs []*heredoc // started on the current line
	test     bool       // inside [[ ]]
	regex    bool       // after =~ inside [[ ]]
	work     *parseWork // shared with the parsers of nested code (backticks, here-documents)
	noArith  map[int]bool
}

// parseWork bounds the work of parsing a piece of code: input crafted so that an
// arithmetic expression is tried and parsed again as a command substitution at every
// nesting level would take exponential time. Past the budget, the parse stops (bad).
type parseWork struct{ n, budget int }

// parseShell parses shell code; bad says it could not be parsed entirely, backtick
// that it uses backticks.
func (a *execAnalysis) parseShell(src string) (l *shList, bad, backtick bool) {
	p := &shParser{a: a, src: src, work: &parseWork{budget: 64*len(src) + 100000}}
	l = p.script()
	return l, p.bad, p.backtick
}

// nested returns a parser of nested code src sharing p's budget.
func (p *shParser) nested(src string) *shParser {
	return &shParser{a: p.a, src: src, work: p.work}
}

// spend counts a step of the parse; past the budget it stops the parse (bad).
func (p *shParser) spend() bool {
	if p.work.n++; p.work.n <= p.work.budget {
		return true
	}
	p.bad = true
	p.pos = len(p.src)
	return false
}

func (p *shParser) addSubst(s *shSubst) string {
	p.a.substs = append(p.a.substs, s)
	return placeholder(len(p.a.substs) - 1)
}

func (p *shParser) peek() *shTok {
	if p.tok == nil {
		p.tok = p.lex()
	}
	return p.tok
}

func (p *shParser) next() *shTok {
	t := p.peek()
	p.tok = nil
	return t
}

func (t *shTok) is(kind tokKind, text string) bool { return t.kind == kind && t.text == text }

func (t *shTok) reserved(w string) bool { return t.kind == tWord && t.plain && t.text == w }

func (p *shParser) expectOp(op string) bool {
	if p.peek().is(tOp, op) {
		p.next()
		return true
	}
	p.bad = true
	return false
}

func (p *shParser) expectWord(w string) bool {
	if p.peek().reserved(w) {
		p.next()
		return true
	}
	p.bad = true
	return false
}

func (p *shParser) skipNewlines() {
	for p.peek().is(tOp, "\n") {
		p.next()
	}
}

// script parses a whole script; tokens no command starts with are skipped (bad).
func (p *shParser) script() *shList {
	l := &shList{}
	for {
		l.items = append(l.items, p.list().items...)
		t := p.peek()
		if t.kind == tEOF {
			break
		}
		// A stray ")" or reserved word, an operator starting a command. A ";;" outside
		// a case ends a command.
		if !t.is(tOp, ";;") {
			p.bad = true
		}
		p.next()
	}
	if len(p.heredocs) > 0 {
		p.bad = true // a here-document without a body
	}
	return l
}

// list parses commands up to a reserved word ending a list, ")", a case item's end or
// the end of the input.
func (p *shParser) list() *shList {
	l := &shList{}
	for {
		p.skipNewlines()
		t := p.peek()
		switch {
		case t.kind == tEOF, t.is(tOp, ")"), t.is(tOp, ";;"), t.is(tOp, ";&"), t.is(tOp, ";;&"),
			t.kind == tWord && t.plain && reserved[t.text]:
			return l
		}
		ao := p.andOr()
		if ao == nil {
			return l
		}
		l.items = append(l.items, ao)
		switch t := p.peek(); {
		case t.is(tOp, ";"), t.is(tOp, "\n"):
			p.next()
		case t.is(tOp, "&"):
			p.next()
			ao.bg = true
		default:
			return l
		}
	}
}

func (p *shParser) andOr() *shAndOr {
	pl := p.pipeline()
	if pl == nil {
		return nil
	}
	ao := &shAndOr{pipes: []*shPipeline{pl}}
	for {
		t := p.peek()
		if !t.is(tOp, "&&") && !t.is(tOp, "||") {
			return ao
		}
		p.next()
		p.skipNewlines()
		next := p.pipeline()
		if next == nil {
			p.bad = true
			return ao
		}
		ao.ops = append(ao.ops, t.text)
		ao.pipes = append(ao.pipes, next)
	}
}

func (p *shParser) pipeline() *shPipeline {
	pl := &shPipeline{}
	for {
		t := p.peek()
		if t.reserved("!") {
			p.next()
			pl.bang = !pl.bang
			continue
		}
		if t.reserved("time") {
			p.next()
			if p.peek().reserved("-p") {
				p.next()
			}
			continue
		}
		break
	}
	c := p.command()
	if c == nil {
		return nil
	}
	pl.cmds = append(pl.cmds, c)
	for p.peek().is(tOp, "|") || p.peek().is(tOp, "|&") {
		p.next()
		p.skipNewlines()
		c := p.command()
		if c == nil {
			p.bad = true
			break
		}
		pl.cmds = append(pl.cmds, c)
	}
	return pl
}

func (p *shParser) command() shCmd {
	t := p.peek()
	switch {
	case t.is(tOp, "("):
		p.next()
		g := &shGroup{body: p.list(), subshell: true}
		p.expectOp(")")
		p.redirs(&g.redir)
		return g
	case t.kind == tArith:
		p.next()
		p.redirs(&t.arith.redir)
		return t.arith
	case t.kind == tWord && t.plain:
		switch t.text {
		case "{":
			p.next()
			g := &shGroup{body: p.list()}
			p.expectWord("}")
			p.redirs(&g.redir)
			return g
		case "if":
			return p.ifCmd()
		case "while", "until":
			p.next()
			n := &shLoop{until: t.text == "until", cond: p.list()}
			p.expectWord("do")
			n.body = p.list()
			p.expectWord("done")
			p.redirs(&n.redir)
			return n
		case "for", "select":
			return p.forCmd()
		case "case":
			return p.caseCmd()
		case "function":
			p.next()
			name := p.peek()
			if name.kind != tWord {
				p.bad = true
				return nil
			}
			p.next()
			if p.peek().is(tOp, "(") {
				p.next()
				p.expectOp(")")
			}
			return p.funcBody(name.text)
		}
		if reserved[t.text] {
			return nil
		}
	}
	if t.kind == tWord || t.kind == tRedir {
		return p.simple()
	}
	return nil
}

// funcBody parses the body of a function definition: a compound command.
func (p *shParser) funcBody(name string) shCmd {
	p.skipNewlines()
	body := p.command()
	if _, simple := body.(*shSimple); body == nil || simple {
		p.bad = true
		if body == nil {
			return nil
		}
	}
	return &shFuncDef{name: name, body: body}
}

func (p *shParser) simple() shCmd {
	s := &shSimple{}
	for {
		t := p.peek()
		switch t.kind {
		case tWord:
			p.next()
			if len(s.words) == 0 && t.plain && t.text == "[[" {
				p.test = true
			}
			s.words = append(s.words, t.text)
			if len(s.words) == 1 && t.plain && funcName.MatchString(t.text) && p.peek().is(tOp, "(") {
				// name ( ) compound-command
				p.next()
				p.expectOp(")")
				return p.funcBody(t.text)
			}
		case tRedir:
			p.redir(&s.shWords)
		default:
			if len(s.words) == 0 {
				return nil
			}
			return s
		}
	}
}

// redir adds the redirection peeked and its target to w.
func (p *shParser) redir(w *shWords) {
	t := p.next()
	switch {
	case t.text == "":
		// duplicates a descriptor: no file
	case t.hd != nil:
		t.hd.owner = w
		w.words = append(w.words, "<<", t.hd.delim)
	default:
		w.words = append(w.words, t.text)
		if target := p.peek(); target.kind == tWord {
			p.next()
			w.words = append(w.words, target.text)
		} else {
			p.bad = true
		}
	}
}

// redirs parses the redirections of a compound command.
func (p *shParser) redirs(w *shWords) {
	for p.peek().kind == tRedir {
		p.redir(w)
	}
}

func (p *shParser) ifCmd() shCmd {
	p.next()
	n := &shIf{}
	for {
		n.conds = append(n.conds, p.list())
		p.expectWord("then")
		n.bodies = append(n.bodies, p.list())
		if !p.peek().reserved("elif") {
			break
		}
		p.next()
	}
	if p.peek().reserved("else") {
		p.next()
		n.els = p.list()
	}
	p.expectWord("fi")
	p.redirs(&n.redir)
	return n
}

func (p *shParser) forCmd() shCmd {
	p.next()
	n := &shFor{}
	switch t := p.peek(); t.kind {
	case tArith:
		p.next()
		n.arith = t.arith
	case tWord:
		p.next()
		n.name = literal(t.text)
		p.skipNewlines()
		if p.peek().reserved("in") {
			p.next()
			n.words = &shWords{}
			for p.peek().kind == tWord {
				n.words.words = append(n.words.words, p.next().text)
			}
		}
	default:
		p.bad = true
		return nil
	}
	if t := p.peek(); t.is(tOp, ";") || t.is(tOp, "\n") {
		p.next()
	}
	p.skipNewlines()
	if p.peek().reserved("{") {
		// for x in a b; { list; } (bash)
		p.next()
		n.body = p.list()
		p.expectWord("}")
	} else {
		p.expectWord("do")
		n.body = p.list()
		p.expectWord("done")
	}
	p.redirs(&n.redir)
	return n
}

func (p *shParser) caseCmd() shCmd {
	p.next()
	n := &shCase{}
	if t := p.peek(); t.kind == tWord {
		p.next()
		n.word.words = []string{t.text}
	} else {
		p.bad = true
		return nil
	}
	p.skipNewlines()
	if !p.expectWord("in") {
		return n
	}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.reserved("esac") {
			p.next()
			break
		}
		if t.is(tOp, "(") {
			p.next()
		}
		it := &shCaseItem{}
		for {
			w := p.peek()
			if w.kind != tWord {
				p.bad = true
				break
			}
			p.next()
			it.patterns.words = append(it.patterns.words, w.text)
			if w.plain && w.text == "*" {
				it.catchAll = true
			}
			if !p.peek().is(tOp, "|") {
				break
			}
			p.next()
		}
		if !p.expectOp(")") {
			break
		}
		it.body = p.list()
		n.items = append(n.items, it)
		t = p.peek()
		if t.is(tOp, ";;") || t.is(tOp, ";&") || t.is(tOp, ";;&") {
			p.next()
			it.next = t.text
			continue
		}
		if !t.reserved("esac") {
			p.bad = true
			break
		}
	}
	p.redirs(&n.redir)
	return n
}

// subList parses the commands of a $( ), <( ) or >( ) substitution, up to its ")".
func (p *shParser) subList() *shList {
	saved, test, regex := p.tok, p.test, p.regex
	p.tok, p.test, p.regex = nil, false, false
	l := &shList{}
	for {
		l.items = append(l.items, p.list().items...)
		t := p.peek()
		if t.is(tOp, ")") {
			p.next()
			break
		}
		p.bad = true
		if t.kind == tEOF {
			break
		}
		p.next()
	}
	p.tok, p.test, p.regex = saved, test, regex
	return l
}

// The lexer.

func isMeta(c byte) bool { return strings.IndexByte(" \t\r\n;&|()<>", c) >= 0 }

func (p *shParser) lex() *shTok {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || (c == '\n' && p.test):
			p.pos++
			continue
		case c == '\\' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '\n':
			p.pos += 2 // line continuation
			continue
		case c == '#':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
	if p.pos >= len(p.src) || !p.spend() {
		return &shTok{kind: tEOF}
	}
	if p.test {
		return p.testToken()
	}
	s := p.src[p.pos:]
	op := func(text string) *shTok {
		p.pos += len(text)
		return &shTok{kind: tOp, text: text}
	}
	switch s[0] {
	case '\n':
		p.pos++
		p.heredocBodies()
		return &shTok{kind: tOp, text: "\n"}
	case ';':
		for _, o := range []string{";;&", ";;", ";&"} {
			if strings.HasPrefix(s, o) {
				return op(o)
			}
		}
		return op(";")
	case '&':
		switch {
		case strings.HasPrefix(s, "&&"):
			return op("&&")
		case strings.HasPrefix(s, "&>>"):
			p.pos += 3
			return &shTok{kind: tRedir, text: ">>"}
		case strings.HasPrefix(s, "&>"):
			p.pos += 2
			return &shTok{kind: tRedir, text: ">"}
		}
		return op("&")
	case '|':
		if strings.HasPrefix(s, "||") || strings.HasPrefix(s, "|&") {
			return op(s[:2])
		}
		return op("|")
	case '(':
		if strings.HasPrefix(s, "((") {
			if ar := p.arith(p.pos + 2); ar != nil {
				return &shTok{kind: tArith, arith: ar}
			}
		}
		return op("(")
	case ')':
		return op(")")
	case '<', '>':
		if len(s) < 2 || s[1] != '(' {
			return p.redirToken()
		}
	}
	return p.word()
}

// testToken lexes the words of a [[ ]] test: blanks separate them; parentheses, &&,
// || and ! are words of their own, < and > comparisons (no redirections); ]] ends it.
func (p *shParser) testToken() *shTok {
	s := p.src[p.pos:]
	word := func(n int, text string) *shTok {
		p.pos += n
		return &shTok{kind: tWord, text: text, plain: true}
	}
	switch {
	case strings.HasPrefix(s, "]]") && (len(s) == 2 || isMeta(s[2])):
		p.test, p.regex = false, false
		return word(2, "]]")
	case strings.HasPrefix(s, "&&") || strings.HasPrefix(s, "||"):
		return word(2, s[:2])
	case s[0] == '<':
		return word(1, "-lt") // a string comparison
	case s[0] == '>':
		return word(1, "-gt")
	case !p.regex && (s[0] == '(' || s[0] == ')'):
		return word(1, s[:1])
	}
	t := p.word()
	p.regex = t.plain && t.text == "=~"
	return t
}

// redirToken lexes a redirection operator.
func (p *shParser) redirToken() *shTok {
	s := p.src[p.pos:]
	tok := func(n int, text string) *shTok {
		p.pos += n
		return &shTok{kind: tRedir, text: text}
	}
	switch {
	case strings.HasPrefix(s, "<<<"):
		return tok(3, "<<<")
	case strings.HasPrefix(s, "<<"):
		return p.heredocToken()
	case strings.HasPrefix(s, "<&") || strings.HasPrefix(s, ">&"):
		// <&0, >&2, >&-, <&$fd duplicate a descriptor; ">& file" redirects to a file.
		p.pos += 2
		j := p.pos
		for j < len(p.src) && (p.src[j] == '-' || (p.src[j] >= '0' && p.src[j] <= '9')) {
			j++
		}
		switch {
		case j > p.pos:
			p.pos = j
			return &shTok{kind: tRedir}
		case j < len(p.src) && p.src[j] == '$':
			p.word()
			return &shTok{kind: tRedir}
		case s[0] == '<':
			return &shTok{kind: tRedir, text: "<"}
		}
		return &shTok{kind: tRedir, text: ">"}
	case strings.HasPrefix(s, ">>"):
		return tok(2, ">>")
	case strings.HasPrefix(s, "<>"):
		return tok(2, "<")
	case strings.HasPrefix(s, ">|"):
		return tok(2, ">")
	}
	return tok(1, s[:1])
}

// heredocToken lexes <<DELIM, <<-DELIM, <<'DELIM': its body follows the line.
func (p *shParser) heredocToken() *shTok {
	p.pos += 2
	h := &heredoc{}
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		h.tabs = true
		p.pos++
	}
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
	var d strings.Builder
	for ; p.pos < len(p.src) && !isMeta(p.src[p.pos]); p.pos++ {
		switch c := p.src[p.pos]; c {
		case '\'', '"', '\\':
			h.quoted = true
		default:
			d.WriteByte(c)
		}
	}
	h.delim = d.String()
	if h.delim == "" {
		p.bad = true
	}
	p.heredocs = append(p.heredocs, h)
	return &shTok{kind: tRedir, text: "<<", hd: h}
}

// heredocBodies skips the bodies of the here-documents started on the line that just
// ended. The substitutions of an unquoted one's body run.
func (p *shParser) heredocBodies() {
	if len(p.heredocs) == 0 {
		return
	}
	hds := p.heredocs
	p.heredocs = nil
	j := p.pos
	for _, h := range hds {
		start, found := j, false
		for j < len(p.src) && !found {
			if !p.spend() {
				return
			}
			end := strings.IndexByte(p.src[j:], '\n')
			next := j + end + 1
			if end < 0 {
				end, next = len(p.src)-j, len(p.src)
			}
			line := p.src[j : j+end]
			if h.tabs {
				line = strings.TrimLeft(line, "\t")
			}
			if found = line == h.delim; !found {
				j = next
				continue
			}
			if !h.quoted && h.owner != nil {
				h.owner.extra = append(h.owner.extra, p.expansions(p.src[start:j])...)
			}
			j = next
		}
		if !found {
			// Not ended by its delimiter: the shell reads the rest as the body; the
			// analysis keeps parsing it as commands.
			p.bad = true
			return
		}
	}
	p.pos = j
}

// expansions parses the substitutions of a here-document body and returns their ids.
func (p *shParser) expansions(body string) []int {
	q := p.nested(body)
	var b strings.Builder
	for q.pos < len(q.src) && q.spend() {
		switch q.src[q.pos] {
		case '\\':
			q.pos += 2
		case '$':
			q.dollar(&b, true)
		case '`':
			q.backquote(&b)
		default:
			q.pos++
		}
	}
	p.bad = p.bad || q.bad
	p.backtick = p.backtick || q.backtick
	return placeholders(b.String())
}

// word lexes a word.
func (p *shParser) word() *shTok {
	var b strings.Builder
	plain := true
	for p.pos < len(p.src) && p.spend() {
		c := p.src[p.pos]
		switch {
		case p.test && (c == ' ' || c == '\t' || c == '\r' || c == '\n' || (!p.regex && (c == '(' || c == ')'))):
			return &shTok{kind: tWord, text: b.String(), plain: plain}
		case p.test && !p.regex && (strings.HasPrefix(p.src[p.pos:], "&&") || strings.HasPrefix(p.src[p.pos:], "||")):
			return &shTok{kind: tWord, text: b.String(), plain: plain}
		case p.test && strings.IndexByte(";&|<>()", c) >= 0:
			b.WriteByte(c)
			p.pos++
		case c == '(' && arrayStart.MatchString(b.String()):
			p.array(&b)
			plain = false
		case (c == '<' || c == '>') && p.pos+1 < len(p.src) && p.src[p.pos+1] == '(':
			// <(list), >(list): a process substitution, a path standing for a pipe.
			p.pos += 2
			b.WriteString(p.addSubst(&shSubst{kind: '<', body: p.subList()}))
			plain = false
		case (c == '<' || c == '>') && plain && isDigits(b.String()):
			// 2>file: the descriptor number is part of the redirection.
			return p.redirToken()
		case isMeta(c):
			return &shTok{kind: tWord, text: b.String(), plain: plain}
		case c == '\\':
			plain = false
			switch {
			case p.pos+1 >= len(p.src):
				p.pos++ // a trailing backslash escapes nothing
			case p.src[p.pos+1] == '\n':
				p.pos += 2 // line continuation
			default:
				p.escaped(&b, p.src[p.pos+1])
				p.pos += 2
			}
		case c == '\'':
			plain = false
			j := strings.IndexByte(p.src[p.pos+1:], '\'')
			if j < 0 {
				p.bad = true
				j = len(p.src) - p.pos - 1
			}
			// $ and glob characters inside single quotes are literal.
			b.WriteString(quoteMarks.Replace(strings.ReplaceAll(p.src[p.pos+1:p.pos+1+j], "$", literalDollar)))
			p.pos = min(len(p.src), p.pos+j+2)
		case c == '"':
			plain = false
			p.dquote(&b)
		case c == '$':
			plain = false
			p.dollar(&b, false)
		case c == '`':
			plain = false
			p.backquote(&b)
		case c == '~' && (b.Len() == 0 || assignName.MatchString(b.String())):
			// An unquoted ~ starting a word or an assignment's value is $HOME (followed
			// like the variable: unknown unless the script assigns it); ~user, ~+ and ~-
			// name other directories, unknown too.
			plain = false
			p.pos++
			if p.pos == len(p.src) || p.src[p.pos] == '/' || isMeta(p.src[p.pos]) {
				b.WriteString("${HOME}")
			} else {
				b.WriteString(unresolvedSubst)
				for p.pos < len(p.src) && strings.IndexByte("/ \t\n;|&<>()'\"\\$`", p.src[p.pos]) < 0 {
					p.pos++ // the user name
				}
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	return &shTok{kind: tWord, text: b.String(), plain: plain}
}

// escaped writes the character c escaped by a backslash.
func (p *shParser) escaped(b *strings.Builder, c byte) {
	if c == '$' {
		b.WriteString(literalDollar)
	} else {
		b.WriteString(quoteMarks.Replace(string(c)))
	}
}

// arrayStart matches the start of an array assignment, NAME=( or NAME+=(.
var arrayStart = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=$`)

// array lexes the elements of an array assignment: their substitutions run, the value
// is unknown.
func (p *shParser) array(b *strings.Builder) {
	p.pos++
	b.WriteString(unresolvedSubst)
	saved := p.tok
	p.tok = nil
	for {
		t := p.next()
		if t.kind == tWord {
			for _, id := range placeholders(t.text) {
				b.WriteString(placeholder(id))
			}
			continue
		}
		if t.is(tOp, ")") {
			break
		}
		if !t.is(tOp, "\n") {
			p.bad = true
			break
		}
	}
	p.tok = saved
}

// dquote lexes a double-quoted string.
func (p *shParser) dquote(b *strings.Builder) {
	p.pos++
	for p.pos < len(p.src) && p.spend() {
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			return
		case c == '\\' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '\n':
			p.pos += 2 // line continuation
		case c == '\\' && p.pos+1 < len(p.src) && strings.IndexByte("$`\"\\", p.src[p.pos+1]) >= 0:
			p.escaped(b, p.src[p.pos+1])
			p.pos += 2
		case c == '$':
			p.dollar(b, true)
		case c == '`':
			p.backquote(b)
		default:
			b.WriteString(quoteMarks.Replace(string(c)))
			p.pos++
		}
	}
	p.bad = true // unterminated
}

func isNameStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

// dollar lexes what starts with "$": a variable, ${...}, $(...), $((...)), $'...'.
func (p *shParser) dollar(b *strings.Builder, quoted bool) {
	s := p.src[p.pos:]
	switch {
	case strings.HasPrefix(s, "$(("):
		if ar := p.arith(p.pos + 3); ar != nil {
			b.WriteString(p.addSubst(&shSubst{kind: '(', arith: ar}))
			return
		}
		fallthrough // $( (list) )
	case strings.HasPrefix(s, "$("):
		p.pos += 2
		b.WriteString(p.addSubst(&shSubst{kind: '$', body: p.subList()}))
	case strings.HasPrefix(s, "${"):
		p.param(b)
	case strings.HasPrefix(s, "$'") && !quoted:
		// ANSI-C quoting: literal, with backslash escapes.
		p.pos += 2
		for p.pos < len(p.src) && p.src[p.pos] != '\'' {
			if p.src[p.pos] == '\\' && p.pos+1 < len(p.src) {
				p.pos++
			}
			p.escaped(b, p.src[p.pos])
			p.pos++
		}
		if p.pos >= len(p.src) {
			p.bad = true
		}
		p.pos++
	case strings.HasPrefix(s, "$\"") && !quoted:
		p.pos++
		p.dquote(b)
	case len(s) > 1 && isNameStart(s[1]):
		j := 2
		for j < len(s) && isNameChar(s[j]) {
			j++
		}
		b.WriteString(s[:j])
		p.pos += j
	case len(s) > 1 && strings.IndexByte("0123456789#?$!@*-", s[1]) >= 0:
		b.WriteString(s[:2])
		p.pos += 2
	default:
		b.WriteString(literalDollar) // a lone $ is literal
		p.pos++
	}
}

// param lexes a ${...} parameter expansion (kept as written, its substitutions
// replaced).
func (p *shParser) param(b *strings.Builder) {
	b.WriteString("${")
	p.pos += 2
	for p.pos < len(p.src) && p.spend() {
		c := p.src[p.pos]
		switch {
		case c == '}':
			b.WriteByte('}')
			p.pos++
			return
		case c == '\\' && p.pos+1 < len(p.src):
			if p.src[p.pos+1] != '\n' {
				p.escaped(b, p.src[p.pos+1])
			}
			p.pos += 2
		case c == '\'':
			j := strings.IndexByte(p.src[p.pos+1:], '\'')
			if j < 0 {
				p.bad = true
				p.pos = len(p.src)
				return
			}
			b.WriteString(quoteMarks.Replace(strings.ReplaceAll(p.src[p.pos+1:p.pos+1+j], "$", literalDollar)))
			p.pos += j + 2
		case c == '"':
			p.dquote(b)
		case c == '$':
			p.dollar(b, true)
		case c == '`':
			p.backquote(b)
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	p.bad = true // unterminated
}

// backquote lexes a `...` command substitution.
func (p *shParser) backquote(b *strings.Builder) {
	p.pos++
	p.backtick = true
	var inner strings.Builder
	closed := false
	for p.pos < len(p.src) && p.spend() {
		c := p.src[p.pos]
		if c == '\\' && p.pos+1 < len(p.src) && strings.IndexByte("$`\\", p.src[p.pos+1]) >= 0 {
			inner.WriteByte(p.src[p.pos+1])
			p.pos += 2
			continue
		}
		p.pos++
		if c == '`' {
			closed = true
			break
		}
		inner.WriteByte(c)
	}
	q := p.nested(inner.String())
	body := q.script()
	p.bad = p.bad || q.bad || !closed
	b.WriteString(p.addSubst(&shSubst{kind: '`', body: body}))
}

// arith lexes an arithmetic expression starting at start (after "((" or "$((") up to
// the matching "))"; nil (and nothing consumed) when the parentheses do not close as
// one (nested subshells).
func (p *shParser) arith(start int) *shArith {
	if p.noArith[start] {
		return nil // tried before
	}
	pos, bad, backtick, nsubst := p.pos, p.bad, p.backtick, len(p.a.substs)
	p.pos = start
	var b strings.Builder
	depth := 0
	for p.pos < len(p.src) && p.spend() {
		c := p.src[p.pos]
		switch {
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case c == ')':
			if p.pos+1 < len(p.src) && p.src[p.pos+1] == ')' {
				p.pos += 2
				text := b.String()
				return &shArith{text: text, subs: placeholders(text)}
			}
			p.pos = len(p.src) // not arithmetic
			continue
		case c == '\\' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '\n':
			p.pos += 2
			continue
		case c == '$':
			p.dollar(&b, true)
			continue
		case c == '`':
			p.backquote(&b)
			continue
		case c == '"':
			p.dquote(&b)
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	p.a.substs = p.a.substs[:nsubst]
	if p.work.n > p.work.budget {
		p.bad, p.pos = true, len(p.src) // the parse stops
		return nil
	}
	p.pos, p.bad, p.backtick = pos, bad, backtick
	if p.noArith == nil {
		p.noArith = map[int]bool{}
	}
	p.noArith[start] = true // parsed again as a command substitution or subshells
	return nil
}

// arithAssigned matches the variables an arithmetic expression assigns (x=1, x+=2,
// x++, --x).
var arithAssigned = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(?:[-+*/%&|^]|<<|>>)?=(?:[^=]|$)|([A-Za-z_][A-Za-z0-9_]*)\s*(?:\+\+|--)|(?:\+\+|--)\s*([A-Za-z_][A-Za-z0-9_]*)`)

// assignedIn returns the variables arithmetic text assigns.
func assignedIn(text string) []string {
	var names []string
	for _, m := range arithAssigned.FindAllStringSubmatch(text, -1) {
		names = append(names, m[1]+m[2]+m[3])
	}
	return names
}

// maxWords bounds the words brace expansion, variables with several values and find's
// {} make of a command: past it, the command is opaque.
const maxWords = 1024

// braceSeg brace-expands the words of a command (bash, zsh and ksh do, and /bin/sh is
// bash on macOS; dash would open a file literally named "{a,b}", which nobody creates).
// Assignments are not expanded. ok is false when the command would have more than
// maxWords words (and more than it had).
func braceSeg(seg []string) (out []string, ok bool) {
	limit := max(maxWords, len(seg))
	for _, w := range seg {
		if assign.MatchString(w) {
			out = append(out, w)
		} else {
			out = append(out, braceExpand(w)...)
		}
		if len(out) > limit {
			return nil, false
		}
	}
	return out, true
}

// braceExpand expands the unquoted brace expressions of a word, {a,b} and {1..3} or
// {a..e} (with an increment), as the shell does before any other expansion; "{}", "{x}"
// and ${...} stay as they are. Quoted braces are marked (see splitShell). It stops past
// maxWords words.
func braceExpand(w string) []string {
	for i := 0; i < len(w); i++ {
		if w[i] != '{' {
			continue
		}
		j := matchBrace(w, i)
		if j < 0 {
			return []string{w}
		}
		if i > 0 && w[i-1] == '$' {
			i = j // ${...}
			continue
		}
		alts := braceAlternatives(w[i+1 : j])
		if alts == nil {
			continue // literal braces; the ones inside may still expand
		}
		var out []string
		for _, alt := range alts {
			out = append(out, braceExpand(w[:i]+alt+w[j+1:])...)
			if len(out) > maxWords {
				return out[:maxWords+1] // too many (see braceSeg)
			}
		}
		return out
	}
	return []string{w}
}

// matchBrace returns the index of the "}" closing the "{" at w[i], or -1.
func matchBrace(w string, i int) int {
	depth := 0
	for j := i; j < len(w); j++ {
		switch w[j] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

// assignName matches the start of an assignment word, up to its value ("NAME=").
var assignName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=$`)

var braceRange = regexp.MustCompile(`^(-?[0-9]+|[A-Za-z])\.\.(-?[0-9]+|[A-Za-z])(?:\.\.(-?[0-9]+))?$`)

// braceAlternatives returns the words a brace expression's body (between the braces)
// stands for: its comma-separated parts or the elements of its sequence; nil when it is
// neither.
func braceAlternatives(body string) []string {
	var parts []string
	depth, start := 0, 0
	for k := 0; k < len(body); k++ {
		switch body[k] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:k])
				start = k + 1
			}
		}
	}
	if parts != nil {
		return append(parts, body[start:])
	}
	m := braceRange.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	step := 1
	if m[3] != "" {
		step, _ = strconv.Atoi(m[3])
		if step < 0 {
			step = -step
		}
		if step == 0 {
			step = 1
		}
	}
	from, errFrom := strconv.Atoi(m[1])
	to, errTo := strconv.Atoi(m[2])
	letters := errFrom != nil && errTo != nil
	switch {
	case letters:
		from, to = int(m[1][0]), int(m[2][0])
	case errFrom != nil || errTo != nil:
		return nil // {1..a}
	}
	// {01..10}: a leading zero pads every number to the widest end.
	width := 0
	if !letters && (strings.HasPrefix(strings.TrimPrefix(m[1], "-"), "0") && len(strings.TrimPrefix(m[1], "-")) > 1 ||
		strings.HasPrefix(strings.TrimPrefix(m[2], "-"), "0") && len(strings.TrimPrefix(m[2], "-")) > 1) {
		width = max(len(m[1]), len(m[2]))
	}
	var out []string
	for v := from; len(out) <= maxWords; {
		switch {
		case letters:
			out = append(out, string(rune(v)))
		case width > 0:
			out = append(out, fmt.Sprintf("%0*d", width, v))
		default:
			out = append(out, strconv.Itoa(v))
		}
		if v == to || (from < to && v+step > to) || (from > to && v-step < to) {
			break
		}
		if from < to {
			v += step
		} else {
			v -= step
		}
	}
	return out
}
