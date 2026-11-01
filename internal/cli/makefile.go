package cli

// Makefile test targets whose flags or variables smates' default command would drop.

import (
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dotneet/smates/internal/config"
)

// ---- Makefile ----

// makeHint is a Makefile test target whose flags or environment smates' own command
// for the suite would not pass.
type makeHint struct {
	Suite   string
	File    string // repo-relative Makefile
	Target  string
	Line    string // the recipe line
	Command string // the runner invocation without positional package/path arguments
	Env     [][2]string
}

var makeTargetRe = regexp.MustCompile(`^([A-Za-z0-9_.%/-]+(?:\s+[A-Za-z0-9_.%/-]+)*)\s*::?(?:[^=]|$)`)

// makefileHints reads the test and test-* targets of the Makefile at the repository
// root and at each suite root.
func makefileHints(root string, suites []config.Suite) []makeHint {
	dirs := []string{"."}
	for _, s := range suites {
		if !slices.Contains(dirs, s.Root) {
			dirs = append(dirs, s.Root)
		}
	}
	var out []makeHint
	seen := map[string]bool{}
	for _, d := range dirs {
		for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
			rel := joinRel(d, name)
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				continue
			}
			recipes := makeTestRecipes(string(data))
			targets := slices.Sorted(maps.Keys(recipes)) // "test" before "test-*"
			for _, target := range targets {
				for _, line := range recipes[target] {
					h, ok := parseRecipeLine(line, d, suites)
					if !ok || seen[h.Suite] {
						continue
					}
					seen[h.Suite] = true
					h.File, h.Target, h.Line = rel, target, line
					out = append(out, h)
				}
			}
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Suite < out[j].Suite })
	return out
}

// makeTestRecipes returns the recipe lines (continuations joined) of the test and
// test-* targets, in order of the targets' names.
func makeTestRecipes(mk string) map[string][]string {
	out := map[string][]string{}
	var cur []string
	var raw []string
	for _, l := range strings.Split(mk, "\n") {
		raw = append(raw, strings.TrimRight(l, "\r"))
	}
	for i := 0; i < len(raw); i++ {
		l := raw[i]
		if strings.HasPrefix(l, "\t") {
			if cur == nil {
				continue
			}
			for strings.HasSuffix(l, "\\") && i+1 < len(raw) {
				i++
				l = strings.TrimSuffix(l, "\\") + " " + strings.TrimSpace(raw[i])
			}
			t := strings.TrimLeft(strings.TrimSpace(l), "@-+")
			for _, c := range cur {
				out[c] = append(out[c], strings.TrimSpace(t))
			}
			continue
		}
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		cur = nil
		if m := makeTargetRe.FindStringSubmatch(l); m != nil {
			for _, t := range strings.Fields(m[1]) {
				if t == "test" || strings.HasPrefix(t, "test-") || strings.HasPrefix(t, "test_") {
					cur = append(cur, t)
				}
			}
		}
	}
	return out
}

// parseRecipeLine finds a direct runner invocation with flags or environment in a
// recipe line run from directory dir.
func parseRecipeLine(line, dir string, suites []config.Suite) (makeHint, bool) {
	var env [][2]string
	cd := dir
	for _, seg := range shellSepRe.Split(line, -1) {
		words := strings.Fields(seg)
		if len(words) >= 2 && words[0] == "cd" {
			cd = path.Clean(joinRel(dir, words[1]))
			continue
		}
		if len(words) > 0 && words[0] == "export" {
			words = words[1:]
		}
		var local [][2]string
		for len(words) > 0 && strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "-") {
			k, v, _ := strings.Cut(words[0], "=")
			local = append(local, [2]string{k, strings.Trim(v, `"'`)})
			words = words[1:]
		}
		runner, at := "", -1
		for i, w := range words {
			base := path.Base(w)
			switch {
			case (base == "go" || w == "$(GO)" || w == "${GO}") && i+1 < len(words) && words[i+1] == "test":
				runner, at = "go", i+2
			case base == "bun" && i+1 < len(words) && words[i+1] == "test":
				runner, at = "bun", i+2
			case base == "vitest" || base == "jest" || base == "pytest":
				runner, at = base, i+1
			}
			if runner != "" {
				break
			}
		}
		if runner == "" {
			if len(words) == 0 { // only assignments: "export TZ=UTC" for the next commands
				env = append(env, local...)
			}
			continue
		}
		env = append(env, local...)
		cmd := slices.Clone(words[:at])
		flags := 0
		for _, w := range words[at:] {
			if strings.HasPrefix(w, "./") || strings.HasPrefix(w, "../") || strings.Contains(w, "...") || w == "." {
				continue // packages / test paths: smates passes its own
			}
			if strings.HasPrefix(w, "-") {
				flags++
			}
			cmd = append(cmd, w)
		}
		if runner == "vitest" && len(cmd) == at && at == 1 {
			cmd = append(cmd, "run")
		}
		if flags == 0 && len(env) == 0 {
			return makeHint{}, false
		}
		suite := ""
		for _, s := range suites {
			if s.Runner == runner && s.Root == cd {
				suite = s.Name
				break
			}
			if s.Runner == runner && suite == "" {
				suite = s.Name
			}
		}
		if suite == "" {
			return makeHint{}, false
		}
		return makeHint{Suite: suite, Command: strings.Join(cmd, " "), Env: env}, true
	}
	return makeHint{}, false
}

// configHints renders the commented smates.yml lines for each Makefile hint, per suite.
func configHints(hints []makeHint) map[string][]string {
	out := map[string][]string{}
	for _, h := range hints {
		lines := []string{
			fmt.Sprintf("# %s target %q runs: %s", h.File, h.Target, h.Line),
			"# smates runs the runner itself; to keep those flags/variables, uncomment:",
			"# command: " + strconv.Quote(h.Command),
		}
		if len(h.Env) > 0 {
			lines = append(lines, "# env:")
			for _, kv := range h.Env {
				lines = append(lines, "#   "+kv[0]+": "+yamlStr(kv[1]))
			}
		}
		out[h.Suite] = lines
	}
	return out
}
