package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Editing Claude Code's settings.json byte-carefully: smates splices its Stop hook
// entry into the file's text and removes exactly that text again, so everything else
// (key order, formatting, other hooks, keys smates does not know) stays as it was, byte
// for byte. Install followed by uninstall restores the original file.

// jsonSpan locates a value in a JSON document: [start, end) are its bytes; keyStart is
// where the member's key starts (objects) or start (arrays).
type jsonSpan struct {
	key                  string
	keyStart, start, end int
}

// jsonChildren returns the members of the object, or the elements of the array, at
// data[start:end].
func jsonChildren(data []byte, start, end int) ([]jsonSpan, error) {
	dec := json.NewDecoder(bytes.NewReader(data[start:end]))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	object := t == json.Delim('{')
	if !object && t != json.Delim('[') {
		return nil, errors.New("not an object or array")
	}
	var out []jsonSpan
	for dec.More() {
		var sp jsonSpan
		if object {
			off := start + int(dec.InputOffset())
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			sp.key, _ = kt.(string)
			sp.keyStart = off + bytes.IndexByte(data[off:end], '"')
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		sp.end = start + int(dec.InputOffset())
		sp.start = sp.end - len(raw)
		if !object {
			sp.keyStart = sp.start
		}
		out = append(out, sp)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func findKey(spans []jsonSpan, key string) (jsonSpan, bool) {
	for _, s := range spans {
		if s.key == key {
			return s, true
		}
	}
	return jsonSpan{}, false
}

func isKind(data []byte, sp jsonSpan, open byte) bool {
	return sp.end > sp.start && data[sp.start] == open
}

// lineIndent is the whitespace that starts the line containing offset i.
func lineIndent(data []byte, i int) string {
	ls := bytes.LastIndexByte(data[:i], '\n') + 1
	j := ls
	for j < len(data) && (data[j] == ' ' || data[j] == '\t') {
		j++
	}
	return string(data[ls:j])
}

// marshalIndented renders v as JSON indented by unit, its lines after the first
// prefixed with prefix, without HTML escaping (commands contain > and &).
func marshalIndented(v any, prefix, unit string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, unit)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// claudeDoc is a settings.json being edited.
type claudeDoc struct {
	data []byte
	unit string // indentation unit
}

// smatesEntry is a smates Stop hook entry with the spans needed to edit or remove it.
type smatesEntry struct {
	command  string
	cmdSpan  jsonSpan   // the "command" value
	entry    jsonSpan   // the hook entry in its group's "hooks" array
	group    jsonSpan   // the group in "Stop"
	entries  []jsonSpan // the group's entries
	groupArr jsonSpan   // the group's "hooks" array
}

// stop returns the root members, the "hooks" member and the "Stop" member (ok false
// when absent).
func (d *claudeDoc) stop() (root []jsonSpan, hooks, stop jsonSpan, hasHooks, hasStop bool, err error) {
	s, e := rootBounds(d.data)
	root, err = jsonChildren(d.data, s, e)
	if err != nil {
		return
	}
	if hooks, hasHooks = findKey(root, "hooks"); !hasHooks || !isKind(d.data, hooks, '{') {
		hasHooks = false
		return
	}
	members, err := jsonChildren(d.data, hooks.start, hooks.end)
	if err != nil {
		return
	}
	stop, hasStop = findKey(members, "Stop")
	hasStop = hasStop && isKind(d.data, stop, '[')
	return
}

// entries lists smates' Stop hook entries.
func (d *claudeDoc) entries() ([]smatesEntry, error) {
	if len(bytes.TrimSpace(d.data)) == 0 {
		return nil, nil
	}
	_, _, stop, _, hasStop, err := d.stop()
	if err != nil || !hasStop {
		return nil, err
	}
	groups, err := jsonChildren(d.data, stop.start, stop.end)
	if err != nil {
		return nil, err
	}
	var out []smatesEntry
	for _, g := range groups {
		if !isKind(d.data, g, '{') {
			continue
		}
		gm, err := jsonChildren(d.data, g.start, g.end)
		if err != nil {
			return nil, err
		}
		arr, ok := findKey(gm, "hooks")
		if !ok || !isKind(d.data, arr, '[') {
			continue
		}
		entries, err := jsonChildren(d.data, arr.start, arr.end)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !isKind(d.data, e, '{') {
				continue
			}
			em, err := jsonChildren(d.data, e.start, e.end)
			if err != nil {
				return nil, err
			}
			c, ok := findKey(em, "command")
			var cmd string
			if !ok || json.Unmarshal(d.data[c.start:c.end], &cmd) != nil || !strings.Contains(cmd, claudeHookMarker) {
				continue
			}
			out = append(out, smatesEntry{command: cmd, cmdSpan: c, entry: e, group: g, entries: entries, groupArr: arr})
		}
	}
	return out, nil
}

func (d *claudeDoc) splice(start, end int, text []byte) {
	d.data = append(d.data[:start:start], append(text, d.data[end:]...)...)
}

// insert adds a member (key != "") or an element at the end of the container at
// [cstart, cend).
func (d *claudeDoc) insert(cstart, cend int, key string, v any) error {
	children, err := jsonChildren(d.data, cstart, cend)
	if err != nil {
		return err
	}
	member := func(text []byte) []byte {
		if key == "" {
			return text
		}
		k, _ := json.Marshal(key)
		return append(append(k, ": "...), text...)
	}
	if n := len(children); n > 0 {
		at := children[n-1].end
		if !bytes.ContainsRune(d.data[cstart:children[0].keyStart], '\n') {
			// The children are on one line: so is the new one.
			d.splice(at, at, append([]byte(", "), member(marshalIndented(v, "", ""))...))
			return nil
		}
		indent := lineIndent(d.data, children[n-1].keyStart)
		d.splice(at, at, append([]byte(",\n"+indent), member(marshalIndented(v, indent, d.unit))...))
		return nil
	}
	indent := lineIndent(d.data, cstart) + d.unit
	text := member(marshalIndented(v, indent, d.unit))
	// Empty container: put the child on its own line.
	closeIndent := lineIndent(d.data, cstart)
	d.splice(cstart+1, cend-1, append(append([]byte("\n"+indent), text...), "\n"+closeIndent...))
	return nil
}

// lineOffset is the distance from the start of i's line to i.
func lineOffset(data []byte, i int) int {
	return i - (bytes.LastIndexByte(data[:i], '\n') + 1)
}

// remove deletes child i of the container at [cstart, cend), with its separator.
func (d *claudeDoc) remove(cstart, cend int, children []jsonSpan, i int) {
	switch {
	case len(children) == 1:
		d.splice(cstart+1, cend-1, nil)
	case i > 0:
		d.splice(children[i-1].end, children[i].end, nil)
	default:
		d.splice(children[0].keyStart, children[1].keyStart, nil)
	}
}

func indexOf(spans []jsonSpan, sp jsonSpan) int {
	for i, s := range spans {
		if s.start == sp.start {
			return i
		}
	}
	return -1
}

// removeEntry removes a smates entry: the entry, or its group when that is all the group
// holds, or the "Stop" key, or the "hooks" key, whichever is the largest thing that
// holds nothing else.
func (d *claudeDoc) removeEntry(e smatesEntry) error {
	if len(e.entries) > 1 {
		d.remove(e.groupArr.start, e.groupArr.end, e.entries, indexOf(e.entries, e.entry))
		return nil
	}
	root, hooks, stop, _, _, err := d.stop()
	if err != nil {
		return err
	}
	groups, err := jsonChildren(d.data, stop.start, stop.end)
	if err != nil {
		return err
	}
	if len(groups) > 1 {
		d.remove(stop.start, stop.end, groups, indexOf(groups, e.group))
		return nil
	}
	members, err := jsonChildren(d.data, hooks.start, hooks.end)
	if err != nil {
		return err
	}
	if len(members) > 1 {
		d.remove(hooks.start, hooks.end, members, indexOf(members, stop))
		return nil
	}
	s, end := rootBounds(d.data)
	d.remove(s, end, root, indexOf(root, hooks))
	return nil
}

// rootBounds trims the whitespace around the root object.
func rootBounds(data []byte) (int, int) {
	start := len(data) - len(bytes.TrimLeft(data, " \t\r\n"))
	end := len(bytes.TrimRight(data, " \t\r\n"))
	return start, end
}

func readClaudeDoc(path string) (*claudeDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	d := &claudeDoc{data: data, unit: "  "}
	if len(bytes.TrimSpace(data)) == 0 {
		return d, nil
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("%s is not valid JSON; fix it first", path)
	}
	s, e := rootBounds(data)
	if data[s] != '{' {
		return nil, fmt.Errorf("%s: not a JSON object", path)
	}
	root, err := jsonChildren(data, s, e)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(root) > 0 && lineOffset(data, root[0].keyStart) > 0 && bytes.ContainsRune(data[s:root[0].keyStart], '\n') {
		d.unit = lineIndent(data, root[0].keyStart)
	}
	return d, nil
}

func (d *claudeDoc) write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, d.data, 0o644)
}

