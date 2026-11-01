package cli

import (
	"strconv"
	"strings"
)

// nodeSatisfies reports whether a Node version ("v20.11.1") satisfies an npm semver
// range as found in package.json engines.node (">=18", "^20 || ^22", "18.x",
// "20 - 22"). known is false when the range or the version cannot be parsed; callers
// then stay silent rather than guess.
func nodeSatisfies(version, rng string) (ok, known bool) {
	v, n := parsePartial(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	if n < 3 {
		return false, false
	}
	rng = strings.TrimSpace(rng)
	if rng == "" {
		return false, false
	}
	for _, alt := range strings.Split(rng, "||") {
		cs, good := parseRange(strings.TrimSpace(alt))
		if !good {
			return false, false
		}
		all := true
		for _, c := range cs {
			if !c.match(v) {
				all = false
				break
			}
		}
		if all {
			ok = true
		}
	}
	return ok, true
}

type semverCmp struct {
	op string // ">=", "<", ">", "<=", "="
	v  [3]int
}

func (c semverCmp) match(v [3]int) bool {
	d := cmpVersion(v, c.v)
	switch c.op {
	case ">=":
		return d >= 0
	case ">":
		return d > 0
	case "<":
		return d < 0
	case "<=":
		return d <= 0
	}
	return d == 0
}

func cmpVersion(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parsePartial parses "1", "1.2", "1.2.3", "1.x", "*" (pre-release and build suffixes
// dropped) and returns how many leading components were numbers.
func parsePartial(s string) ([3]int, int) {
	var v [3]int
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" || s == "*" || s == "x" || s == "X" {
		return v, 0
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return v, -1
	}
	n := 0
	for i, p := range parts {
		if p == "x" || p == "X" || p == "*" {
			break
		}
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return v, -1
		}
		v[i] = x
		n = i + 1
	}
	return v, n
}

// upper returns the exclusive upper bound of a partial version with n components.
func upper(v [3]int, n int) [3]int {
	switch n {
	case 1:
		return [3]int{v[0] + 1, 0, 0}
	case 2:
		return [3]int{v[0], v[1] + 1, 0}
	}
	return [3]int{v[0], v[1], v[2] + 1}
}

func parseRange(s string) ([]semverCmp, bool) {
	if s == "" || s == "*" || s == "x" {
		return nil, true
	}
	if a, b, found := strings.Cut(s, " - "); found {
		lo, n1 := parsePartial(strings.TrimSpace(a))
		hi, n2 := parsePartial(strings.TrimSpace(b))
		if n1 < 0 || n2 < 0 {
			return nil, false
		}
		cs := []semverCmp{{">=", lo}}
		if n2 == 3 {
			cs = append(cs, semverCmp{"<=", hi})
		} else if n2 > 0 {
			cs = append(cs, semverCmp{"<", upper(hi, n2)})
		}
		return cs, true
	}
	// Join operators separated from their version (">= 18").
	var toks []string
	for _, f := range strings.Fields(s) {
		if len(toks) > 0 && strings.Trim(toks[len(toks)-1], "<>=~^") == "" {
			toks[len(toks)-1] += f
			continue
		}
		toks = append(toks, f)
	}
	var out []semverCmp
	for _, t := range toks {
		op := ""
		for _, p := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
			if strings.HasPrefix(t, p) {
				op, t = p, t[len(p):]
				break
			}
		}
		v, n := parsePartial(strings.TrimPrefix(t, "v"))
		if n < 0 {
			return nil, false
		}
		switch op {
		case "", "=":
			if n == 0 {
				continue
			}
			if n == 3 {
				out = append(out, semverCmp{"=", v})
			} else {
				out = append(out, semverCmp{">=", v}, semverCmp{"<", upper(v, n)})
			}
		case "^":
			hi := upper(v, n)
			switch {
			case n == 0:
				continue
			case v[0] > 0 || n == 1:
				hi = [3]int{v[0] + 1, 0, 0}
			case v[1] > 0 || n == 2:
				hi = [3]int{0, v[1] + 1, 0}
			}
			out = append(out, semverCmp{">=", v}, semverCmp{"<", hi})
		case "~":
			if n == 0 {
				continue
			}
			m := n
			if m == 3 {
				m = 2
			}
			out = append(out, semverCmp{">=", v}, semverCmp{"<", upper(v, m)})
		case ">=", "<":
			if n == 0 {
				return nil, false
			}
			out = append(out, semverCmp{op, v})
		case ">":
			if n == 0 {
				return nil, false
			}
			if n < 3 {
				out = append(out, semverCmp{">=", upper(v, n)})
			} else {
				out = append(out, semverCmp{">", v})
			}
		case "<=":
			if n == 0 {
				return nil, false
			}
			if n < 3 {
				out = append(out, semverCmp{"<", upper(v, n)})
			} else {
				out = append(out, semverCmp{"<=", v})
			}
		}
	}
	return out, true
}
