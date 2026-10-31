package agent

import (
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/selector"
)

func TestAvailable(t *testing.T) {
	tests := []struct {
		name          string
		cfg           config.AgentConfig
		key, token    string
		want          bool
		wantReasonHas string
	}{
		{name: "api key", key: "k", want: true},
		{name: "auth token", token: "t", want: true},
		{name: "no credentials", wantReasonHas: "ANTHROPIC_API_KEY"},
		{name: "command needs no key", cfg: config.AgentConfig{Command: "claude -p"}, want: true},
		{name: "disabled", cfg: config.AgentConfig{Enabled: "never", Command: "x"}, key: "k", wantReasonHas: "never"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envAPIKey, tt.key)
			t.Setenv(envAuthToken, tt.token)
			got, reason := Available(tt.cfg)
			if got != tt.want {
				t.Fatalf("Available = %v (%s), want %v", got, reason, tt.want)
			}
			if got && reason != "" {
				t.Errorf("reason %q for an available agent", reason)
			}
			if !got && (reason == "" || !strings.Contains(reason, tt.wantReasonHas)) {
				t.Errorf("reason = %q, want it to mention %q", reason, tt.wantReasonHas)
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Setenv(envAPIKey, "k")
	t.Setenv(envBaseURL, "https://proxy.example.com/")
	if _, ok := New(config.AgentConfig{Command: "fx ask --json"}).(*commandBackend); !ok {
		t.Error("command config must select the command backend")
	}
	b, ok := New(config.AgentConfig{}).(*apiBackend)
	if !ok {
		t.Fatal("default must be the API backend")
	}
	if b.baseURL != "https://proxy.example.com" || b.apiKey != "k" {
		t.Errorf("backend = %+v", b)
	}
	t.Setenv(envBaseURL, "")
	if b := New(config.AgentConfig{}).(*apiBackend); b.baseURL != defaultBaseURL {
		t.Errorf("default base URL = %q", b.baseURL)
	}
}

func TestCacheKey(t *testing.T) {
	base := func() *Request {
		return &Request{
			Config: config.AgentConfig{Model: "claude-haiku-5-5", Effort: "low"},
			Orphans: []Orphan{
				{Path: "b.sh", Status: "modified", OldBlob: "1", NewBlob: "2"},
				{Path: "a.sh", Status: "added", NewBlob: "3"},
			},
			Selection: testSelection(),
		}
	}
	key := CacheKey(base())
	if len(key) != 64 {
		t.Fatalf("key %q is not a hex sha256", key)
	}

	same := []struct {
		name   string
		mutate func(r *Request)
	}{
		{"orphan order", func(r *Request) { r.Orphans[0], r.Orphans[1] = r.Orphans[1], r.Orphans[0] }},
		{"suite order", func(r *Request) { s := r.Selection.Suites; s[0], s[1] = s[1], s[0] }},
		{"test order", func(r *Request) { ts := r.Selection.Suites[0].Tests; ts[0], ts[1] = ts[1], ts[0] }},
		{"selection state", func(r *Request) { r.Selection.Suites[0].Tests[0].Selected = true }},
		{"base revision", func(r *Request) { r.Orphans[0].Base = "origin/main" }},
		{"default model spelled out", func(r *Request) { r.Config.Model = "" }},
	}
	for _, tt := range same {
		r := base()
		tt.mutate(r)
		if got := CacheKey(r); got != key {
			t.Errorf("%s changed the key", tt.name)
		}
	}

	differ := []struct {
		name   string
		mutate func(r *Request)
	}{
		{"new blob", func(r *Request) { r.Orphans[0].NewBlob = "9" }},
		{"old blob", func(r *Request) { r.Orphans[0].OldBlob = "9" }},
		{"status", func(r *Request) { r.Orphans[1].Status = "modified" }},
		{"extra orphan", func(r *Request) { r.Orphans = append(r.Orphans, Orphan{Path: "c.sh"}) }},
		{"model", func(r *Request) { r.Config.Model = "claude-sonnet-5-5" }},
		{"effort", func(r *Request) { r.Config.Effort = "high" }},
		{"instructions", func(r *Request) { r.Config.Instructions = "hint" }},
		{"command", func(r *Request) { r.Config.Command = "claude -p" }},
		{"new test", func(r *Request) {
			r.Selection.Suites[0].Tests = append(r.Selection.Suites[0].Tests, &selector.Test{ID: "src/new.test.ts"})
		}},
		{"test moved between suites", func(r *Request) {
			s := r.Selection.Suites
			s[1].Tests = append(s[1].Tests, s[0].Tests[0])
			s[0].Tests = s[0].Tests[1:]
		}},
	}
	for _, tt := range differ {
		r := base()
		tt.mutate(r)
		if got := CacheKey(r); got == key {
			t.Errorf("%s did not change the key", tt.name)
		}
	}
}
