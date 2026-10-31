package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI emulates POST /v1/messages with a scripted sequence of replies.
type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	replies []fakeReply
	reqs    []recordedRequest
}

type fakeReply struct {
	status  int
	header  map[string]string
	body    string
	delay   time.Duration
	inspect func(t *testing.T, r recordedRequest) // optional per-request checks
}

type recordedRequest struct {
	header http.Header
	raw    []byte
	body   map[string]json.RawMessage
	msgs   []struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	rec := recordedRequest{header: r.Header.Clone(), raw: raw}
	if err := json.Unmarshal(raw, &rec.body); err != nil {
		f.t.Errorf("request body is not JSON: %v", err)
	}
	_ = json.Unmarshal(rec.body["messages"], &rec.msgs)

	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	if len(f.replies) == 0 {
		f.mu.Unlock()
		f.t.Errorf("unexpected extra request #%d", len(f.reqs))
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no more replies"}}`, http.StatusBadRequest)
		return
	}
	rep := f.replies[0]
	f.replies = f.replies[1:]
	f.mu.Unlock()

	if rep.inspect != nil {
		rep.inspect(f.t, rec)
	}
	if rep.delay > 0 {
		select {
		case <-time.After(rep.delay):
		case <-r.Context().Done():
			return
		}
	}
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	w.Header().Set("content-type", "application/json")
	if rep.status == 0 {
		rep.status = http.StatusOK
	}
	w.WriteHeader(rep.status)
	io.WriteString(w, rep.body)
}

func (f *fakeAPI) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.reqs...)
}

// message renders a Messages API response.
func message(stop string, blocks ...string) string {
	return fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[%s],"stop_reason":%q,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":1000,"cache_creation_input_tokens":50}}`,
		strings.Join(blocks, ","), stop)
}

func toolUse(id, name, input string) string {
	return fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":%s}`, id, name, input)
}

func reply(stop string, blocks ...string) fakeReply {
	return fakeReply{body: message(stop, blocks...)}
}

const submitTests = `{"decision":"tests","tests":[{"suite":"web","test":"src/db.test.ts","why":"db helper runs scripts/seed.sh"}],"no_impact":["scripts/gone.sh"],"summary":"seed.sh is run by the db tests"}`

func newTestBackend(t *testing.T, replies ...fakeReply) (*apiBackend, *fakeAPI) {
	t.Helper()
	fake := &fakeAPI{t: t, replies: replies}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	b := newAPIBackend(testConfig(), srv.URL+"/", "test-key", "")
	b.retryBase = time.Millisecond
	b.retryMax = 5 * time.Millisecond
	return b, fake
}

