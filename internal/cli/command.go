package cli

// Plumbing shared by the commands: the flag set with the common flags and --format,
// argument checks, and the text or JSON output of a command's report.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// command is the flag set of a command. Its parse methods return usage errors (exit
// code 2).
type command struct {
	*flag.FlagSet
	// format is --format (nil without withFormat), one of formats.
	format  *string
	formats []string
	// bareFormatError leaves the accepted formats out of the error for an unknown
	// --format (the message flaky and stats have always printed).
	bareFormatError bool
	// jsonAlias is the deprecated --json (nil without deprecatedJSON).
	jsonAlias *bool
}

// newCommand is the flag set of the command name ("list", "state show", ...); flag
// errors and -h print to stderr.
func newCommand(name string, stderr io.Writer) *command {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return &command{FlagSet: fs}
}

// common registers the flags of commands that load the project (-C, --config, --state,
// --suite, -v).
func (c *command) common(cf *commonFlags) {
	cf.register(c.FlagSet)
}

// withFormat registers --format, which takes one of formats; the first is the default.
func (c *command) withFormat(formats ...string) {
	c.formats = formats
	c.format = c.String("format", formats[0], "output: "+strings.Join(formats, ", "))
}

// deprecatedJSON registers --json, the deprecated spelling of --format json.
func (c *command) deprecatedJSON(usage string) {
	c.jsonAlias = c.Bool("json", false, usage)
}

// parse parses args and checks --format.
func (c *command) parse(args []string) error {
	if err := c.Parse(args); err != nil {
		return usageError{err}
	}
	return c.checkFormat()
}

// parseInterspersed is parse for flags that may come after positional arguments
// (`smates config add-ignore 'docs/**' --dry-run`); "--" ends the flags. It returns
// the positional arguments.
func (c *command) parseInterspersed(args []string) ([]string, error) {
	var pos []string
	for {
		if err := c.Parse(args); err != nil {
			return nil, usageError{err}
		}
		rest := c.Args()
		if len(rest) == 0 {
			break
		}
		if i := len(args) - len(rest) - 1; i >= 0 && args[i] == "--" {
			pos = append(pos, rest...) // everything after "--" is positional
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return pos, c.checkFormat()
}

// checkFormat rejects an unknown --format, then applies the deprecated --json.
func (c *command) checkFormat() error {
	if c.format == nil {
		return nil
	}
	if !slices.Contains(c.formats, *c.format) {
		if c.bareFormatError {
			return usageError{fmt.Errorf("unknown format %q", *c.format)}
		}
		return usageError{fmt.Errorf("unknown format %q (want %s)", *c.format, strings.Join(c.formats, " or "))}
	}
	if c.jsonAlias != nil && *c.jsonAlias {
		*c.format = "json"
	}
	return nil
}

// asJSON reports whether the command prints JSON (--format json).
func (c *command) asJSON() bool {
	return c.format != nil && *c.format == "json"
}

// emit prints the command's report: as JSON with --format json, else with text.
func (c *command) emit(w io.Writer, report any, text func()) error {
	if c.asJSON() {
		return writeJSON(w, report)
	}
	text()
	return nil
}

// noArgs rejects positional arguments.
func (c *command) noArgs() error {
	if c.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected arguments %v", c.Args())}
	}
	return nil
}

// oneArg returns the single positional argument; usage is the error otherwise.
func (c *command) oneArg(usage string) (string, error) {
	if c.NArg() != 1 {
		return "", usageError{errors.New(usage)}
	}
	return c.Arg(0), nil
}

// writeJSON writes v as indented JSON (HTML characters unescaped).
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
