package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
)

// Defaults applied when the configuration leaves a field empty (config.Normalize
// normally fills them; these keep the package usable with a zero AgentConfig).
// DefaultModel is the Anthropic default; each provider has its own (see providers).
const (
	DefaultModel    = "claude-haiku-5-5"
	DefaultMaxTurns = 12
	DefaultTimeout  = 120 * time.Second
	defaultBaseURL  = "https://api.anthropic.com"
)

// Environment variables read by the built-in backend for Anthropic (see providers for
// the others).
const (
	envAPIKey    = "ANTHROPIC_API_KEY"
	envAuthToken = "ANTHROPIC_AUTH_TOKEN"
	envBaseURL   = "ANTHROPIC_BASE_URL"
)

// Available reports whether an agent can run with cfg, and why not when it cannot.
// The external command backend only needs cfg.Command; the built-in backend needs the
// API key of its provider (see providerOf).
func Available(cfg config.AgentConfig) (bool, string) {
	if cfg.Enabled == "never" {
		return false, "agent disabled (agent.enabled: never)"
	}
	if strings.TrimSpace(cfg.Command) != "" {
		return true, ""
	}
	if providerOf(cfg).configured() {
		return true, ""
	}
	return false, missingCredentials(cfg)
}

// New returns the backend selected by cfg: the external command when cfg.Command is
// set, otherwise the built-in client of the selected provider, configured from the
// environment (API key and optional base URL, e.g. OPENAI_API_KEY and OPENAI_BASE_URL).
func New(cfg config.AgentConfig) Backend {
	if strings.TrimSpace(cfg.Command) != "" {
		return &commandBackend{cfg: cfg}
	}
	return builtinBackend(cfg)
}

// Describe names the agent cfg runs, for humans: the external command, or the model
// and provider of the built-in backend.
func Describe(cfg config.AgentConfig) string {
	if c := strings.TrimSpace(cfg.Command); c != "" {
		return c
	}
	return ModelOf(cfg) + " (" + ProviderOf(cfg) + ")"
}

// CacheKey identifies a request's answer: it changes when the prompt version, the
// model (or external command), the agent instructions, the orphans (path, status and
// blobs) or the test inventory change. Already-selected tests are deliberately not
// part of the key: the agent only adds tests, so its answer stays valid.
func CacheKey(req *Request) string {
	h := sha256.New()
	field := func(parts ...string) {
		for _, p := range parts {
			io.WriteString(h, p)
			h.Write([]byte{0})
		}
		h.Write([]byte{'\n'})
	}
	field("prompt", PromptVersion)
	if c := strings.TrimSpace(req.Config.Command); c != "" {
		field("command", c)
	} else {
		field("provider", ProviderOf(req.Config), "model", ModelOf(req.Config), "effort", req.Config.Effort)
	}
	field("instructions", req.Config.Instructions)

	orphans := append([]Orphan(nil), req.Orphans...)
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Path < orphans[j].Path })
	for _, o := range orphans {
		field("orphan", o.Path, o.Status, o.OldBlob, o.NewBlob)
	}
	if req.Selection != nil {
		type suiteIDs struct {
			name string
			ids  []string
		}
		var suites []suiteIDs
		for _, s := range req.Selection.Suites {
			ids := make([]string, 0, len(s.Tests))
			for _, t := range s.Tests {
				ids = append(ids, t.ID)
			}
			sort.Strings(ids)
			suites = append(suites, suiteIDs{s.Name, ids})
		}
		sort.Slice(suites, func(i, j int) bool { return suites[i].name < suites[j].name })
		for _, s := range suites {
			field(append([]string{"suite", s.name}, s.ids...)...)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func maxTurnsOf(cfg config.AgentConfig) int {
	if cfg.MaxTurns > 0 {
		return cfg.MaxTurns
	}
	return DefaultMaxTurns
}

func timeoutOf(cfg config.AgentConfig) time.Duration {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return DefaultTimeout
}

// checkRequest validates the fields every backend needs.
func checkRequest(req *Request) error {
	switch {
	case req == nil:
		return errors.New("agent: nil request")
	case req.Repo == nil:
		return errors.New("agent: request has no repository")
	case req.Selection == nil:
		return errors.New("agent: request has no selection")
	case len(req.Orphans) == 0:
		return errors.New("agent: request has no orphans")
	}
	return nil
}

// effectiveConfig returns req.Config, or the backend's configuration when the request
// carries none. Model, limits and instructions are read from the result.
func effectiveConfig(req *Request, backend config.AgentConfig) config.AgentConfig {
	if req.Config == (config.AgentConfig{}) {
		return backend
	}
	return req.Config
}

func logWriter(req *Request) io.Writer {
	if req.Log == nil {
		return io.Discard
	}
	return req.Log
}

func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "agent: "+format+"\n", args...)
}