func TestAPIToolLoop(t *testing.T) {
	f := newFixture(t)
	// The thinking block contains characters an HTML-escaping encoder would rewrite,
	// and odd spacing: it must come back semantically and byte-for-byte compact-equal.
	thinking := `{"type":"thinking", "thinking":"", "signature":"Sig<&>+/=="}`
	backend, fake := newTestBackend(t,
		reply("tool_use",
			thinking,
			`{"type":"text","text":"Looking."}`,
			toolUse("tu_1", "grep", `{"pattern":"seed\\.sh"}`),
			toolUse("tu_2", "read_file", `{"path":"../outside"}`),
			toolUse("tu_3", "tests_for_file", `{"path":"src/helpers/db.ts"}`),
		),
		reply("tool_use", toolUse("tu_4", "submit_selection", submitTests)),
	)
	var log bytes.Buffer
	req := f.request(testConfig())
	req.Log = &log
	d, err := backend.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("Decide: %v\nlog:\n%s", err, log.String())
	}
	if len(d.Add) != 1 || d.Add[0].Suite != "web" || d.Add[0].Test != "src/db.test.ts" || d.Add[0].Reason != "agent" {
		t.Fatalf("Add = %+v", d.Add)
	}
	if d.RunAll || len(d.NoImpact) != 1 || d.Model != "claude-haiku-5-5" {
		t.Errorf("decision = %+v", d)
	}
	want := Usage{InputTokens: 200, OutputTokens: 40, CacheReadTokens: 2000, CacheWriteTokens: 100, Turns: 2}
	if d.Usage != want {
		t.Errorf("Usage = %+v, want %+v", d.Usage, want)
	}

	reqs := fake.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests", len(reqs))
	}
	first := reqs[0]
	if got := first.header.Get("x-api-key"); got != "test-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := first.header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := first.header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "thinking"} {
		if _, ok := first.body[k]; ok {
			t.Errorf("request must not send %s", k)
		}
	}
	for k, want := range map[string]string{
		"model":         `"claude-haiku-5-5"`,
		"max_tokens":    `16000`,
		"output_config": `{"effort":"low"}`,
		"cache_control": `{"type":"ephemeral"}`,
		"tool_choice":   `{"type":"auto"}`,
	} {
		if got := string(first.body[k]); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	var system string
	_ = json.Unmarshal(first.body["system"], &system)
	for _, w := range []string{"only ADD tests", "untrusted", "run_all", "parallel"} {
		if !strings.Contains(system, w) {
			t.Errorf("system prompt lacks %q", w)
		}
	}
	if len(first.msgs) != 1 || first.msgs[0].Role != "user" {
		t.Fatalf("first request messages = %+v", first.msgs)
	}
	var brief struct{ Text string }
	_ = json.Unmarshal(first.msgs[0].Content[0], &brief)
	for _, w := range []string{"scripts/seed.sh (modified)", "scripts/gone.sh (deleted)", "+set -e", "-echo bye", "web: runner vitest, root ., 2 tests (1 already selected)", "src/db.test.ts", "1 of 7 tests are selected deterministically (changed: 1)"} {
		if !strings.Contains(brief.Text, w) {
			t.Errorf("brief lacks %q:\n%s", w, brief.Text)
		}
	}

	// Second request: history is append-only, the assistant turn is echoed verbatim
	// and all tool results travel in one user message.
	second := reqs[1]
	if !bytes.HasPrefix(second.raw, first.raw[:bytes.Index(first.raw, []byte(`"messages"`))]) {
		t.Error("request prefix (model, tools, system) changed between turns")
	}
	if len(second.msgs) != 3 {
		t.Fatalf("second request has %d messages", len(second.msgs))
	}
	if string(second.msgs[0].Content[0]) != string(first.msgs[0].Content[0]) {
		t.Error("first user message changed")
	}
	asst := second.msgs[1]
	if asst.Role != "assistant" || len(asst.Content) != 5 {
		t.Fatalf("assistant turn = %+v", asst)
	}
	var compact bytes.Buffer
	_ = json.Compact(&compact, []byte(thinking))
	if string(asst.Content[0]) != compact.String() {
		t.Errorf("thinking block not echoed verbatim:\n got %s\nwant %s", asst.Content[0], compact.String())
	}
	user := second.msgs[2]
	if user.Role != "user" || len(user.Content) != 3 {
		t.Fatalf("tool results message = %+v", user)
	}
	var results []toolResult
	for _, c := range user.Content {
		var r toolResult
		_ = json.Unmarshal(c, &r)
		results = append(results, r)
	}
	if results[0].ToolUseID != "tu_1" || results[0].IsError || !strings.Contains(results[0].Content, "src/helpers/db.ts:2:") {
		t.Errorf("grep result = %+v", results[0])
	}
	if results[1].ToolUseID != "tu_2" || !results[1].IsError || !strings.Contains(results[1].Content, "outside the repository") {
		t.Errorf("read_file result = %+v", results[1])
	}
	if results[2].ToolUseID != "tu_3" || !strings.Contains(results[2].Content, "web\tsrc/db.test.ts") {
		t.Errorf("tests_for_file result = %+v", results[2])
	}
	for _, w := range []string{"turn 1: grep, read_file, tests_for_file", "turn 2: decision: add 1"} {
		if !strings.Contains(log.String(), w) {
			t.Errorf("log lacks %q:\n%s", w, log.String())
		}
	}
}

