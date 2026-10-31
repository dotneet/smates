// Package agent asks an LLM which tests an "orphan" change can affect: a changed file
// that no recorded or static dependency explains (CI scripts, Dockerfiles, SQL applied
// by a service, fixtures read by an untraced subprocess...). The agent can only add
// tests or request a full run; it never removes deterministic selections.
package agent

import (
	"context"
	"io"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/selector"
)

// PromptVersion is part of the cache key; bump it when prompts or tools change.
const PromptVersion = "2"

// Orphan is a changed file the deterministic analysis could not attribute.
type Orphan struct {
	Path string `json:"path"`
	// Status: "added", "modified" or "deleted".
	Status string `json:"status"`
	// OldBlob is the blob SHA the file had when the state was recorded ("" if new or
	// unknown); NewBlob the current one ("" if deleted).
	OldBlob string `json:"old_blob,omitempty"`
	NewBlob string `json:"new_blob,omitempty"`
	// Base is a git revision to diff against when OldBlob is not available locally.
	Base string `json:"-"`
}

// Request is one agent invocation.
type Request struct {
	Repo    *gitx.Repo
	Config  config.AgentConfig
	Orphans []Orphan
	// Selection is the deterministic selection (suites, tests, reasons, deps). The agent
	// reads it; it must not modify it.
	Selection *selector.Result
	// Log receives human-readable progress (may be io.Discard).
	Log io.Writer
}

// Decision is the agent's answer, validated against the test inventory.
type Decision struct {
	// RunAll requests every test of every suite.
	RunAll bool `json:"run_all"`
	// Add lists tests (or whole suites when Test is "") to add, with reasons.
	Add []selector.Addition `json:"add"`
	// NoImpact lists orphans the agent judged irrelevant to any test.
	NoImpact []string `json:"no_impact"`
	// Summary is a short explanation for humans.
	Summary string `json:"summary"`
	Usage   Usage  `json:"usage"`
	Model   string `json:"model"`
	// Dropped are additions that named tests/suites that do not exist.
	Dropped []string `json:"dropped,omitempty"`
}

// Usage reports token consumption.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	Turns            int `json:"turns"`
}

// Backend runs the agent.
type Backend interface {
	Decide(ctx context.Context, req *Request) (*Decision, error)
}
