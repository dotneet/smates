package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dotneet/smates/internal/config"
)

const (
	maxOutputTokens = 16000
	// promptTokenBudget caps the estimated prompt size of any single request.
	promptTokenBudget = 90000
	// bytesPerToken is a conservative estimate for JSON-heavy prompts.
	bytesPerToken = 3.5
	// maxResponseBytes bounds how much of an HTTP response is read.
	maxResponseBytes = 32 << 20
)

// apiBackend runs the agent loop against a provider's HTTP API (see dialect).
type apiBackend struct {
	cfg       config.AgentConfig
	provider  provider
	baseURL   string
	apiKey    string
	authToken string
	http      *http.Client

	maxAttempts int           // per request, including the first
	retryBase   time.Duration // first backoff delay
	retryMax    time.Duration // backoff cap
	budget      int           // estimated prompt tokens per request
	// noEffort is set once the model rejected the effort setting.
	noEffort atomic.Bool
}

// newAPIBackend returns an Anthropic Messages API backend.
func newAPIBackend(cfg config.AgentConfig, baseURL, apiKey, authToken string) *apiBackend {
	return newProviderBackend(cfg, providers[0], baseURL, apiKey, authToken)
}

func newProviderBackend(cfg config.AgentConfig, p provider, baseURL, apiKey, authToken string) *apiBackend {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = p.baseURL
	}
	return &apiBackend{
		cfg:         cfg,
		provider:    p,
		baseURL:     baseURL,
		apiKey:      apiKey,
		authToken:   authToken,
		http:        &http.Client{},
		maxAttempts: 4,
		retryBase:   time.Second,
		retryMax:    30 * time.Second,
		budget:      promptTokenBudget,
	}
}

// builtinBackend returns the backend of the provider selected by cfg, configured
// from the environment.
func builtinBackend(cfg config.AgentConfig) *apiBackend {
	p := providerOf(cfg)
	var token string
	if p.tokenEnv != "" {
		token = os.Getenv(p.tokenEnv)
	}
	return newProviderBackend(cfg, p, os.Getenv(p.baseURLEnv), os.Getenv(p.keyEnv), token)
}

// dialect is one provider's wire protocol.
type dialect interface {
	// path is the endpoint, relative to the base URL.
	path() string
	authorize(h http.Header, key, token string)
	// loopRequest returns the body of one turn of the tool loop.
	loopRequest(p requestParams, turns []turn) any
	// explainRequest returns the body of a single request, without tools, whose answer
	// is JSON matching schema.
	explainRequest(p requestParams, brief string, schema json.RawMessage) any
	decode(data []byte) (*modelReply, error)
	apiError(status int, h http.Header, data []byte) *APIError
	// effortRejected reports whether err says the model does not accept an effort.
	effortRejected(err *APIError) bool
}

type requestParams struct {
	model, system, effort string
	maxTokens             int
}

// turn is one message of the tool loop, in a provider-neutral form.
type turn struct {
	// model marks a model turn, whose content (modelReply.content) is echoed back verbatim.
	model     bool
	assistant []json.RawMessage
	// A user turn: tool results, then text (the brief, or a reminder).
	results []toolOutcome
	text    string
}

type toolOutcome struct {
	id, content string
	isError     bool
}

type toolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Normalized stop reasons; other values are the provider's own.
const (
	stopRefusal   = "refusal"
	stopMaxTokens = "max_tokens"
)

// modelReply is a decoded model response.
type modelReply struct {
	// content is the model turn in the provider's encoding.
	content []json.RawMessage
	calls   []toolCall
	text    string
	stop    string
	detail  string // refusal category, when known
	usage   Usage
}

// APIError is a non-2xx response from the provider's API.
type APIError struct {
	Provider   string
	Status     int
	Type       string
	Message    string
	Param      string // the request parameter at fault, when the API names it
	RequestID  string
	retryAfter time.Duration
	retryHint  string // x-should-retry header
}

