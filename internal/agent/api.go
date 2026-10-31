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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dotneet/smates/internal/config"
)

const (
	anthropicVersion = "2023-06-01"
	maxOutputTokens  = 16000
	// promptTokenBudget caps the estimated prompt size of any single request.
	promptTokenBudget = 90000
	// bytesPerToken is a conservative estimate for JSON-heavy prompts.
	bytesPerToken = 3.5
	// maxResponseBytes bounds how much of an HTTP response is read.
	maxResponseBytes = 32 << 20
)

// apiBackend runs the agent loop against the Anthropic Messages API over HTTPS.
type apiBackend struct {
	cfg       config.AgentConfig
	baseURL   string
	apiKey    string
	authToken string
	http      *http.Client

	maxAttempts int           // per request, including the first
	retryBase   time.Duration // first backoff delay
	retryMax    time.Duration // backoff cap
	budget      int           // estimated prompt tokens per request
}

func newAPIBackend(cfg config.AgentConfig, baseURL, apiKey, authToken string) *apiBackend {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &apiBackend{
		cfg:         cfg,
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

// Wire types. Content blocks are kept as raw JSON so assistant turns (including
// thinking blocks and their signatures) are echoed back byte for byte.
type (
	apiMessage struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	apiRequest struct {
		Model        string            `json:"model"`
		MaxTokens    int               `json:"max_tokens"`
		OutputConfig *outputConfig     `json:"output_config,omitempty"`
		CacheControl cacheControl      `json:"cache_control"`
		System       string            `json:"system"`
		Tools        json.RawMessage   `json:"tools"`
		ToolChoice   map[string]string `json:"tool_choice"`
		Messages     []apiMessage      `json:"messages"`
	}
	outputConfig struct {
		Effort string `json:"effort"`
	}
	cacheControl struct {
		Type string `json:"type"`
	}
	apiResponse struct {
		ID          string            `json:"id"`
		Content     []json.RawMessage `json:"content"`
		StopReason  string            `json:"stop_reason"`
		StopDetails *struct {
			Category    string `json:"category"`
			Explanation string `json:"explanation"`
		} `json:"stop_details"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	// blockHeader is the part of a content block the loop inspects.
	blockHeader struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
		Text  string          `json:"text"`
	}
	toolResult struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		Content   string `json:"content"`
		IsError   bool   `json:"is_error,omitempty"`
	}
	textBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
)

// APIError is a non-2xx response from the Messages API.
type APIError struct {
	Status     int
	Type       string
	Message    string
	RequestID  string
	retryAfter time.Duration
	retryHint  string // x-should-retry header
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("anthropic API: HTTP %d", e.Status)
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

// Decide runs the tool loop until the model submits a selection or a limit is hit.
func (b *apiBackend) Decide(ctx context.Context, req *Request) (*Decision, error) {
	if err := checkRequest(req); err != nil {
		return nil, err
	}
	if b.apiKey == "" && b.authToken == "" {
		return nil, errors.New("agent: " + envAPIKey + " (or " + envAuthToken + ") is not set")
	}
	cfg := effectiveConfig(req, b.cfg)
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(cfg))
	defer cancel()

	log := logWriter(req)
	model := modelOf(cfg)
	maxTurns := maxTurnsOf(cfg)
	tb := newToolbox(req)
	started := time.Now()

	brief := buildBrief(ctx, req, tb, apiBrief)
	msgs := []apiMessage{{Role: "user", Content: []json.RawMessage{mustJSON(textBlock{"text", brief})}}}
	var usage Usage
	corrected := false
	fail := func(err error) (*Decision, error) {
		logf(log, "failed after %d turns in %s (%s): %v", usage.Turns, time.Since(started).Round(time.Millisecond), usageLine(usage), err)
		return nil, err
	}

	for turn := 1; ; turn++ {
		if turn > maxTurns {
			return fail(fmt.Errorf("agent: no decision after %d turns", maxTurns))
		}
		body, err := b.requestBody(model, cfg, msgs)
		if err != nil {
			return fail(err)
		}
		if est := estimateTokens(body); est > b.budget {
			return fail(fmt.Errorf("agent: input token budget exhausted (next request ~%d tokens, budget %d)", est, b.budget))
		}
		resp, err := b.send(ctx, body)
		if err != nil {
			return fail(fmt.Errorf("agent: turn %d: %w", turn, err))
		}
		usage.Turns = turn
		usage.InputTokens += resp.Usage.InputTokens
		usage.OutputTokens += resp.Usage.OutputTokens
		usage.CacheReadTokens += resp.Usage.CacheReadInputTokens
		usage.CacheWriteTokens += resp.Usage.CacheCreationInputTokens
		msgs = append(msgs, apiMessage{Role: "assistant", Content: resp.Content})

		switch resp.StopReason {
		case "refusal":
			detail := ""
			if resp.StopDetails != nil && resp.StopDetails.Category != "" {
				detail = " (" + resp.StopDetails.Category + ")"
			}
			return fail(fmt.Errorf("agent: model refused the request%s", detail))
		case "max_tokens":
			return fail(errors.New("agent: response hit max_tokens before a decision"))
		}

		var uses []blockHeader
		for _, raw := range resp.Content {
			var h blockHeader
			if err := json.Unmarshal(raw, &h); err != nil {
				return fail(fmt.Errorf("agent: malformed content block: %w", err))
			}
			if h.Type == "tool_use" {
				uses = append(uses, h)
			}
		}
		if len(uses) == 0 {
			return fail(fmt.Errorf("agent: model stopped (%s) without calling %s", resp.StopReason, toolSubmit))
		}

		// A submission ends the run, unless it names tests that do not exist and a
		// turn is left to let the model correct them once.
		results := make([]json.RawMessage, len(uses))
		submitted := false
		for i, u := range uses {
			if u.Name != toolSubmit {
				continue
			}
			sub, err := parseSubmission(u.Input)
			if err != nil {
				results[i] = mustJSON(toolResult{Type: "tool_result", ToolUseID: u.ID, Content: err.Error(), IsError: true})
				submitted = true
				continue
			}
			d := toDecision(sub, req)
			if len(d.Dropped) > 0 && !corrected && turn < maxTurns {
				corrected = true
				msg := "These entries do not match any test and were not accepted:\n- " + strings.Join(d.Dropped, "\n- ") +
					"\nFind the exact IDs with search_tests (or use test \"*\" for a whole suite), then call submit_selection again with the complete selection."
				results[i] = mustJSON(toolResult{Type: "tool_result", ToolUseID: u.ID, Content: msg, IsError: true})
				submitted = true
				logf(log, "turn %d: submission named %d unknown tests; asking for a correction", turn, len(d.Dropped))
				continue
			}
			d.Usage, d.Model = usage, model
			logf(log, "turn %d: %s in %s (%s)", turn, describe(d), time.Since(started).Round(time.Millisecond), usageLine(usage))
			return d, nil
		}

		b.runTools(ctx, tb, uses, results)
		names := toolNames(uses)
		logf(log, "turn %d: %s (%s)", turn, names, usageLine(usage))
		if err := ctx.Err(); err != nil {
			return fail(fmt.Errorf("agent: %w", err))
		}

		next := apiMessage{Role: "user", Content: results}
		if note := b.reminder(turn, maxTurns, body, submitted); note != "" {
			next.Content = append(next.Content, mustJSON(textBlock{"text", note}))
		}
		msgs = append(msgs, next)
	}
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

// runTools executes the non-submission tool calls concurrently, filling results in
// call order.
func (b *apiBackend) runTools(ctx context.Context, tb *toolbox, uses []blockHeader, results []json.RawMessage) {
	var wg sync.WaitGroup
	for i, u := range uses {
		if results[i] != nil {
			continue
		}
		if u.Name == toolSubmit {
			// Only reachable when another submission in the same turn was handled.
			results[i] = mustJSON(toolResult{Type: "tool_result", ToolUseID: u.ID, Content: "ignored: only one submission per turn", IsError: true})
			continue
		}
		wg.Add(1)
		go func(i int, u blockHeader) {
			defer wg.Done()
			out, err := tb.run(ctx, u.Name, u.Input)
			r := toolResult{Type: "tool_result", ToolUseID: u.ID, Content: out}
			if err != nil {
				r.Content, r.IsError = err.Error(), true
			}
			if r.Content == "" {
				r.Content = "(empty)"
			}
			results[i] = mustJSON(r)
		}(i, u)
	}
	wg.Wait()
}

func (b *apiBackend) requestBody(model string, cfg config.AgentConfig, msgs []apiMessage) ([]byte, error) {
	r := apiRequest{
		Model:        model,
		MaxTokens:    maxOutputTokens,
		CacheControl: cacheControl{Type: "ephemeral"},
		System:       systemPrompt(cfg, false),
		Tools:        toolDefs,
		ToolChoice:   map[string]string{"type": "auto"},
		Messages:     msgs,
	}
	if cfg.Effort != "" {
		r.OutputConfig = &outputConfig{Effort: cfg.Effort}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("agent: encode request: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func estimateTokens(body []byte) int { return int(float64(len(body)) / bytesPerToken) }

// send posts one request, retrying overload, rate-limit and network errors with
// exponential backoff (honouring retry-after) as long as the deadline allows.
func (b *apiBackend) send(ctx context.Context, body []byte) (*apiResponse, error) {
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

func (b *apiBackend) post(ctx context.Context, body []byte) (*apiResponse, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("content-type", "application/json")
	hreq.Header.Set("anthropic-version", anthropicVersion)
	hreq.Header.Set("user-agent", "smates-agent/"+PromptVersion)
	if b.apiKey != "" {
		hreq.Header.Set("x-api-key", b.apiKey)
	} else {
		hreq.Header.Set("authorization", "Bearer "+b.authToken)
	}
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
		e := &APIError{
			Status:     hresp.StatusCode,
			RequestID:  hresp.Header.Get("request-id"),
			retryAfter: parseRetryAfter(hresp.Header.Get("retry-after")),
			retryHint:  hresp.Header.Get("x-should-retry"),
		}
		var body struct {
			Error struct{ Type, Message string }
		}
		if json.Unmarshal(data, &body) == nil && body.Error.Type != "" {
			e.Type, e.Message = body.Error.Type, body.Error.Message
		} else {
			e.Message = clip(strings.TrimSpace(string(data)), 300)
		}
		return nil, e
	}
	var resp apiResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("agent: " + err.Error())
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func toolNames(uses []blockHeader) string {
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
