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

// stripComments removes shell comments (a # starting a word, outside quotes).
func stripComments(text string) string {
	var b strings.Builder
	inS, inD := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		case c == '#' && !inS && !inD && (i == 0 || text[i-1] == ' ' || text[i-1] == '\t' || text[i-1] == '\n' || text[i-1] == ';'):
			for i < len(text) && text[i] != '\n' {
				i++
			}
			if i < len(text) {
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Quoted "$", "*", "?", "[", "{" and "}" in words produced by splitShell are marked so
// they are not mistaken for an unresolved variable, a glob pattern or a brace expansion;
// literal restores them.
const (
	literalDollar = "\x01"
	literalStar   = "\x03"
	literalQuery  = "\x04"
	literalSet    = "\x05"
	literalOpen   = "\x06"
	literalClose  = "\x07"
)

var (
	quoteMarks   = strings.NewReplacer("*", literalStar, "?", literalQuery, "[", literalSet, "{", literalOpen, "}", literalClose)
	unquoteMarks = strings.NewReplacer(literalDollar, "$", literalStar, "*", literalQuery, "?", literalSet, "[", literalOpen, "{", literalClose, "}")
	// globChars makes quoted glob characters active again (a variable used unquoted);
	// braces stay marked: the shell expands braces before variables.
	globChars = strings.NewReplacer(literalStar, "*", literalQuery, "?", literalSet, "[") // keeps literalDollar
)

func literal(word string) string { return unquoteMarks.Replace(word) }

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

// arithmetic reports whether cmd[i:] starts a (( expression )) command: "((" not right
// after another "(" (nested subshells), closed by "))" with no parenthesis between.
func arithmetic(cmd string, i int) bool {
	if !strings.HasPrefix(cmd[i:], "((") {
		return false
	}
	if before := strings.TrimRight(cmd[:i], " \t"); strings.HasSuffix(before, "(") {
		return false
	}
	end := strings.Index(cmd[i+2:], "))")
	return end >= 0 && !strings.ContainsAny(cmd[i+2:i+2+end], "()")
}

// Segments of splitShell that open and close a ( ... ) subshell.
const (
	subshellOpen  = "\x02("
	subshellClose = "\x02)"
)

// splitShell splits a shell command line into simple commands (on |, ||, &&, ;, &,
// newlines) and words, honoring quotes. Redirection operators become separate words;
// parentheses become subshellOpen and subshellClose segments. The patterns of case
// items are left out.
func splitShell(cmd string) [][]string {
	var segs [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	// cases counts the case commands open; pattern is set where the patterns of an item
	// start (after "case WORD in", ";;", ";&" or ";;&"). They run nothing, and the ")"
	// ending them closes no subshell.
	cases, pattern := 0, false
	flushWord := func() {
		if inWord {
			w := cur.String()
			words = append(words, w)
			cur.Reset()
			inWord = false
			switch n := len(words); {
			case w == "in" && n >= 3 && words[n-3] == "case" && len(trimKeywords(words[:n-3])) == 0:
				cases++
				pattern = true
			case w == "esac" && cases > 0 && len(trimKeywords(words)) == 0:
				cases--
			}
		}
	}
	flushSeg := func() {
		flushWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}
	type heredoc struct {
		delim string
		tabs  bool // <<-: leading tabs are stripped
	}
	var heredocs []heredoc // here-documents whose bodies follow the current line
	for i := 0; i < len(cmd); i++ {
		if pattern {
			flushSeg()
			pattern = false
			j := i
			for j < len(cmd) && strings.IndexByte(" \t\n", cmd[j]) >= 0 {
				j++
			}
			esac := strings.HasPrefix(cmd[j:], "esac") && (j+4 == len(cmd) || strings.IndexByte(" \t\n;|&)", cmd[j+4]) >= 0)
			if end := patternEnd(cmd, j); !esac && end >= 0 {
				i = end
				continue
			}
		}
		c := cmd[i]
		switch {
		case c == '\\' && i+1 < len(cmd) && cmd[i+1] == '\n':
			i++ // line continuation
		case c == '\\' && i+1 < len(cmd):
			i++
			cur.WriteString(quoteMarks.Replace(cmd[i : i+1]))
			inWord = true
		case c == '\\':
			// a trailing backslash escapes nothing
		case c == '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				j = len(cmd) - i - 1
			}
			// $ and glob characters inside single quotes are literal: mark them so they
			// are not mistaken for an unresolved variable or a pattern (literal restores
			// them).
			cur.WriteString(quoteMarks.Replace(strings.ReplaceAll(cmd[i+1:i+1+j], "$", literalDollar)))
			i += j + 1
			inWord = true
		case c == '"':
			i++
			param := 0 // inside ${...}, whose braces are not marked
			for ; i < len(cmd) && cmd[i] != '"'; i++ {
				switch {
				case cmd[i] == '\\' && i+1 < len(cmd):
					i++
					if cmd[i] == '\n' {
						continue // line continuation
					}
				case cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '{':
					cur.WriteString("${")
					i++
					param++
					continue
				case param > 0 && (cmd[i] == '{' || cmd[i] == '}'):
					if cmd[i] == '{' {
						param++
					} else {
						param--
					}
					cur.WriteByte(cmd[i])
					continue
				}
				cur.WriteString(quoteMarks.Replace(cmd[i : i+1]))
			}
			inWord = true
		case c == '&' && i+1 < len(cmd) && cmd[i+1] == '>':
			// &> file / &>> file: redirection, not a separator.
			flushWord()
			i++
			op := ">"
			if i+1 < len(cmd) && cmd[i+1] == '>' {
				i++
				op = ">>"
			}
			words = append(words, op)
		case c == '(' && arithmetic(cmd, i) && !inWord && len(words) == 0:
			// (( expression )): reads no file; its << and < are operators.
			flushSeg()
			i += 2 + strings.Index(cmd[i+2:], "))") + 1
		case c == '(' || c == ')':
			flushSeg()
			if c == '(' {
				segs = append(segs, []string{subshellOpen})
			} else {
				segs = append(segs, []string{subshellClose})
			}
		case c == '\n' && len(heredocs) > 0:
			// Skip the bodies of the here-documents started on this line, when each ends
			// with its delimiter line (otherwise it is not one: keep parsing).
			flushSeg()
			j := i
			for _, h := range heredocs {
				found := false
				for !found && j < len(cmd) {
					line := cmd[j+1:]
					end := strings.IndexByte(line, '\n')
					if end < 0 {
						end = len(line)
					}
					line = line[:end]
					j += end + 1
					if h.tabs {
						line = strings.TrimLeft(line, "\t")
					}
					found = line == h.delim
				}
				if !found {
					j = i
					break
				}
			}
			i = j
			heredocs = nil
		case c == ';' && cases > 0 && i+1 < len(cmd) && (cmd[i+1] == ';' || cmd[i+1] == '&'):
			// ;; ;& ;;& end a case item: the next one starts with its patterns.
			flushSeg()
			i++
			if cmd[i] == ';' && i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
			}
			pattern = true
		case c == '|' || c == ';' || c == '&' || c == '\n':
			flushSeg()
		case c == '<' && strings.HasPrefix(cmd[i:], "<<<"):
			// <<< word: a here-string, the word is the input.
			flushWord()
			i += 2
			words = append(words, "<<<")
		case c == '<' && strings.HasPrefix(cmd[i:], "<<"):
			// <<DELIM, <<-DELIM, <<'DELIM': a here-document; its body follows the line.
			flushWord()
			i += 2
			h := heredoc{}
			if i < len(cmd) && cmd[i] == '-' {
				h.tabs = true
				i++
			}
			for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t') {
				i++
			}
			var d strings.Builder
			for ; i < len(cmd) && !strings.ContainsRune(" \t\n;|&<>()", rune(cmd[i])); i++ {
				if cmd[i] != '\'' && cmd[i] != '"' && cmd[i] != '\\' {
					d.WriteByte(cmd[i])
				}
			}
			i--
			h.delim = d.String()
			heredocs = append(heredocs, h)
			words = append(words, "<<", h.delim)
		case c == '<' || c == '>':
			// A file descriptor number right before the operator (2>file) is part of it.
			if inWord && isDigits(cur.String()) {
				cur.Reset()
				inWord = false
			}
			flushWord()
			op := string(c)
			if i+1 < len(cmd) && cmd[i+1] == '>' {
				op += ">"
				i++
			}
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				// >&2, 2>&1, <&0: duplicates a descriptor, no file involved.
				i++
				for i+1 < len(cmd) && (cmd[i+1] == '-' || (cmd[i+1] >= '0' && cmd[i+1] <= '9')) {
					i++
				}
				continue
			}
			words = append(words, op)
		case c == ' ' || c == '\t':
			flushWord()
		case c == '~' && (!inWord || assignName.MatchString(cur.String())):
			// An unquoted ~ starting a word or an assignment's value is $HOME (followed
			// like the variable: unknown unless the script assigns it); ~user, ~+ and ~-
			// name other directories, unknown too.
			if i+1 == len(cmd) || strings.IndexByte("/ \t\n;|&<>()", cmd[i+1]) >= 0 {
				cur.WriteString("${HOME}")
			} else {
				cur.WriteString(unresolvedSubst)
				for i+1 < len(cmd) && strings.IndexByte("/ \t\n;|&<>()'\"\\$`", cmd[i+1]) < 0 {
					i++ // the user name
				}
			}
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	flushSeg()
	return segs
}

// patternEnd returns the index of the unquoted ")" ending the patterns of a case item
// that start at cmd[i], or -1.
func patternEnd(cmd string, i int) int {
	for ; i < len(cmd); i++ {
		switch cmd[i] {
		case '\\':
			i++
		case '\'', '"':
			j := strings.IndexByte(cmd[i+1:], cmd[i])
			if j < 0 {
				return -1
			}
			i += j + 1
		case ')':
			return i
		}
	}
	return -1
}
