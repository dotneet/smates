package cli

// Workflows that run smates only on pull requests: no run on the default branch
// records the state their pull requests start from ("seeds" it).

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// seedCron is the schedule the patch adds: nightly, off the full hour most scheduled
// workflows use.
const seedCron = "23 3 * * *"

// seedKind says whether a workflow's runs record state that pull requests can start
// from.
type seedKind int

const (
	seedNo      seedKind = iota // pull requests (or manual runs) only
	seedYes                     // pushes to the default branch, or a schedule
	seedUnknown                 // started by other workflows (workflow_call, workflow_run): they decide
)

// workflowEvents returns the events of a workflow's on: (a string, a list or a
// mapping) with their settings (nil for the string and list forms), and the on: key
// and value nodes (nil when there is none).
func workflowEvents(top *yaml.Node) (map[string]*yaml.Node, *yaml.Node, *yaml.Node) {
	if top == nil || top.Kind != yaml.MappingNode {
		return nil, nil, nil
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		// YAML 1.1 reads an unquoted on as true; yaml.v3 keeps the text.
		if k := top.Content[i]; k.Value == "on" {
			v := top.Content[i+1]
			ev := map[string]*yaml.Node{}
			switch v.Kind {
			case yaml.ScalarNode:
				ev[v.Value] = nil
			case yaml.SequenceNode:
				for _, e := range v.Content {
					ev[e.Value] = nil
				}
			case yaml.MappingNode:
				for j := 0; j+1 < len(v.Content); j += 2 {
					ev[v.Content[j].Value] = v.Content[j+1]
				}
			}
			return ev, k, v
		}
	}
	return nil, nil, nil
}

// workflowSeed classifies a workflow's triggers (def: the default branch); pr says
// whether it runs on pull requests, push whether it has a push trigger at all.
func workflowSeed(top *yaml.Node, def string) (kind seedKind, pr, push bool) {
	ev, _, _ := workflowEvents(top)
	if ev == nil {
		return seedUnknown, false, false
	}
	_, pr = ev["pull_request"]
	if _, ok := ev["pull_request_target"]; ok {
		pr = true
	}
	cfg, push := ev["push"]
	switch {
	case push && pushReaches(cfg, def):
		return seedYes, pr, push
	case ev["schedule"] != nil || hasKey(ev, "schedule"):
		return seedYes, pr, push
	case hasKey(ev, "workflow_call") || hasKey(ev, "workflow_run") || hasKey(ev, "repository_dispatch"):
		return seedUnknown, pr, push
	}
	return seedNo, pr, push
}

func hasKey(m map[string]*yaml.Node, k string) bool {
	_, ok := m[k]
	return ok
}

// pushReaches reports whether a push trigger's branch filters let a push to branch
// def through (a filter smates cannot read lets it through).
func pushReaches(cfg *yaml.Node, def string) bool {
	if cfg == nil || cfg.Kind != yaml.MappingNode {
		return true
	}
	br, ign := mapGet(cfg, "branches"), mapGet(cfg, "branches-ignore")
	if br == nil && ign == nil {
		// With only tags filters, branch pushes do not trigger the workflow.
		return mapGet(cfg, "tags") == nil && mapGet(cfg, "tags-ignore") == nil
	}
	if br != nil {
		in := false
		for _, p := range seqValues(br) {
			if neg, ok := strings.CutPrefix(p, "!"); ok {
				if branchMatch(neg, def) {
					in = false
				}
			} else if branchMatch(p, def) {
				in = true
			}
		}
		return in
	}
	for _, p := range seqValues(ign) {
		if branchMatch(p, def) {
			return false
		}
	}
	return true
}

func seqValues(n *yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, e := range n.Content {
			out = append(out, e.Value)
		}
		return out
	}
	return nil
}

