package cli

// `smates config`: safe edits of smates.yml (no hand-editing YAML).

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

// configEdit is the result of an edit of smates.yml (the JSON schema of `smates config
// ... --format json`, and of "applied" in `smates orphans --apply --format json`).
type configEdit struct {
	File string `json:"file"`
	Key  string `json:"key"`
	// Added and Present: add-ignore/add-input (and orphans --apply).
	Added   []string `json:"added,omitempty"`
	Present []string `json:"present,omitempty"`
	// Value and Previous: set.
	Value    *string `json:"value,omitempty"`
	Previous *string `json:"previous,omitempty"`
	Changed  bool    `json:"changed"`
	Written  bool    `json:"written"`
	DryRun   bool    `json:"dry_run"`
	Diff     string  `json:"diff,omitempty"`
}

// configFile returns the config file to edit: explicit, or the one at the repository
// root of dir.
func configFile(dir, explicit string) (abs, display string, err error) {
	if explicit != "" {
		return absPath(explicit), explicit, nil
	}
	if dir == "" {
		dir = "."
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		return "", "", asEnvError(err)
	}
	for _, n := range config.FileNames {
		p := filepath.Join(repo.Root, n)
		if _, err := os.Stat(p); err == nil {
			return p, n, nil
		}
	}
	return "", "", asConfigError(errors.New("no smates.yml at the repository root: run `smates init` first"))
}

// editConfig applies edit to the config file at path and writes the result unless
// dryRun (always computing the diff). An unreadable config or an edit it refuses
// (invalid config, invalid value) is a configuration error (exit 3), a failed write an
// environment error (exit 4).
func editConfig(path, display string, dryRun bool, res *configEdit, edit func([]byte) ([]byte, error)) error {
	// Edit the target of a symlinked config, keeping the link.
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return asConfigError(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return asConfigError(err)
	}
	out, err := edit(data)
	if err != nil {
		return asConfigError(fmt.Errorf("%s: %w", display, err))
	}
	res.File, res.DryRun = display, dryRun
	res.Changed = string(out) != string(data)
	if !res.Changed {
		return nil
	}
	res.Diff = unifiedDiff(filepath.ToSlash(display), string(data), string(out))
	if dryRun {
		return nil
	}
	if err := writeFileAtomic(path, out); err != nil {
		return asEnvError(err)
	}
	res.Written = true
	return nil
}

// writeFileAtomic replaces path with data, keeping its mode.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".smates-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// addGlobs adds globs to a top-level list of the config file.
func addGlobs(path, display, key string, globs []string, dryRun bool) (*configEdit, error) {
	res := &configEdit{Key: key, Added: []string{}, Present: []string{}}
	err := editConfig(path, display, dryRun, res, func(data []byte) ([]byte, error) {
		out, added, present, err := config.AddToList(data, key, globs)
		if err != nil {
			return nil, err
		}
		res.Added, res.Present = append(res.Added, added...), append(res.Present, present...)
		return out, nil
	})
	return res, err
}

const configCommands = `usage: smates config add-ignore <glob>...   add changes that never require tests (top-level ignore:)
       smates config add-input <glob>...    add files every test depends on (top-level inputs:)
       smates config set <key> <value>      set a scalar key (orphans, untested, state, agent.model, flaky.retry, ...)`

const configUsage = configCommands + `
flags: --dry-run (print the diff, write nothing), --format text|json, -C dir, --config file`

func cmdConfig(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError{errors.New(configUsage)}
	}
	sub, rest := args[0], args[1:]
	c := newCommand("config "+sub, stderr)
	dir := c.String("C", "", "run as if started in `dir`")
	file := c.String("config", "", "config file (default: smates.yml at the repository root)")
	dryRun := c.Bool("dry-run", false, "print the diff instead of writing")
	c.withFormat("text", "json")
	if isHelp(sub) {
		printGroupHelp(stdout, configCommands, c)
		return nil
	}
	pos, err := c.parseInterspersed(rest)
	if err != nil {
		return err
	}
	var res *configEdit
	switch sub {
	case "add-ignore", "add-input":
		if len(pos) == 0 {
			return usageError{fmt.Errorf("usage: smates config %s <glob>...", sub)}
		}
		key := "ignore"
		if sub == "add-input" {
			key = "inputs"
		}
		path, display, err := configFile(*dir, *file)
		if err != nil {
			return err
		}
		if res, err = addGlobs(path, display, key, pos, *dryRun); err != nil {
			return err
		}
	case "set":
		if len(pos) != 2 {
			return usageError{fmt.Errorf("usage: smates config set <key> <value> (keys: %s)", strings.Join(config.ScalarKeys(), ", "))}
		}
		path, display, err := configFile(*dir, *file)
		if err != nil {
			return err
		}
		res = &configEdit{Key: pos[0], Value: &pos[1]}
		err = editConfig(path, display, *dryRun, res, func(data []byte) ([]byte, error) {
			out, prev, err := config.SetScalar(data, pos[0], pos[1])
			if err == nil && prev != "" {
				res.Previous = &prev
			}
			return out, err
		})
		if err != nil {
			return err
		}
	default:
		return usageError{fmt.Errorf("unknown config command %q\n%s", sub, configUsage)}
	}
	return c.emit(stdout, res, func() { printConfigEdit(stdout, res) })
}

func printConfigEdit(w io.Writer, res *configEdit) {
	for _, g := range res.Present {
		fmt.Fprintf(w, "already in %s: %s\n", res.Key, g)
	}
	verb := "added to"
	if res.DryRun {
		verb = "would add to"
	}
	for _, g := range res.Added {
		fmt.Fprintf(w, "%s %s: %s\n", verb, res.Key, g)
	}
	if res.Value != nil && res.Changed {
		was := ""
		if res.Previous != nil {
			was = fmt.Sprintf(" (was %s)", *res.Previous)
		}
		verb := "set"
		if res.DryRun {
			verb = "would set"
		}
		fmt.Fprintf(w, "%s %s: %s%s\n", verb, res.Key, *res.Value, was)
	}
	switch {
	case !res.Changed:
		fmt.Fprintf(w, "%s: nothing to change\n", res.File)
	case res.DryRun:
		fmt.Fprintf(w, "\n%s(dry run: %s not changed)\n", res.Diff, res.File)
	}
}
