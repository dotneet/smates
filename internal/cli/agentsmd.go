package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ---- the section `smates init` suggests for AGENTS.md / CLAUDE.md ----

// The lines that delimit the section `smates init --agents-md` adds to AGENTS.md.
const (
	agentsBegin = "<!-- smates:begin (added by `smates init --agents-md`) -->"
	agentsEnd   = "<!-- smates:end -->"
)

// agentsSnippet is the section for AGENTS.md / CLAUDE.md.
const agentsSnippet = agentsBegin + `
## Tests

- After changing code, run ` + "`smates run --output failures`" + `: it runs only the tests your change can affect. Exit 0 passed, 1 tests failed, 2-4 a usage, configuration or environment problem.
- To fix failures, rerun the ` + "`smates run ... --only SUITE:UNIT`" + ` command its summary prints until it passes.
- ` + "`smates guide`" + ` explains the rest (orphans, ` + "`smates why <file>`" + `, long runs).
` + agentsEnd + "\n"

// agentsMDReport is what init did about the agent instructions file (`agents_md` in
// `smates init --format json`).
type agentsMDReport struct {
	// File is AGENTS.md or CLAUDE.md.
	File string `json:"file"`
	// Status: "added" (--agents-md wrote it), "present" (the file has the section),
	// "suggested" (not written: Snippet is the section to add).
	Status  string `json:"status"`
	Snippet string `json:"snippet,omitempty"`
}

// addAgentsMD suggests the smates section for the repository's agent instructions file
// (AGENTS.md, else CLAUDE.md), or appends it when write is set (creating AGENTS.md
// when neither exists). Without either file and without write it does nothing.
func (rep *initReport) addAgentsMD(root string, write bool) error {
	name := ""
	for _, n := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(root, n)); err == nil {
			name = n
			break
		}
	}
	if name == "" && !write {
		return nil
	}
	if name == "" {
		name = "AGENTS.md"
	}
	path := filepath.Join(root, name)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content := string(data)
	switch {
	case strings.Contains(content, agentsBegin):
		rep.AgentsMD = &agentsMDReport{File: name, Status: "present"}
	case !write:
		rep.AgentsMD = &agentsMDReport{File: name, Status: "suggested", Snippet: agentsSnippet}
	default:
		if content != "" {
			content = strings.TrimRight(content, "\n") + "\n\n"
		}
		if err := os.WriteFile(path, []byte(content+agentsSnippet), 0o644); err != nil {
			return err
		}
		rep.AgentsMD = &agentsMDReport{File: name, Status: "added"}
		rep.FilesWritten = append(rep.FilesWritten, name)
	}
	return nil
}

// print reports it in the text output of init (nil: nothing to say).
func (a *agentsMDReport) print(w io.Writer) {
	if a == nil {
		return
	}
	switch a.Status {
	case "added":
		fmt.Fprintf(w, "\nadded the smates section to %s\n", a.File)
	case "present":
		fmt.Fprintf(w, "\n%s already has the smates section\n", a.File)
	case "suggested":
		fmt.Fprintf(w, "\nFor coding agents, add this to %s (or run `smates init --agents-md`):\n\n%s", a.File, a.Snippet)
	}
}
