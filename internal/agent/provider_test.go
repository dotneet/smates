package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

func TestProviderSelection(t *testing.T) {
	tests := []struct {
		name          string
		provider      string
		env           []string
		want          string
		wantModel     string
		available     bool
		wantReasonHas string
	}{
		{name: "anthropic first", env: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"}, want: ProviderAnthropic, wantModel: "claude-haiku-5-5", available: true},
		{name: "anthropic token", env: []string{"ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY"}, want: ProviderAnthropic, available: true},
		{name: "openai before openrouter", env: []string{"OPENAI_API_KEY", "OPENROUTER_API_KEY"}, want: ProviderOpenAI, wantModel: "gpt-6-luna", available: true},
		{name: "openrouter alone", env: []string{"OPENROUTER_API_KEY"}, want: ProviderOpenRouter, wantModel: "anthropic/claude-haiku-5.5", available: true},
		{name: "explicit auto", provider: "auto", env: []string{"OPENROUTER_API_KEY"}, want: ProviderOpenRouter, available: true},
		{name: "explicit wins", provider: ProviderOpenRouter, env: []string{"ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"}, want: ProviderOpenRouter, available: true},
		{name: "explicit without its key", provider: ProviderOpenAI, env: []string{"ANTHROPIC_API_KEY"}, want: ProviderOpenAI, wantReasonHas: "OPENAI_API_KEY for agent.provider openai"},
		{name: "no keys", want: ProviderAnthropic, wantReasonHas: "ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN), OPENAI_API_KEY or OPENROUTER_API_KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearCredentials(t)
			for _, k := range tt.env {
				t.Setenv(k, "x")
			}
			cfg := config.AgentConfig{Provider: tt.provider}
			if got := ProviderOf(cfg); got != tt.want {
				t.Errorf("ProviderOf = %q, want %q", got, tt.want)
			}
			if tt.wantModel != "" && ModelOf(cfg) != tt.wantModel {
				t.Errorf("ModelOf = %q, want %q", ModelOf(cfg), tt.wantModel)
			}
			ok, why := Available(cfg)
			if ok != tt.available || (!ok && !strings.Contains(why, tt.wantReasonHas)) {
				t.Errorf("Available = %v, %q", ok, why)
			}
			if b := New(cfg).(*apiBackend); b.provider.name != tt.want {
				t.Errorf("New picked %q", b.provider.name)
			}
		})
	}
}

func TestModelOverrideAndDescribe(t *testing.T) {
	clearCredentials(t)
	t.Setenv("OPENAI_API_KEY", "k")
	cfg := config.AgentConfig{Model: "gpt-5.4-mini"}
	if ModelOf(cfg) != "gpt-5.4-mini" || Describe(cfg) != "gpt-5.4-mini (openai)" {
		t.Errorf("ModelOf %q, Describe %q", ModelOf(cfg), Describe(cfg))
	}
	if Describe(config.AgentConfig{Command: "fx ask"}) != "fx ask" {
		t.Error("command description")
	}
	// Every provider's credentials are hidden from tests, not only the selected one's.
	if got := credentialEnv(); !slices.Equal(got, runner.AgentSecrets) {
		t.Errorf("credentialEnv = %v, runner hides %v", got, runner.AgentSecrets)
	}
}