// branchMatch matches a branch against a GitHub filter pattern (* stops at /, **
// does not).
func branchMatch(pat, branch string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; {
		case c == '*' && i+1 < len(pat) && pat[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString(".?")
		case c == '+':
			b.WriteString("+")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(branch)
}

// flowFilter renders a path filter of an event (paths: or paths-ignore:, key) as one
// line, from its value v: a list of strings or one string; ok is false for anything
// else.
func flowFilter(key string, v *yaml.Node) (string, bool) {
	switch v.Kind {
	case yaml.ScalarNode:
		return key + ": [" + yamlFlowStr(v.Value) + "]", true
	case yaml.SequenceNode:
		var items []string
		for _, n := range v.Content {
			if n.Kind != yaml.ScalarNode {
				return "", false
			}
			items = append(items, yamlFlowStr(n.Value))
		}
		return key + ": [" + strings.Join(items, ", ") + "]", true
	}
	return "", false
}

// lineEdit replaces lines From..To (1-based, inclusive) of a file; To = From-1 inserts
// before line From.
type lineEdit struct {
	From, To int
	Lines    []string
}

// prOnlyContextRe finds references to context that exists only in pull request runs.
var prOnlyContextRe = regexp.MustCompile(`github\.event\.pull_request|github\.head_ref|github\.base_ref|event_name\s*==\s*'pull_request`)

// seedPatch returns the edit that adds a push trigger for the default branch (unless
// the workflow has one) and a nightly schedule to the on: of a workflow that runs only
// on pull requests, the notes to print about it, and false when no edit is needed or
// possible (a note then says why, when it is needed).
func seedPatch(file string, data []byte, def string) (lineEdit, []string, bool) {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
		return lineEdit{}, nil, false
	}
	top := doc.Content[0]
	kind, pr, hasPush := workflowSeed(top, def)
	if kind != seedNo || !pr {
		return lineEdit{}, nil, false
	}
	lines := strings.Split(string(data), "\n")
	ev, key, val := workflowEvents(top)
	notes := []string{file + " runs only on pull requests: no run on the default branch records the state, so the first run of every pull request starts cold (it runs everything without full history)"}
	if val.Kind == yaml.MappingNode && val.Style&yaml.FlowStyle != 0 || val.Kind == yaml.SequenceNode && slices.ContainsFunc(val.Content, func(n *yaml.Node) bool { return n.Kind != yaml.ScalarNode }) {
		notes[0] += "; add `push: branches: [" + def + "]` and a nightly `schedule:` to its on: by hand"
		return lineEdit{}, notes, false
	}
	// The on: block ends before the next top-level key (or the end of the file).
	end := len(lines)
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i] == key && i+2 < len(top.Content) {
			end = top.Content[i+2].Line - 1
		}
	}
	for end > key.Line && (strings.TrimSpace(lines[end-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#")) {
		end--
	}
	keyInd := strings.Repeat(" ", key.Column-1)
	evInd := keyInd + "  "
	if val.Kind == yaml.MappingNode && len(val.Content) > 0 {
		evInd = strings.Repeat(" ", val.Content[0].Column-1)
	}
	subInd := evInd + "  "
	for j := 1; j < len(val.Content); j += 2 {
		if c := val.Content[j]; c.Kind == yaml.MappingNode && len(c.Content) > 0 && c.Style&yaml.FlowStyle == 0 {
			subInd = strings.Repeat(" ", c.Content[0].Column-1)
			break
		}
	}
	var add []string
	if !hasPush {
		add = append(add, evInd+"push:   # runs on the default branch record the state pull requests start from",
			subInd+"branches: ["+yamlFlowStr(def)+"]")
		// The pull request's path filters, so that pushes run when pull requests would.
		if prCfg := ev["pull_request"]; prCfg != nil && prCfg.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(prCfg.Content); j += 2 {
				k := prCfg.Content[j]
				if k.Value != "paths" && k.Value != "paths-ignore" {
					continue
				}
				if prCfg.Style&yaml.FlowStyle != 0 {
					// pull_request: {paths: [...]}: its lines hold the event key too, so
					// the filter is written from the nodes.
					l, ok := flowFilter(k.Value, prCfg.Content[j+1])
					if !ok {
						notes[0] += "; add `push: branches: [" + def + "]` with the pull request's " + k.Value + " and a nightly `schedule:` to its on: by hand"
						return lineEdit{}, notes, false
					}
					add = append(add, subInd+l)
					continue
				}
				from, to := k.Line, end
				if j+2 < len(prCfg.Content) {
					to = prCfg.Content[j+2].Line - 1
				} else {
					to = eventEnd(val, prCfg, end)
				}
				for to > from && strings.TrimSpace(lines[to-1]) == "" {
					to--
				}
				ind := strings.Repeat(" ", k.Column-1)
				for _, l := range lines[from-1 : to] {
					// Re-indent from the pull request's settings to push's.
					if strings.HasPrefix(l, ind) {
						l = subInd + l[len(ind):]
					}
					add = append(add, l)
				}
			}
		}
	} else {
		notes = append(notes, file+": its push trigger never runs on "+def+": add "+def+" to its branches if pushes there should refresh the state")
	}
	add = append(add, evInd+"schedule:   # nightly full run on the default branch (see the smates docs on --shadow)",
		subInd+"- cron: '"+seedCron+"'")
	notes[0] += "; the patch adds "
	if !hasPush {
		notes[0] += "push to " + def + " and "
	}
	notes[0] += "a nightly schedule to its on: (keep just the schedule if a full run per merge costs too much: the state is then at most a day old)"
	if prOnlyContextRe.Match(data) {
		notes = append(notes, file+" refers to pull-request-only context (github.event.pull_request, github.head_ref, event_name checks): make sure its jobs also run on push and schedule")
	}
	if val.Kind == yaml.MappingNode {
		return lineEdit{From: end + 1, To: end, Lines: add}, notes, true
	}
	// on: pull_request or on: [pull_request, ...]: rewritten as a mapping.
	repl := []string{keyInd + "on:"}
	for _, n := range sortedEvents(val) {
		repl = append(repl, evInd+n+":")
	}
	return lineEdit{From: key.Line, To: end, Lines: append(repl, add...)}, notes, true
}