func (e *APIError) Error() string {
	name := e.Provider
	if name == "" {
		name = ProviderAnthropic
	}
	msg := fmt.Sprintf("%s API: HTTP %d", name, e.Status)
	if e.Type != "" {
		msg += " " + e.Type
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.RequestID != "" {
		msg += " (request-id " + e.RequestID + ")"
	}
	return msg
}

func (e *APIError) retryable() bool {
	switch e.retryHint {
	case "true":
		return true
	case "false":
		return false
	}
	switch e.Status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

func (b *apiBackend) hasCredentials() bool { return b.apiKey != "" || b.authToken != "" }

func (b *apiBackend) modelFor(cfg config.AgentConfig) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	return b.provider.model
}

func (b *apiBackend) params(model, system string, cfg config.AgentConfig, maxTokens int) requestParams {
	p := requestParams{model: model, system: system, effort: cfg.Effort, maxTokens: maxTokens}
	if b.noEffort.Load() {
		p.effort = ""
	}
	return p
}

// dropEffort reports whether err rejected the effort sent with p; the effort is then
// left out of this backend's later requests.
func (b *apiBackend) dropEffort(err error, p requestParams) bool {
	var apiErr *APIError
	if p.effort == "" || !errors.As(err, &apiErr) || !b.provider.dialect.effortRejected(apiErr) {
		return false
	}
	b.noEffort.Store(true)
	return true
}

// Decide runs the tool loop until the model submits a selection or a limit is hit.
func (b *apiBackend) Decide(ctx context.Context, req *Request) (*Decision, error) {
	if err := checkRequest(req); err != nil {
		return nil, err
	}
	if !b.hasCredentials() {
		return nil, errors.New("agent: " + b.provider.credentialHint() + " is not set")
	}
	cfg := effectiveConfig(req, b.cfg)
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(cfg))
	defer cancel()

	log := logWriter(req)
	model := b.modelFor(cfg)
	system := systemPrompt(cfg, false)
	maxTurns := maxTurnsOf(cfg)
	tb := newToolbox(req)
	started := time.Now()

	turns := []turn{{text: buildBrief(ctx, req, tb, apiBrief)}}
	var usage Usage
	corrected := false
	fail := func(err error) (*Decision, error) {
		logf(log, "failed after %d turns in %s (%s): %v", usage.Turns, time.Since(started).Round(time.Millisecond), usageLine(usage), err)
		return nil, err
	}

	for turnNo := 1; ; turnNo++ {
		if turnNo > maxTurns {
			return fail(fmt.Errorf("agent: no decision after %d turns", maxTurns))
		}
		p := b.params(model, system, cfg, maxOutputTokens)
		body, err := encodeBody(b.provider.dialect.loopRequest(p, turns))
		if err != nil {
			return fail(err)
		}
		if est := estimateTokens(body); est > b.budget {
			return fail(fmt.Errorf("agent: input token budget exhausted (next request ~%d tokens, budget %d)", est, b.budget))
		}
		resp, err := b.send(ctx, body)
		if err != nil && b.dropEffort(err, p) {
			logf(log, "model %s does not accept effort %q; retrying without it", model, p.effort)
			p.effort = ""
			if body, err = encodeBody(b.provider.dialect.loopRequest(p, turns)); err == nil {
				resp, err = b.send(ctx, body)
			}
		}
		if err != nil {
			return fail(fmt.Errorf("agent: turn %d: %w", turnNo, err))
		}
		usage.Turns = turnNo
		usage.add(resp.usage)
		turns = append(turns, turn{model: true, assistant: resp.content})

		switch resp.stop {
		case stopRefusal:
			detail := ""
			if resp.detail != "" {
				detail = " (" + resp.detail + ")"
			}
			return fail(fmt.Errorf("agent: model refused the request%s", detail))
		case stopMaxTokens:
			return fail(errors.New("agent: response hit max_tokens before a decision"))
		}

		uses := resp.calls
		if len(uses) == 0 {
			return fail(fmt.Errorf("agent: model stopped (%s) without calling %s", resp.stop, toolSubmit))
		}

		// A submission ends the run, unless it names tests that do not exist and a
		// turn is left to let the model correct them once.
		results := make([]toolOutcome, len(uses))
		done := make([]bool, len(uses))
		submitted := false
		for i, u := range uses {
			if u.Name != toolSubmit {
				continue
			}
			sub, err := parseSubmission(u.Input)
			if err != nil {
				results[i], done[i] = toolOutcome{id: u.ID, content: err.Error(), isError: true}, true
				submitted = true
				continue
			}
			d := toDecision(sub, req)
			if len(d.Dropped) > 0 && !corrected && turnNo < maxTurns {
				corrected = true
				msg := "These entries do not match any test and were not accepted:\n- " + strings.Join(d.Dropped, "\n- ") +
					"\nFind the exact IDs with search_tests (or use test \"*\" for a whole suite), then call submit_selection again with the complete selection."
				results[i], done[i] = toolOutcome{id: u.ID, content: msg, isError: true}, true
				submitted = true
				logf(log, "turn %d: submission named %d unknown tests; asking for a correction", turnNo, len(d.Dropped))
				continue
			}
			d.Usage, d.Model = usage, model
			logf(log, "turn %d: %s in %s (%s)", turnNo, describe(d), time.Since(started).Round(time.Millisecond), usageLine(usage))
			return d, nil
		}

		b.runTools(ctx, tb, uses, results, done)
		logf(log, "turn %d: %s (%s)", turnNo, toolNames(uses), usageLine(usage))
		if err := ctx.Err(); err != nil {
			return fail(fmt.Errorf("agent: %w", err))
		}
		turns = append(turns, turn{results: results, text: b.reminder(turnNo, maxTurns, body, submitted)})
	}
}

