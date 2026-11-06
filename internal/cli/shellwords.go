package cli

// Reading shell command lines (workflow run: blocks, Makefile recipes) without running
// or expanding them.

import "strings"

// shellWords splits a shell command line into words the way the shell groups them,
// without expanding anything: quotes, $(...), ${...}, $((...)) and backquotes stay
// inside their word (so `$(go list ./... | grep -v x)` is one word), and the control
// operators (&&, ||, ;, |, &) and redirections (>, >>, <, 2>&1) are words of their own.
// A # starting a word ends the line.
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			if s[i+1] != '\n' {
				cur.WriteString(s[i : i+2])
			}
			i++
		case c == '\'' || c == '"' || c == '`' || (c == '$' && i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '{')):
			end := shellSpan(s, i)
			cur.WriteString(s[i:end])
			i = end - 1
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '#' && cur.Len() == 0:
			flush()
			return words
		case c == '>' || c == '<':
			// A redirection, with the descriptor before it (2>) and a duplicated one
			// after it (>&1).
			fd := cur.String()
			if strings.Trim(fd, "0123456789") != "" {
				flush()
				fd = ""
			}
			cur.Reset()
			op := fd + string(c)
			if i+1 < len(s) && (s[i+1] == c || s[i+1] == '&') {
				i++
				op += string(s[i])
				for i+1 < len(s) && (s[i+1] >= '0' && s[i+1] <= '9' || s[i+1] == '-') && op[len(op)-1] == '&' {
					i++
					op += string(s[i])
				}
			}
			words = append(words, op)
		case c == '&' || c == '|' || c == ';':
			flush()
			op := string(c)
			if c != ';' && i+1 < len(s) && s[i+1] == c {
				i++
				op += string(c)
			}
			words = append(words, op)
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return words
}

// shellSpan returns the end (exclusive) of the quoted string, backquoted command or
// $(...) / ${...} substitution starting at s[i], or len(s) when it is not closed.
func shellSpan(s string, i int) int {
	switch s[i] {
	case '\'':
		if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
			return i + 2 + j
		}
	case '`':
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '`':
				return j + 1
			}
		}
	case '"':
		for j := i + 1; j < len(s); j++ {
			switch {
			case s[j] == '\\':
				j++
			case s[j] == '"':
				return j + 1
			case s[j] == '`', s[j] == '$' && j+1 < len(s) && (s[j+1] == '(' || s[j+1] == '{'):
				j = shellSpan(s, j) - 1
			}
		}
	case '$':
		open, close := s[i+1], byte(')')
		if open == '{' {
			close = '}'
		}
		depth := 0
		for j := i + 1; j < len(s); j++ {
			switch c := s[j]; {
			case c == '\\':
				j++
			case c == '\'' || c == '"' || c == '`', c == '$' && j+1 < len(s) && (s[j+1] == '(' || s[j+1] == '{'):
				j = shellSpan(s, j) - 1
			case c == open:
				depth++
			case c == close:
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
	}
	return len(s)
}

// shellControlOp reports whether w is a control operator that ends a simple command.
func shellControlOp(w string) bool {
	switch w {
	case "&&", "||", ";", "|", "&", "|&":
		return true
	}
	return false
}

// shellRedirect reports whether w is a redirection operator.
func shellRedirect(w string) bool {
	t := strings.TrimLeft(w, "0123456789")
	return t != "" && (t[0] == '<' || t[0] == '>')
}

// shellLiteral returns the value of a word that expands to itself after quote removal
// (no $ expansion, command substitution or glob), and whether it does.
func shellLiteral(w string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		c := w[i]
		switch {
		case c == '\\' && i+1 < len(w):
			i++
			b.WriteByte(w[i])
		case c == '\'':
			end := shellSpan(w, i)
			if end == i+1 || w[end-1] != '\'' {
				return "", false
			}
			b.WriteString(w[i+1 : end-1])
			i = end - 1
		case c == '"':
			end := shellSpan(w, i)
			inner := w[i+1 : max(i+1, end-1)]
			if end == i+1 || w[end-1] != '"' || strings.ContainsAny(inner, "`") || shellExpands(inner) {
				return "", false
			}
			b.WriteString(strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`).Replace(inner))
			i = end - 1
		case c == '`', c == '*', c == '?', c == '[':
			return "", false
		case c == '$':
			if shellExpands(w[i:]) {
				return "", false
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// shellExpands reports whether s holds a $ that the shell expands ($x, ${x}, $(x),
// $1, $@ ...).
func shellExpands(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '$' && strings.ContainsRune("({_@*#?!-$0123456789", rune(s[i+1])) || s[i] == '$' && (s[i+1] >= 'a' && s[i+1] <= 'z' || s[i+1] >= 'A' && s[i+1] <= 'Z') {
			return true
		}
	}
	return false
}