func TestAPIRetries(t *testing.T) {
	overloaded := fakeReply{status: 529, header: map[string]string{"retry-after": "0"}, body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}
	tests := []struct {
		name     string
		replies  []fakeReply
		wantErr  string
		wantReqs int
	}{
		{name: "529 then success", replies: []fakeReply{overloaded, {status: 500, body: "oops"}, reply("tool_use", toolUse("t", "submit_selection", submitTests))}, wantReqs: 3},
		{name: "429 honours retry-after", replies: []fakeReply{{status: 429, header: map[string]string{"retry-after": "0.01"}, body: `{}`}, reply("tool_use", toolUse("t", "submit_selection", submitTests))}, wantReqs: 2},
		{name: "gives up after 4 attempts", replies: []fakeReply{overloaded, overloaded, overloaded, overloaded}, wantErr: "HTTP 529 overloaded_error: Overloaded", wantReqs: 4},
		{name: "400 is not retried", replies: []fakeReply{{status: 400, header: map[string]string{"request-id": "req_9"}, body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`}}, wantErr: "HTTP 400 invalid_request_error: bad (request-id req_9)", wantReqs: 1},
		{name: "x-should-retry false wins", replies: []fakeReply{{status: 503, header: map[string]string{"x-should-retry": "false"}, body: `{}`}}, wantErr: "HTTP 503", wantReqs: 1},
		{name: "retry-after beyond deadline", replies: []fakeReply{{status: 429, header: map[string]string{"retry-after": "3600"}, body: `{}`}}, wantErr: "no time left to retry", wantReqs: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			b, fake := newTestBackend(t, tt.replies...)
			_, err := b.Decide(context.Background(), f.request(testConfig()))
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if got := len(fake.requests()); got != tt.wantReqs {
				t.Errorf("requests = %d, want %d", got, tt.wantReqs)
			}
		})
	}
}

func TestAPIFailures(t *testing.T) {
	tests := []struct {
		name    string
		cfg     func(c *configT)
		replies []fakeReply
		budget  int
		wantErr string
	}{
		{name: "refusal", replies: []fakeReply{{body: `{"content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"},"usage":{}}`}}, wantErr: "refused the request (cyber)"},
		{name: "max_tokens", replies: []fakeReply{reply("max_tokens", `{"type":"text","text":"..."}`)}, wantErr: "max_tokens"},
		{name: "end_turn without submit", replies: []fakeReply{reply("end_turn", `{"type":"text","text":"I think nothing."}`)}, wantErr: "without calling submit_selection"},
		{name: "turn limit", cfg: func(c *configT) { c.MaxTurns = 2 },
			replies: []fakeReply{
				reply("tool_use", toolUse("a", "search_tests", `{"query":"db"}`)),
				{body: message("tool_use", toolUse("b", "search_tests", `{"query":"db"}`)), inspect: func(t *testing.T, r recordedRequest) {
					if !strings.Contains(string(r.raw), "This is your last turn") {
						t.Error("last-turn reminder missing")
					}
				}},
			}, wantErr: "no decision after 2 turns"},
		{name: "deadline", cfg: func(c *configT) { c.Timeout = 50 * time.Millisecond },
			replies: []fakeReply{{delay: 5 * time.Second, body: message("tool_use")}}, wantErr: "context deadline exceeded"},
		{name: "token budget", budget: 100, wantErr: "input token budget exhausted"},
		{name: "malformed response", replies: []fakeReply{{body: `{"content":"nope"}`}, {body: `{"content":"nope"}`}, {body: `{"content":"nope"}`}, {body: `{"content":"nope"}`}}, wantErr: "decode response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			b, _ := newTestBackend(t, tt.replies...)
			if tt.budget > 0 {
				b.budget = tt.budget
			}
			cfg := testConfig()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			var log bytes.Buffer
			req := f.request(cfg)
			req.Log = &log
			d, err := b.Decide(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Decide = %+v, %v; want error %q", d, err, tt.wantErr)
			}
			if !strings.Contains(log.String(), "failed after") {
				t.Errorf("failure not logged:\n%s", log.String())
			}
		})
	}
}

func TestAPICorrectsUnknownTests(t *testing.T) {
	f := newFixture(t)
	bad := `{"decision":"tests","tests":[{"suite":"web","test":"src/seed.test.ts","why":"guess"},{"suite":"web","test":"db.test.ts","why":"suffix"}],"no_impact":[],"summary":"s"}`
	b, fake := newTestBackend(t,
		reply("tool_use", toolUse("s1", "submit_selection", bad)),
		fakeReply{body: message("tool_use", toolUse("s2", "submit_selection", bad)), inspect: func(t *testing.T, r recordedRequest) {
			last := r.msgs[len(r.msgs)-1]
			var res toolResult
			_ = json.Unmarshal(last.Content[0], &res)
			if res.ToolUseID != "s1" || !res.IsError || !strings.Contains(res.Content, "src/seed.test.ts") {
				t.Errorf("correction result = %+v", res)
			}
		}},
	)
	d, err := b.Decide(context.Background(), f.request(testConfig()))
	if err != nil {
		t.Fatal(err)
	}
	// The second, identical submission is accepted: the suffix match is kept and the
	// unknown test recorded as dropped.
	if len(d.Add) != 1 || d.Add[0].Test != "src/db.test.ts" || len(d.Dropped) != 1 {
		t.Fatalf("decision = %+v", d)
	}
	if len(fake.requests()) != 2 || d.Usage.Turns != 2 {
		t.Errorf("turns = %d", d.Usage.Turns)
	}
}

func TestAPIRunAllAndBearerToken(t *testing.T) {
	f := newFixture(t)
	fake := &fakeAPI{t: t, replies: []fakeReply{reply("tool_use", toolUse("s", "submit_selection", `{"decision":"run_all","tests":[],"no_impact":[],"summary":"CI runner image changed"}`))}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	t.Setenv(envAPIKey, "")
	t.Setenv(envAuthToken, "tok")
	t.Setenv(envBaseURL, srv.URL)
	cfg := testConfig()
	cfg.Effort = ""
	backend := New(cfg)
	d, err := backend.Decide(context.Background(), f.request(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !d.RunAll || d.Summary != "CI runner image changed" {
		t.Fatalf("decision = %+v", d)
	}
	r := fake.requests()[0]
	if got := r.header.Get("authorization"); got != "Bearer tok" {
		t.Errorf("authorization = %q", got)
	}
	if r.header.Get("x-api-key") != "" {
		t.Error("x-api-key must not be sent with a bearer token")
	}
	if _, ok := r.body["output_config"]; ok {
		t.Error("output_config sent without an effort")
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := map[string]time.Duration{
		"":     0,
		"2":    2 * time.Second,
		"0.5":  500 * time.Millisecond,
		"-1":   0,
		"junk": 0,
	}
	for in, want := range tests {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got < 59*time.Minute {
		t.Errorf("HTTP date retry-after = %v", got)
	}
}

func TestSystemPromptStable(t *testing.T) {
	cfg := testConfig()
	if systemPrompt(cfg, false) != systemPrompt(cfg, false) {
		t.Fatal("system prompt is not deterministic")
	}
	cfg.Instructions = "Integration tests live in e2e/."
	s := systemPrompt(cfg, false)
	if !strings.HasSuffix(s, "smates configuration:\nIntegration tests live in e2e/.") {
		t.Errorf("instructions not appended:\n%s", s)
	}
	ext := systemPrompt(cfg, true)
	if strings.Contains(ext, "submit_selection") || strings.Contains(ext, "search_tests") {
		t.Error("external prompt mentions built-in tools")
	}
}