func (u *Usage) add(v Usage) {
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
	u.CacheReadTokens += v.CacheReadTokens
	u.CacheWriteTokens += v.CacheWriteTokens
}

// reminder nudges the model to conclude when the turn or token budget runs low.
func (b *apiBackend) reminder(turn, maxTurns int, lastBody []byte, submitted bool) string {
	if submitted {
		return ""
	}
	switch {
	case turn+1 >= maxTurns:
		return "This is your last turn: call submit_selection now with your best decision (run_all if you cannot rule out broad impact)."
	case estimateTokens(lastBody) > b.budget*2/3:
		return "The context budget is nearly used up: call submit_selection now with your best decision (run_all if you cannot rule out broad impact)."
	}
	return ""
}

// runTools executes the tool calls not done yet concurrently, filling results in call
// order.
func (b *apiBackend) runTools(ctx context.Context, tb *toolbox, uses []toolCall, results []toolOutcome, done []bool) {
	var wg sync.WaitGroup
	for i, u := range uses {
		if done[i] {
			continue
		}
		if u.Name == toolSubmit {
			// Only reachable when another submission in the same turn was handled.
			results[i] = toolOutcome{id: u.ID, content: "ignored: only one submission per turn", isError: true}
			continue
		}
		wg.Add(1)
		go func(i int, u toolCall) {
			defer wg.Done()
			out, err := tb.run(ctx, u.Name, u.Input)
			r := toolOutcome{id: u.ID, content: out}
			if err != nil {
				r.content, r.isError = err.Error(), true
			}
			if r.content == "" {
				r.content = "(empty)"
			}
			results[i] = r
		}(i, u)
	}
	wg.Wait()
}

// encodeBody renders a request body without HTML escaping.
func encodeBody(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("agent: encode request: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func estimateTokens(body []byte) int { return int(float64(len(body)) / bytesPerToken) }

// send posts one request, retrying overload, rate-limit and network errors with
// exponential backoff (honouring retry-after) as long as the deadline allows.
func (b *apiBackend) send(ctx context.Context, body []byte) (*modelReply, error) {
	for attempt := 1; ; attempt++ {
		resp, err := b.post(ctx, body)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil || attempt >= b.maxAttempts {
			return nil, err
		}
		var apiErr *APIError
		wait := b.backoff(attempt)
		if errors.As(err, &apiErr) {
			if !apiErr.retryable() {
				return nil, err
			}
			if apiErr.retryAfter > 0 {
				wait = apiErr.retryAfter
			}
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			return nil, fmt.Errorf("%w (no time left to retry)", err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C:
		}
	}
}

func (b *apiBackend) backoff(attempt int) time.Duration {
	d := b.retryBase << (attempt - 1)
	if d > b.retryMax || d <= 0 {
		d = b.retryMax
	}
	// ±20% jitter so parallel CI jobs do not retry in lockstep.
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func (b *apiBackend) post(ctx context.Context, body []byte) (*modelReply, error) {
	d := b.provider.dialect
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+d.path(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("content-type", "application/json")
	hreq.Header.Set("user-agent", "smates-agent/"+PromptVersion)
	d.authorize(hreq.Header, b.apiKey, b.authToken)
	hresp, err := b.http.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer hresp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if hresp.StatusCode/100 != 2 {
		e := d.apiError(hresp.StatusCode, hresp.Header, data)
		e.Provider = b.provider.name
		e.retryAfter = parseRetryAfter(hresp.Header.Get("retry-after"))
		e.retryHint = hresp.Header.Get("x-should-retry")
		return nil, e
	}
	resp, err := d.decode(data)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		apiErr.Provider = b.provider.name
	}
	return resp, err
}

// parseRetryAfter accepts delta-seconds (possibly fractional) or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func mustJSON(v any) json.RawMessage {
	b, err := encodeBody(v)
	if err != nil {
		panic(err.Error())
	}
	return b
}

func toolNames(uses []toolCall) string {
	counts := map[string]int{}
	for _, u := range uses {
		counts[u.Name]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		if counts[n] > 1 {
			names[i] = fmt.Sprintf("%s x%d", n, counts[n])
		}
	}
	return strings.Join(names, ", ")
}

func usageLine(u Usage) string {
	return fmt.Sprintf("tokens: %d in, %d cache read, %d cache write, %d out", u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens)
}

func describe(d *Decision) string {
	switch {
	case d.RunAll:
		return "decision run_all"
	case len(d.Add) == 0:
		return "decision none"
	}
	return fmt.Sprintf("decision: add %d", len(d.Add))
}