func TestNewReadsProviderEnv(t *testing.T) {
	clearCredentials(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	t.Setenv("OPENROUTER_BASE_URL", "https://gateway.example.com/v1/")
	b := New(config.AgentConfig{}).(*apiBackend)
	if b.baseURL != "https://gateway.example.com/v1" || b.apiKey != "or-key" || b.authToken != "" {
		t.Errorf("backend = %+v", b)
	}
	t.Setenv("OPENAI_API_KEY", "oa-key")
	b = New(config.AgentConfig{}).(*apiBackend)
	if b.provider.name != ProviderOpenAI || b.baseURL != "https://api.openai.com/v1" || b.apiKey != "oa-key" {
		t.Errorf("backend = %+v", b)
	}
}

func TestStrictSchema(t *testing.T) {
	var fns []functionDef
	if err := json.Unmarshal(responsesTools, &fns); err != nil {
		t.Fatal(err)
	}
	strict := map[string]bool{}
	for _, f := range fns {
		strict[f.Name] = f.Strict
	}
	// Optional parameters are not allowed in strict mode.
	want := map[string]bool{toolSubmit: true, toolGitDiff: true, toolGrep: false, toolReadFile: false}
	for name, w := range want {
		if strict[name] != w {
			t.Errorf("%s strict = %v, want %v", name, strict[name], w)
		}
	}
	var chat []chatTool
	if err := json.Unmarshal(chatTools, &chat); err != nil || len(chat) != len(fns) || chat[0].Function.Name != toolListFiles {
		t.Errorf("chat tools %s", chatTools)
	}
}

func newDialectBackend(t *testing.T, name string, replies ...fakeReply) (*apiBackend, *fakeAPI) {
	t.Helper()
	p, _ := lookupProvider(name)
	fake := &fakeAPI{t: t, replies: replies, path: "/v1" + p.dialect.path()}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	b := newProviderBackend(testConfig(), p, srv.URL+"/v1", "test-key", "")
	b.retryBase = time.Millisecond
	b.retryMax = 5 * time.Millisecond
	return b, fake
}

// responsesReply renders a Responses API response with the given output items.
func responsesReply(status string, items ...string) fakeReply {
	return fakeReply{body: fmt.Sprintf(`{"id":"resp_1","object":"response","status":%q,"output":[%s],"usage":{"input_tokens":1100,"input_tokens_details":{"cached_tokens":1000,"cache_write_tokens":0},"output_tokens":20}}`,
		status, strings.Join(items, ","))}
}

func functionCall(callID, name, args string) string {
	b, _ := json.Marshal(args)
	return fmt.Sprintf(`{"type":"function_call","id":"fc_%s","call_id":%q,"name":%q,"arguments":%s,"status":"completed"}`, callID, callID, name, b)
}

func TestResponsesToolLoop(t *testing.T) {
	f := newFixture(t)
	reasoning := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc<&>=="}`
	backend, fake := newDialectBackend(t, ProviderOpenAI,
		responsesReply("completed", reasoning,
			functionCall("c1", "grep", `{"pattern":"seed\\.sh"}`),
			functionCall("c2", "read_file", `{"path":"../etc/passwd"}`)),
		responsesReply("completed", functionCall("c3", "submit_selection", submitTests)),
	)
	cfg := testConfig()
	cfg.Model = "gpt-test"
	d, err := backend.Decide(context.Background(), f.request(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Add) != 1 || d.Add[0].Test != "src/db.test.ts" || d.Model != "gpt-test" {
		t.Fatalf("decision = %+v", d)
	}
	if u := d.Usage; u.Turns != 2 || u.InputTokens != 200 || u.CacheReadTokens != 2000 || u.OutputTokens != 40 {
		t.Errorf("usage = %+v", u)
	}

	reqs := fake.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	first := reqs[0]
	if got := first.header.Get("authorization"); got != "Bearer test-key" {
		t.Errorf("authorization = %q", got)
	}
	for field, want := range map[string]string{"model": `"gpt-test"`, "store": `false`, "reasoning": `{"effort":"low"}`, "tool_choice": `"auto"`, "max_output_tokens": "16000"} {
		if got := string(first.body[field]); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
	if !strings.Contains(string(first.body["instructions"]), "submit_selection") || !strings.Contains(string(first.body["include"]), "reasoning.encrypted_content") {
		t.Error("instructions or include missing")
	}

	var input []map[string]any
	if err := json.Unmarshal(reqs[1].body["input"], &input); err != nil {
		t.Fatal(err)
	}
	// brief, reasoning, two calls, two outputs
	if len(input) != 6 || input[0]["role"] != "user" || input[1]["encrypted_content"] != "enc<&>==" || input[2]["call_id"] != "c1" {
		t.Fatalf("input = %v", input)
	}
	if !strings.Contains(string(reqs[1].raw), `"encrypted_content":"enc<&>=="`) {
		t.Error("reasoning item not echoed verbatim")
	}
	out1, out2 := input[4], input[5]
	if out1["type"] != "function_call_output" || out1["call_id"] != "c1" || !strings.Contains(out1["output"].(string), "src/helpers/db.ts:2:") {
		t.Errorf("grep output = %v", out1)
	}
	if out2["call_id"] != "c2" || !strings.HasPrefix(out2["output"].(string), "Error: ") {
		t.Errorf("read_file error output = %v", out2)
	}
}

func TestResponsesStops(t *testing.T) {
	refusal := `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"no"}]}`
	incomplete := fakeReply{body: `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{}}`}
	text := `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"nothing to do"}]}`
	for name, tt := range map[string]struct {
		rep     fakeReply
		wantErr string
	}{
		"refusal":    {responsesReply("completed", refusal), "refused the request"},
		"incomplete": {incomplete, "max_tokens"},
		"no call":    {responsesReply("completed", text), "model stopped (completed) without calling submit_selection"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			b, _ := newDialectBackend(t, ProviderOpenAI, tt.rep)
			if _, err := b.Decide(context.Background(), f.request(testConfig())); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestEffortDroppedWhenRejected(t *testing.T) {
	f := newFixture(t)
	rejected := fakeReply{status: 400, header: map[string]string{"x-request-id": "req_7"},
		body: `{"error":{"message":"Unsupported parameter: 'reasoning.effort' is not supported with this model.","type":"invalid_request_error","param":"reasoning.effort","code":"unsupported_parameter"}}`}
	noReasoning := func(t *testing.T, r recordedRequest) {
		if _, ok := r.body["reasoning"]; ok {
			t.Error("reasoning sent after the model rejected it")
		}
	}
	turn1 := responsesReply("completed", functionCall("c1", "search_tests", `{"query":"db"}`))
	turn1.inspect = noReasoning
	turn2 := responsesReply("completed", functionCall("c2", "submit_selection", submitTests))
	turn2.inspect = noReasoning
	b, fake := newDialectBackend(t, ProviderOpenAI, rejected, turn1, turn2)
	if _, err := b.Decide(context.Background(), f.request(testConfig())); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n != 3 {
		t.Errorf("%d requests", n)
	}

	// Other 400s are reported as before.
	b, _ = newDialectBackend(t, ProviderOpenAI, fakeReply{status: 400, header: map[string]string{"x-request-id": "req_8"},
		body: `{"error":{"message":"Invalid model","type":"invalid_request_error","param":"model","code":null}}`})
	_, err := b.Decide(context.Background(), f.request(testConfig()))
	if err == nil || !strings.Contains(err.Error(), "openai API: HTTP 400 invalid_request_error: Invalid model (request-id req_8)") {
		t.Errorf("err = %v", err)
	}
}

// chatReply renders a Chat Completions response.
func chatReply(finish, message string) fakeReply {
	return fakeReply{body: fmt.Sprintf(`{"id":"gen-1","object":"chat.completion","choices":[{"index":0,"finish_reason":%q,"message":%s}],"usage":{"prompt_tokens":1150,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":1000,"cache_write_tokens":50}}}`, finish, message)}
}

func chatToolCalls(details string, calls ...[3]string) string {
	var parts []string
	for _, c := range calls {
		args, _ := json.Marshal(c[2])
		parts = append(parts, fmt.Sprintf(`{"type":"function","index":0,"id":%q,"function":{"name":%q,"arguments":%s}}`, c[0], c[1], args))
	}
	msg := `{"role":"assistant","content":null,"refusal":null,"reasoning":null,"tool_calls":[` + strings.Join(parts, ",") + `]`
	if details != "" {
		msg += `,"reasoning_details":` + details
	}
	return msg + "}"
}

func TestChatToolLoop(t *testing.T) {
	f := newFixture(t)
	details := `[{"type":"reasoning.encrypted","data":"sig<&>","id":"r1","format":"anthropic-claude-v1","index":0}]`
	backend, fake := newDialectBackend(t, ProviderOpenRouter,
		chatReply("tool_calls", chatToolCalls(details, [3]string{"t1", "tests_for_file", `{"path":"src/helpers/db.ts"}`})),
		// OpenRouter reports an upstream failure inside a 200 response: retried.
		fakeReply{body: `{"error":{"code":502,"message":"Provider returned error"}}`},
		chatReply("tool_calls", chatToolCalls("", [3]string{"t2", "submit_selection", submitTests})),
	)
	cfg := testConfig()
	cfg.Model = "anthropic/claude-test"
	d, err := backend.Decide(context.Background(), f.request(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Add) != 1 || d.Usage.Turns != 2 || d.Usage.InputTokens != 200 || d.Usage.CacheReadTokens != 2000 || d.Usage.CacheWriteTokens != 100 {
		t.Fatalf("decision = %+v", d)
	}
	reqs := fake.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	first := reqs[0]
	if first.header.Get("authorization") != "Bearer test-key" || first.header.Get("x-title") != "smates" {
		t.Errorf("headers = %v", first.header)
	}
	for field, want := range map[string]string{"model": `"anthropic/claude-test"`, "cache_control": `{"type":"ephemeral"}`, "reasoning": `{"effort":"low"}`, "max_tokens": "16000"} {
		if got := string(first.body[field]); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
	var msgs []map[string]any
	if err := json.Unmarshal(reqs[2].body["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || msgs[0]["role"] != "system" || msgs[1]["role"] != "user" || msgs[2]["role"] != "assistant" || msgs[3]["role"] != "tool" {
		t.Fatalf("messages = %v", msgs)
	}
	if _, ok := msgs[2]["refusal"]; ok {
		t.Error("response-only fields echoed back")
	}
	if !strings.Contains(string(reqs[2].raw), `"reasoning_details":`+details) {
		t.Errorf("reasoning_details not echoed verbatim: %s", reqs[2].raw)
	}
	if msgs[3]["tool_call_id"] != "t1" || !strings.Contains(msgs[3]["content"].(string), "web\tsrc/db.test.ts") {
		t.Errorf("tool message = %v", msgs[3])
	}
}

func TestChatStops(t *testing.T) {
	for name, tt := range map[string]struct {
		rep     fakeReply
		wantErr string
	}{
		"length":  {chatReply("length", `{"role":"assistant","content":"..."}`), "max_tokens"},
		"refusal": {chatReply("stop", `{"role":"assistant","content":null,"refusal":"I can't"}`), "refused"},
		"filter":  {chatReply("content_filter", `{"role":"assistant","content":null}`), "refused the request (content_filter)"},
		"no call": {chatReply("stop", `{"role":"assistant","content":"done"}`), "model stopped (stop)"},
		"400":     {fakeReply{status: 400, body: `{"error":{"message":"nope/nope is not a valid model ID","code":400}}`}, "openrouter API: HTTP 400: nope/nope is not a valid model ID"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			b, _ := newDialectBackend(t, ProviderOpenRouter, tt.rep)
			if _, err := b.Decide(context.Background(), f.request(testConfig())); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestExplainDialects(t *testing.T) {
	answer := `{"file":"scripts/seed.sh","line":4,"reason":"seed.sh now loads fixtures/extra.sql, which does not exist.","confidence":"high"}`
	quoted, _ := json.Marshal("```json\n" + answer + "\n```")
	tests := map[string]struct {
		rep       fakeReply
		formatKey string
		format    string
	}{
		ProviderOpenAI: {
			rep:       responsesReply("completed", `{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":`+string(quoted)+`}]}`),
			formatKey: "text", format: `"format":{"type":"json_schema","name":"explanation","schema":`,
		},
		ProviderOpenRouter: {
			rep:       chatReply("stop", `{"role":"assistant","content":`+string(quoted)+`}`),
			formatKey: "response_format", format: `"json_schema":{"name":"explanation","strict":true,"schema":`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tt.rep.inspect = func(t *testing.T, r recordedRequest) {
				if !strings.Contains(string(r.body[tt.formatKey]), tt.format) {
					t.Errorf("%s = %s", tt.formatKey, r.body[tt.formatKey])
				}
				if _, ok := r.body["tools"]; ok {
					t.Error("explanations must not offer tools")
				}
				if !strings.Contains(string(r.raw), "psql < fixtures/extra.sql") {
					t.Error("brief missing")
				}
			}
			b, _ := newDialectBackend(t, name, tt.rep)
			b.cfg.Model = ""
			req := f.explainRequest()
			req.Config.Model = ""
			ex, usage, err := b.explain(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if ex.File != "scripts/seed.sh" || ex.Line != 4 || ex.Model != b.provider.model || usage.Turns != 1 {
				t.Errorf("explanation %+v, usage %+v", ex, usage)
			}
		})
	}
}

func TestEffortRejectedByOpenAI(t *testing.T) {
	tests := []struct {
		name string
		err  APIError
		want bool
	}{
		{"responses param", APIError{Status: 400, Param: "reasoning.effort", Message: "Unsupported parameter: 'reasoning.effort' is not supported with this model."}, true},
		{"chat argument", APIError{Status: 400, Message: "Unrecognized request argument supplied: reasoning_effort"}, true},
		{"reasoning item", APIError{Status: 400, Param: "input", Message: "Item 'rs_1' of type 'reasoning' was provided without its required following item."}, false},
		{"thinking signature", APIError{Status: 400, Message: "Invalid reasoning_details: thought signature is missing"}, false},
		{"other status", APIError{Status: 500, Param: "reasoning.effort"}, false},
	}
	for _, tt := range tests {
		if got := effortRejectedByOpenAI(&tt.err); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A rejected effort is remembered across backends, so each explanation does not first
// fail on it.
func TestEffortRejectionRemembered(t *testing.T) {
	f := newFixture(t)
	answer, _ := json.Marshal(`{"file":"scripts/seed.sh","line":4,"reason":"r","confidence":"high"}`)
	ok := responsesReply("completed", `{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":`+string(answer)+`}]}`)
	second := ok
	second.inspect = func(t *testing.T, r recordedRequest) {
		if _, sent := r.body["reasoning"]; sent {
			t.Error("effort sent again after the model rejected it")
		}
	}
	fake := &fakeAPI{t: t, path: "/v1/responses", replies: []fakeReply{
		{status: 400, body: `{"error":{"message":"Unsupported parameter: 'reasoning.effort' is not supported with this model.","type":"invalid_request_error","param":"reasoning.effort","code":"unsupported_parameter"}}`},
		ok, second,
	}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	p, _ := lookupProvider(ProviderOpenAI)
	req := f.explainRequest()
	req.Config.Model = "no-reasoning-" + t.Name()
	for i := 0; i < 2; i++ {
		b := newProviderBackend(req.Config, p, srv.URL+"/v1", "k", "")
		if _, _, err := b.explain(context.Background(), req); err != nil {
			t.Fatalf("explanation %d: %v", i+1, err)
		}
	}
	if n := len(fake.requests()); n != 3 {
		t.Errorf("%d requests, want 3", n)
	}
}