func installClaudeHook(path string, command string, stdout io.Writer) error {
	d, err := readClaudeDoc(path)
	if err != nil {
		return err
	}
	entries, err := d.entries()
	if err != nil {
		return err
	}
	if len(entries) == 1 && entries[0].command == command {
		fmt.Fprintf(stdout, "smates Claude Code Stop hook already installed in %s\n", path)
		return nil
	}
	entry := struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}{"command", command, claudeHookTimeout}
	group := map[string]any{"hooks": []any{entry}}
	switch {
	case len(entries) > 0:
		// Update the first entry's command in place; drop duplicates.
		cmd := marshalIndented(command, "", "")
		d.splice(entries[0].cmdSpan.start, entries[0].cmdSpan.end, cmd)
		for {
			es, err := d.entries()
			if err != nil {
				return err
			}
			if len(es) <= 1 {
				break
			}
			if err := d.removeEntry(es[len(es)-1]); err != nil {
				return err
			}
		}
	case len(bytes.TrimSpace(d.data)) == 0:
		d.data = append(marshalIndented(map[string]any{"hooks": map[string]any{"Stop": []any{group}}}, "", d.unit), '\n')
	default:
		_, hooks, stop, hasHooks, hasStop, err := d.stop()
		if err != nil {
			return err
		}
		s, e := rootBounds(d.data)
		switch {
		case hasStop:
			err = d.insert(stop.start, stop.end, "", group)
		case hasHooks:
			err = d.insert(hooks.start, hooks.end, "Stop", []any{group})
		default:
			err = d.insert(s, e, "hooks", map[string]any{"Stop": []any{group}})
		}
		if err != nil {
			return err
		}
	}
	if !json.Valid(d.data) {
		return fmt.Errorf("internal error: editing %s produced invalid JSON; left unchanged", path)
	}
	if err := d.write(path); err != nil {
		return err
	}
	verb := "installed"
	if len(entries) > 0 {
		verb = "updated"
	}
	fmt.Fprintf(stdout, "%s the smates Claude Code Stop hook in %s\n"+
		"When Claude ends its turn, the tests its changes can affect run; failures make it continue and fix them.\n", verb, path)
	return nil
}

func uninstallClaudeHook(path string, stdout io.Writer) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "no Claude Code settings at %s\n", path)
		return nil
	}
	d, err := readClaudeDoc(path)
	if err != nil {
		return err
	}
	removed := false
	for {
		es, err := d.entries()
		if err != nil {
			return err
		}
		if len(es) == 0 {
			break
		}
		if err := d.removeEntry(es[0]); err != nil {
			return err
		}
		removed = true
	}
	if !removed {
		fmt.Fprintf(stdout, "no smates Stop hook in %s\n", path)
		return nil
	}
	if !json.Valid(d.data) {
		return fmt.Errorf("internal error: editing %s produced invalid JSON; left unchanged", path)
	}
	if err := d.write(path); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed the smates Stop hook from %s (other settings are kept)\n", path)
	return nil
}

// claudeHookInstalled reports whether the settings file has a smates Stop hook.
func claudeHookInstalled(path string) (bool, error) {
	d, err := readClaudeDoc(path)
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(d.data)) == 0 {
		return false, nil
	}
	es, err := d.entries()
	return len(es) > 0, err
}