// eventEnd is the last line of an event's settings within the on: mapping val that
// ends at line end.
func eventEnd(val, cfg *yaml.Node, end int) int {
	for j := 0; j+1 < len(val.Content); j += 2 {
		if val.Content[j+1] == cfg && j+2 < len(val.Content) {
			return val.Content[j+2].Line - 1
		}
	}
	return end
}

// sortedEvents lists the events of a string or list on: in their order.
func sortedEvents(val *yaml.Node) []string {
	if val.Kind == yaml.ScalarNode {
		return []string{val.Value}
	}
	var out []string
	for _, e := range val.Content {
		out = append(out, e.Value)
	}
	return out
}

// seedGap is a workflow that runs smates only on pull requests while no workflow
// running smates on the default branch saves state under the same cache key.
type seedGap struct {
	File string
	Keys []string // the cache keys left without a seed ("" for the default)
}

// smatesSeedGaps finds the workflows of .github/workflows that run smates only on pull
// requests with nothing seeding their state.
// smatesWorkflows lists the workflows (repo-relative) with a step using the smates
// action.
func smatesWorkflows(root string) []string {
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
			continue
		}
		if len(smatesCacheKeys(doc.Content[0])) > 0 {
			out = append(out, ".github/workflows/"+filepath.Base(f))
		}
	}
	return out
}

// existingSmatesSteps counts the smates steps the workflows of .github/workflows
// already have (action steps and run: steps calling smates) and returns the cache-key
// inputs their action steps set.
func existingSmatesSteps(root string) (n int, keys []string) {
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
			continue
		}
		jobs := mapGet(doc.Content[0], "jobs")
		if jobs == nil || jobs.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			steps := mapGet(jobs.Content[i+1], "steps")
			if steps == nil || steps.Kind != yaml.SequenceNode {
				continue
			}
			for _, st := range steps.Content {
				if u := mapGet(st, "uses"); u != nil && strings.HasPrefix(u.Value, "dotneet/smates@") {
					n++
					if k := mapGet(mapGet(st, "with"), "cache-key"); k != nil && k.Value != "" {
						keys = append(keys, k.Value)
					}
				} else if r := mapGet(st, "run"); r != nil && smatesRunRe.MatchString(r.Value) {
					n++
				}
			}
		}
	}
	return n, keys
}

func smatesSeedGaps(root, def string) []seedGap {
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	type use struct {
		file string
		kind seedKind
		pr   bool
		keys []string
	}
	var uses []use
	seeded := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
			continue
		}
		top := doc.Content[0]
		keys := smatesCacheKeys(top)
		if len(keys) == 0 {
			continue
		}
		kind, pr, _ := workflowSeed(top, def)
		u := use{file: ".github/workflows/" + filepath.Base(f), kind: kind, pr: pr, keys: keys}
		uses = append(uses, u)
		if kind != seedNo {
			for _, k := range keys {
				seeded[k] = true
			}
		}
	}
	var out []seedGap
	for _, u := range uses {
		if u.kind != seedNo || !u.pr {
			continue
		}
		g := seedGap{File: u.file}
		for _, k := range u.keys {
			if !seeded[k] {
				g.Keys = append(g.Keys, k)
			}
		}
		if len(g.Keys) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// smatesCacheKeys returns the state cache keys of the smates steps of a workflow: the
// action's cache-key input (sorted, unique; "" for none), "(own storage)" for an
// action step with cache: false, "(state pull)" for a run: step calling smates.
func smatesCacheKeys(top *yaml.Node) []string {
	var keys []string
	jobs := mapGet(top, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		steps := mapGet(jobs.Content[i+1], "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for _, st := range steps.Content {
			if u := mapGet(st, "uses"); u != nil && strings.HasPrefix(u.Value, "dotneet/smates@") {
				with := mapGet(st, "with")
				key := ""
				if k := mapGet(with, "cache-key"); k != nil {
					key = k.Value
				}
				if c := mapGet(with, "cache"); c != nil && strings.TrimSpace(c.Value) == "false" {
					key = "(own storage)"
				}
				keys = append(keys, key)
			} else if r := mapGet(st, "run"); r != nil && smatesRunRe.MatchString(r.Value) {
				keys = append(keys, "(state pull)")
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

var smatesRunRe = regexp.MustCompile(`(?m)(?:^|[\s;&|(])smates\s+(?:run|select)\b`)
